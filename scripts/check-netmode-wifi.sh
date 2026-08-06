#!/bin/sh
# netmode-wifi проверяется запуском, а не чтением.
#
# Скрипт — единственное место, где применяется конфигурация wireless после
# смены upstream, и ошибиться в нём дорого: он просит netifd перечитать
# конфиг станционного радио на живом роутере, к которому владелец может быть
# подключён по этой же сети. Go-тесты его не касаются вовсе — они проверяют
# разбор КОДА возврата, а не то, при каких условиях этот код возникает.
#
# Что проверяется по-настоящему:
#   1. синтаксис (sh -n) — опечатка иначе всплывёт на роутере;
#   2. порядок глаголов: `network reload` строго ПЕРЕД `reconf`. Без reload
#      netifd не перечитывает UCI, и reconf возвращает 0, не сделав ничего
#      (docs/recon/apply-verbs.md §6) — то есть самый тихий из возможных
#      провалов ловится только здесь;
#   3. коды возврата из ADR-0027 живым запуском, включая различие
#      «занято» (3) / «оба глагола отказали» (4) / «нет предусловия» (7);
#   4. грепом — что тупой `wifi` не пробрался в скрипт (ADR-0025).
#
# Как подставляется окружение: копия скрипта с переписанным блоком путей.
# Копия, а не переменные окружения в самом скрипте: на роутере он
# запускается от root, и переопределяемый путь к /sbin/wifi был бы дырой
# ради удобства тестов. Полнота подстановки проверяется грепом — иначе тест
# однажды начнёт гонять оригинал и молча пройдёт.
set -eu

SRC=files/usr/local/bin/netmode-wifi

# Имя радио в сценариях НЕ radio0: скрипт не имеет права знать это имя
# заранее (ADR-0019), и тест, гоняющий radio0, не заметил бы захардкоженного
# литерала.
R=radio7

[ -f "$SRC" ] || { echo "check-netmode-wifi: нет $SRC" >&2; exit 1; }

failed=0
mark=0
RC=0

bad() {
	echo "check-netmode-wifi: $*" >&2
	failed=$((failed + 1))
}

# note подводит итог сценария. Отдельно от bad, чтобы провалившийся сценарий
# не отчитывался тут же строкой об успехе.
note() {
	if [ "$failed" -eq "$mark" ]; then
		printf '   ок      %s\n' "$*"
	else
		printf '   ПРОВАЛ  %s\n' "$*"
		mark=$failed
	fi
}

# ── S11. синтаксис ──
#
# Идёт первым, хотя в таблице ADR стоит последним: на неразбираемом скрипте
# остальные десять сценариев проверяли бы поведение шелла, а не наше.
sh -n "$SRC" || { echo "check-netmode-wifi: $SRC не разбирается как sh" >&2; exit 1; }
note "S11 синтаксис (sh -n)"

# ── песочница ──

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT INT TERM

BIN="$TMP/bin"
# Запасной глагол подставляется НЕ в $BIN: он зовётся по абсолютному пути, и
# если однажды поедет через PATH, песочница это заметит.
#
# Каталоги названы так, чтобы не оканчиваться на подменяемые пути: назови мы
# их sbin и lock, проверка полноты подстановки ниже находила бы собственный
# результат и была бы вечно красной (или, хуже, вечно зелёной).
SBIN="$TMP/zapas"
LOCKDIR="$TMP/zamok"
RUN="$TMP/netmode-wifi"
mkdir -p "$BIN" "$SBIN" "$LOCKDIR"

sed \
	-e "s|^LOCK=/var/lock/netmode-wifi\.lock\$|LOCK=$LOCKDIR/netmode-wifi.lock|" \
	-e "s|^WIFI_SBIN=/sbin/wifi\$|WIFI_SBIN=$SBIN/wifi|" \
	"$SRC" >"$RUN"
chmod 0755 "$RUN"

LOCKFILE="$LOCKDIR/netmode-wifi.lock"

# Подстановка обязана быть полной. Недобитый абсолютный путь означал бы, что
# проверка либо трогает настоящий /var/lock, либо молча ничего не проверяет.
# Комментарии из рассмотрения исключены: в шапке скрипта пути упомянуты
# намеренно, и подменять текст объяснения незачем.
leftovers=$(grep -vE '^[[:space:]]*#' "$RUN" | grep -nE '/var/lock|/sbin/wifi' || true)
if [ -n "$leftovers" ]; then
	echo "check-netmode-wifi: подстановка путей неполная — песочница гоняла бы не то, что поедет на роутер:" >&2
	printf '%s\n' "$leftovers" | sed 's/^/           /' >&2
	exit 1
fi
grep -q "^LOCK=$LOCKDIR/" "$RUN" || { echo "check-netmode-wifi: LOCK не подставлен" >&2; exit 1; }
grep -q "^WIFI_SBIN=$SBIN/" "$RUN" || { echo "check-netmode-wifi: WIFI_SBIN не подставлен" >&2; exit 1; }
note "подстановка путей полная"

sh -n "$RUN" || { echo "check-netmode-wifi: копия не разбирается как sh" >&2; exit 1; }

# PATH песочницы содержит ТОЛЬКО то, что скрипту действительно нужно. Так
# «нет flock» воспроизводится удалением одного файла, а не надеждой на то,
# что в системе его нет: на Linux он есть, на macOS его нет, и проверка
# обязана вести себя одинаково.
for u in mkdir dirname; do
	p=$(command -v "$u" || true)
	[ -n "$p" ] || { echo "check-netmode-wifi: в системе нет $u" >&2; exit 1; }
	ln -sf "$p" "$BIN/$u"
done

# logger подставной: дубль диагностики в syslog — часть требования (её
# читают через logread), и проверить её можно только перехватив.
cat >"$BIN/logger" <<EOF
#!/bin/sh
shift 2  # -t netmode-wifi
printf '%s\n' "\$*" >>"$TMP/syslog"
EOF

cat >"$BIN/flock" <<'EOF'
#!/bin/sh
exit "${STUB_FLOCK_RC:-0}"
EOF

# ubus подставной. Пишет argv в журнал действий — без этого нельзя проверить
# ни ПОРЯДОК глаголов, ни то, что device взят из аргумента.
# STUB_UBUS_FAIL — список методов (reload, reconf), которые отказывают.
cat >"$BIN/ubus" <<EOF
#!/bin/sh
printf 'ubus %s\n' "\$*" >>"$TMP/actions"
m=\${3:-}
case " \${STUB_UBUS_FAIL:-} " in
	*" \$m "*) printf 'ubus: %s отказал\n' "\$m" >&2; exit 1 ;;
esac
exit 0
EOF

cat >"$SBIN/wifi" <<EOF
#!/bin/sh
printf 'wifi %s\n' "\$*" >>"$TMP/actions"
[ "\${STUB_WIFI_FAIL:-}" = 1 ] || exit 0
printf 'wifi: отказал\n' >&2
exit 1
EOF

chmod 0755 "$BIN/logger" "$BIN/flock" "$BIN/ubus" "$SBIN/wifi"

# ── оснастка сценариев ──

reset() {
	mark=$failed
	rm -f "$TMP/actions" "$TMP/syslog" "$TMP/out" "$TMP/err" "$LOCKFILE"
	: >"$TMP/actions"
	: >"$TMP/syslog"
	STUB_UBUS_FAIL=""
	STUB_WIFI_FAIL=""
	STUB_FLOCK_RC=0
	export STUB_UBUS_FAIL STUB_WIFI_FAIL STUB_FLOCK_RC
}

run() {
	set +e
	PATH="$BIN" /bin/sh "$RUN" "$@" >"$TMP/out" 2>"$TMP/err"
	RC=$?
	set -e
}

expect_rc() {
	[ "$RC" = "$1" ] || bad "$2: код $RC, ожидался $1 (stderr: $(cat "$TMP/err"))"
}

did() { grep -qF "$1" "$TMP/actions"; }

# before — первое вхождение $1 обязано быть раньше первого вхождения $2.
before() {
	a=$(grep -nF "$1" "$TMP/actions" | head -1 | cut -d: -f1)
	b=$(grep -nF "$2" "$TMP/actions" | head -1 | cut -d: -f1)
	[ -n "$a" ] && [ -n "$b" ] && [ "$a" -lt "$b" ]
}

RELOAD='ubus call network reload'
RECONF="ubus call network.wireless reconf {\"device\":\"$R\"}"

# ── S1. обычное применение ──
#
# Главное здесь не код 0, а ПОРЯДОК: reload раньше reconf. Обратный порядок
# (или reconf в одиночку) даёт rc=0 и не делает ничего — измерено.
reset
run "$R"
expect_rc 0 "обычное применение"
did "$RELOAD" || bad "S1: не было 'ubus call network reload'"
did "$RECONF" || bad "S1: не было reconf с device из аргумента"
before "$RELOAD" "$RECONF" ||
	bad "S1: reconf вызван раньше reload — netifd не перечёл бы UCI, и rc=0 солгал бы"
did "wifi " && bad "S1: запасной глагол вызван при исправном основном"
grep -q 'переключ' "$TMP/out" &&
	bad "S1: скрипт заявил о переключении, хотя судить об исходе не может (ADR-0027)"
note "S1 обычное применение: 0, reload раньше reconf, запасной не звался"

# ── S2. reconf отказал ──
reset
STUB_UBUS_FAIL="reconf"
run "$R"
expect_rc 0 "reconf отказал, запасной прошёл"
did "wifi reconf $R" || bad "S2: запасной глагол не вызван или вызван не с reconf"
grep -q 'reconf' "$TMP/syslog" || bad "S2: обе попытки обязаны быть видны в логе"
note "S2 reconf отказал: запасной 'wifi reconf', 0"

# ── S3. network reload отказал ──
#
# reconf после неудачного reload звать нельзя: он вернёт 0 поверх
# непрочитанного UCI, и провал станет неотличим от успеха.
reset
STUB_UBUS_FAIL="reload"
run "$R"
expect_rc 0 "reload отказал, запасной прошёл"
did "network.wireless" && bad "S3: reconf вызван поверх неудавшегося reload"
did "wifi reconf $R" || bad "S3: после отказа reload не вызван запасной глагол"
note "S3 reload отказал: reconf пропущен, сразу запасной"

# ── S4. ГЛАВНОЕ: оба глагола отказали ──
#
# Конфигурация не применялась. Демон обязан узнать об этом кодом 4 и получить
# ОБЕ причины — а получает он только stderr.
reset
STUB_UBUS_FAIL="reconf"
STUB_WIFI_FAIL=1
run "$R"
expect_rc 4 "оба глагола отказали"
grep -q 'основной глагол' "$TMP/err" || bad "S4: причина отказа основного не попала в stderr"
grep -q 'запасной глагол' "$TMP/err" || bad "S4: причина отказа запасного не попала в stderr"
grep -q 'ОШИБКА' "$TMP/out" && bad "S4: аварийный текст ушёл в stdout — демон его не увидит"
grep -q 'ОШИБКА' "$TMP/syslog" || bad "S4: в syslog не продублировано, по ssh читать будет нечего"
note "S4 оба глагола отказали: 4, обе причины в stderr и не в stdout"

# ── S5. замок занят ──
reset
STUB_FLOCK_RC=1
run "$R"
expect_rc 3 "замок занят"
did "ubus call" && bad "S5: занятый замок не остановил скрипт до первого глагола"
note "S5 замок занят: 3, система не тронута"

# ── S6. нет flock ──
#
# Проверка обязана стоять ДО взятия замка: иначе «нет утилиты» неотличимо от
# «занято». Доказывается тем, что файл замка даже не создан.
reset
mv "$BIN/flock" "$TMP/flock.hidden"
run "$R"
mv "$TMP/flock.hidden" "$BIN/flock"
expect_rc 7 "нет flock"
[ ! -e "$LOCKFILE" ] || bad "S6: файл замка создан — проверка стоит ПОСЛЕ взятия замка"
did "ubus call" && bad "S6: без flock дело дошло до глагола"
note "S6 нет flock: 7, до замка"

# ── S7. нет ubus ──
#
# Именно 7, а не 4: отсутствие основного глагола — это нет предусловия,
# чинится доставкой пакета, а не повтором. Запасной в одиночку узость
# операции не доказывает.
reset
mv "$BIN/ubus" "$TMP/ubus.hidden"
run "$R"
mv "$TMP/ubus.hidden" "$BIN/ubus"
expect_rc 7 "нет ubus"
did "wifi " && bad "S7: без ubus скрипт ушёл в запасной глагол вместо кода 7"
note "S7 нет ubus: 7, а не 4"

# ── S8. нет /sbin/wifi и reconf отказал ──
#
# Зеркало S7: предусловие БЫЛО, запасной глагол необязателен, поэтому его
# отсутствие — это 4 («конфигурация не применялась»), а не 7.
reset
STUB_UBUS_FAIL="reconf"
mv "$SBIN/wifi" "$TMP/wifi.hidden"
run "$R"
mv "$TMP/wifi.hidden" "$SBIN/wifi"
expect_rc 4 "нет запасного глагола"
grep -q 'запасного глагола нет' "$TMP/err" ||
	bad "S8: код 4 без внятного текста — по нему нельзя понять, чего именно не хватило"
note "S8 нет /sbin/wifi при отказавшем reconf: 4, не 7"

# ── S9. плохой аргумент ──
reset
run
expect_rc 1 "без аргумента"
did "ubus call" && bad "S9: без аргумента дело дошло до глагола"

run ""
expect_rc 1 "пустой аргумент"
did "ubus call" && bad "S9: пустой аргумент дошёл до глагола"

run -radio0
expect_rc 1 "аргумент начинается с дефиса"
did "ubus call" && bad "S9: аргумент с дефисом дошёл до глагола"

run "$R" лишний
expect_rc 1 "два аргумента"
did "ubus call" && bad "S9: два аргумента дошли до глагола"
note "S9 плохой аргумент (пусто, -radio0, два): 1, ubus не звался"

# ── S10. статическая: тупой `wifi` запрещён навсегда ──
#
# ADR-0025: `wifi` целиком измеренно роняет домашнюю точку. Запрет держится
# текстом, потому что иначе он держался бы памятью того, кто правит скрипт
# через год. Комментарии из рассмотрения исключены: в шапке запрещённые формы
# перечислены НАМЕРЕННО, и грепу там ловить нечего.
reset
# Собственное имя скрипта (netmode-wifi) и строка блока путей из
# рассмотрения исключены: первое — не глагол, второе подставлено песочницей.
code=$(grep -vE '^[[:space:]]*#' "$RUN" | grep -v '^WIFI_SBIN=' | sed 's/netmode-wifi//g')

offenders=$(printf '%s\n' "$code" | grep -n 'wifi' | grep -v 'wifi reconf' || true)
[ -z "$offenders" ] || {
	bad "S10: 'wifi' встречается в форме, отличной от 'wifi reconf':"
	printf '%s\n' "$offenders" | sed 's/^/           /' >&2
}

# Отдельно — вызовы запасного глагола через переменную: без reconf он
# превращается в тот самый тупой wifi, который роняет домашнюю точку.
offenders=$(printf '%s\n' "$code" | grep -nE '^[[:space:]]*"\$WIFI_SBIN"' | grep -v ' reconf ' || true)
[ -z "$offenders" ] || bad "S10: запасной глагол вызван без подкоманды reconf: $offenders"

for forbidden in 'wifi reload' 'wifi up' 'wifi down' 'network restart' 'init.d/network'; do
	printf '%s\n' "$code" | grep -qF "$forbidden" &&
		bad "S10: в скрипте есть запрещённое навсегда '$forbidden' (ADR-0025)"
done
note "S10 запрещённые глаголы отсутствуют, wifi только как 'wifi reconf'"

if [ "$failed" -ne 0 ]; then
	echo "" >&2
	echo "netmode-wifi ведёт себя не так, как обещает его контракт" >&2
	echo "(коды возврата — в шапке скрипта, ADR-0027 и docs/contracts/executor.md)." >&2
	exit 1
fi

echo "-- check-netmode-wifi: синтаксис и 11 сценариев поведения"
