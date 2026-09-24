# shellcheck shell=sh
# shellcheck disable=SC2034 # переменные читают скрипты, подключающие этот файл
# Общие настройки тестовой VM с OpenWrt (docs/vm-utm.md).
#
# Подключается из остальных scripts/vm/*.sh через `. common.sh`. Всё, что
# должно совпадать между UTM и чистым QEMU — подсеть, пробросы, размер
# памяти, — живёт ТОЛЬКО здесь: разойдись они, VM под UTM и под QEMU
# отвечали бы на разных портах, и deploy.sh стучался бы не туда
# (стережёт scripts/check-vm-scripts.sh).
#
# Любое значение переопределяется переменной окружения с тем же именем.

# Версия образа — та же ветка, что на роутере (SPEC: vanilla OpenWrt 25.12).
: "${OPENWRT_VERSION:=25.12.5}"
: "${OPENWRT_MIRROR:=https://downloads.openwrt.org}"

: "${VM_NAME:=netmoded-vm}"
: "${VM_MEM:=512}"      # МБ
: "${VM_CPUS:=2}"

# Порты на Mac (только 127.0.0.1) → порты в VM.
: "${VM_SSH_PORT:=18022}"
: "${VM_HTTP_PORT:=18088}"

# Сеть QEMU user-net (в UTM — «Emulated VLAN»).
#
# Подсеть совпадает с LAN свежего OpenWrt (192.168.1.1/24 на eth0), а
# пробросы идут на 192.168.1.1. Поэтому ssh работает сразу после первой
# загрузки, без консоли и без правки образа. Шлюз и DNS user-net — .2 и .3:
# их provision.sh прописывает в lan, чтобы apk ходил в интернет.
VM_NET=192.168.1.0/24
VM_NET_HOST=192.168.1.2
VM_NET_DNS=192.168.1.3
VM_NET_DHCPSTART=192.168.1.100
VM_GUEST=192.168.1.1
VM_GUEST_SSH=22
# 8088 — порт демона из сида files/etc/config/netmode.
VM_GUEST_HTTP=8088

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
VM_DIR=$ROOT/build/vm
VM_CACHE=$VM_DIR/cache
VM_DISK=$VM_DIR/disk.qcow2
VM_BUNDLE=$VM_DIR/$VM_NAME.utm
VM_KNOWN_HOSTS=$VM_DIR/known_hosts

UTMCTL=${UTMCTL:-/Applications/UTM.app/Contents/MacOS/utmctl}

say() { printf '\n\033[1m%s\033[0m\n' "$*"; }
die() { echo "  ✗ $*" >&2; exit 1; }

need() {
	for c in "$@"; do
		command -v "$c" >/dev/null 2>&1 || die "нет команды $c (brew install qemu?)"
	done
}

# Стенд рассчитан на Apple Silicon: образ armsr/armv8 и бинарь arm64
# идут под hvf без эмуляции. На Linux/aarch64 чистый QEMU тоже годится
# (с -accel kvm), поэтому здесь только предупреждение.
check_host() {
	case "$(uname -s)/$(uname -m)" in
		Darwin/arm64) ;;
		Linux/aarch64) ;;
		*) echo "  ⚠ $(uname -s)/$(uname -m): стенд рассчитан на Mac с Apple Silicon, VM будет эмулироваться медленно" >&2 ;;
	esac
}

# Хост-ключ VM меняется при каждом пересоздании, поэтому known_hosts свой,
# отдельный от ~/.ssh/known_hosts: иначе после destroy/up ssh отказал бы
# с «REMOTE HOST IDENTIFICATION HAS CHANGED».
VM_SSH_OPTS="-o UserKnownHostsFile=$VM_KNOWN_HOSTS -o StrictHostKeyChecking=accept-new -o LogLevel=ERROR"

vm_ssh() {
	# shellcheck disable=SC2086 # VM_SSH_OPTS — список опций
	ssh -p "$VM_SSH_PORT" $VM_SSH_OPTS root@127.0.0.1 "$@"
}

# Ждём, пока в VM поднимется dropbear. Первая загрузка занимает до минуты.
#
# «Поднялся» — это ответ sshd, а не удачный вход: BatchMode не спрашивает
# пароля, и если образ почему-то его требует, «Permission denied» тоже
# значит «ssh жив». Тогда provision.sh спросит пароль один раз, кладя ключ.
# На свежем OpenWrt у root пароля нет, и dropbear пускает без него.
wait_ssh() {
	printf '  · жду ssh на 127.0.0.1:%s ' "$VM_SSH_PORT"
	i=0
	while [ "$i" -lt "${1:-90}" ]; do
		if out=$(vm_ssh -o ConnectTimeout=2 -o BatchMode=yes true </dev/null 2>&1) ||
			echo "$out" | grep -q 'Permission denied'; then
			echo "— есть"
			return 0
		fi
		printf '.'
		sleep 2
		i=$((i + 1))
	done
	echo
	return 1
}
