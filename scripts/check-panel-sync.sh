#!/bin/sh
# internal/httpapi/panel/ обязан быть побайтовой копией web/.
#
# Панель зашита в бинарь через go:embed и читается ОТТУДА, а не с диска.
# Копию делает scripts/sync-panel.sh, но делает её `make build`, а не git:
# правку в web/ можно закоммитить, а копию — забыть. Тогда сборка возьмёт
# старые файлы, роутер отдаст прежнюю панель, и владелец будет чинить то,
# что уже починено, — ровно про это предупреждает web/README.md.
#
# TestPanelIsEmbedded ловит только ОТСУТСТВИЕ каталога. Рассинхрон не ловил
# никто: правка панели проходила все одиннадцать гвардов зелёной.
#
# Проверок ДВЕ, и это не дублирование. Идут в том же порядке, что и ниже
# по тексту: сначала дерево, потом коммит.
#
#   1. В РАБОЧЕМ ДЕРЕВЕ. Подсказка тому, кто запускает скрипт руками перед
#      коммитом: правку видно до того, как она станет историей. Внутри
#      `make verify` эта проверка вырождена — verify зависит от build, build
#      от panel, то есть синк уже прошёл и файлы на диске совпадают всегда.
#      Зато `make checks` в одиночку зависимостей не имеет и проверяет честно.
#   2. В КОММИТЕ. Главная. HEAD от только что сделанного синка не зависит —
#      значит только эта проверка и способна поймать коммит, в котором копия
#      отстала, а это и есть тот отказ, ради которого скрипт написан.
set -eu

SRC=web
DST=internal/httpapi/panel

if [ ! -d "$SRC" ] || [ ! -d "$DST" ]; then
	echo "-- check-panel-sync: копии ещё нет, пропуск"
	exit 0
fi

# Список берём из самого sync-panel.sh, а не из find по web/: там лежат ещё
# mock-server.mjs и README.md, которые в бинарь не едут и ехать не должны.
# Читаем его аргументы cp, чтобы список не пришлось держать в двух местах:
# разойдись они — гейт стерёг бы не то, что копируется.
FILES=$(grep '^cp ' scripts/sync-panel.sh | tr ' ' '\n' | sed -n 's|^web/||p')

if [ -z "$FILES" ]; then
	echo "check-panel-sync: не удалось вычитать список файлов из scripts/sync-panel.sh" >&2
	echo "  скрипт изменился — почините разбор, а не глушите проверку" >&2
	exit 1
fi

n=$(printf '%s\n' $FILES | wc -l | tr -d ' ')

# Пустой список поймать мало: разбор может УСЕЧЬСЯ. Перепишите в
# sync-panel.sh одну строку `cp web/a web/b` в цикл — и сюда доедет один
# файл вместо пяти. Гвард сузится втрое и останется ЗЕЛЁНЫМ, а полагаться
# на него к тому моменту уже будут. Ложное зелёное хуже отсутствия гварда,
# поэтому сверяем с тем, что реально лежит в копии.
have=$(find "$DST" -type f | wc -l | tr -d ' ')
if [ "$n" -ne "$have" ]; then
	echo "check-panel-sync: разбор дал $n файлов, а в $DST лежит $have" >&2
	echo "  список вычитан из scripts/sync-panel.sh и разошёлся с копией." >&2
	echo "  Почините разбор в check-panel-sync.sh — иначе гвард стережёт не всё." >&2
	exit 1
fi

# ─── 1. рабочее дерево ───

dirty=""
for f in $FILES; do
	if [ ! -f "$DST/$f" ]; then
		dirty="$dirty $f(нет в копии)"
	elif ! cmp -s "$SRC/$f" "$DST/$f"; then
		dirty="$dirty $f"
	fi
done

if [ -n "$dirty" ]; then
	echo "check-panel-sync: копия панели отстала от web/ —$dirty" >&2
	echo "  бинарь отдаст СТАРУЮ панель. Почините и закоммитьте копию:" >&2
	echo "    scripts/sync-panel.sh && git add $DST" >&2
	exit 1
fi

# ─── 2. коммит ───

if ! git rev-parse --verify -q HEAD >/dev/null 2>&1; then
	echo "-- check-panel-sync: копия совпадает с web/ ($n файлов), истории ещё нет"
	exit 0
fi

# Существование проверяем ОТДЕЛЬНО от содержимого: `git show` на пропавшем
# пути молчит и отдаёт пустоту, а git hash-object на пустом входе успешно
# возвращает хеш пустого блоба. Без cat-file пропавший файл выглядел бы как
# пустой — и совпал бы с другим таким же пропавшим.
stale=""
for f in $FILES; do
	git cat-file -e "HEAD:$SRC/$f" 2>/dev/null || { stale="$stale $f(нет в HEAD:$SRC)"; continue; }
	git cat-file -e "HEAD:$DST/$f" 2>/dev/null || { stale="$stale $f(нет в HEAD:$DST)"; continue; }
	a=$(git rev-parse "HEAD:$SRC/$f")
	b=$(git rev-parse "HEAD:$DST/$f")
	[ "$a" = "$b" ] || stale="$stale $f"
done

if [ -n "$stale" ]; then
	echo "check-panel-sync: в HEAD копия панели разошлась с web/ —$stale" >&2
	echo "  этот коммит собирается со СТАРОЙ панелью. Почините:" >&2
	echo "    scripts/sync-panel.sh && git add $DST && git commit --amend" >&2
	exit 1
fi

echo "-- check-panel-sync: копия совпадает с web/ ($n файлов, включая HEAD)"
