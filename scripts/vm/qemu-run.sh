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

TCG=no
case "$(uname -s)/$(uname -m)" in
	Darwin/arm64) ACCEL="-accel hvf -cpu host" ;;
	Linux/aarch64) if [ -w /dev/kvm ]; then ACCEL="-accel kvm -cpu host"; else TCG=yes; fi ;;
	*) TCG=yes ;;
esac
[ "$TCG" = no ] || ACCEL="-accel tcg -cpu cortex-a53"

if [ "$TCG" = yes ]; then
	# Под эмуляцией UEFI не используется: прошивка edk2 на каждой тёплой
	# перезагрузке думала от полутора до десяти минут (замерено на x86_64
	# без KVM), а provision.sh перезагружает VM обязательно. Ядро релиза
	# грузится напрямую — то же, что GRUB взял бы с первого раздела диска.
	[ -f "$VM_KERNEL" ] || die "нет ядра $VM_KERNEL — сначала ./scripts/vm/fetch-image.sh"
	BOOT="-kernel $VM_KERNEL"
	APPEND="root=/dev/vda2 rootwait console=ttyAMA0 noinitrd"
else
	# Прошивка UEFI — pflash: код только для чтения (общий), переменные — своя
	# копия у VM. Так же грузит и UTM. Голый `-bios` — только запасной:
	# без переменных в pflash перезагрузка изредка виснет сразу после
	# «Exiting boot services».
	#
	# Пары «код — шаблон переменных»: brew qemu (macOS) и пакет
	# qemu-efi-aarch64 (Debian/Ubuntu).
	FW=''
	VARS_TPL=''
	for pair in \
		"$(brew --prefix qemu 2>/dev/null)/share/qemu/edk2-aarch64-code.fd:$(brew --prefix qemu 2>/dev/null)/share/qemu/edk2-arm-vars.fd" \
		/usr/share/qemu/edk2-aarch64-code.fd:/usr/share/qemu/edk2-arm-vars.fd \
		/usr/share/AAVMF/AAVMF_CODE.fd:/usr/share/AAVMF/AAVMF_VARS.fd; do
		if [ -f "${pair%%:*}" ] && [ -f "${pair#*:}" ]; then
			FW=${pair%%:*} VARS_TPL=${pair#*:}
			break
		fi
	done

	if [ -n "$FW" ]; then
		[ -f "$VM_DIR/qemu-efi-vars.fd" ] || cp "$VARS_TPL" "$VM_DIR/qemu-efi-vars.fd"
		PFLASH="-drive if=pflash,format=raw,readonly=on,file=$FW -drive if=pflash,format=raw,file=$VM_DIR/qemu-efi-vars.fd"
	else
		for f in /usr/share/qemu-efi-aarch64/QEMU_EFI.fd /usr/share/qemu/QEMU_EFI.fd; do
			[ -f "$f" ] && { FW=$f; break; }
		done
		[ -n "$FW" ] || die "не нашёл UEFI-прошивку aarch64 (macOS: brew install qemu; Debian/Ubuntu: apt install qemu-efi-aarch64)"
		echo "  ⚠ UEFI без pflash ($FW): под эмуляцией перезагрузка может зависнуть" >&2
		PFLASH="-bios $FW"
	fi

	BOOT=$PFLASH
	APPEND=
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

# Диск — первым в порядке загрузки (bootindex=0), а у сетевой карты нет
# загрузочного ПЗУ (romfile=). Иначе UEFI по сохранённому порядку сперва
# пробует PXE и HTTP-загрузку по IPv4 и IPv6 — каждая ждёт таймаута, и под
# эмуляцией перезагрузка растягивалась до десяти минут.
# shellcheck disable=SC2086 # ACCEL, PFLASH, OUT — списки аргументов
exec qemu-system-aarch64 -name "$VM_NAME" -M virt $ACCEL \
	-smp "$VM_CPUS" -m "$VM_MEM" $BOOT ${APPEND:+-append "$APPEND"} \
	-drive "if=none,id=hd0,format=qcow2,file=$DISK" \
	-device virtio-blk-pci,drive=hd0,bootindex=0 \
	-netdev "$NETDEV" -device "virtio-net-pci,netdev=n0,romfile=" \
	-device virtio-rng-pci \
	$OUT
