#!/bin/sh
# Панель должна укладываться в 60 КБ gzip суммарно.
#
# Бюджет не косметический: файлы отдаёт сам роутер, а страница грузится
# в тот момент, когда с сетью уже что-то не так. Рантайм preact+htm занимает
# 5.3 КБ, остальное — на разметку, стили и логику.
#
# Считается gzip, а не сырой размер, потому что демон отдаёт статику
# с Content-Encoding: gzip — это заодно экономит CPU роутера.
set -eu

limit=61440
warn=49152

if [ ! -d web ]; then
	echo "-- check-web-size: web/ ещё нет, пропуск"
	exit 0
fi

total=0
found=0
for f in $(find web -type f \( -name '*.html' -o -name '*.css' -o -name '*.js' \) \
	-not -path 'web/mock/*' -not -name 'mock-server.*' | sort); do
	n=$(gzip -c "$f" | wc -c | tr -d ' ')
	total=$((total + n))
	found=$((found + 1))
	printf '   %-52s %6s Б gzip\n' "$f" "$n"
done

if [ "$found" -eq 0 ]; then
	echo "-- check-web-size: файлов панели ещё нет, пропуск"
	exit 0
fi

kb=$(awk "BEGIN{printf \"%.1f\", $total/1024}")

if [ "$total" -gt "$limit" ]; then
	echo "check-web-size: итого ${kb} КБ gzip, предел 60 КБ — ПРЕВЫШЕН"
	exit 1
fi

if [ "$total" -gt "$warn" ]; then
	echo "-- check-web-size: итого ${kb} КБ gzip (внимание: порог 48 КБ)"
else
	echo "-- check-web-size: итого ${kb} КБ gzip"
fi
