#!/bin/sh
# Создаёт VM в UTM из build/vm/disk.qcow2 и запускает её.
#
#   ./scripts/vm/utm-create.sh            создать (если ещё нет) и запустить
#   ./scripts/vm/utm-create.sh --no-start только создать и зарегистрировать
#
# Бандл build/vm/netmoded-vm.utm собирается руками — config.plist
# ConfigurationVersion 4, ключи сверены с исходниками UTM
# (Configuration/UTMQemuConfiguration*.swift). UTM открывает его на месте,
# без копирования в свою библиотеку: удалять бандл, пока VM зарегистрирована
# в UTM, нельзя — сначала ./scripts/vm/destroy.sh.
#
# Если UTM откажется открыть бандл (формат поменялся в новой версии UTM),
# тот же диск запускается без UTM: ./scripts/vm/up.sh --qemu.
set -eu
. "$(dirname "$0")/common.sh"

START=yes
[ "${1:-}" = --no-start ] && START=no

[ "$(uname -s)" = Darwin ] || die "UTM есть только на macOS; на этой машине — ./scripts/vm/up.sh --qemu"
[ -x "$UTMCTL" ] || die "нет $UTMCTL — поставьте UTM (https://mac.getutm.app) в /Applications"

utm_has_vm() { "$UTMCTL" status "$VM_NAME" >/dev/null 2>&1; }

# Шаблон EFI-переменных: UTM создаёт его сам только при сохранении VM из
# своего интерфейса, а бандл, собранный снаружи, без него не загрузится.
# Берём тот же файл, что взял бы UTM, — из его ресурсов; запасной — из brew.
efi_vars_template() {
	for f in /Applications/UTM.app/Contents/Resources/qemu/edk2-arm-vars.fd \
		"$(brew --prefix qemu 2>/dev/null)/share/qemu/edk2-arm-vars.fd"; do
		[ -f "$f" ] && { echo "$f"; return 0; }
	done
	return 1
}

if utm_has_vm; then
	echo "  · VM $VM_NAME уже зарегистрирована в UTM"
else
	say "Сборка бандла $VM_BUNDLE"

	if [ -e "$VM_BUNDLE" ]; then
		die "$VM_BUNDLE уже есть, но UTM его не знает — удалите: ./scripts/vm/destroy.sh"
	fi

	# Диск нужен только при сборке: зарегистрированная VM держит его уже
	# внутри бандла, и повторный up.sh не должен на этом спотыкаться.
	[ -f "$VM_DISK" ] || die "нет диска $VM_DISK — сначала ./scripts/vm/fetch-image.sh"
	VARS=$(efi_vars_template) || die "не нашёл edk2-arm-vars.fd ни в UTM.app, ни в brew qemu"
	mkdir -p "$VM_BUNDLE/Data"
	# Диск ПЕРЕНОСИТСЯ в бандл, а не копируется: второй копии qcow2 с
	# другим состоянием быть не должно. qemu-run.sh знает оба места.
	mv "$VM_DISK" "$VM_BUNDLE/Data/disk.qcow2"
	cp "$VARS" "$VM_BUNDLE/Data/efi_vars.fd"
	chmod 644 "$VM_BUNDLE/Data/efi_vars.fd"

	UUID=$(uuidgen)
	DISK_ID=$(uuidgen)
	# Локально администрируемый MAC: младшие биты первого октета 10.
	# BSD od на macOS дописывает пустую строку — без «NF…exit» awk напечатал
	# бы шаблон дважды, и QEMU не принял бы «52:xx:…:xx52:::::».
	MAC=$(od -An -N5 -tx1 /dev/urandom | awk 'NF { printf "52:%s:%s:%s:%s:%s", $1, $2, $3, $4, $5; exit }' | tr a-f A-F)

	cat > "$VM_BUNDLE/config.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Backend</key>
	<string>QEMU</string>
	<key>ConfigurationVersion</key>
	<integer>4</integer>
	<key>Information</key>
	<dict>
		<key>Name</key>
		<string>$VM_NAME</string>
		<key>UUID</key>
		<string>$UUID</string>
		<key>IconCustom</key>
		<false/>
		<key>Icon</key>
		<string>linux</string>
		<key>Notes</key>
		<string>netmoded test rig: OpenWrt $OPENWRT_VERSION armsr/armv8. ssh -p $VM_SSH_PORT root@127.0.0.1, panel http://127.0.0.1:$VM_HTTP_PORT</string>
	</dict>
	<key>System</key>
	<dict>
		<key>Architecture</key>
		<string>aarch64</string>
		<key>Target</key>
		<string>virt</string>
		<key>CPU</key>
		<string>default</string>
		<key>CPUFlagsAdd</key>
		<array/>
		<key>CPUFlagsRemove</key>
		<array/>
		<key>CPUCount</key>
		<integer>$VM_CPUS</integer>
		<key>ForceMulticore</key>
		<false/>
		<key>MemorySize</key>
		<integer>$VM_MEM</integer>
		<key>JITCacheSize</key>
		<integer>0</integer>
	</dict>
	<key>QEMU</key>
	<dict>
		<key>DebugLog</key>
		<false/>
		<key>UEFIBoot</key>
		<true/>
		<key>RNGDevice</key>
		<true/>
		<key>BalloonDevice</key>
		<false/>
		<key>TPMDevice</key>
		<false/>
		<key>Hypervisor</key>
		<true/>
		<key>TSO</key>
		<false/>
		<key>RTCLocalTime</key>
		<false/>
		<key>PS2Controller</key>
		<false/>
		<key>AdditionalArguments</key>
		<array/>
	</dict>
	<key>Input</key>
	<dict>
		<key>UsbBusSupport</key>
		<string>Disabled</string>
		<key>UsbSharing</key>
		<false/>
		<key>MaximumUsbShare</key>
		<integer>3</integer>
	</dict>
	<key>Sharing</key>
	<dict>
		<key>DirectoryShareMode</key>
		<string>None</string>
		<key>DirectoryShareReadOnly</key>
		<false/>
		<key>ClipboardSharing</key>
		<false/>
	</dict>
	<key>Display</key>
	<array/>
	<key>Drive</key>
	<array>
		<dict>
			<key>Identifier</key>
			<string>$DISK_ID</string>
			<key>ImageName</key>
			<string>disk.qcow2</string>
			<key>ImageType</key>
			<string>Disk</string>
			<key>Interface</key>
			<string>VirtIO</string>
			<key>InterfaceVersion</key>
			<integer>1</integer>
			<key>ReadOnly</key>
			<false/>
		</dict>
	</array>
	<key>Network</key>
	<array>
		<dict>
			<key>Mode</key>
			<string>Emulated</string>
			<key>Hardware</key>
			<string>virtio-net-pci</string>
			<key>MacAddress</key>
			<string>$MAC</string>
			<key>IsolateFromHost</key>
			<false/>
			<key>VlanGuestAddress</key>
			<string>$VM_NET</string>
			<key>VlanHostAddress</key>
			<string>$VM_NET_HOST</string>
			<key>VlanDnsServerAddress</key>
			<string>$VM_NET_DNS</string>
			<key>VlanDhcpStartAddress</key>
			<string>$VM_NET_DHCPSTART</string>
			<key>PortForward</key>
			<array>
				<dict>
					<key>Protocol</key>
					<string>TCP</string>
					<key>HostAddress</key>
					<string>127.0.0.1</string>
					<key>HostPort</key>
					<integer>$VM_SSH_PORT</integer>
					<key>GuestAddress</key>
					<string>$VM_GUEST</string>
					<key>GuestPort</key>
					<integer>$VM_GUEST_SSH</integer>
				</dict>
				<dict>
					<key>Protocol</key>
					<string>TCP</string>
					<key>HostAddress</key>
					<string>127.0.0.1</string>
					<key>HostPort</key>
					<integer>$VM_HTTP_PORT</integer>
					<key>GuestAddress</key>
					<string>$VM_GUEST</string>
					<key>GuestPort</key>
					<integer>$VM_GUEST_HTTP</integer>
				</dict>
			</array>
		</dict>
	</array>
	<key>Serial</key>
	<array>
		<dict>
			<key>Mode</key>
			<string>Terminal</string>
			<key>Target</key>
			<string>Auto</string>
			<!-- Обязателен при Mode=Terminal: окно консоли UTM разворачивает
			     его через «terminal!» и без него падает вместе с VM
			     (VMDisplayQemuTerminalWindowController.swift, UTM 4.7.5).
			     Значения — умолчания UTMConfigurationTerminal. -->
			<key>Terminal</key>
			<dict>
				<key>ForegroundColor</key>
				<string>#ffffff</string>
				<key>BackgroundColor</key>
				<string>#000000</string>
				<key>Font</key>
				<string>Menlo</string>
				<key>FontSize</key>
				<integer>12</integer>
				<key>CursorBlink</key>
				<true/>
			</dict>
		</dict>
	</array>
	<key>Sound</key>
	<array/>
</dict>
</plist>
PLIST
	plutil -lint -s "$VM_BUNDLE/config.plist" || die "config.plist не прошёл plutil -lint"
	echo "  ✓ $VM_BUNDLE"

	# Регистрация в UTM: открытие бандла добавляет его в список VM.
	open -g -a UTM "$VM_BUNDLE"
	i=0
	until utm_has_vm; do
		[ "$i" -lt 30 ] || die "UTM не увидел $VM_NAME за 30 с — откройте $VM_BUNDLE в UTM руками"
		sleep 1
		i=$((i + 1))
	done
	echo "  ✓ VM $VM_NAME зарегистрирована в UTM"
fi

[ "$START" = yes ] || exit 0

case "$("$UTMCTL" status "$VM_NAME" 2>/dev/null)" in
	started) echo "  · VM уже запущена" ;;
	*)
		# utmctl выходит с 0, даже если UTM упал на запуске («OSStatus
		# error -609»), — верим только статусу.
		"$UTMCTL" start "$VM_NAME" || true
		i=0
		until [ "$("$UTMCTL" status "$VM_NAME" 2>/dev/null)" = started ]; do
			[ "$i" -lt 15 ] || die "UTM не запустил $VM_NAME — см. ~/Library/Logs/DiagnosticReports/UTM-*.ips; лог QEMU: включите QEMU → DebugLog в $VM_BUNDLE/config.plist"
			sleep 1
			i=$((i + 1))
		done
		echo "  ✓ VM запущена (консоль — окно Serial в UTM)"
		;;
esac
