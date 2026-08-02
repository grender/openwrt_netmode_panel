#!/bin/sh
# Бинарь должен влезать в 10 МБ (SPEC §1). Предупреждение с 8 МБ.
set -eu

target=${1:-build/netmoded}
limit=10485760
warn=8388608

if [ ! -f "$target" ]; then
	echo "check-size: нет файла $target" >&2
	exit 1
fi

# stat разный на darwin и linux
size=$(stat -f%z "$target" 2>/dev/null || stat -c%s "$target")
mb=$(awk "BEGIN{printf \"%.2f\", $size/1048576}")

if [ "$size" -gt "$limit" ]; then
	echo "check-size: $target = ${mb} МБ, предел 10 МБ — ПРЕВЫШЕН"
	echo "  Причина почти всегда зависимость: сначала check-stdlib.sh."
	echo "  Дальше по порядку: gzip встроенных ассетов + Content-Encoding,"
	echo "  -trimpath, убрать net/http/pprof."
	exit 1
fi

if [ "$size" -gt "$warn" ]; then
	echo "-- check-size: ${mb} МБ (внимание: порог предупреждения 8 МБ)"
else
	echo "-- check-size: ${mb} МБ"
fi
