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
#
# ДВА НАБОРА, а не один. С фазой моста (ADR-0030) таксономий стало две:
# переключение внешней сети (allReasons, wifi.fail.*) и операции проброса
# (allBridgeReasons, bridge.fail.*). Гвард прогоняет ОДНУ И ТУ ЖЕ проверку
# дважды с разными параметрами — копия проверки разошлась бы с оригиналом
# молча, и вторая таксономия стереглась бы хуже первой ровно в тот момент,
# когда о разнице все забыли.
set -eu

SPEC=docs/api/openapi.yaml
I18N=web/i18n.js
APP=web/app.js
MOCK=web/mock-server.mjs

for f in "$SPEC" "$I18N" "$APP" "$MOCK" \
	internal/httpapi/upstreamhandler.go internal/httpapi/bridgehandler.go; do
	[ -f "$f" ] || { echo "check-fail-reasons: нет $f" >&2; exit 1; }
done

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail=0

# ─── одна проверка, параметризованная набором ───
#
# $1 имя набора (для сообщений и имён временных файлов)
# $2 файл Go            $3 имя среза в нём
# $4 имя схемы openapi  $5 префикс ключей i18n
# $6 имя набора в app.js $7 имя таблицы в моке
# $8 пол разбора (MIN)
#
# Нижняя граница разбора у каждого набора своя. Приём взят у
# check-panel-sync.sh: если разбор перестанет что-то находить, все списки
# сойдутся на ПУСТОТЕ и гвард позеленеет — а полагаться на него к тому
# моменту уже будут. Ложное зелёное хуже отсутствия гварда.
#
# Число живёт в вызове, а не выводится из Go: вывести его из Go значило бы
# принять пустой срез за истину. Растёт таксономия — растёт и аргумент, и
# это единственное место, где ручное обновление уместно: оно краснеет само,
# а не молчит.
check_set() {
	name=$1; GO=$2; SLICE=$3; SCHEMA=$4; PREFIX=$5; APPSET=$6; MOCKTAB=$7; MIN=$8
	# Отметка на входе: итог набора обязан говорить правду. Печать «ок»
	# безусловной строкой в конце врала бы поверх собственных же
	# сообщений об ошибках — ровно то, что делает гвард бесполезным на
	# беглом чтении вывода.
	mark=$fail

	# ─── 1. Go: срез, по объявлению ───
	#
	# Срез перечисляет ИДЕНТИФИКАТОРЫ, а строки лежат в блоке const выше.
	# Разрешаем их явно: взять строки грепом по всему файлу значило бы читать
	# употребления, а константу можно объявить и не положить в срез — для
	# демона её тогда нет.
	sed -n "/^var $SLICE = \[\]FailReason{/,/^}/p" "$GO" \
		| sed -n 's/^[[:space:]]*\([A-Za-z][A-Za-z0-9_]*\),$/\1/p' > "$tmp/$name-go-idents"

	nidents=$(wc -l < "$tmp/$name-go-idents" | tr -d ' ')

	: > "$tmp/$name-go"
	while IFS= read -r id; do
		[ -z "$id" ] && continue
		sed -n "s/^[[:space:]]*$id[[:space:]]\{1,\}FailReason = \"\([a-z0-9_]*\)\".*/\1/p" "$GO" >> "$tmp/$name-go"
	done < "$tmp/$name-go-idents"
	sort -u -o "$tmp/$name-go" "$tmp/$name-go"

	ngo=$(wc -l < "$tmp/$name-go" | tr -d ' ')

	if [ "$nidents" -ne "$ngo" ]; then
		echo "check-fail-reasons [$name]: разбор сузился — в $SLICE $nidents имён, а строк разрешилось $ngo" >&2
		echo "  идентификатор из среза не нашёлся в блоке const $GO." >&2
		echo "  Почините разбор в check-fail-reasons.sh, а не глушите проверку." >&2
		exit 1
	fi

	if [ "$ngo" -lt "$MIN" ]; then
		echo "check-fail-reasons [$name]: разбор сузился — из $SLICE в $GO вычитано $ngo причин, ожидалось не меньше $MIN" >&2
		echo "  либо перечисление действительно урезали (тогда правьте вызов и объясняйте почему)," >&2
		echo "  либо разбор перестал его находить — и тогда гвард сверял бы пустоту с пустотой." >&2
		exit 1
	fi

	# ─── 2. openapi: enum под <схема>.reason ───
	#
	# Область сужена до нужной схемы: enum в файле не один, и брать первый
	# попавшийся значило бы сверяться со списком режимов или состояний джоба.
	awk -v schema="    $SCHEMA:" '
		$0 == schema             { lf = 1; next }
		/^    [A-Za-z]/          { lf = 0 }
		lf && /^        reason:/ { r = 1; next }
		lf && /^        [a-z]/   { r = 0; e = 0 }
		r && /^          enum:/  { e = 1; next }
		r && e && /^            - [a-z0-9_]+$/ { sub(/^ *- /, ""); print; next }
		r && e                   { e = 0 }
	' "$SPEC" | sort -u > "$tmp/$name-openapi"

	# ─── 3. i18n: обе локали, ключ считается только если есть И title, И text ───
	#
	# Половина пары бесполезна: без .text панель напечатает в теле карточки
	# сам ключ «<префикс>.stale_draft.text».
	for loc in ru en; do
		if [ -z "$(i18n_slice "$loc")" ]; then
			echo "check-fail-reasons [$name]: разбор сузился — в $I18N не нашлась локаль '$loc'" >&2
			echo "  файл переформатировали. Почините разбор в check-fail-reasons.sh." >&2
			exit 1
		fi
		i18n_codes "$loc" title "$PREFIX" > "$tmp/$name-i18n-$loc-title"
		i18n_codes "$loc" text "$PREFIX"  > "$tmp/$name-i18n-$loc-text"

		half=$(comm -3 "$tmp/$name-i18n-$loc-title" "$tmp/$name-i18n-$loc-text" | tr -d '\t')
		if [ -n "$half" ]; then
			echo "check-fail-reasons [$name]: в $I18N ($loc) у причины есть только половина пары title/text:" >&2
			echo "$half" | sed 's/^/    /' >&2
			echo "  панель напечатает недостающую половину как сам ключ. Допишите вторую строку." >&2
			fail=1
		fi
		comm -12 "$tmp/$name-i18n-$loc-title" "$tmp/$name-i18n-$loc-text" > "$tmp/$name-i18n-$loc"
	done

	# ─── 4. app.js: набор ───
	#
	# Литерал переносится по строкам, поэтому берётся весь блок от `new Set([`
	# до закрывающей скобки, а уже из него — строки в кавычках.
	sed -n "/^const $APPSET = new Set(\[/,/\]);/p" "$APP" \
		| sed -n "s/[^']*'\([a-z0-9_]*\)'/\1\n/gp" \
		| sed -n 's/^\([a-z0-9_]\{1,\}\)$/\1/p' | sort -u > "$tmp/$name-app"

	# ─── 5. mock: коды, которые мок умеет отдавать ───
	sed -n "/^const $MOCKTAB = {/,/^};/p" "$MOCK" \
		| sed -n "s/^[[:space:]]*[^:]*:[[:space:]]*'\([a-z0-9_]*\)'.*/\1/p" | sort -u > "$tmp/$name-mock"

	# ─── запасной ключ unknown ───
	#
	# Это фолбэк ПАНЕЛИ («причина неизвестна, обновите панель»), а не код
	# демона. Оба условия проверяются явно, иначе unknown вечно числился бы
	# расхождением: в словаре он лишний против Go, а выкинь его из словаря —
	# панель напечатает «<префикс>.unknown.title» тому, кто и так уже видит
	# что-то незнакомое.
	#
	# Блок стоит ДО анти-вакуума, и порядок содержательный: пока unknown
	# лежит в наборе локали, он подпирает счётчик, и набор на одну причину
	# короче выглядит полным.
	for loc in ru en; do
		if ! grep -qx unknown "$tmp/$name-i18n-$loc"; then
			echo "check-fail-reasons [$name]: в $I18N ($loc) нет запасного ключа $PREFIX.unknown (нужны и .title, и .text)" >&2
			echo "  без него панель на незнакомом коде напечатает в интерфейсе сам ключ." >&2
			fail=1
		fi
	done
	if grep -qx unknown "$tmp/$name-go"; then
		echo "check-fail-reasons [$name]: 'unknown' попал в $SLICE в $GO" >&2
		echo "  это фолбэк панели, а не код демона: демон обязан называть причину, а не разводить руками." >&2
		fail=1
	fi
	for loc in ru en; do
		grep -vx unknown "$tmp/$name-i18n-$loc" > "$tmp/$name-i18n-$loc.codes" || true
		mv "$tmp/$name-i18n-$loc.codes" "$tmp/$name-i18n-$loc"
	done

	# ─── анти-вакуум для остальных источников правды ───
	#
	# Мока здесь нет намеренно: он сценарный, его пол — ниже и отдельный.
	for pair in "openapi:$SPEC (enum $SCHEMA.reason)" \
		"i18n-ru:$I18N (локаль ru)" \
		"i18n-en:$I18N (локаль en)" \
		"app:$APP (набор $APPSET)"; do
		src=${pair%%:*}
		where=${pair#*:}
		n=$(wc -l < "$tmp/$name-$src" | tr -d ' ')
		if [ "$n" -lt "$MIN" ]; then
			echo "check-fail-reasons [$name]: разбор сузился — из $where вычитано $n причин, ожидалось не меньше $MIN" >&2
			echo "  либо список действительно урезали, либо разбор перестал его находить." >&2
			echo "  Второе опаснее: сойдясь на пустоте, гвард позеленел бы и перестал стеречь." >&2
			fail=1
		fi
	done

	nmock=$(wc -l < "$tmp/$name-mock" | tr -d ' ')
	if [ "$nmock" -lt 1 ]; then
		echo "check-fail-reasons [$name]: разбор сузился — в $MOCK не нашлось ни одной причины в $MOCKTAB" >&2
		echo "  пол у мока — единица, а не $MIN: он сценарный и вправе знать не все причины." >&2
		echo "  Но пустая таблица делает проверку подмножества бессмысленной: пустое множество" >&2
		echo "  подмножество чего угодно, и гвард позеленел бы, ничего не сверив." >&2
		fail=1
	fi

	# ─── сверка: Go против каждого источника ───
	for pair in "openapi:$SPEC, enum $SCHEMA.reason:    добавьте код в enum $SCHEMA.reason в $SPEC, с описанием в тоне соседей" \
		"i18n-ru:$I18N, локаль ru:    добавьте '$PREFIX.<код>.title' и '.text' в локаль ru в $I18N" \
		"i18n-en:$I18N, локаль en:    добавьте '$PREFIX.<код>.title' и '.text' в локаль en в $I18N" \
		"app:$APP, набор $APPSET:    добавьте код в набор $APPSET в $APP"; do
		src=$(printf '%s' "$pair" | cut -d: -f1)
		where=$(printf '%s' "$pair" | cut -d: -f2)
		howto=$(printf '%s' "$pair" | cut -d: -f3-)

		missing=$(comm -23 "$tmp/$name-go" "$tmp/$name-$src")
		if [ -n "$missing" ]; then
			echo "check-fail-reasons [$name]: причина есть в $SLICE ($GO), но её нет в $where:" >&2
			echo "$missing" | sed 's/^/    /' >&2
			echo "$howto" >&2
			fail=1
		fi

		extra=$(comm -13 "$tmp/$name-go" "$tmp/$name-$src")
		if [ -n "$extra" ]; then
			echo "check-fail-reasons [$name]: код есть в $where, но его нет в $SLICE ($GO):" >&2
			echo "$extra" | sed 's/^/    /' >&2
			echo "    демон такого не отдаст. Либо уберите его оттуда, либо заведите причину в $SLICE." >&2
			fail=1
		fi
	done

	# ─── мок: подмножество, а не равенство ───
	mockextra=$(comm -13 "$tmp/$name-go" "$tmp/$name-mock")
	if [ -n "$mockextra" ]; then
		echo "check-fail-reasons [$name]: мок ($MOCK, $MOCKTAB) отдаёт код, которого нет в $SLICE ($GO):" >&2
		echo "$mockextra" | sed 's/^/    /' >&2
		echo "    на моке панель отрисует то, чего с роутера не придёт никогда." >&2
		fail=1
	fi

	if [ "$fail" -eq "$mark" ]; then
		echo "   ок      $name: $ngo причин в пяти местах, мок знает $nmock"
	else
		echo "   ПРОВАЛ  $name: расхождение выше"
	fi
}

# ─── разбор словаря панели ───

i18n_slice() {
	awk -v loc="$1" '
		$0 ~ ("^\t" loc ": \\{$")  { on = 1; next }
		on && /^\t[A-Za-z-]+: \{$/ { on = 0 }
		on                         { print }
	' "$I18N"
}

i18n_codes() { # $1 = локаль, $2 = суффикс ключа, $3 = префикс ключа
	i18n_slice "$1" \
		| sed -n "s/^[[:space:]]*'$3\.\([a-z0-9_]*\)\.$2':.*/\1/p" \
		| sort -u
}

# ─── два прогона ───

check_set upstream internal/httpapi/upstreamhandler.go allReasons \
	LastFail wifi.fail REASONS UPSTREAM_REASON 10

check_set bridge internal/httpapi/bridgehandler.go allBridgeReasons \
	BridgeLastFail bridge.fail BRIDGE_REASONS BRIDGE_REASON 9

[ "$fail" -ne 0 ] && exit 1

echo "-- check-fail-reasons: обе таксономии сходятся в пяти местах каждая"
