#!/bin/sh
# Таксономия причин провала переключения обязана совпадать во всех пяти местах.
#
# Правило родом из ADR-0025 (вердикт выносится по наблюдению, а не по коду
# возврата — отсюда закрытый набор кодов) и ADR-0028 (девятая причина
# stale_draft: черновик UCI застрял, чинится не повтором, а `uci revert
# wireless` по ssh). Контракт кодов описан в docs/api/openapi.yaml, схема
# LastFail.
#
# Почему это гвард, а не договорённость. Над allReasons в
# internal/httpapi/upstreamhandler.go уже лежит подробный комментарий: «у
# каждой новой строки отсюда есть обязательный хвост — перевод в web/i18n.js
# и enum в openapi». Комментарий, который просит человека не забыть, — это не
# инвариант; здесь он не сработал на ПЕРВОМ ЖЕ добавлении. stale_draft приехал
# только в Go, а openapi, обе локали панели, набор REASONS и мок остались на
# восьми. Владелец с застрявшим черновиком получил бы фолбэк «демон прислал
# код, которого эта версия панели не знает — обновите панель», хотя панель и
# демон приехали одной сборкой, — и совет «повторите» вместо единственного
# работающего `uci revert wireless`.
#
# Сверяются ПЯТЬ списков (i18n считается за два — по локали на каждую):
#
#   internal/httpapi/upstreamhandler.go   allReasons — ИСТОЧНИК ПРАВДЫ
#            ↕
#   docs/api/openapi.yaml                 enum у LastFail.reason
#   web/i18n.js                           wifi.fail.<код>.{title,text}, ru и en
#   web/app.js                            набор REASONS
#   web/mock-server.mjs                   коды, которые мок умеет отдавать
#
# Go читается ПО ОБЪЯВЛЕНИЮ: из среза allReasons, а не из употреблений по
# файлу. Причина, не попавшая в срез, для демона не существует (см. там же), и
# сверять надо ровно то, что демон способен отдать.
#
# Асимметрия одна и намеренная: мок — сценарный, он вправе знать не все
# причины. Но ПОДМНОЖЕСТВО, а не пересечение: код, которого нет в Go, мок
# показывать не может — это выдумка, которая не приедет с роутера никогда.
set -eu

GO=internal/httpapi/upstreamhandler.go
SPEC=docs/api/openapi.yaml
I18N=web/i18n.js
APP=web/app.js
MOCK=web/mock-server.mjs

for f in "$GO" "$SPEC" "$I18N" "$APP" "$MOCK"; do
	[ -f "$f" ] || { echo "check-fail-reasons: нет $f" >&2; exit 1; }
done

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail=0

# Нижняя граница разбора. Приём взят у check-panel-sync.sh: если разбор
# перестанет что-то находить, все списки сойдутся на ПУСТОТЕ и гвард
# позеленеет — а полагаться на него к тому моменту уже будут. Ложное зелёное
# хуже отсутствия гварда, поэтому у каждого источника правды есть пол.
#
# Число живёт здесь, а не выводится из Go: вывести его из Go значило бы
# принять пустой allReasons за истину. Растёт таксономия — растёт и эта
# строка, и это единственное место, где ручное обновление уместно: оно
# краснеет само, а не молчит.
MIN=9

# ─── 1. Go: allReasons, по объявлению ───
#
# Срез перечисляет ИДЕНТИФИКАТОРЫ (reasonStaleDraft), а строки лежат в блоке
# const выше. Разрешаем их явно: взять строки грепом по всему файлу значило бы
# читать употребления, а константу можно объявить и не положить в срез — для
# демона её тогда нет.
sed -n '/^var allReasons = \[\]FailReason{/,/^}/p' "$GO" \
	| sed -n 's/^[[:space:]]*\(reason[A-Za-z0-9_]*\),$/\1/p' > "$tmp/go-idents"

nidents=$(wc -l < "$tmp/go-idents" | tr -d ' ')

: > "$tmp/go"
while IFS= read -r id; do
	[ -z "$id" ] && continue
	sed -n "s/^[[:space:]]*$id[[:space:]]\{1,\}FailReason = \"\([a-z0-9_]*\)\".*/\1/p" "$GO" >> "$tmp/go"
done < "$tmp/go-idents"
sort -u -o "$tmp/go" "$tmp/go"

ngo=$(wc -l < "$tmp/go" | tr -d ' ')

if [ "$nidents" -ne "$ngo" ]; then
	echo "check-fail-reasons: разбор сузился — в allReasons $nidents имён, а строк разрешилось $ngo" >&2
	echo "  идентификатор из среза не нашёлся в блоке const $GO." >&2
	echo "  Почините разбор в check-fail-reasons.sh, а не глушите проверку." >&2
	exit 1
fi

if [ "$ngo" -lt "$MIN" ]; then
	echo "check-fail-reasons: разбор сузился — из allReasons в $GO вычитано $ngo причин, ожидалось не меньше $MIN" >&2
	echo "  либо перечисление действительно урезали (тогда правьте MIN и объясняйте почему)," >&2
	echo "  либо разбор перестал его находить — и тогда гвард сверял бы пустоту с пустотой." >&2
	exit 1
fi

# ─── 2. openapi: enum под LastFail.reason ───
#
# Область сужена до схемы LastFail: enum в этом файле не один, и брать первый
# попавшийся значило бы сверяться со списком режимов или состояний джоба.
awk '
	/^    LastFail:/          { lf = 1; next }
	/^    [A-Za-z]/           { lf = 0 }
	lf && /^        reason:/  { r = 1; next }
	lf && /^        [a-z]/    { r = 0; e = 0 }
	r && /^          enum:/   { e = 1; next }
	r && e && /^            - [a-z0-9_]+$/ { sub(/^ *- /, ""); print; next }
	r && e                    { e = 0 }
' "$SPEC" | sort -u > "$tmp/openapi"

# ─── 3. i18n: обе локали, ключ считается только если есть И title, И text ───
#
# Половина пары бесполезна: без .text панель напечатает в теле карточки сам
# ключ «wifi.fail.stale_draft.text».
i18n_slice() {
	awk -v loc="$1" '
		$0 ~ ("^\t" loc ": \\{$")  { on = 1; next }
		on && /^\t[A-Za-z-]+: \{$/ { on = 0 }
		on                         { print }
	' "$I18N"
}

i18n_codes() { # $1 = локаль, $2 = суффикс ключа
	i18n_slice "$1" \
		| sed -n "s/^[[:space:]]*'wifi\.fail\.\([a-z0-9_]*\)\.$2':.*/\1/p" \
		| sort -u
}

for loc in ru en; do
	if [ -z "$(i18n_slice "$loc")" ]; then
		echo "check-fail-reasons: разбор сузился — в $I18N не нашлась локаль '$loc'" >&2
		echo "  файл переформатировали. Почините разбор в check-fail-reasons.sh." >&2
		exit 1
	fi
	i18n_codes "$loc" title > "$tmp/i18n-$loc-title"
	i18n_codes "$loc" text  > "$tmp/i18n-$loc-text"

	half=$(comm -3 "$tmp/i18n-$loc-title" "$tmp/i18n-$loc-text" | tr -d '\t')
	if [ -n "$half" ]; then
		echo "check-fail-reasons: в $I18N ($loc) у причины есть только половина пары title/text:" >&2
		echo "$half" | sed 's/^/    /' >&2
		echo "  панель напечатает недостающую половину как сам ключ. Допишите вторую строку." >&2
		fail=1
	fi
	comm -12 "$tmp/i18n-$loc-title" "$tmp/i18n-$loc-text" > "$tmp/i18n-$loc"
done

# ─── 4. app.js: набор REASONS ───
#
# Литерал переносится по строкам, поэтому берётся весь блок от `new Set([` до
# закрывающей скобки, а уже из него — строки в кавычках.
sed -n '/^const REASONS = new Set(\[/,/\]);/p' "$APP" \
	| sed -n "s/[^']*'\([a-z0-9_]*\)'/\1\n/gp" \
	| sed -n 's/^\([a-z0-9_]\{1,\}\)$/\1/p' | sort -u > "$tmp/app"

# ─── 5. mock: коды, которые мок умеет отдавать ───
sed -n '/^const UPSTREAM_REASON = {/,/^};/p' "$MOCK" \
	| sed -n "s/^[[:space:]]*[^:]*:[[:space:]]*'\([a-z0-9_]*\)'.*/\1/p" | sort -u > "$tmp/mock"

# ─── запасной ключ unknown ───
#
# Это фолбэк ПАНЕЛИ («причина неизвестна, обновите панель»), а не код демона.
# Оба условия проверяются явно, иначе unknown вечно числился бы расхождением:
# в словаре он лишний против Go, а выкинь его из словаря — панель напечатает
# «wifi.fail.unknown.title» тому, кто и так уже видит что-то незнакомое.
#
# Блок стоит ДО анти-вакуума, а не после, и порядок тут содержательный: пока
# unknown лежит в наборе локали, он подпирает счётчик и восьмёрка настоящих
# кодов выглядит девяткой. Ровно этот случай и был на дереве, где гвард
# писался, — пол по i18n не сработал бы.
for loc in ru en; do
	if ! grep -qx unknown "$tmp/i18n-$loc"; then
		echo "check-fail-reasons: в $I18N ($loc) нет запасного ключа wifi.fail.unknown (нужны и .title, и .text)" >&2
		echo "  без него панель на незнакомом коде напечатает в интерфейсе сам ключ." >&2
		fail=1
	fi
done
if grep -qx unknown "$tmp/go"; then
	echo "check-fail-reasons: 'unknown' попал в allReasons в $GO" >&2
	echo "  это фолбэк панели, а не код демона: демон обязан называть причину, а не разводить руками." >&2
	fail=1
fi
# Из словаря убираем — дальше считаются и сверяются только настоящие коды.
for loc in ru en; do
	grep -vx unknown "$tmp/i18n-$loc" > "$tmp/i18n-$loc.codes" || true
	mv "$tmp/i18n-$loc.codes" "$tmp/i18n-$loc"
done

# ─── анти-вакуум для остальных источников правды ───
#
# Мока здесь нет намеренно: он сценарный, его пол — ниже и отдельный.
for pair in "openapi:$SPEC (enum LastFail.reason)" \
	"i18n-ru:$I18N (локаль ru)" \
	"i18n-en:$I18N (локаль en)" \
	"app:$APP (набор REASONS)"; do
	name=${pair%%:*}
	where=${pair#*:}
	n=$(wc -l < "$tmp/$name" | tr -d ' ')
	if [ "$n" -lt "$MIN" ]; then
		echo "check-fail-reasons: разбор сузился — из $where вычитано $n причин, ожидалось не меньше $MIN" >&2
		echo "  либо список действительно урезали, либо разбор перестал его находить." >&2
		echo "  Второе опаснее: сойдясь на пустоте, гвард позеленел бы и перестал стеречь." >&2
		fail=1
	fi
done

nmock=$(wc -l < "$tmp/mock" | tr -d ' ')
if [ "$nmock" -lt 1 ]; then
	echo "check-fail-reasons: разбор сузился — в $MOCK не нашлось ни одной причины в UPSTREAM_REASON" >&2
	echo "  пол у мока — единица, а не $MIN: он сценарный и вправе знать не все причины." >&2
	echo "  Но пустая таблица делает проверку подмножества бессмысленной: пустое множество" >&2
	echo "  подмножество чего угодно, и гвард позеленел бы, ничего не сверив." >&2
	fail=1
fi

# ─── сверка: Go против каждого источника ───

hint() { # $1 = имя источника
	case "$1" in
	openapi)  echo "    добавьте код в enum LastFail.reason в $SPEC, с описанием в тоне соседей" ;;
	i18n-ru)  echo "    добавьте 'wifi.fail.<код>.title' и '.text' в локаль ru в $I18N" ;;
	i18n-en)  echo "    добавьте 'wifi.fail.<код>.title' и '.text' в локаль en в $I18N" ;;
	app)      echo "    добавьте код в набор REASONS в $APP" ;;
	esac
}

for pair in "openapi:$SPEC, enum LastFail.reason" \
	"i18n-ru:$I18N, локаль ru" \
	"i18n-en:$I18N, локаль en" \
	"app:$APP, набор REASONS"; do
	name=${pair%%:*}
	where=${pair#*:}

	missing=$(comm -23 "$tmp/go" "$tmp/$name")
	if [ -n "$missing" ]; then
		echo "check-fail-reasons: причина есть в allReasons ($GO), но её нет в $where:" >&2
		echo "$missing" | sed 's/^/    /' >&2
		hint "$name" >&2
		fail=1
	fi

	extra=$(comm -13 "$tmp/go" "$tmp/$name")
	if [ -n "$extra" ]; then
		echo "check-fail-reasons: код есть в $where, но его нет в allReasons ($GO):" >&2
		echo "$extra" | sed 's/^/    /' >&2
		echo "    демон такого не отдаст. Либо уберите его оттуда, либо заведите причину в allReasons." >&2
		fail=1
	fi
done

# ─── мок: подмножество, а не равенство ───
mockextra=$(comm -13 "$tmp/go" "$tmp/mock")
if [ -n "$mockextra" ]; then
	echo "check-fail-reasons: мок ($MOCK, UPSTREAM_REASON) отдаёт код, которого нет в allReasons ($GO):" >&2
	echo "$mockextra" | sed 's/^/    /' >&2
	echo "    на моке панель отрисует то, чего с роутера не придёт никогда." >&2
	echo "    Уберите код из UPSTREAM_REASON либо заведите причину в allReasons." >&2
	fail=1
fi

[ "$fail" -ne 0 ] && exit 1

echo "-- check-fail-reasons: таксономия сходится в пяти местах ($ngo причин, мок знает $nmock)"
