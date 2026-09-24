#!/bin/sh
# Останавливает тестовую VM — ту, что запущена: в UTM или в QEMU.
set -eu
. "$(dirname "$0")/common.sh"

if [ -f "$VM_DIR/qemu.pid" ] && kill -0 "$(cat "$VM_DIR/qemu.pid")" 2>/dev/null; then
	kill "$(cat "$VM_DIR/qemu.pid")"
	rm -f "$VM_DIR/qemu.pid"
	echo "  ✓ QEMU остановлен"
fi
if [ -x "$UTMCTL" ] && [ "$("$UTMCTL" status "$VM_NAME" 2>/dev/null)" = started ]; then
	"$UTMCTL" stop "$VM_NAME"
	echo "  ✓ UTM: $VM_NAME остановлена"
fi
