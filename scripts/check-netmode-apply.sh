#!/bin/sh
# netmode-apply проверяется запуском, а не чтением.
#
# Скрипт — единственное место, где режим применяется на живом роутере, и
# ошибиться в нём дороже всего: он останавливает сервисы, перезапускает
# firewall и решает, поднимать ли туннель. Go-тесты его не касаются вовсе —
# они проверяют разбор КОДА возврата, а не то, при каких условиях этот код
# возникает.
#
# Проверяются две вещи:
#   1. синтаксис (sh -n) — опечатка иначе всплывёт на роутере;
#   2. поведение на подставных /etc/init.d/* — прежде всего то, что при
#      неудавшемся firewall restart целевой сервис НЕ стартует. Стартовать
#      туннель поверх невычищенных цепочек nftables значит показать
#      работающий режим, мимо которого молча идёт трафик.
#
# Как подставляется окружение: копия скрипта с переписанными путями
# /etc/init.d и /var/lock. Копия, а не переменные окружения в самом скрипте:
# на роутере он запускается от root, и переопределяемый путь к init-скриптам
# был бы дырой ради удобства тестов. Полнота подстановки проверяется ниже
# грепом — иначе тест однажды начнёт гонять оригинал и молча пройдёт.
set -eu

SRC=files/usr/local/bin/netmode-apply

[ -f "$SRC" ] || { echo "check-netmode-apply: нет $SRC" >&2; exit 1; }

# ── 1. синтаксис ──

sh -n "$SRC" || { echo "check-netmode-apply: $SRC не разбирается как sh" >&2; exit 1; }

# ── 2. песочница ──

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT INT TERM

BIN="$TMP/bin"
# Каталоги названы так, чтобы не оканчиваться на подменяемые пути: иначе
# проверка полноты подстановки ниже находила бы собственный результат.
INITD="$TMP/initd"
STATE="$TMP/state"
RUN="$TMP/netmode-apply"
mkdir -p "$BIN" "$INITD" "$STATE" "$TMP/lock"

sed -e "s|/etc/init.d|$INITD|g" -e "s|^LOCK=/var/lock/|LOCK=$TMP/lock/|" "$SRC" >"$RUN"
chmod 0755 "$RUN"

# Подстановка обязана быть полной: недобитый абсолютный путь означал бы,
# что проверка трогает настоящие сервисы или молча ничего не проверяет.
if grep -q '/etc/init\.d' "$RUN" || ! grep -q "^LOCK=$TMP/lock/" "$RUN"; then
	echo "check-netmode-apply: пути в копии подставлены не полностью" >&2
	exit 1
fi

# PATH песочницы содержит ТОЛЬКО то, что скрипту действительно нужно.
# Так «нет flock» воспроизводится удалением одного файла, а не надеждой на
# то, что в системе его нет: на Linux он есть, на macOS его нет, и проверка
# должна вести себя одинаково.
for u in mkdir dirname sleep rm; do
	p=$(command -v "$u" || true)
	[ -n "$p" ] || { echo "check-netmode-apply: в системе нет $u" >&2; exit 1; }
	ln -sf "$p" "$BIN/$u"
done

# logger подставной: диагностика в syslog — часть требования (её читают
# через logread), и проверить её можно только перехватив.
cat >"$BIN/logger" <<EOF
#!/bin/sh
shift 2  # -t netmode-apply
printf '%s\n' "\$*" >>"$TMP/syslog"
EOF

cat >"$BIN/flock" <<'EOF'
#!/bin/sh
exit "${STUB_FLOCK_RC:-0}"
EOF

# Подставной init-скрипт. Ведёт журнал действий и состояние «запущен»
# маркерным файлом — так `running` отвечает правду о том, что сделали
# предыдущие шаги, а не заданный заранее ответ.
for name in nikki b4 firewall; do
	cat >"$INITD/$name" <<EOF
#!/bin/sh
set -u
name=$name
action=\${1:-}
printf '%s %s\n' "\$name" "\$action" >>"$TMP/actions"

# STUB_FAIL — отказ всегда, STUB_FAIL_ONCE — только первый раз
# (им проверяется, что повторная попытка firewall restart вообще есть).
case " \${STUB_FAIL:-} " in
	*" \$name:\$action "*) exit 1 ;;
esac
case " \${STUB_FAIL_ONCE:-} " in
	*" \$name:\$action "*)
		if [ ! -f "$STATE/once.\$name.\$action" ]; then
			: >"$STATE/once.\$name.\$action"
			exit 1
		fi
		;;
esac

case "\$action" in
	start)
		# STUB_NOMARK — старт «удался», но сервис не работает: так
		# воспроизводится расхождение, которое ловит шаг 5.
		[ "\${STUB_NOMARK:-}" = "\$name" ] || : >"$STATE/run.\$name"
		;;
	stop) rm -f "$STATE/run.\$name" ;;
	running) [ -f "$STATE/run.\$name" ] ;;
	*) : ;;
esac
EOF
	chmod 0755 "$INITD/$name"
done

# nft подставной: замок перезапуска (своя таблица inet netmode_guard) — это
# kill switch, и проверить его можно только по журналу и состоянию. Ставится
# через `nft -f -` с policy drop, снимается `nft delete table`.
cat >"$BIN/nft" <<EOF
#!/bin/sh
if [ "\$1" = "-f" ]; then
	# cat в PATH песочницы нет намеренно — читаем встроенным read.
	body=; while IFS= read -r l; do body="\$body \$l"; done
	case "\$body" in
		*netmode_guard*"policy drop"*) printf 'nft guard-on\n' >>"$TMP/actions"; : >"$STATE/guard" ;;
		*) printf 'nft -f ?\n' >>"$TMP/actions" ;;
	esac
	exit 0
fi
case "\$*" in
	"delete table inet netmode_guard") printf 'nft guard-off\n' >>"$TMP/actions"; rm -f "$STATE/guard" ;;
	*) printf 'nft %s\n' "\$*" >>"$TMP/actions" ;;
esac
exit 0
EOF

chmod 0755 "$BIN/logger" "$BIN/flock" "$BIN/nft"

# ── 3. сценарии ──

failed=0
mark=0
RC=0

bad() {
	echo "check-netmode-apply: $*" >&2
	failed=$((failed + 1))
}

# note подводит итог сценария. Отдельно от bad, чтобы провалившийся
# сценарий не отчитывался тут же строкой об успехе.
note() {
	if [ "$failed" -eq "$mark" ]; then
		printf '   ок      %s\n' "$*"
	else
		printf '   ПРОВАЛ  %s\n' "$*"
		mark=$failed
	fi
}

reset() {
	mark=$failed
	rm -f "$STATE"/* "$TMP/actions" "$TMP/syslog" 2>/dev/null || true
	: >"$TMP/actions"
	: >"$TMP/syslog"
	STUB_FAIL=""
	STUB_FAIL_ONCE=""
	STUB_NOMARK=""
	STUB_FLOCK_RC=0
	export STUB_FAIL STUB_FAIL_ONCE STUB_NOMARK STUB_FLOCK_RC
}

apply() {
	set +e
	PATH="$BIN" /bin/sh "$RUN" "$1" >"$TMP/out" 2>"$TMP/err"
	RC=$?
	set -e
}

expect_rc() {
	[ "$RC" = "$1" ] || bad "$2: код $RC, ожидался $1 (stderr: $(cat "$TMP/err"))"
}

did() { grep -q "^$1\$" "$TMP/actions"; }

# S1 — обычное переключение доходит до конца.
reset
apply nikki
expect_rc 0 "обычное переключение в nikki"
did "nikki stop" || bad "S1: не было стопа nikki"
did "firewall restart" || bad "S1: не было перезапуска firewall"
did "nikki start" || bad "S1: сервис не стартовал"
did "nikki enable" || bad "S1: автозапуск не включён"
did "b4 disable" || bad "S1: автозапуск чужого режима не снят"
note "S1 обычное переключение: 0"

# S2 — ГЛАВНОЕ. firewall не перезапустился: код 4 И туннель не поднят.
# Пропустить этот отказ значит поднять сервис поверх возможных чужих
# цепочек — панель покажет режим, мимо которого идёт трафик.
reset
STUB_FAIL="firewall:restart"
apply nikki
expect_rc 4 "firewall не перезапустился"
did "nikki start" && bad "S2: сервис стартовал поверх невычищенных цепочек"
[ "$(grep -c '^firewall restart$' "$TMP/actions")" = 2 ] ||
	bad "S2: попыток перезапуска $(grep -c '^firewall restart$' "$TMP/actions"), ожидалось 2"
grep -q 'ОШИБКА' "$TMP/err" || bad "S2: аварийное сообщение не попало в stderr"
grep -q 'ОШИБКА' "$TMP/out" && bad "S2: аварийное сообщение ушло в stdout — демон его не увидит"
grep -q 'firewall' "$TMP/syslog" || bad "S2: в syslog не продублировано"
note "S2 firewall не перезапустился: 4, целевой сервис не стартовал"

# S3 — повторная попытка существует и работает: первый вызов упал, второй нет.
reset
STUB_FAIL_ONCE="firewall:restart"
apply nikki
expect_rc 0 "firewall упал один раз"
did "nikki start" || bad "S3: после удачного повтора сервис не стартовал"
note "S3 firewall упал однажды: повтор вытащил переключение"

# S4 — enable молча не падает: ненулевой код и запись в журнале.
reset
STUB_FAIL="nikki:enable"
apply nikki
expect_rc 5 "не удалось включить автозапуск"
grep -q 'enable' "$TMP/err" || bad "S4: отказ enable ушёл в тишину, а не в stderr"
grep -q 'автозапуск' "$TMP/syslog" || bad "S4: отказ enable не попал в syslog"
did "nikki start" || bad "S4: до автозапуска дело не дошло"
note "S4 не удалось включить автозапуск: 5 и внятное сообщение"

# S5 — замок занят: это не поломка, у неё свой код.
reset
STUB_FLOCK_RC=1
apply nikki
expect_rc 3 "переключение уже идёт"
did "nikki stop" && bad "S5: занятый замок не остановил скрипт до изменений"
note "S5 замок занят: 3, система не тронута"

# S6 — нет предусловия: flock отсутствует.
reset
mv "$BIN/flock" "$TMP/flock.hidden"
apply nikki
mv "$TMP/flock.hidden" "$BIN/flock"
expect_rc 7 "нет flock"
note "S6 нет flock: 7"

# S7 — верификация: старт «удался», но сервис не работает.
reset
STUB_NOMARK="nikki"
apply nikki
expect_rc 6 "верификация не сошлась"
did "nikki start" || bad "S7: старта не было вовсе"
note "S7 состояние не сошлось: 6"

# S8 — некорректный аргумент: баг вызывающего.
reset
apply мусор
expect_rc 1 "неизвестный режим"
did "firewall restart" && bad "S8: неизвестный режим дошёл до перезапуска firewall"
note "S8 неизвестный режим: 1"

# S9 — off: ничего не запускаем, но firewall всё равно перезапускаем.
reset
apply off
expect_rc 0 "выключение обхода"
did "firewall restart" || bad "S9: без перезапуска цепочки прошлого режима остались бы"
did "nikki start" && bad "S9: в режиме off ничего стартовать не должно"
note "S9 выключение обхода: 0"

# line — номер первой строки журнала с действием (пусто — не было).
line() { grep -n "^$1\$" "$TMP/actions" | head -1 | cut -d: -f1; }

# S10 — kill switch: перезапуск nikki идёт под замком. Без него между
# cleanup (stop nikki удаляет его таблицу перехвата) и стартом ~0,65 с
# трафик LAN шёл напрямую с домашнего IP (docs/recon/exit-chains.md).
reset
apply nikki
expect_rc 0 "переключение в nikki под замком"
on=$(line "nft guard-on"); stop=$(line "nikki stop"); en=$(line "nikki enable"); off=$(line "nft guard-off")
{ [ -n "$on" ] && [ -n "$stop" ] && [ "$on" -lt "$stop" ]; } || bad "S10: замок не поставлен ДО остановки nikki"
{ [ -n "$off" ] && [ -n "$en" ] && [ "$off" -gt "$en" ]; } || bad "S10: замок снят не после проверки работающего nikki"
[ -f "$STATE/guard" ] && bad "S10: после удачного применения замок остался"
note "S10 перезапуск nikki под замком: поставлен до stop, снят после проверки"

# S11 — nikki не поднялся: fail closed, замок ОСТАЁТСЯ (решение владельца).
reset
STUB_FAIL="nikki:start"
apply nikki
expect_rc 5 "nikki не запустился"
[ -f "$STATE/guard" ] || bad "S11: при неудавшемся старте замок снят — трафик LAN пошёл бы напрямую"
grep -q 'замок' "$TMP/err" "$TMP/out" "$TMP/syslog" || bad "S11: не сказано, что замок остался и как его снять"
note "S11 nikki не поднялся: 5, замок держится"

# S12 — режим off: владелец сам выбрал «без обхода» — замок снимается и не ставится.
reset
: >"$STATE/guard"
apply off
expect_rc 0 "выключение снимает замок"
did "nft guard-on" && bad "S12: в режиме off поставлен замок"
[ -f "$STATE/guard" ] && bad "S12: замок от прошлого провала не снят выключением"
note "S12 off: замок снят"

if [ "$failed" -ne 0 ]; then
	echo "" >&2
	echo "netmode-apply ведёт себя не так, как обещает его контракт" >&2
	echo "(коды возврата — в шапке скрипта и docs/contracts/executor.md)." >&2
	exit 1
fi

echo "-- check-netmode-apply: синтаксис и 12 сценариев поведения"
