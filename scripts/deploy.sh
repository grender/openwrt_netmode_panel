#!/bin/sh
# Сборка и доставка netmoded на роутер.
#
#   ./scripts/deploy.sh                 собрать и залить только бинарь
#   ./scripts/deploy.sh --install       полная установка: бинарь, netmode-apply,
#                                       init.d, сид конфига, автозапуск
#   ./scripts/deploy.sh --run           залить и запустить в консоли
#   ./scripts/deploy.sh --host root@10.0.0.1 --port 8089
#
# Вход по паролю: ssh спросит его ОДИН раз. Дальше все команды идут через
# то же соединение (ControlMaster), поэтому повторных запросов не будет.
#
# Что делает фаза 1 на роутере: правит только ВЫКЛЮЧЕННЫЕ секции
# wifi-iface и переключает режим через netmode-apply. Активную сеть не
# трогает, конфиг не откатывает, /etc/b4/b4.json не читает.
set -eu

HOST=root@192.168.9.1
PORT=8088
RUN=no
INSTALL=no
REMOTE=/usr/local/bin/netmoded
APPLY=/usr/local/bin/netmode-apply
INITD=/etc/init.d/netmoded

while [ $# -gt 0 ]; do
	case "$1" in
		--run)     RUN=yes ;;
		--install) INSTALL=yes ;;
		--host)    HOST=$2; shift ;;
		--port)    PORT=$2; shift ;;
		-h|--help) sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
		*) echo "неизвестный аргумент: $1" >&2; exit 2 ;;
	esac
	shift
done

cd "$(dirname "$0")/.."

VER="0.1.0-phase1-$(date +%Y%m%d)"
BIN=build/netmoded

say() { printf '\n\033[1m%s\033[0m\n' "$*"; }

# ─────────── сборка ───────────

say "Сборка $VER"
scripts/sync-panel.sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath \
	-ldflags="-s -w -X main.version=$VER" -o "$BIN" ./cmd/netmoded
scripts/check-size.sh "$BIN"

# ─────────── одно соединение на всё ───────────

CTL=$(mktemp -u /tmp/netmoded-ssh-XXXXXX)
cleanup() { ssh -S "$CTL" -O exit "$HOST" 2>/dev/null || true; }
trap cleanup EXIT INT TERM

say "Подключение к $HOST — введите пароль (спросят один раз)"
ssh -M -S "$CTL" -o ControlPersist=180 -fN "$HOST"

sh_() { ssh -S "$CTL" "$HOST" "$@"; }
put() { sh_ "cat > $2.new && chmod $3 $2.new && mv $2.new $2"; }

# ─────────── проверки до заливки ───────────

say "Проверки на роутере"

ARCH=$(sh_ 'uname -m')
[ "$ARCH" = aarch64 ] || { echo "  ✗ архитектура $ARCH, бинарь под aarch64" >&2; exit 1; }
echo "  ✓ архитектура: $ARCH"

FREE=$(sh_ "df -k /usr/local 2>/dev/null || df -k /" | awk 'NR==2{print $4}')
[ "${FREE:-0}" -ge 7168 ] || { echo "  ✗ свободно ${FREE} КБ, нужно ~7168" >&2; exit 1; }
echo "  ✓ свободно: $((FREE / 1024)) МБ"

# Порт проверяем, только если демон ещё не наш: иначе он же его и держит.
if sh_ "netstat -lnt 2>/dev/null | grep -q ':$PORT '" && ! sh_ "pgrep -f netmoded >/dev/null 2>&1"; then
	echo "  ✗ порт $PORT занят не нами:" >&2
	sh_ "netstat -lntp 2>/dev/null | grep ':$PORT '" >&2
	echo "    укажите другой: --port 8089" >&2
	exit 1
fi
echo "  ✓ порт $PORT свободен либо занят нами"

LAN=$(sh_ "uci get network.lan.ipaddr 2>/dev/null" | sed 's#/.*##')
[ -n "$LAN" ] || { echo "  ✗ демон не стартует без адреса — это защита, а не сбой" >&2; exit 1; }
echo "  ✓ LAN-адрес: $LAN"

# ─────────── заливка ───────────

say "Заливка"

# Останавливаем прошлый экземпляр: поверх работающего файла не записать.
sh_ "[ -x $INITD ] && $INITD stop >/dev/null 2>&1; killall netmoded 2>/dev/null; sleep 0.3; mkdir -p $(dirname $REMOTE)" || true

put - "$REMOTE" 0755 < "$BIN"
echo "  ✓ $REMOTE"

GOT=$(sh_ "$REMOTE -version")
[ "$GOT" = "$VER" ] || { echo "  ✗ версия на роутере '$GOT' ≠ собранной '$VER'" >&2; exit 1; }
echo "  ✓ версия совпала: $GOT"

if [ "$INSTALL" = yes ]; then
	put - "$APPLY" 0755 < files/usr/local/bin/netmode-apply
	echo "  ✓ $APPLY"

	put - "$INITD" 0755 < files/etc/init.d/netmoded
	echo "  ✓ $INITD"

	# Сид конфига НЕ перезаписывает существующий: там уже может быть
	# выбранный режим и правки владельца.
	if sh_ "[ -f /etc/config/netmode ]"; then
		echo "  · /etc/config/netmode уже есть, не трогаем"
	else
		sed "s/option listen '192.168.9.1'/option listen '$LAN'/; s/option port '8088'/option port '$PORT'/" \
			files/etc/config/netmode | sh_ "cat > /etc/config/netmode.new && chmod 0600 /etc/config/netmode.new && mv /etc/config/netmode.new /etc/config/netmode"
		echo "  ✓ /etc/config/netmode (listen=$LAN, port=$PORT, права 0600)"
	fi

	sh_ "$INITD enable" >/dev/null 2>&1 || true
	echo "  ✓ автозапуск включён"
fi

# ─────────── как открыть панель ───────────

say "Готово"

if [ "$RUN" = yes ]; then
	echo "Запускаю в консоли. Ctrl-C останавливает."
	echo
	ssh -S "$CTL" -t "$HOST" "$REMOTE" || true
	echo
	TOKEN=$(sh_ "cat /etc/netmoded/token 2>/dev/null" || true)
	[ -n "$TOKEN" ] && echo "Панель: http://$LAN:$PORT/?token=$TOKEN"
elif [ "$INSTALL" = yes ]; then
	sh_ "$INITD start" >/dev/null 2>&1 || true
	sleep 1
	TOKEN=$(sh_ "cat /etc/netmoded/token 2>/dev/null" || true)
	if [ -n "$TOKEN" ]; then
		echo "Панель: http://$LAN:$PORT/?token=$TOKEN"
	else
		echo "Демон запущен, но токен ещё не создан. Посмотрите: ssh $HOST logread -e netmoded"
	fi
	cat <<EOF

Осталось сделать РУКАМИ — демон чужой crontab не правит:

    ssh $HOST 'crontab -l | grep -v happ2clash | crontab -'

Расписание обновления подписки теперь внутри демона (SPEC §9), и две
копии дёргали бы конвертер вдвое чаще, чем нужно.
EOF
else
	cat <<EOF
Залит только бинарь. Полная установка (init.d, конфиг, автозапуск):

    ./scripts/deploy.sh --install

Запустить вручную и посмотреть лог:

    ssh $HOST $REMOTE
EOF
fi
