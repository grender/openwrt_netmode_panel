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

if [ ! -f "$VM_CACHE/$IMG.gz" ]; then
	curl -fL --progress-bar -o "$VM_CACHE/$IMG.gz.part" "$URL/$IMG.gz"
	mv "$VM_CACHE/$IMG.gz.part" "$VM_CACHE/$IMG.gz"
fi
curl -fsSL -o "$VM_CACHE/sha256sums-$OPENWRT_VERSION" "$URL/sha256sums"

WANT=$(awk -v f="*$IMG.gz" '$2 == f || $2 == substr(f, 2) { print $1 }' "$VM_CACHE/sha256sums-$OPENWRT_VERSION")
[ -n "$WANT" ] || die "в sha256sums нет $IMG.gz"
if command -v sha256sum >/dev/null 2>&1; then
	GOT=$(sha256sum "$VM_CACHE/$IMG.gz" | awk '{print $1}')
else
	GOT=$(shasum -a 256 "$VM_CACHE/$IMG.gz" | awk '{print $1}')
fi
if [ "$GOT" != "$WANT" ]; then
	rm -f "$VM_CACHE/$IMG.gz"
	die "sha256 не совпал (скачанный файл удалён, запустите ещё раз)"
fi
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
