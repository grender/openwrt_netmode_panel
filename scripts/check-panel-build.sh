#!/bin/sh
# internal/httpapi/panel/ обязан быть собран из того исходника, что лежит
# рядом с ним в этом же коммите.
#
# Панель зашита в бинарь через go:embed и читается ОТТУДА, а не с диска.
# Пока артефакт был побайтовой копией, инвариант проверялся через cmp. Как
# только между исходником и артефактом появляется преобразование, сравнивать
# нечего — и утверждение приходится проверять по манифесту происхождения,
# который пишет scripts/build-panel.sh.
#
# TestPanelIsEmbedded ловит только ОТСУТСТВИЕ каталога. Рассинхрон не ловит
# никто, кроме этого скрипта: правка панели проходила все прочие гварды
# зелёной, и это ровно тот отказ, ради которого он написан.
#
# ЯРУСОВ ЧЕТЫРЕ, и это не дублирование:
#
#   Я0 форма       — манифест разбирается, и он описывает ВЕСЬ каталог.
#   Я1 артефакт    — то, что лежит в каталоге, совпадает с манифестом.
#   Я2 исходник    — входы на диске совпадают с записанными в манифест.
#   Я3 коммит      — то же самое, но в HEAD. Главный ярус.
#
# Я3 главный, потому что не зависит ни от чего, что только что сделал make.
# CI в проекте нет: коммит — всё, что видит следующий, и только этот ярус
# способен поймать коммит, в котором артефакт отстал от исходника.
#
# ЗАВИСИМОСТЕЙ НЕТ, и это инвариант, а не совпадение: sh, git, find и один
# из sha256sum/shasum. `make verify` обязан работать на машине, где есть
# только Go и шелл. Node нужен цели panel, но не проверке.
set -eu

DST=internal/httpapi/panel
LOCK=internal/httpapi/panel.lock.json

# Фиксированный набор имён, который в артефакте обязан быть. Меняется вместе
# с формой артефакта — и меняться должен ОСОЗНАННО: это прямой наследник
# растяжки «разбор дал N файлов, а в каталоге лежит M». Манифест, потерявший
# записи, не имеет права совпасть с каталогом, потерявшим те же файлы.
EXPECTED="index.html app.js app.css i18n.js vendor/htm-preact-standalone.module.js"

if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 | cut -d' ' -f1; }
else
	echo "check-panel-build: нет ни sha256sum, ни shasum — проверить нечем" >&2
	echo "  это не повод пропустить проверку: поставьте coreutils" >&2
	exit 1
fi

# ─── Я0: форма ───

[ -f "$LOCK" ] || {
	echo "check-panel-build: нет манифеста $LOCK" >&2
	echo "  соберите панель: make panel" >&2
	exit 1
}
[ -d "$DST" ] || {
	echo "check-panel-build: нет каталога $DST — бинарь соберётся с пустой панелью" >&2
	exit 1
}

# Разбор фиксированного формата, который пишет build-panel.sh: две секции
# по одной записи на строку. Границы секций считаются по строкам-скобкам,
# а не по «первому вхождению», — иначе путь, совпавший с именем секции,
# сдвинул бы разбор молча.
section() { # $1 — имя секции, stdin — манифест
	awk -v want="$1" '
		$0 ~ "^  \"" want "\": \\{$" { inside = 1; next }
		inside && $0 ~ "^  \\}," { exit }
		inside && $0 ~ "^  \\}$"  { exit }
		inside { print }
	'
}
pairs() { sed -n 's|^    "\([^"]*\)": "\([^"]*\)".*$|\1 \2|p'; }

INPUTS=$(section inputs < "$LOCK" | pairs)
ARTIFACT=$(section artifact < "$LOCK" | pairs)
DIGEST=$(sed -n 's|^  "inputs_digest": "\([^"]*\)".*$|\1|p' "$LOCK")

# Антивакуум. Разбор способен УСЕЧЬСЯ: поменяйте отступ в build-panel.sh —
# и сюда доедет ноль записей, а гейт останется зелёным, потому что сверять
# станет нечего. Ложное зелёное хуже отсутствия гварда.
[ -n "$INPUTS" ] || { echo "check-panel-build: в манифесте не разобрано ни одного входа — почините разбор" >&2; exit 1; }
[ -n "$ARTIFACT" ] || { echo "check-panel-build: в манифесте не разобрано ни одного файла артефакта" >&2; exit 1; }
[ -n "$DIGEST" ] || { echo "check-panel-build: в манифесте нет inputs_digest" >&2; exit 1; }

n=$(printf '%s\n' "$ARTIFACT" | wc -l | tr -d ' ')
have=$(find "$DST" -type f | wc -l | tr -d ' ')
if [ "$n" -ne "$have" ]; then
	echo "check-panel-build: манифест описывает $n файлов, в $DST лежит $have" >&2
	echo "  пересоберите панель: make panel" >&2
	exit 1
fi

for f in $EXPECTED; do
	printf '%s\n' "$ARTIFACT" | cut -d' ' -f1 | grep -qxF "$f" || {
		echo "check-panel-build: в артефакте нет обязательного $f" >&2
		echo "  если форма артефакта изменилась намеренно — правьте EXPECTED здесь же" >&2
		exit 1
	}
done

# ─── Я1: артефакт совпадает с манифестом ───
#
# Ловит правку встроенного бандла руками. Прежний гейт этого не умел вовсе:
# он сравнивал копию с исходником, и одинаковая правка в обоих местах
# проходила бы зелёной.

bad=""
printf '%s\n' "$ARTIFACT" | while read -r f h; do
	[ -n "$f" ] || continue
	[ -f "$DST/$f" ] || { echo "  нет файла $DST/$f" >&2; exit 1; }
	got=$(sha256 < "$DST/$f")
	[ "$got" = "$h" ] || { echo "  $f: содержимое разошлось с манифестом" >&2; exit 1; }
done || {
	echo "check-panel-build: артефакт не совпадает с манифестом (см. выше)" >&2
	echo "  пересоберите панель: make panel" >&2
	exit 1
}

# ─── Я2: исходник на диске совпадает с записанным ───

stale=""
printf '%s\n' "$INPUTS" | while read -r f h; do
	[ -n "$f" ] || continue
	[ -f "$f" ] || { echo "  входа $f больше нет" >&2; exit 1; }
	got=$(git hash-object "$f")
	[ "$got" = "$h" ] || { echo "  $f правлен после сборки" >&2; exit 1; }
done || {
	echo "check-panel-build: исходник панели правлен, а панель не пересобрана (см. выше)" >&2
	echo "  бинарь отдаст СТАРУЮ панель. Почините:" >&2
	echo "    make panel && git add $DST $LOCK" >&2
	exit 1
}

# ─── Я3: коммит ───

if ! git rev-parse --verify -q HEAD >/dev/null 2>&1; then
	echo "-- check-panel-build: панель собрана из своего исходника ($have файлов), истории ещё нет"
	exit 0
fi

git cat-file -e "HEAD:$LOCK" 2>/dev/null || {
	echo "check-panel-build: манифеста нет в HEAD — закоммитьте $LOCK" >&2
	exit 1
}

# Манифест берётся ИЗ КОММИТА, а не с диска: сверять коммит с рабочим
# деревом значило бы доверять тому, что только что сделал make.
HEAD_LOCK=$(git cat-file blob "HEAD:$LOCK")
HEAD_INPUTS=$(printf '%s\n' "$HEAD_LOCK" | section inputs | pairs)
HEAD_ARTIFACT=$(printf '%s\n' "$HEAD_LOCK" | section artifact | pairs)

[ -n "$HEAD_INPUTS" ] || { echo "check-panel-build: в HEAD-манифесте не разобрано ни одного входа" >&2; exit 1; }
[ -n "$HEAD_ARTIFACT" ] || { echo "check-panel-build: в HEAD-манифесте не разобрано ни одного файла артефакта" >&2; exit 1; }

# Существование проверяется ОТДЕЛЬНО от содержимого: git show на пропавшем
# пути молчит и отдаёт пустоту, а хеш пустого входа успешно считается. Без
# cat-file пропавший файл выглядел бы как пустой и совпал бы с другим таким же.
printf '%s\n' "$HEAD_INPUTS" | while read -r f h; do
	[ -n "$f" ] || continue
	git cat-file -e "HEAD:$f" 2>/dev/null || { echo "  входа $f нет в коммите" >&2; exit 1; }
	got=$(git rev-parse "HEAD:$f")
	[ "$got" = "$h" ] || { echo "  $f в коммите новее, чем панель" >&2; exit 1; }
done || {
	echo "check-panel-build: в HEAD панель собрана не из того исходника, что закоммичен (см. выше)" >&2
	echo "  этот коммит увозит СТАРУЮ панель. Почините:" >&2
	echo "    make panel && git add $DST $LOCK && git commit --amend" >&2
	exit 1
}

printf '%s\n' "$HEAD_ARTIFACT" | while read -r f h; do
	[ -n "$f" ] || continue
	git cat-file -e "HEAD:$DST/$f" 2>/dev/null || { echo "  файла $DST/$f нет в коммите" >&2; exit 1; }
	got=$(git cat-file blob "HEAD:$DST/$f" | sha256)
	[ "$got" = "$h" ] || { echo "  $DST/$f в коммите не тот, что описан манифестом" >&2; exit 1; }
done || {
	echo "check-panel-build: в HEAD артефакт разошёлся со своим манифестом (см. выше)" >&2
	exit 1
}

echo "-- check-panel-build: панель собрана из своего исходника ($have файлов, включая HEAD)"
