#!/bin/sh
# Автооткат отменён решением владельца.
#
# SPEC §6 требует откат прямым текстом, поэтому следующий читатель спеки —
# человек или агент — попытается «починить недостающее». Этот скрипт и есть
# защита от доброжелательного возврата отменённой функции.
#
# Отменено целиком: tar-снапшоты /etc/config, восстановление, персистентный
# взвод, хук восстановления при загрузке, возврат одной опции при неудаче.
set -eu

fail=0

# Комментарии вырезаются по той же причине, что и в check-b4json.sh:
# объяснение «почему отката нет» обязано быть в коде, а не изгоняться гейтом.
strip_comments() {
	sed 's|\([^:]\)//.*|\1|; s|^\s*//.*||'
}

scan() {
	pattern=$1
	for f in $(find internal cmd -name '*.go' 2>/dev/null); do
		strip_comments < "$f" | grep -niE "$pattern" | sed "s|^|$f:|"
	done
}

hits=$(scan 'SnapshotConfigs|RestoreConfigs|restoreSnapshot|rollbackTimer|pending-rollback' || true)
if [ -n "$hits" ]; then
	echo "check-no-rollback: найдены следы механизма отката:"
	echo "$hits" | sed 's/^/  /'
	fail=1
fi

hits=$(scan 'tar\b.*/etc/config|/etc/config.*\.tar' || true)
if [ -n "$hits" ]; then
	echo "check-no-rollback: найдено архивирование /etc/config:"
	echo "$hits" | sed 's/^/  /'
	fail=1
fi

if [ "$fail" -ne 0 ]; then
	echo ""
	echo "Откат отменён осознанно (см. docs/adr/). Если решение меняется —"
	echo "сначала ADR, потом код."
	exit 1
fi

echo "-- check-no-rollback: следов отката нет"
