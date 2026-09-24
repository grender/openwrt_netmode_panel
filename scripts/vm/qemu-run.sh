#!/bin/sh
# Запускает тот же диск VM прямо в QEMU, без UTM.
#
#   ./scripts/vm/qemu-run.sh            в этом терминале, консоль OpenWrt здесь же
#                                       (выход из QEMU: Ctrl-A, затем X)
#   ./scripts/vm/qemu-run.sh --daemon   в фоне; консоль пишется в
#                                       build/vm/console.log, pid — build/vm/qemu.pid
#
# Сеть и пробросы — те же, что у UTM-варианта (common.sh), поэтому
# provision.sh, deploy.sh и ssh.sh не различают, чем запущена VM.
set -eu
. "$(dirname "$0")/common.sh"

DAEMON=no
[ "${1:-}" = --daemon ] && DAEMON=yes

need qemu-system-aarch64
check_host

# Диск лежит либо отдельно, либо уже внутри UTM-бандла (utm-create.sh его
# туда переносит). Одновременно UTM и QEMU один qcow2 открывать не должны:
# QEMU сам откажет по блокировке образа.
DISK=$VM_DISK
[ -f "$DISK" ] || DISK=$VM_BUNDLE/Data/disk.qcow2
[ -f "$DISK" ] || die "нет диска — сначала ./scripts/vm/fetch-image.sh"

case "$(uname -s)" in
	Darwin) ACCEL="-accel hvf -cpu host" ;;
	Linux) if [ -w /dev/kvm ] && [ "$(uname -m)" = aarch64 ]; then ACCEL="-accel kvm -cpu host"; else ACCEL="-accel tcg -cpu cortex-a53"; fi ;;
	*) ACCEL="-accel tcg -cpu cortex-a53" ;;
esac

# Прошивка UEFI: код только для чтения — общий, переменные — свои у VM.
FW=
for d in "$(brew --prefix qemu 2>/dev/null)/share/qemu" /usr/share/qemu /usr/share/qemu-efi-aarch64 /usr/share/AAVMF; do
	for f in edk2-aarch64-code.fd QEMU_EFI.fd AAVMF_CODE.fd; do
		[ -f "$d/$f" ] && { FW=$d/$f; break 2; }
	done
done
[ -n "$FW" ] || die "не нашёл UEFI-прошивку aarch64 (brew install qemu)"

if [ -f "$(dirname "$FW")/edk2-arm-vars.fd" ] && [ ! -f "$VM_DIR/qemu-efi-vars.fd" ]; then
	cp "$(dirname "$FW")/edk2-arm-vars.fd" "$VM_DIR/qemu-efi-vars.fd"
fi
if [ -f "$VM_DIR/qemu-efi-vars.fd" ]; then
	PFLASH="-drive if=pflash,format=raw,readonly=on,file=$FW -drive if=pflash,format=raw,file=$VM_DIR/qemu-efi-vars.fd"
else
	PFLASH="-bios $FW"
fi

NETDEV="user,id=n0,net=$VM_NET,host=$VM_NET_HOST,dns=$VM_NET_DNS,dhcpstart=$VM_NET_DHCPSTART"
NETDEV="$NETDEV,hostfwd=tcp:127.0.0.1:$VM_SSH_PORT-$VM_GUEST:$VM_GUEST_SSH"
NETDEV="$NETDEV,hostfwd=tcp:127.0.0.1:$VM_HTTP_PORT-$VM_GUEST:$VM_GUEST_HTTP"

if [ "$DAEMON" = yes ]; then
	if [ -f "$VM_DIR/qemu.pid" ] && kill -0 "$(cat "$VM_DIR/qemu.pid")" 2>/dev/null; then
		echo "  · QEMU уже запущен (pid $(cat "$VM_DIR/qemu.pid"))"
		exit 0
	fi
	OUT="-display none -serial file:$VM_DIR/console.log -monitor none -daemonize -pidfile $VM_DIR/qemu.pid"
else
	OUT="-nographic"
fi

# shellcheck disable=SC2086 # ACCEL, PFLASH, OUT — списки аргументов
exec qemu-system-aarch64 -name "$VM_NAME" -M virt $ACCEL \
	-smp "$VM_CPUS" -m "$VM_MEM" $PFLASH \
	-drive "if=virtio,format=qcow2,file=$DISK" \
	-netdev "$NETDEV" -device "virtio-net-pci,netdev=n0" \
	-device virtio-rng-pci \
	$OUT
