#!/bin/sh
# Останавливает тестовую VM — ту, что запущена: в UTM или в QEMU.
set -eu
. "$(dirname "$0")/common.sh"

if [ -f "$VM_DIR/qemu.pid" ] && kill -0 "$(cat "$VM_DIR/qemu.pid")" 2>/dev/null; then
	PID=$(cat "$VM_DIR/qemu.pid")
	kill "$PID"
	# Ждём настоящего выхода: пока процесс жив, он держит проброшенные
	# порты, и следующий запуск QEMU падал бы на «Could not set up host
	# forwarding rule».
	i=0
	while kill -0 "$PID" 2>/dev/null; do
		[ "$i" -lt 20 ] || { kill -9 "$PID" 2>/dev/null || true; break; }
		sleep 0.5
		i=$((i + 1))
	done
	rm -f "$VM_DIR/qemu.pid"
	echo "  ✓ QEMU остановлен"
fi
if [ -x "$UTMCTL" ] && [ "$("$UTMCTL" status "$VM_NAME" 2>/dev/null)" = started ]; then
	"$UTMCTL" stop "$VM_NAME"
	echo "  ✓ UTM: $VM_NAME остановлена"
fi
