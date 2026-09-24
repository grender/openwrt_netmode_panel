#!/bin/sh
# Удаляет тестовую VM целиком: из UTM и с диска.
#
#   ./scripts/vm/destroy.sh         VM и её диск; скачанный образ остаётся в кэше
#   ./scripts/vm/destroy.sh --all   вместе с кэшем образа
set -eu
. "$(dirname "$0")/common.sh"

"$(dirname "$0")/down.sh"

if [ -x "$UTMCTL" ] && "$UTMCTL" status "$VM_NAME" >/dev/null 2>&1; then
	# utmctl delete удаляет и сам бандл — для VM, открытой «на месте», тоже.
	"$UTMCTL" delete "$VM_NAME"
	echo "  ✓ UTM: $VM_NAME удалена"
fi

rm -rf "$VM_BUNDLE" "$VM_DISK" "$VM_DIR/qemu-efi-vars.fd" "$VM_DIR/console.log" "$VM_KNOWN_HOSTS"
[ "${1:-}" = --all ] && rm -rf "$VM_CACHE"
echo "  ✓ build/vm очищен${1:+ (с кэшем)}"
