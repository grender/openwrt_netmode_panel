#!/bin/sh
# Скачивает образ OpenWrt armsr/armv8 и готовит из него диск VM.
#
#   ./scripts/vm/fetch-image.sh           скачать (из кэша, если есть) и
#                                         сделать build/vm/disk.qcow2
#   ./scripts/vm/fetch-image.sh --force   пересоздать диск (VM начнётся
#                                         с чистого листа)
#
# Версия — OPENWRT_VERSION (по умолчанию см. common.sh).
set -eu
. "$(dirname "$0")/common.sh"

FORCE=no
[ "${1:-}" = --force ] && FORCE=yes

need curl qemu-img gunzip

IMG=openwrt-$OPENWRT_VERSION-armsr-armv8-generic-ext4-combined-efi.img
URL=$OPENWRT_MIRROR/releases/$OPENWRT_VERSION/targets/armsr/armv8
mkdir -p "$VM_CACHE"

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

# Скачивает файл релиза в кэш (если его там нет) и сверяет с sha256sums.
fetch_verified() {
	[ -f "$VM_CACHE/sha256sums-$OPENWRT_VERSION" ] ||
		curl -fsSL -o "$VM_CACHE/sha256sums-$OPENWRT_VERSION" "$URL/sha256sums"
	if [ ! -f "$VM_CACHE/$1" ]; then
		curl -fL --progress-bar -o "$VM_CACHE/$1.part" "$URL/$1"
		mv "$VM_CACHE/$1.part" "$VM_CACHE/$1"
	fi
	want=$(awk -v f="$1" '$2 == "*" f || $2 == f { print $1 }' "$VM_CACHE/sha256sums-$OPENWRT_VERSION")
	[ -n "$want" ] || die "в sha256sums нет $1"
	if [ "$(sha256 "$VM_CACHE/$1")" != "$want" ]; then
		rm -f "$VM_CACHE/$1"
		die "sha256 $1 не совпал (скачанный файл удалён, запустите ещё раз)"
	fi
}

# Ядро отдельным файлом — для запуска без UEFI под эмуляцией (qemu-run.sh:
# там прошивка на каждой перезагрузке думает минутами). Маленькое, поэтому
# качается всегда, даже если диск уже есть.
if [ ! -f "$VM_KERNEL" ]; then
	fetch_verified "$(basename "$VM_KERNEL")"
	echo "  ✓ ядро $(basename "$VM_KERNEL")"
fi

# После utm-create.sh диск живёт внутри UTM-бандла. Пересоздавать его там
# из-под зарегистрированной VM нельзя — сперва destroy.sh.
if [ -f "$VM_BUNDLE/Data/disk.qcow2" ]; then
	[ "$FORCE" = no ] || die "диск уже в UTM-бандле; начать с чистого листа: ./scripts/vm/destroy.sh"
	echo "  · диск уже в UTM-бандле: $VM_BUNDLE"
	exit 0
fi
if [ -f "$VM_DISK" ] && [ "$FORCE" = no ]; then
	echo "  · диск уже есть: $VM_DISK (пересоздать: --force)"
	exit 0
fi

say "Образ OpenWrt $OPENWRT_VERSION (armsr/armv8)"
fetch_verified "$IMG.gz"
echo "  ✓ sha256 совпал"

# Образы OpenWrt дополнены подписью после gzip-потока: gunzip ругается
# «trailing garbage ignored» и выходит с кодом 2. Это норма, а не порча —
# целостность уже проверена хэшем выше.
rc=0
gunzip -c "$VM_CACHE/$IMG.gz" > "$VM_CACHE/$IMG" 2>/dev/null || rc=$?
[ "$rc" -eq 0 ] || [ "$rc" -eq 2 ] || die "gunzip вернул $rc"

mkdir -p "$VM_DIR"
# Размер диска не растягиваем: корневой раздел образа от этого не вырос
# бы, а ~100 МБ корня хватает с запасом (деплою нужно ~7 МБ).
qemu-img convert -f raw -O qcow2 "$VM_CACHE/$IMG" "$VM_DISK.new"
mv "$VM_DISK.new" "$VM_DISK"
rm -f "$VM_CACHE/$IMG"
echo "  ✓ $VM_DISK"
