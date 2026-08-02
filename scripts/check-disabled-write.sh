#!/bin/sh
# Инвариант записи фазы 1 (ADR-0009).
#
# Формулировка, которую этот скрипт стережёт:
#
#   Демон никогда не делает станционную секцию включённой. Опция `disabled`
#   записывается ровно в одном месте — при создании секции, и только со
#   значением "1". Ни удалять её, ни менять на другое значение нельзя.
#
# Почему уточнение про создание — не лазейка, а часть инварианта: отсутствие
# опции `disabled` означает ВКЛЮЧЕНО (подтверждено на живом роутере: у
# активной wifinet0 этой опции нет). Значит создать секцию, не написав
# `disabled`, — значит создать включённую станционную секцию, то есть ровно
# то, что инвариант запрещает.
#
# Снятие ограничения — фаза 2, и тогда этот скрипт удаляется вместе с ADR-0009.
set -eu

DIRS=""
for d in internal/wireless internal/manager internal/httpapi; do
	[ -d "$d" ] && DIRS="$DIRS $d"
done

if [ -z "$DIRS" ]; then
	echo "-- check-disabled-write: пакетов записи ещё нет, пропуск"
	exit 0
fi

fail=0

# Удаление опции disabled запрещено всегда: секция без неё считается включённой.
hits=$(grep -rn 'UCIDelete' --include='*.go' $DIRS 2>/dev/null \
	| grep -v '_test\.go:' | grep '"disabled"' || true)
if [ -n "$hits" ]; then
	echo "check-disabled-write: удаление опции disabled запрещено —"
	echo "  секция без неё считается ВКЛЮЧЁННОЙ:"
	echo "$hits" | sed 's/^/  /'
	fail=1
fi

# Запись disabled допустима, но только со значением "1" и только один раз.
writes=$(grep -rn 'UCISet' --include='*.go' $DIRS 2>/dev/null \
	| grep -v '_test\.go:' | grep '"disabled"' || true)

if [ -n "$writes" ]; then
	bad=$(printf '%s\n' "$writes" | grep -v '"1"' || true)
	if [ -n "$bad" ]; then
		echo "check-disabled-write: disabled записывается значением, отличным от \"1\":"
		echo "$bad" | sed 's/^/  /'
		echo "  Включать сеть — это смена upstream, то есть фаза 2."
		fail=1
	fi

	n=$(printf '%s\n' "$writes" | grep -c . || true)
	if [ "$n" -gt 1 ]; then
		echo "check-disabled-write: disabled пишется в $n местах, допустимо одно"
		echo "  (путь создания секции):"
		echo "$writes" | sed 's/^/  /'
		fail=1
	fi
fi

[ "$fail" -ne 0 ] && exit 1

echo "-- check-disabled-write: инвариант фазы 1 соблюдён"
