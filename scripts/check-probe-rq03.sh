#!/bin/sh
# Песочница для измерительной оснастки RQ-03: доказывает страховку БЕЗ роутера.
#
# Зачем. Пробник docs/recon/probes/rq03-apply.sh переключает внешнюю сеть на
# живом роутере, к которому владелец подключён ТОЛЬКО по wifi. Обрыв связи
# посреди прогона — ожидаемый исход опыта, а не авария. Возвращает конфигурацию
# сторож docs/recon/probes/guard.sh, и если он тихо не сработает, владелец
# останется с роутером без интернета и без ssh. Поэтому сторож обязан быть
# проверен ДО того, как его повезут на железо.
#
# Что здесь проверяется по-настоящему: весь сторож (протухшее сердцебиение,
# дедлайн, штатный конец, перезагрузка, abort, отсутствующий и падающий
# restore.sh, сохранность чужого crontab) и та часть пробника, которая не
# требует ubus (генерация возврата, отказ при нечем отсоединиться, разбор
# аргументов).
#
# Чего здесь НЕТ и не будет без роутера: всё, что идёт через ubus и
# jsonfilter, — разрешение радио, инвентаризация глаголов, сам замер и
# аварийный порог. Эмуляция ubus была бы отдельной ложью такого же размера,
# как то, что она проверяет. Это названо вслух в docs/recon/probes/README.md.
#
# Приём с подменой путей и подставными утилитами — тот же, что в
# scripts/check-netmode-apply.sh.
set -eu

SRC_PROBE=docs/recon/probes/rq03-apply.sh
SRC_GUARD=docs/recon/probes/guard.sh

[ -f "$SRC_PROBE" ] && [ -f "$SRC_GUARD" ] || {
	echo "check-probe-rq03: нет $SRC_PROBE или $SRC_GUARD" >&2
	exit 1
}

SB=$(mktemp -d /tmp/rq03-sandbox-XXXXXX)
trap 'rm -rf "$SB"' EXIT INT TERM

RQ=$SB/rq03
BIN=$SB/bin
mkdir -p "$RQ" "$BIN"

fail=0
ok()   { printf '   ок      %s\n' "$*"; }
bad()  { printf '   ПРОВАЛ  %s\n' "$*"; fail=1; }

# ─────────────────────── подстановка путей и её полнота ──────────────────────
#
# Подменяем ТОЛЬКО помеченный блок путей. Если завтра в скрипте появится ещё
# один абсолютный путь мимо блока, проверка полноты это увидит — иначе
# песочница гоняла бы не то, что поедет на роутер.

subst() {
	sed \
		-e "s|^RQ_DIR=/root/rq03\$|RQ_DIR=$RQ|" \
		-e "s|^WIRELESS_CFG=/etc/config/wireless\$|WIRELESS_CFG=$SB/wireless|" \
		-e "s|^UPTIME_FILE=/proc/uptime\$|UPTIME_FILE=$SB/uptime|" \
		-e "s|^WIFI_SBIN=/sbin/wifi\$|WIFI_SBIN=$BIN/wifi|" \
		-e "s|^INITD_NETWORK=/etc/init.d/network\$|INITD_NETWORK=$BIN/network|" \
		"$1" >"$2"
	chmod +x "$2"
}

subst "$SRC_PROBE" "$SB/probe.sh"
subst "$SRC_GUARD" "$SB/guard.sh"

# Полнота: вне комментариев не должно остаться ни одного исходного пути.
leftovers=$(
	for f in "$SB/probe.sh" "$SB/guard.sh"; do
		grep -vE '^[[:space:]]*#' "$f" |
			grep -nE '/root/rq03|/proc/uptime|/etc/config/wireless|/sbin/wifi|/etc/init\.d/network' |
			sed "s|^|$(basename "$f"):|" || true
	done
)
if [ -n "$leftovers" ]; then
	bad "подстановка путей неполная — песочница гоняла бы не то, что поедет на роутер:"
	printf '%s\n' "$leftovers" | sed 's/^/           /'
else
	ok "подстановка путей полная"
fi

sh -n "$SB/probe.sh" && sh -n "$SB/guard.sh" && ok "синтаксис обоих скриптов" ||
	bad "синтаксис"

# ────────────────────────────── подставные утилиты ───────────────────────────

cat >"$BIN/logger" <<'EOF'
#!/bin/sh
exit 0
EOF

# crontab поверх файла. Нужен настоящий, а не заглушка: сторож обязан НЕ
# затирать чужие строки, и проверить это можно только сохраняя состояние.
cat >"$BIN/crontab" <<EOF
#!/bin/sh
F=$SB/crontab
case "\${1:-}" in
	-l) [ -f "\$F" ] || exit 1; cat "\$F" ;;
	-)  cat >"\$F" ;;
	*)  exit 2 ;;
esac
EOF

# uci поверх текстового файла в формате `uci show`.
cat >"$BIN/uci" <<EOF
#!/bin/sh
F=$SB/wireless
[ -f "\$F" ] || : >"\$F"
while [ \$# -gt 0 ]; do case "\$1" in -q) shift ;; *) break ;; esac; done
case "\${1:-}" in
	show)   cat "\$F" ;;
	get)    v=\$(grep "^\$2=" "\$F" | head -1 | cut -d= -f2- | tr -d "'")
	        [ -n "\$v" ] || exit 1
	        printf '%s\n' "\$v" ;;
	set)    k=\${2%%=*}; v=\${2#*=}
	        grep -v "^\$k=" "\$F" >"\$F.new" 2>/dev/null || :
	        printf "%s='%s'\n" "\$k" "\$v" >>"\$F.new"
	        mv "\$F.new" "\$F" ;;
	commit) echo commit >>$SB/uci-commits ;;
	*)      exit 2 ;;
esac
EOF

cat >"$BIN/wifi" <<EOF
#!/bin/sh
echo "wifi \$*" >>$SB/wifi-calls
EOF

chmod +x "$BIN/logger" "$BIN/crontab" "$BIN/uci" "$BIN/wifi"

# PATH из подставных утилит плюс ссылки на настоящие базовые. Список закрытый
# намеренно: так можно СПРЯТАТЬ nohup/setsid и проверить отказ отсоединения.
link_base() {
	for u in sh cat rm mkdir sleep sed awk grep printf date cp mv chmod tr cut head tail wc touch kill ps expr id dirname basename mktemp; do
		for d in /bin /usr/bin; do
			if [ -x "$d/$u" ] && [ ! -e "$BIN/$u" ]; then ln -s "$d/$u" "$BIN/$u"; fi
		done
	done
}
link_base

# Второй каталог — утилиты, которых пробник требует по списку, но которые нам
# нужны только присутствующими: до их вызова он в этих сценариях не доходит.
# Разделение нужно, чтобы отличить отказ «нет утилиты» (код 4) от отказа
# «нечем отсоединить» (код 6). Один урезанный PATH дал бы первый и выдал бы
# его за второй — проверка была бы зелёной и врала.
BINX=$SB/binx
mkdir -p "$BINX"
for u in flock jsonfilter ubus; do
	printf '#!/bin/sh\nexit 0\n' >"$BINX/$u"
	chmod +x "$BINX/$u"
done

PATH_NOTOOLS=$BIN               # нет flock/jsonfilter/ubus  → ждём код 4
PATH_NODETACH=$BIN:$BINX        # утилиты есть, отсоединить нечем → ждём код 6
PATH_FULL=$BIN:$BINX:/usr/bin:/bin

# ──────────────────────────── стенд для сторожа ──────────────────────────────

reset_guard() {
	rm -rf "$RQ"; mkdir -p "$RQ"
	cp "$SB/guard.sh" "$RQ/guard.sh"
	rm -f "$SB/restored" "$SB/crontab"
	echo 1000 >"$SB/uptime"
	cat >"$RQ/restore.sh" <<EOF
#!/bin/sh
echo restored >>$SB/restored
EOF
	chmod +x "$RQ/restore.sh"
	printf '%s\n' "* * * * * $RQ/guard.sh" >"$SB/crontab"
}

guard() { PATH="$PATH_FULL" sh "$SB/guard.sh" "$@" >/dev/null 2>&1 || true; }

restored() { [ -f "$SB/restored" ]; }

echo
echo "── сторож ──"

# S1. Главный процесс убит: отметка живости протухла.
reset_guard
echo 900 >"$RQ/beat"          # 100 с назад при uptime 1000, порог 20
guard once
if restored && [ -f "$RQ/RESTORED" ]; then ok "S1 протухшее сердцебиение → возврат"
else bad "S1 протухшее сердцебиение: возврат не сработал"; fi

# S2. Дедлайн истёк, хотя главный жив.
reset_guard
echo 1000 >"$RQ/beat"
echo 900 >"$RQ/deadline"
guard once
if restored; then ok "S2 истёкший дедлайн → возврат"
else bad "S2 истёкший дедлайн: возврат не сработал"; fi

# S3. Штатный конец: конфигурацию не трогаем, себя убираем.
reset_guard
echo 1000 >"$RQ/beat"
: >"$RQ/DONE"
guard once
if ! restored && [ -f "$RQ/GUARD-OFF" ] && ! grep -q "$RQ/guard.sh" "$SB/crontab"; then
	ok "S3 DONE → конфиг не тронут, строка crontab снята"
else
	bad "S3 DONE: тронул конфиг ($( ! restored && echo нет || echo ДА)) либо не убрался"
fi

# S4. Перезагрузка: отметка живости из будущего.
reset_guard
echo 5000 >"$RQ/beat"         # больше текущего uptime 1000
guard once
if restored; then ok "S4 отметка из будущего (перезагрузка) → возврат"
else bad "S4 перезагрузка не распознана"; fi

# S5. Аварийный возврат по команде владельца.
reset_guard
echo 1000 >"$RQ/beat"
guard abort
if restored && [ -f "$RQ/ABORTED" ]; then ok "S5 abort → возврат и отметка"
else bad "S5 abort не отработал"; fi

# S6. Возвращать нечем — молчать нельзя.
reset_guard
echo 900 >"$RQ/beat"
rm -f "$RQ/restore.sh"
guard once
if [ -f "$RQ/RESTORE-MISSING" ]; then ok "S6 нет restore.sh → громкая отметка, а не тишина"
else bad "S6 отсутствие restore.sh осталось незамеченным"; fi

# S7. Возврат упал — это отдельный исход, а не успех.
reset_guard
echo 900 >"$RQ/beat"
printf '#!/bin/sh\nexit 1\n' >"$RQ/restore.sh"
chmod +x "$RQ/restore.sh"
guard once
if [ -f "$RQ/RESTORE-FAILED" ] && [ ! -f "$RQ/RESTORED" ]; then
	ok "S7 упавший возврат → RESTORE-FAILED, а не тихий успех"
else
	bad "S7 упавший возврат выдан за успешный"
fi

# S8. Чужой crontab не трогаем. Классическая мина `crontab -l | ... | crontab -`.
reset_guard
echo 1000 >"$RQ/beat"
: >"$RQ/DONE"
printf '%s\n' "0 3 * * * /root/backup.sh" >"$SB/crontab"
guard once
if grep -q '/root/backup.sh' "$SB/crontab"; then
	ok "S8 чужая строка crontab уцелела"
else
	bad "S8 сторож затёр чужой crontab — ровно то, ради чего в нём проверка перед записью"
fi

# ──────────────────────────────── пробник ────────────────────────────────────

echo
echo "── пробник ──"

probe() { PATH="$1" sh "$SB/probe.sh" "$@" 2>&1; shift; }

# S9. Генерация возврата: секция БЕЗ опции disabled обязана вернуться
# читаемым '0', а не удалением опции.
rm -rf "$RQ"; mkdir -p "$RQ"
cat >"$SB/wireless" <<'EOF'
wireless.radioA=wifi-device
wireless.radioA.band='2g'
wireless.staOn=wifi-iface
wireless.staOn.device='radioA'
wireless.staOn.mode='sta'
wireless.staOn.ssid='ActiveNet'
wireless.staOff=wifi-iface
wireless.staOff.device='radioA'
wireless.staOff.mode='sta'
wireless.staOff.ssid='OtherNet'
wireless.staOff.disabled='1'
wireless.apHome=wifi-iface
wireless.apHome.device='radioB'
wireless.apHome.mode='ap'
wireless.apHome.ssid='HomeAP'
EOF
echo 1000 >"$SB/uptime"
out=$(PATH="$PATH_FULL" sh "$SB/probe.sh" --emit-restore 2>&1 || true)

if printf '%s\n' "$out" | grep -q "wireless.staOn.disabled='0'"; then
	ok "S9 секция без опции disabled возвращается как '0'"
else
	bad "S9 секция без disabled не восстанавливается читаемым значением"
fi
if printf '%s\n' "$out" | grep -q "wireless.staOff.disabled='1'"; then
	ok "S9 выключенная секция возвращается как '1'"
else
	bad "S9 выключенная секция восстановлена неверно"
fi
if printf '%s\n' "$out" | grep -q 'uci delete'; then
	bad "S9 в возврате есть uci delete — включённость стала бы неявной"
else
	ok "S9 в возврате нет ни одного uci delete"
fi
if printf '%s\n' "$out" | grep -q 'apHome'; then
	bad "S9 в возврат попала секция домашней точки — её трогать нельзя"
else
	ok "S9 секция домашней точки в возврат не попала"
fi
if printf '%s\n' "$out" | tail -1 | grep -qx 'wifi'; then
	ok "S9 последний глагол возврата — самый тупой (wifi целиком)"
else
	bad "S9 возврат заканчивается не полным wifi"
fi

# S10. Нет обязательной утилиты → отказ КОДОМ 4 и ноль мутаций.
rm -rf "$RQ"; mkdir -p "$RQ"
: >"$SB/uci-commits"
set +e
PATH="$PATH_NOTOOLS" sh "$SB/probe.sh" >"$SB/notools.out" 2>&1
rc=$?
set -e
if [ "$rc" = 4 ] && [ ! -s "$SB/uci-commits" ]; then
	ok "S10 нет flock/jsonfilter/ubus → код 4, ноль коммитов"
else
	bad "S10 неполный инструментарий: код $rc (ждали 4), коммитов $(wc -l <"$SB/uci-commits" | tr -d ' ')"
fi

# S11. Утилиты на месте, а отсоединиться нечем → отказ КОДОМ 6 и ноль мутаций.
#
# Это отдельный сценарий, а не вариант S10, и разница принципиальная. Первая
# версия этой проверки гоняла урезанный PATH, получала код 4 и подписывала его
# «нечем отсоединить»: зелёная проверка, которая не проверяла ничего. Отказ
# отсоединения — последний рубеж перед мутацией конфигурации, и подтверждать
# его надо тем самым кодом, а не любым ненулевым.
rm -rf "$RQ"; mkdir -p "$RQ"
# Сторож на месте — его доставляет обёртка. Без него пробник отказывает раньше,
# кодом 10, и сценарий мерил бы не то.
cp "$SB/guard.sh" "$RQ/guard.sh"
: >"$SB/uci-commits"
set +e
PATH="$PATH_NODETACH" sh "$SB/probe.sh" >"$SB/nodetach.out" 2>&1
rc=$?
set -e
if [ "$rc" = 6 ] && [ ! -s "$SB/uci-commits" ]; then
	ok "S11 нечем отсоединить → код 6, ноль коммитов"
else
	bad "S11 отсоединение: код $rc (ждали 6). Вывод: $(tail -1 "$SB/nodetach.out")"
fi

# S12. Разбор аргументов.
set +e
PATH="$PATH_FULL" sh "$SB/probe.sh" --несуществующий >/dev/null 2>&1
rc_unknown=$?
PATH="$PATH_FULL" sh "$SB/probe.sh" --budget пятьсот >/dev/null 2>&1
rc_budget=$?
PATH="$PATH_FULL" sh "$SB/probe.sh" --help >"$SB/help.out" 2>&1
rc_help=$?
set -e
if [ "$rc_unknown" = 2 ] && [ "$rc_budget" = 2 ] && [ "$rc_help" = 0 ] && [ -s "$SB/help.out" ]; then
	ok "S12 неизвестный аргумент и нечисловой бюджет → код 2, --help работает"
else
	bad "S12 разбор аргументов: unknown=$rc_unknown budget=$rc_budget help=$rc_help"
fi

# S13. Проводной режим не оставляет следов там, где на роутере флеш.
#
# RQ_DIR в песочнице подменён на $SB/rq03 — он играет роль /root/rq03, то есть
# постоянной памяти роутера. Обещание режима --wired ровно одно: туда не
# попадает ничего. Проверяем буквально: каталога не появилось, а временный
# каталог в tmpfs убран за собой.
rm -rf "$RQ"
before_tmp=$(ls -d /tmp/rq03-* 2>/dev/null | wc -l | tr -d ' ')
set +e
PATH="$PATH_FULL" sh "$SB/probe.sh" --wired --emit-restore >"$SB/wired.out" 2>&1
rc=$?
set -e
after_tmp=$(ls -d /tmp/rq03-* 2>/dev/null | wc -l | tr -d ' ')

if [ "$rc" = 0 ] && [ ! -d "$RQ" ] && grep -q "disabled='0'" "$SB/wired.out"; then
	ok "S13 --wired: возврат снят, каталог на «флеше» не создан"
else
	bad "S13 --wired: код $rc, каталог $( [ -d "$RQ" ] && echo СОЗДАН || echo нет), возврат $( grep -q "disabled=" "$SB/wired.out" && echo есть || echo НЕТ)"
fi
if [ "$after_tmp" = "$before_tmp" ]; then
	ok "S13 --wired: временный каталог в tmpfs убран за собой"
else
	bad "S13 --wired: в /tmp осталось $((after_tmp - before_tmp)) каталогов rq03-*"
fi

# S14. Сбор результата в проводном режиме.
#
# Сценарий появился после того, как первый настоящий прогон на роутере прошёл
# успешно и целиком пропал: обёртка обнуляла переменную с потоком в ветке
# успеха, и сбор молча отказывался ровно на удачных замерах. Измерение стоит
# владельцу вечера и телефона на точке — терять его из-за парсера нельзя.
SPLIT_IN=$SB/stream.txt
SPLIT_RAW=$SB/raw
mkdir -p "$SPLIT_RAW"
cat >"$SPLIT_IN" <<'STREAM'
шум до маркеров, попасть в файлы не должен
===8<=== summary.txt
СВОДКА-МАРКЕР
===8<=== inventory.txt
ИНВЕНТАРЬ-МАРКЕР
===8<=== results.tsv
РЕЗУЛЬТАТЫ-МАРКЕР
===8<=== rq04-scan.txt
СКАН-МАРКЕР
===8<=== events.tsv
СОБЫТИЯ-МАРКЕР
===8<=== samples.tsv
СЭМПЛЫ-МАРКЕР
===8<=== probe.log
ЖУРНАЛ-МАРКЕР
===8<=== END
STREAM

set +e
RQ03_RAW="$SPLIT_RAW" sh scripts/probe-rq03.sh --split "$SPLIT_IN" >"$SB/split.out" 2>&1
rc=$?
set -e

missing=
for pair in \
	"27-rq03-apply-verbs.txt:СВОДКА-МАРКЕР" \
	"27-rq03-apply-verbs.txt:ИНВЕНТАРЬ-МАРКЕР" \
	"27-rq03-apply-verbs.txt:РЕЗУЛЬТАТЫ-МАРКЕР" \
	"27-rq03-apply-verbs.txt:СКАН-МАРКЕР" \
	"28-rq03-samples.tsv:СЭМПЛЫ-МАРКЕР" \
	"29-rq03-events.txt:СОБЫТИЯ-МАРКЕР" \
	"29-rq03-events.txt:ЖУРНАЛ-МАРКЕР"; do
	f=${pair%%:*}
	m=${pair#*:}
	grep -q "$m" "$SPLIT_RAW/$f" 2>/dev/null || missing="$missing $f:$m"
done

if [ "$rc" = 0 ] && [ -z "$missing" ] && ! grep -q 'шум до маркеров' "$SPLIT_RAW/28-rq03-samples.tsv"; then
	ok "S14 сбор: все семь блоков разложены по трём файлам, шум отброшен"
else
	bad "S14 сбор: код $rc, потеряно:${missing:- ничего}"
fi

# Пустой поток обязан провалиться ГРОМКО, а не создать три пустых файла,
# которые потом прочтут как «замер ничего не нашёл».
: >"$SB/empty.txt"
set +e
RQ03_RAW="$SPLIT_RAW" sh scripts/probe-rq03.sh --split "$SB/empty.txt" >"$SB/split-empty.out" 2>&1
rc=$?
set -e
if [ "$rc" != 0 ] && grep -q 'ничего не выгрузил' "$SB/split-empty.out"; then
	ok "S14 пустой поток → отказ ненулевым кодом, а не три пустых файла"
else
	bad "S14 пустой поток: код $rc (ждали ненулевой)"
fi

echo
if [ "$fail" -ne 0 ]; then
	echo "check-probe-rq03: ПРОВАЛ — на роутер это везти нельзя" >&2
	exit 1
fi
echo "-- check-probe-rq03: страховка проверена (14 сценариев), ubus-часть не покрыта осознанно"
