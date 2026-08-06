#!/bin/sh
# Сторож возврата пробника RQ-03. Разворачивается в /root/rq03/guard.sh.
#
# Зачем он вообще есть. Владелец подключён к роутеру ТОЛЬКО по WiFi, через
# домашнюю точку, а меряем мы применение конфигурации к станционному радио —
# второму диапазону ТОЙ ЖЕ phy0. Обрыв ssh посреди замера — не авария, а
# ожидаемый исход опыта. Значит вернуть конфигурацию обязан сам роутер, без
# участия ноутбука и без участия главного процесса пробника: тот мог быть
# убит вместе с сессией.
#
# Три условия, любое из которых означает «главного больше нет, возвращаем»:
#
#   1. протухла отметка живости /root/rq03/beat — главный процесс трогает её
#      раз в секунду, порог протухания STALE секунд;
#   2. истёк абсолютный дедлайн /root/rq03/deadline — весь прогон обязан
#      уложиться в бюджет, даже если главный жив и залип;
#   3. отметка живости из БУДУЩЕГО — значит роутер перезагружался: время в
#      /proc/uptime монотонно и после перезагрузки начинается заново.
#      Конфигурация при этом уже закоммичена в мутированном виде, и вернуть
#      её больше некому.
#
# Флаг /root/rq03/DONE — единственный штатный конец: главный процесс дошёл до
# конца сам и вернул всё сам. Тогда сторож НЕ трогает конфигурацию, снимает
# себя из crontab и уходит.
#
# Режимы:
#   guard.sh loop    — отсоединённый цикл, проверка раз в TICK секунд
#   guard.sh once    — одна проверка (так его зовёт crontab)
#   guard.sh abort   — немедленный возврат по команде владельца
#
# Время берётся из /proc/uptime, а не из date: монотонно, есть всегда, не
# зависит от ntp и от того, поднялась ли вообще сеть.
#
# Коды возврата: 0 — всё в порядке либо возврат отработал, 2 — непонятный
# режим, 3 — возвращать нечем (нет restore.sh).
set -u

RQ_DIR=/root/rq03
UPTIME_FILE=/proc/uptime

TICK=5      # период цикла, с
STALE=20    # столько секунд разрешено не обновлять beat

TAG=rq03-guard

LOG=$RQ_DIR/guard.log
BEAT=$RQ_DIR/beat
DEADLINE=$RQ_DIR/deadline
DONE=$RQ_DIR/DONE
RESTORE=$RQ_DIR/restore.sh
PIDFILE=$RQ_DIR/guard.pid

now() { awk '{printf "%d\n", $1}' "$UPTIME_FILE" 2>/dev/null; }

say() {
	printf '%s %s\n' "$(now)" "$*" >>"$LOG" 2>/dev/null
	logger -t "$TAG" "$*" 2>/dev/null
	return 0
}

# uncron снимает строку из crontab. Читаем ПЕРЕД записью и пишем только если
# наша строка там действительно есть: `crontab -l | grep -v ... | crontab -`
# при неудавшемся чтении затёр бы владельцу весь crontab пустотой.
uncron() {
	command -v crontab >/dev/null 2>&1 || return 0
	cur=$(crontab -l 2>/dev/null) || return 0
	case "$cur" in
		*"$RQ_DIR/guard.sh"*) ;;
		*) return 0 ;;
	esac
	printf '%s\n' "$cur" | grep -v "$RQ_DIR/guard.sh" | crontab - 2>/dev/null
	say "строка сторожа снята из crontab"
	return 0
}

# stop_daemon гасит цикл, если гасящий — не он сам.
stop_daemon() {
	[ -f "$PIDFILE" ] || return 0
	p=$(cat "$PIDFILE" 2>/dev/null)
	case "$p" in
		'' | *[!0-9]*) rm -f "$PIDFILE"; return 0 ;;
	esac
	rm -f "$PIDFILE"
	[ "$p" = "$$" ] && return 0
	kill "$p" 2>/dev/null
	return 0
}

# retire — общий конец: сторож больше не нужен ни в каком виде.
retire() {
	uncron
	stop_daemon
	rm -f "$BEAT"
	: >"$RQ_DIR/GUARD-OFF"
}

restore_now() {
	say "ВОЗВРАТ: $1"
	if [ -f "$RESTORE" ]; then
		sh "$RESTORE" >>"$LOG" 2>&1
		rc=$?
		say "restore.sh код $rc"
		if [ "$rc" = 0 ]; then
			: >"$RQ_DIR/RESTORED"
		else
			: >"$RQ_DIR/RESTORE-FAILED"
			say "ВНИМАНИЕ: возврат не удался, конфигурация осталась изменённой"
		fi
	else
		# Возвращать нечем — молчать нельзя, это худший исход из всех.
		: >"$RQ_DIR/RESTORE-MISSING"
		say "ВНИМАНИЕ: нет $RESTORE, возвращать нечем"
		retire
		return 3
	fi
	retire
	return 0
}

# check — одна проверка. 0 = продолжаем наблюдение, 1 = сторож отработал
# и больше не нужен.
check() {
	# Каталога нет вовсе: пробник убран, а строка в crontab осталась.
	if [ ! -d "$RQ_DIR" ]; then
		uncron
		return 1
	fi

	if [ -f "$DONE" ]; then
		say "DONE: прогон закончен сам, конфигурацию не трогаю"
		retire
		return 1
	fi

	t=$(now)
	case "$t" in
		'' | *[!0-9]*)
			# Часов нет — судить не о чем. Это не повод крушить конфигурацию.
			say "не читается $UPTIME_FILE, проверка пропущена"
			return 0
			;;
	esac

	if [ -f "$DEADLINE" ]; then
		d=$(cat "$DEADLINE" 2>/dev/null)
		case "$d" in
			'' | *[!0-9]*) d="" ;;
		esac
		if [ -n "$d" ] && [ "$t" -gt "$d" ]; then
			restore_now "истёк дедлайн прогона (uptime $t > $d)"
			return 1
		fi
	fi

	if [ ! -f "$BEAT" ]; then
		restore_now "нет отметки живости $BEAT"
		return 1
	fi

	b=$(cat "$BEAT" 2>/dev/null)
	case "$b" in
		'' | *[!0-9]*)
			restore_now "отметка живости не читается"
			return 1
			;;
	esac

	# Отметка из будущего: uptime пошёл заново, то есть роутер перезагрузился
	# уже после начала прогона. Конфигурация загрузилась мутированной.
	if [ "$b" -gt "$t" ]; then
		restore_now "отметка живости из будущего ($b > $t) — роутер перезагружался"
		return 1
	fi

	age=$((t - b))
	if [ "$age" -gt "$STALE" ]; then
		restore_now "отметка живости протухла ($age с > $STALE с) — главный процесс мёртв"
		return 1
	fi

	return 0
}

daemon() {
	echo $$ >"$PIDFILE"
	say "сторож взведён: цикл ${TICK}с, порог протухания ${STALE}с"
	while :; do
		check || return 0
		sleep "$TICK"
	done
}

case "${1:-once}" in
	loop)
		daemon
		;;
	once)
		check
		;;
	abort)
		say "аварийный возврат по команде владельца"
		restore_now "abort" || exit 3
		: >"$RQ_DIR/ABORTED"
		;;
	*)
		echo "guard.sh: режим loop|once|abort, получено '$1'" >&2
		exit 2
		;;
esac

exit 0
