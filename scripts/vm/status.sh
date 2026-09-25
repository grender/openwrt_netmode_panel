#!/bin/sh
# Состояние тестовой VM одной строкой на поле (desktop/, ADR-0047):
#   backend=utm|qemu|none
#   state=running|stopped|absent
set -eu
. "$(dirname "$0")/common.sh"
B=$(vm_backend)
case "$B" in
	none) S=absent ;;
	qemu)
		if [ -f "$VM_DIR/qemu.pid" ] && kill -0 "$(cat "$VM_DIR/qemu.pid")" 2>/dev/null; then S=running; else S=stopped; fi
		;;
	utm)
		if [ "$("$UTMCTL" status "$VM_NAME" 2>/dev/null)" = started ]; then S=running; else S=stopped; fi
		;;
esac
echo "backend=$B"
echo "state=$S"
