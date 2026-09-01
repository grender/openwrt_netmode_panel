#!/bin/sh
# netmode-bridge проверяется запуском, а не чтением — по тем же причинам,
# что netmode-wifi: скрипт перечитывает сеть живого роутера, к которому
# владелец подключён по этой же сети, и ошибиться в нём дорого. Go-тесты
# проверяют разбор КОДА возврата, а не условия его возникновения.
#
# Что проверяется по-настоящему:
#   1. синтаксис (sh -n) — опечатка иначе всплывёт на роутере;
#   2. коды возврата из шапки скрипта живым запуском, включая тройку
#      «занято» (3) / «применение не прошло» (4) / «нет предусловия» (7)
#      и отдельный код 8 у probe («не ответил» — факт, а не отказ);
#   3. порядок глаголов применения: network reload строго ПЕРЕД firewall
#      reload, а у access сеть не перечитывается вовсе;
#   4. страховочный стоп relayd при disable — ровно одна ступень, без
#      лестницы из kill'ов (ADR-0025);
#   5. журналом действий ЦЕЛИКОМ — что на путях отказа не появляется
#      лишних глаголов;
#   6. грепом — что запрещённые формы (network restart, wifi-глаголы,
#      eval) не пробрались в скрипт.
#
# Окружение подставляется копией скрипта с переписанным блоком путей —
# копия, а не переменные окружения: на роутере скрипт бежит от root, и
# переопределяемые пути были бы дырой ради удобства тестов. Полнота
# подстановки проверяется грепом.
set -eu

SRC=files/usr/local/bin/netmode-bridge

# Имена в сценариях НЕ eth1 и НЕ phy0.0-sta0: скрипт не имеет права знать
# имена заранее (ADR-0019), и тест с настоящими именами не заметил бы
# захардкоженного литерала.
P=p7
D=w3

[ -f "$SRC" ] || { echo "check-netmode-bridge: нет $SRC" >&2; exit 1; }

failed=0
mark=0
RC=0

bad() {
	echo "check-netmode-bridge: $*" >&2
	failed=$((failed + 1))
}

note() {
	if [ "$failed" -eq "$mark" ]; then
		printf '   ок      %s\n' "$*"
	else
		printf '   ПРОВАЛ  %s\n' "$*"
		mark=$failed
	fi
}

# ── синтаксис — первым: на неразбираемом скрипте остальные сценарии
# проверяли бы поведение шелла, а не наше ──
sh -n "$SRC" || { echo "check-netmode-bridge: $SRC не разбирается как sh" >&2; exit 1; }
note "S0 синтаксис (sh -n)"

# ── песочница ──

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT INT TERM

BIN="$TMP/bin"
# Каталоги названы так, чтобы не оканчиваться на подменяемые пути, иначе
# проверка полноты подстановки находила бы собственный результат.
LOCKDIR="$TMP/zamok"
SYSDIR="$TMP/sysset"
INITDIR="$TMP/inity"
RUN="$TMP/netmode-bridge"
mkdir -p "$BIN" "$LOCKDIR" "$SYSDIR/$P" "$INITDIR"

sed \
	-e "s|^LOCK=/var/lock/netmode-bridge\.lock\$|LOCK=$LOCKDIR/netmode-bridge.lock|" \
	-e "s|^SYSNET=/sys/class/net\$|SYSNET=$SYSDIR|" \
	-e "s|^FW_INIT=/etc/init\.d/firewall\$|FW_INIT=$INITDIR/firewall|" \
	-e "s|^RELAYD_INIT=/etc/init\.d/relayd\$|RELAYD_INIT=$INITDIR/relayd|" \
	"$SRC" >"$RUN"
chmod 0755 "$RUN"

LOCKFILE="$LOCKDIR/netmode-bridge.lock"

# Подстановка обязана быть полной: недобитый абсолютный путь означал бы,
# что песочница трогает настоящую систему или молча гоняет не то.
leftovers=$(grep -vE '^[[:space:]]*#' "$RUN" | grep -nE '/var/lock|/sys/class/net|/etc/init\.d' || true)
if [ -n "$leftovers" ]; then
	echo "check-netmode-bridge: подстановка путей неполная:" >&2
	printf '%s\n' "$leftovers" | sed 's/^/           /' >&2
	exit 1
fi
grep -q "^LOCK=$LOCKDIR/" "$RUN" || { echo "check-netmode-bridge: LOCK не подставлен" >&2; exit 1; }
grep -q "^SYSNET=$SYSDIR\$" "$RUN" || { echo "check-netmode-bridge: SYSNET не подставлен" >&2; exit 1; }
grep -q "^FW_INIT=$INITDIR/" "$RUN" || { echo "check-netmode-bridge: FW_INIT не подставлен" >&2; exit 1; }
grep -q "^RELAYD_INIT=$INITDIR/" "$RUN" || { echo "check-netmode-bridge: RELAYD_INIT не подставлен" >&2; exit 1; }
note "подстановка путей полная"

sh -n "$RUN" || { echo "check-netmode-bridge: копия не разбирается как sh" >&2; exit 1; }

# PATH песочницы содержит только нужное скрипту: «нет apk» воспроизводится
# удалением одного файла, а не надеждой на систему. rm нужен не скрипту,
# а стабам (relayd-init снимает флаг живости) — без него стоп «не работал»
# молча, со 127 в спрятанном stderr.
for u in mkdir dirname cat tr rm; do
	p=$(command -v "$u" || true)
	[ -n "$p" ] || { echo "check-netmode-bridge: в системе нет $u" >&2; exit 1; }
	ln -sf "$p" "$BIN/$u"
done

cat >"$BIN/logger" <<EOF
#!/bin/sh
shift 2  # -t netmode-bridge
printf '%s\n' "\$*" >>"$TMP/syslog"
EOF

cat >"$BIN/flock" <<'EOF'
#!/bin/sh
exit "${STUB_FLOCK_RC:-0}"
EOF

# sleep подставной и мгновенный: настоящая секунда страховочного стопа
# replayd умножилась бы на все сценарии.
cat >"$BIN/sleep" <<EOF
#!/bin/sh
printf 'sleep %s\n' "\$*" >>"$TMP/actions"
EOF

cat >"$BIN/ubus" <<EOF
#!/bin/sh
printf 'ubus %s\n' "\$*" >>"$TMP/actions"
[ "\${STUB_UBUS_RC:-0}" = 0 ] || { printf 'ubus: отказал\n' >&2; exit "\$STUB_UBUS_RC"; }
exit 0
EOF

# apk: `info -e relayd` отвечает по файлу-флагу, `add` — по STUB_APK_ADD_RC.
cat >"$BIN/apk" <<EOF
#!/bin/sh
printf 'apk %s\n' "\$*" >>"$TMP/actions"
case "\$1" in
	info) [ -e "$TMP/relayd.installed" ]; exit \$? ;;
	add) [ "\${STUB_APK_ADD_RC:-0}" = 0 ] || { printf 'apk: фид недоступен\n' >&2; exit "\$STUB_APK_ADD_RC"; }
		: >"$TMP/relayd.installed"; exit 0 ;;
esac
exit 1
EOF

# pidof: живость relayd — файл-флаг, чтобы стоп мог её честно снять.
cat >"$BIN/pidof" <<EOF
#!/bin/sh
printf 'pidof %s\n' "\$*" >>"$TMP/actions"
[ -e "$TMP/relayd.alive" ]
EOF

cat >"$BIN/ping" <<EOF
#!/bin/sh
printf 'ping %s\n' "\$*" >>"$TMP/actions"
exit "\${STUB_PING_RC:-0}"
EOF

cat >"$INITDIR/firewall" <<EOF
#!/bin/sh
printf 'firewall %s\n' "\$*" >>"$TMP/actions"
[ "\${STUB_FW_RC:-0}" = 0 ] || { printf 'fw4: отказал\n' >&2; exit "\$STUB_FW_RC"; }
exit 0
EOF

# Стоп relayd снимает флаг живости, только если ему разрешено «сработать»:
# сценарий кода 5 держит relayd бессмертным.
cat >"$INITDIR/relayd" <<EOF
#!/bin/sh
printf 'relayd-init %s\n' "\$*" >>"$TMP/actions"
[ "\${STUB_RELAYD_STOP_WORKS:-1}" = 1 ] && rm -f "$TMP/relayd.alive"
exit 0
EOF

chmod 0755 "$BIN/logger" "$BIN/flock" "$BIN/sleep" "$BIN/ubus" "$BIN/apk" \
	"$BIN/pidof" "$BIN/ping" "$INITDIR/firewall" "$INITDIR/relayd"

# ── оснастка сценариев ──

reset() {
	mark=$failed
	rm -f "$TMP/actions" "$TMP/syslog" "$TMP/out" "$TMP/err" "$LOCKFILE" \
		"$TMP/relayd.installed" "$TMP/relayd.alive"
	: >"$TMP/actions"
	: >"$TMP/syslog"
	STUB_FLOCK_RC=0 STUB_UBUS_RC=0 STUB_APK_ADD_RC=0 STUB_PING_RC=0
	STUB_FW_RC=0 STUB_RELAYD_STOP_WORKS=1
	export STUB_FLOCK_RC STUB_UBUS_RC STUB_APK_ADD_RC STUB_PING_RC \
		STUB_FW_RC STUB_RELAYD_STOP_WORKS
	# Порт в исправном состоянии: линк есть, гигабит, 42 перескока.
	printf '1\n' >"$SYSDIR/$P/carrier"
	printf '1000\n' >"$SYSDIR/$P/speed"
	printf '42\n' >"$SYSDIR/$P/carrier_changes"
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

before() {
	a=$(grep -nF "$1" "$TMP/actions" | head -1 | cut -d: -f1)
	b=$(grep -nF "$2" "$TMP/actions" | head -1 | cut -d: -f1)
	[ -n "$a" ] && [ -n "$b" ] && [ "$a" -lt "$b" ]
}

# Журнал сверяется ЦЕЛИКОМ: лишний глагол, как бы он ни был записан,
# оставляет лишнюю строку (тот же приём, что S13 у netmode-wifi).
journal_is() {
	want=$1
	shift
	got=$(cat "$TMP/actions")
	[ "$got" = "$want" ] || {
		bad "$*: журнал действий не тот"
		printf 'ожидалось:\n%s\nполучено:\n%s\n' "$want" "$got" | sed 's/^/           /' >&2
	}
}

RELOAD='ubus call network reload'
FW='firewall reload'

# ── S1. status: исправный порт, relayd установлен и жив ──
reset
: >"$TMP/relayd.installed"
: >"$TMP/relayd.alive"
run status "$P"
expect_rc 0 "S1 status"
want='{"relayd_installed":true,"relayd_running":true,"carrier":1,"speed_mbps":1000,"carrier_changes":42}'
[ "$(cat "$TMP/out" | tail -1)" = "$want" ] ||
	bad "S1: JSON не тот: $(cat "$TMP/out" | tail -1)"
note "S1 status: JSON с живым relayd и линком"

# ── S2. status: линк лежит, relayd не установлен ──
#
# carrier нечитаем (EINVAL у выключенного интерфейса) — это «линка нет»,
# а не отказ замера; speed при лежащем линке -1 — это null, а не число.
reset
rm -f "$SYSDIR/$P/carrier"
printf -- '-1\n' >"$SYSDIR/$P/speed"
run status "$P"
expect_rc 0 "S2 status деградированный"
want='{"relayd_installed":false,"relayd_running":false,"carrier":0,"speed_mbps":null,"carrier_changes":42}'
[ "$(cat "$TMP/out" | tail -1)" = "$want" ] ||
	bad "S2: JSON не тот: $(cat "$TMP/out" | tail -1)"
note "S2 status: лежащий линк — carrier 0 и speed null, а не выдумка"

# ── S3. status: нет порта / нет apk — предусловие, код 7 ──
reset
run status net9
expect_rc 7 "S3 нет порта"
reset
mv "$BIN/apk" "$TMP/apk.hidden"
run status "$P"
mv "$TMP/apk.hidden" "$BIN/apk"
expect_rc 7 "S3 нет apk"
note "S3 status: нет порта или apk — 7, а не полуответ"

# ── S4. probe: три исхода тремя кодами ──
reset
run probe 192.0.2.9 "$D"
expect_rc 0 "S4 адрес ответил"
did "ping -c 1 -W 1 -I $D 192.0.2.9" || bad "S4: ping вызван не с теми ключами или не тем адресом"

reset
STUB_PING_RC=1
run probe 192.0.2.9 "$D"
expect_rc 8 "S4 адрес не ответил"

reset
mv "$BIN/ping" "$TMP/ping.hidden"
run probe 192.0.2.9 "$D"
mv "$TMP/ping.hidden" "$BIN/ping"
expect_rc 7 "S4 нет ping"

reset
run probe 'not an ip' "$D"
expect_rc 1 "S4 мусор вместо ip"
did "ping" && bad "S4: мусорный ip дошёл до ping"
note "S4 probe: 0 ответил / 8 молчит / 7 нет ping / 1 мусор"

# ── S5. install: успех, отказ apk, занятый замок ──
reset
run install
expect_rc 0 "S5 install"
did "apk add relayd" || bad "S5: apk add relayd не вызван"

reset
STUB_APK_ADD_RC=3
run install
expect_rc 9 "S5 отказ apk"
grep -q 'фид недоступен' "$TMP/err" || bad "S5: причина apk не дошла до stderr"

reset
STUB_FLOCK_RC=1
run install
expect_rc 3 "S5 замок занят"
did "apk add" && bad "S5: занятый замок не остановил установку"
note "S5 install: 0 / 9 с причиной apk в stderr / 3 без действий"

# ── S6. apply enable: порядок «сеть, потом firewall» и отказы ──
#
# Порядок не украшение: зоны firewall ссылаются на интерфейс homelan,
# который создаёт network reload. Отказ reload прекращает применение —
# перечитывать firewall поверх непересобранной сети значит применить
# правила к сети, которой нет.
reset
run apply enable
expect_rc 0 "S6 enable"
before "$RELOAD" "$FW" || bad "S6: firewall перечитан раньше сети"
journal_is "$RELOAD
$FW" "S6 (enable: ровно два глагола)"

reset
STUB_UBUS_RC=1
run apply enable
expect_rc 4 "S6 reload отказал"
did "firewall" && bad "S6: firewall тронут после отказа network reload"

reset
STUB_FW_RC=1
run apply enable
expect_rc 4 "S6 firewall отказал"
grep -q 'fw4' "$TMP/err" || bad "S6: причина fw4 не дошла до stderr"
note "S6 enable: сеть раньше firewall; отказ любого — 4 без лишних глаголов"

# ── S7. apply access: только firewall, ubus не нужен вовсе ──
reset
mv "$BIN/ubus" "$TMP/ubus.hidden"
run apply access
mv "$TMP/ubus.hidden" "$BIN/ubus"
expect_rc 0 "S7 access без ubus"
journal_is "$FW" "S7 (access: один глагол)"
note "S7 access: один firewall reload, работает без ubus"

# ── S8. apply disable: страховочный стоп relayd ──
#
# Чистый путь: netifd погасил relayd сам — стопа нет в журнале вовсе.
reset
run apply disable
expect_rc 0 "S8 disable чистый"
journal_is "$RELOAD
$FW
pidof relayd" "S8 (disable: без стопа, когда relayd уже мёртв)"

# relayd пережил reload — ровно один стоп, и он помогает.
reset
: >"$TMP/relayd.alive"
run apply disable
expect_rc 0 "S8 disable со стопом"
did "relayd-init stop" || bad "S8: страховочный стоп не вызван по живому relayd"

# Стоп не помог — код 5, и второй ступени (kill) в журнале нет.
reset
: >"$TMP/relayd.alive"
STUB_RELAYD_STOP_WORKS=0
run apply disable
expect_rc 5 "S8 relayd бессмертен"
grep -c 'relayd-init stop' "$TMP/actions" | grep -qx 1 ||
	bad "S8: стопов больше одного — появилась лестница"
did "kill" && bad "S8: в журнале kill — лестница из глаголов (ADR-0025)"
note "S8 disable: стоп только по живому relayd, ровно один, не помог — 5"

# ── S9. предусловия и замок применений ──
reset
STUB_FLOCK_RC=1
run apply enable
expect_rc 3 "S9 замок занят"
did "ubus" && bad "S9: занятый замок не остановил применение"

reset
mv "$BIN/flock" "$TMP/flock.hidden"
run apply enable
expect_rc 7 "S9 нет flock"
[ ! -e "$LOCKFILE" ] || bad "S9: файл замка создан до проверки предусловия"
mv "$TMP/flock.hidden" "$BIN/flock"

reset
mv "$BIN/ubus" "$TMP/ubus.hidden"
run apply enable
mv "$TMP/ubus.hidden" "$BIN/ubus"
expect_rc 7 "S9 нет ubus"
did "firewall" && bad "S9: без ubus дело дошло до firewall"

# status и probe замка не берут: чтение не должно ждать чужого применения.
reset
STUB_FLOCK_RC=1
: >"$TMP/relayd.installed"
run status "$P"
expect_rc 0 "S9 status при занятом замке"
run probe 192.0.2.9 "$D"
expect_rc 0 "S9 probe при занятом замке"
note "S9 замок: применения ждут (3/7), чтения проходят"

# ── S10. плохие аргументы ──
reset
run
expect_rc 1 "S10 без глагола"
run chmod
expect_rc 1 "S10 неизвестный глагол"
run apply
expect_rc 1 "S10 apply без действия"
run apply reboot
expect_rc 1 "S10 apply с чужим действием"
run status
expect_rc 1 "S10 status без порта"
run status -"$P"
expect_rc 1 "S10 имя порта с дефисом"
run status "a..b"
expect_rc 1 "S10 имя порта с '..'"
run probe 192.0.2.9 "-$D"
expect_rc 1 "S10 имя интерфейса с дефисом"
did "ubus\|firewall\|apk add" && bad "S10: мусорный аргумент дошёл до глагола"
note "S10 плохие аргументы: 1, глаголы не звались"

# ── S11. статическая: запрещённые формы ──
#
# Список короче, чем у netmode-wifi, потому что и соблазнов меньше: этому
# скрипту нечего делать с беспроводной частью вовсе. eval и склейка
# кавычками запрещены по той же причине — законного применения нет,
# а из-под грепа они уводят что угодно.
c=$(grep -vE '^[[:space:]]*#' "$RUN")
for f in 'network restart' 'init.d/network' 'wifi'; do
	printf '%s\n' "$c" | grep -qF "$f" &&
		bad "S11: запрещённая форма '$f' в скрипте"
done
printf '%s\n' "$c" | grep -qE '(^|[;&|(){}[:space:]])eval([[:space:]]|$)' &&
	bad "S11: eval в скрипте"
# Склейка ловится только ВНУТРИ слова (ne""twork): голые '' легитимны —
# это пустая строка в case-паттернах самого скрипта.
printf '%s\n' "$c" | grep -qE "[[:alnum:]](''|\"\")[[:alnum:]]" &&
	bad "S11: склейка пустыми кавычками внутри слова"
note "S11 запрещённых форм нет (network restart, wifi, eval, склейка)"

if [ "$failed" -ne 0 ]; then
	echo "" >&2
	echo "netmode-bridge ведёт себя не так, как обещает его контракт" >&2
	echo "(коды возврата — в шапке скрипта и docs/contracts/executor.md)." >&2
	exit 1
fi

echo "-- check-netmode-bridge: синтаксис и 11 сценариев поведения"
