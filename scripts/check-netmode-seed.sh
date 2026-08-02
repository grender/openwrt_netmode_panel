#!/bin/sh
# Секция netmode.main обязана быть ИМЕНОВАННОЙ.
#
# Пойманный на живом роутере дефект: `config main` без имени в кавычках
# создаёт анонимную секцию (@main[0]), а весь код — Go и netmode-apply —
# адресует её как netmode.main.*. С анонимной секцией `uci get` падает
# с "Entry not found", `uci set` — с "Invalid argument", и переключение
# режима отказывает мгновенно.
#
# Фикстуры executor в Go-тестах хранят значения в обычной map по строке
# "netmode.main.mode" и не различают именованные/анонимные секции —
# поэтому весь набор тестов проходил, пока дефект был жив. Этот скрипт
# читает сид как текст, а не через фейк, и специально проверяет именно
# то, что фейк проверить не может.
set -eu

SEED=files/etc/config/netmode

[ -f "$SEED" ] || { echo "check-netmode-seed: нет $SEED" >&2; exit 1; }

# Именованная секция типа main: `config main 'main'` (кавычки любые).
if ! grep -qE "^config[[:space:]]+main[[:space:]]+['\"]main['\"]" "$SEED"; then
	echo "check-netmode-seed: секция main в $SEED анонимна или отсутствует"
	echo "  Нужно: config main 'main'"
	echo "  Без имени 'uci get netmode.main.mode' падает с Entry not found,"
	echo "  а 'uci set netmode.main.mode=...' — с Invalid argument."
	exit 1
fi

echo "-- check-netmode-seed: секция main именована"
