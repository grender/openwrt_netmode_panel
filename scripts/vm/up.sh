#!/bin/sh
# Поднимает тестовую VM с OpenWrt и ставит в неё netmoded — одной командой.
#
#   ./scripts/vm/up.sh           через UTM (по умолчанию)
#   ./scripts/vm/up.sh --qemu    через чистый QEMU, в фоне
#
# Шаги: образ → VM → ожидание ssh → provision.sh → deploy.sh --install.
# Повторный запуск безопасен: каждый шаг пропускает сделанное.
#
# После — панель: http://127.0.0.1:18088/?token=… (ссылку печатает deploy),
# ssh: ./scripts/vm/ssh.sh. Подробности: docs/vm-utm.md.
set -eu
HERE=$(dirname "$0")
. "$HERE/common.sh"

RUNNER=utm
case "${1:-}" in
	""|--utm) ;;
	--qemu) RUNNER=qemu ;;
	-h|--help) sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
	*) die "неизвестный аргумент: $1" ;;
esac

check_host
mkdir -p "$VM_DIR"

"$HERE/fetch-image.sh"

say "Запуск VM ($RUNNER)"
if [ "$RUNNER" = utm ]; then
	"$HERE/utm-create.sh"
else
	"$HERE/qemu-run.sh" --daemon
	echo "  ✓ QEMU в фоне, консоль: $VM_DIR/console.log"
fi

"$HERE/provision.sh"

"$HERE/deploy.sh" --install
