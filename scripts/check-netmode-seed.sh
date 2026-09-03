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

# Опция subscription_url обязана быть в сиде и обязана быть ПУСТОЙ.
#
# Присутствие: адрес подписки владелец вводит руками, и единственное место,
# где он про эту опцию узнаёт, — сид с комментарием. Опция, которой нет в
# файле, не существует для владельца: `uci show netmode` её не покажет, и
# расписание будет молчать без объяснения.
#
# Пустота: непустое значение здесь — это чужой идентификатор подписки,
# уехавший в git. Ни один Go-тест этого не поймает: фейки executor читают
# значения из map, а не из этого файла, и любой строке одинаково рады.
if ! grep -qE "^[[:space:]]*option[[:space:]]+subscription_url[[:space:]]" "$SEED"; then
	echo "check-netmode-seed: в $SEED нет опции subscription_url"
	echo "  Нужно: option subscription_url ''"
	echo "  Без неё владелец не узнает, где задавать адрес подписки."
	exit 1
fi

if ! grep -qE "^[[:space:]]*option[[:space:]]+subscription_url[[:space:]]+(''|\"\")[[:space:]]*$" "$SEED"; then
	echo "check-netmode-seed: subscription_url в $SEED не пуст"
	echo "  В сиде адрес подписки — это секрет, уехавший в git."
	echo "  Задавать его надо на роутере:"
	echo "    uci set netmode.main.subscription_url='...' && uci commit netmode"
	exit 1
fi

echo "-- check-netmode-seed: subscription_url на месте и пуст"
