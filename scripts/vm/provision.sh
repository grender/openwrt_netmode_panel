#!/bin/sh
# Первичная настройка свежего OpenWrt в VM — по ssh через проброшенный порт.
#
#   ./scripts/vm/provision.sh           настроить, если ещё не настроено
#   ./scripts/vm/provision.sh --force   настроить заново (WiFi-топология
#                                       пересобирается с нуля)
#
# Что делает:
#   1. кладёт ваш публичный ключ в /etc/dropbear/authorized_keys;
#   2. прописывает lan шлюз и DNS user-net — чтобы apk ходил в интернет;
#   3. ставит flock (нужен netmode-apply/netmode-wifi), mac80211_hwsim и
#      wpad — имитация WiFi-радио;
#   4. строит WiFi-топологию, похожую на роутер (docs/vm-utm.md):
#        radio0 — станция (upstream), как на роутере;
#        radio1 — домашняя точка netmoded-vm-home в lan, как grenderNet;
#        radio2 — «внешний мир»: точки vm-upstream-a (psk2) и
#                 vm-upstream-b (open) в сети vmup с DHCP 10.99.0.0/24.
#
# nikki и b4 здесь НЕ ставятся — их установка будет делом deploy.sh.
#
# Идемпотентен: признак готовности — /etc/netmoded-vm-provisioned в VM.
set -eu
. "$(dirname "$0")/common.sh"

FORCE=no
[ "${1:-}" = --force ] && FORCE=yes

MARK=/etc/netmoded-vm-provisioned

wait_ssh || die "ssh в VM не отвечает (консоль: окно Serial в UTM либо build/vm/console.log)"

if [ "$FORCE" = no ] && vm_ssh "[ -f $MARK ]"; then
	echo "  · VM уже настроена ($MARK), пропуск (заново: --force)"
	exit 0
fi

say "Настройка VM"

# ── 1. ключ ──
KEY=
for k in "$HOME/.ssh/id_ed25519.pub" "$HOME/.ssh/id_ecdsa.pub" "$HOME/.ssh/id_rsa.pub"; do
	[ -f "$k" ] && { KEY=$k; break; }
done
[ -n "$KEY" ] || die "нет публичного ssh-ключа в ~/.ssh — создайте: ssh-keygen -t ed25519"
vm_ssh "mkdir -p /etc/dropbear && touch /etc/dropbear/authorized_keys &&
	chmod 600 /etc/dropbear/authorized_keys &&
	grep -qxF '$(cat "$KEY")' /etc/dropbear/authorized_keys ||
	echo '$(cat "$KEY")' >> /etc/dropbear/authorized_keys"
echo "  ✓ ключ $KEY"

# ── 2. шлюз и DNS ──
# network reload пересобирает lan и на мгновение рвёт наше же соединение,
# поэтому он уходит в фон, а мы переподключаемся.
vm_ssh "uci set network.lan.gateway='$VM_NET_HOST'
	uci -q delete network.lan.dns || true
	uci add_list network.lan.dns='$VM_NET_DNS'
	uci set system.@system[0].hostname='$VM_NAME'
	uci commit network; uci commit system
	echo '$VM_NAME' > /proc/sys/kernel/hostname
	( sleep 1; /etc/init.d/network reload ) >/dev/null 2>&1 </dev/null &"
sleep 4
wait_ssh 30 || die "VM пропала после network reload"
i=0
until vm_ssh "ping -c1 -W2 downloads.openwrt.org >/dev/null 2>&1"; do
	[ "$i" -lt 10 ] || die "из VM нет интернета (шлюз $VM_NET_HOST, DNS $VM_NET_DNS)"
	sleep 2
	i=$((i + 1))
done
echo "  ✓ интернет через $VM_NET_HOST"

# ── 3. пакеты и 4. WiFi ──
# Скрипт уходит в VM целиком через stdin: переменные хоста подставлены
# здесь, всё экранированное \$ исполняется уже на OpenWrt.
vm_ssh 'sh -s' <<REMOTE
set -eu

apk update >/dev/null
PKGS="kmod-mac80211-hwsim"
# wpad ставим, только если никакого ещё нет: разные варианты wpad-* в apk
# конфликтуют друг с другом.
[ -x /usr/sbin/wpad ] || [ -x /usr/sbin/hostapd ] || PKGS="\$PKGS wpad-basic-mbedtls"
command -v flock >/dev/null 2>&1 || PKGS="\$PKGS flock"
apk add \$PKGS
echo "  ✓ пакеты: \$PKGS"

# Три радио вместо двух по умолчанию. Параметр — в строке modules.d, её
# читает kmodloader при загрузке; сейчас модуль перезагружаем сами.
F=\$(grep -l '^mac80211_hwsim' /etc/modules.d/* 2>/dev/null | head -1)
[ -n "\$F" ] || F=/etc/modules.d/60-mac80211-hwsim
echo 'mac80211_hwsim radios=3' > "\$F"
rmmod mac80211_hwsim 2>/dev/null || true
insmod mac80211_hwsim radios=3
i=0
while [ "\$(ls /sys/class/ieee80211 2>/dev/null | wc -l)" -lt 3 ]; do
	[ "\$i" -lt 10 ] || { echo "  ✗ hwsim не создал три phy" >&2; exit 1; }
	sleep 1; i=\$((i + 1))
done
echo "  ✓ радио: \$(ls /sys/class/ieee80211 | tr '\n' ' ')"

# Секции wifi-device генерирует сам OpenWrt (путь, диапазоны), а
# wifi-iface пишем с нуля.
rm -f /etc/config/wireless
wifi config
while uci -q delete wireless.@wifi-iface[0]; do :; done
for r in radio0 radio1 radio2; do
	uci -q get wireless.\$r >/dev/null || { echo "  ✗ нет wireless.\$r после wifi config" >&2; exit 1; }
	uci -q delete wireless.\$r.disabled || true
	uci set wireless.\$r.country='US'
done
# Как на роутере: radio0 — 2.4 ГГц под станцию, radio1 — 5 ГГц под дом.
uci set wireless.radio0.band='2g'; uci set wireless.radio0.channel='6'
uci set wireless.radio1.band='5g'; uci set wireless.radio1.channel='36'
uci set wireless.radio2.band='2g'; uci set wireless.radio2.channel='6'

uci batch <<'UCI'
set wireless.home=wifi-iface
set wireless.home.device='radio1'
set wireless.home.mode='ap'
set wireless.home.network='lan'
set wireless.home.ssid='netmoded-vm-home'
set wireless.home.encryption='psk2'
set wireless.home.key='netmoded-vm'
set wireless.upa=wifi-iface
set wireless.upa.device='radio2'
set wireless.upa.mode='ap'
set wireless.upa.network='vmup'
set wireless.upa.ssid='vm-upstream-a'
set wireless.upa.encryption='psk2'
set wireless.upa.key='upstream-a-pass'
set wireless.upb=wifi-iface
set wireless.upb.device='radio2'
set wireless.upb.mode='ap'
set wireless.upb.network='vmup'
set wireless.upb.ssid='vm-upstream-b'
set wireless.upb.encryption='none'
set wireless.wifinet0=wifi-iface
set wireless.wifinet0.device='radio0'
set wireless.wifinet0.mode='sta'
set wireless.wifinet0.network='wwan'
set wireless.wifinet0.ssid='vm-upstream-a'
set wireless.wifinet0.encryption='psk2'
set wireless.wifinet0.key='upstream-a-pass'
set wireless.wifinet0.disabled='0'
set wireless.wifinet2=wifi-iface
set wireless.wifinet2.device='radio0'
set wireless.wifinet2.mode='sta'
set wireless.wifinet2.network='wwan'
set wireless.wifinet2.ssid='vm-upstream-b'
set wireless.wifinet2.encryption='none'
set wireless.wifinet2.disabled='1'

set network.wwan=interface
set network.wwan.proto='dhcp'
set network.wwan.defaultroute='0'
set network.wwan.peerdns='0'
set network.vmup_dev=device
set network.vmup_dev.type='bridge'
set network.vmup_dev.name='br-vmup'
set network.vmup=interface
set network.vmup.device='br-vmup'
set network.vmup.proto='static'
set network.vmup.ipaddr='10.99.0.1'
set network.vmup.netmask='255.255.255.0'

set dhcp.vmup=dhcp
set dhcp.vmup.interface='vmup'
set dhcp.vmup.start='100'
set dhcp.vmup.limit='50'
set dhcp.vmup.leasetime='1h'
UCI
# «Внешний мир» не раздаёт шлюз и DNS: иначе wwan увёл бы маршрут по
# умолчанию на адрес этой же VM, и интернет через user-net пропал бы.
uci -q delete dhcp.vmup.dhcp_option || true
uci add_list dhcp.vmup.dhcp_option='3'
uci add_list dhcp.vmup.dhcp_option='6'

# wwan — в зону wan, как на роутере; vmup — своя зона, где DHCP разрешён.
WAN=\$(uci show firewall | sed -n "s/^firewall\.\([^.]*\)\.name='wan'\$/\1/p" | head -1)
[ -n "\$WAN" ] && { uci -q del_list firewall.\$WAN.network='wwan' || true; uci add_list firewall.\$WAN.network='wwan'; }
uci -q delete firewall.vmup || true
uci batch <<'UCI'
set firewall.vmup=zone
set firewall.vmup.name='vmup'
set firewall.vmup.network='vmup'
set firewall.vmup.input='ACCEPT'
set firewall.vmup.output='ACCEPT'
set firewall.vmup.forward='REJECT'
UCI

uci commit wireless; uci commit network; uci commit dhcp; uci commit firewall
/etc/init.d/network reload
/etc/init.d/dnsmasq restart >/dev/null 2>&1 || true
/etc/init.d/firewall restart >/dev/null 2>&1 || true
echo "  ✓ WiFi-топология записана"

date > $MARK
REMOTE

# Станция ассоциируется не мгновенно — ждём, но без отказа: это проверка
# для глаз, а не условие установки.
i=0
until vm_ssh "ubus call network.interface.wwan status 2>/dev/null | grep -q '\"up\": true'"; do
	if [ "$i" -ge 15 ]; then
		echo "  ⚠ wwan пока не поднялся (станция radio0 → vm-upstream-a) — проверьте: ./scripts/vm/ssh.sh 'iw dev; ifstatus wwan'" >&2
		break
	fi
	sleep 2
	i=$((i + 1))
done
[ "$i" -ge 15 ] || echo "  ✓ wwan поднят: станция radio0 подключена к vm-upstream-a"

say "VM настроена"
