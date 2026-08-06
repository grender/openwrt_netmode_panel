#!/bin/sh
# Замер RQ-03/RQ-04 на живом роутере: доставка, запуск, сбор результата.
#
#   ./scripts/probe-rq03.sh                проверить предусловия, ничего не трогать
#   ./scripts/probe-rq03.sh --go           запустить замер (мутирует конфигурацию)
#   ./scripts/probe-rq03.sh --collect      забрать результат после обрыва связи
#   ./scripts/probe-rq03.sh --abort        немедленный возврат и уборка
#   ./scripts/probe-rq03.sh --wired        НИ ОДНОГО файла на роутере
#   ./scripts/probe-rq03.sh --witness MAC  назначить свидетеля вручную
#   ./scripts/probe-rq03.sh --split FILE   разобрать сохранённый поток в raw/
#   ./scripts/probe-rq03.sh --host root@10.0.0.1 --no-cron --include-blunt
#
# Без --go скрипт только проверяет предусловия и печатает, что собирается
# делать. Это не перестраховка: замер переключает внешнюю сеть роутера, к
# которому владелец подключён по wifi, и запускать его случайной опечаткой
# в аргументах нельзя.
#
# ЕСЛИ ВЫ ПО КАБЕЛЮ — связь переживёт любой исход, и замер можно доводить до
# конца, включая грубые глаголы (--include-blunt). Оставьте при этом телефон
# на домашней точке: без клиента на ней вопрос «моргнула ли она» останется без
# прямого ответа, только структурный.
#
# ЕСЛИ ВЫ ПО WIFI — обрыв связи посреди замера ОЖИДАЕМЫЙ ИСХОД, А НЕ СБОЙ:
# именно способность глагола уронить домашнюю точку и выясняется. Замер идёт
# на роутере отсоединённо и переживает смерть ssh; результат забирается
# потом, --collect.
#
# Возврат конфигурации взводится ДО первой мутации тремя слоями (см.
# docs/recon/probes/README.md): сгенерированный restore.sh, отсоединённый
# сторож с проверкой сердцебиения и дедлайна, и строка в crontab на случай
# перезагрузки посреди прогона.
#
# РЕЖИМ --wired. На роутер не копируется НИЧЕГО: скрипт уходит в `sh -s`
# через stdin и на флеш не ложится. Сторожа и crontab нет — возврат висит на
# trap внутри того же шелла плюс на дублёре здесь, на ноутбуке: команды
# возврата берутся ДО замера и применяются, если прогон умер молча.
# Перезагрузку роутера посреди замера этот режим не переживёт.
set -eu

HOST=root@192.168.9.1
MODE=check
NO_CRON=""
BLUNT=""
WIRED=""
WITNESS=""
RESTORE_LOCAL=""
SB_OUT=""
SPLIT_FILE=""
RAW=${RQ03_RAW:-docs/recon/raw}

RQ=/root/rq03
PROBE=docs/recon/probes/rq03-apply.sh
GUARD=docs/recon/probes/guard.sh

while [ $# -gt 0 ]; do
	case "$1" in
		--go)            MODE=go ;;
		--collect)       MODE=collect ;;
		--abort)         MODE=abort ;;
		--host)          HOST=$2; shift ;;
		--no-cron)       NO_CRON="--no-cron" ;;
		--wired)         WIRED="--wired" ;;
		--witness)       WITNESS="--witness $2"; shift ;;
		--include-blunt) BLUNT="--include-blunt" ;;
		--split)         MODE=split; SPLIT_FILE=$2; shift ;;
		-h|--help)       sed -n '2,36p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
		*) echo "probe-rq03: неизвестный аргумент '$1'" >&2; exit 2 ;;
	esac
	shift
done

cd "$(dirname "$0")/.."

[ -f "$PROBE" ] && [ -f "$GUARD" ] || {
	echo "probe-rq03: нет $PROBE или $GUARD" >&2
	exit 1
}

say() { printf '\n\033[1m%s\033[0m\n' "$*"; }

# ─────────── одно соединение на всё ───────────

CTL=$(mktemp -u /tmp/rq03-ssh-XXXXXX)
cleanup() { ssh -S "$CTL" -O exit "$HOST" 2>/dev/null || true; }
trap cleanup EXIT INT TERM

sh_() { ssh -S "$CTL" "$HOST" "$@"; }
put() { sh_ "cat > $2.new && chmod $3 $2.new && mv $2.new $2"; }

# Отдельная функция для команд, которые МОГУТ оборвать связь: обрыв здесь —
# результат замера, а не ошибка запуска, и говорить о нём надо словами.
sh_soft() { ssh -S "$CTL" "$HOST" "$@" 2>/dev/null || return 1; }

deliver() {
	if [ -n "$WIRED" ]; then
		echo "  · проводной режим: на роутер не копируется ничего"
		return 0
	fi
	sh_ "mkdir -p $RQ"
	put - "$RQ/rq03-apply.sh" 0755 < "$PROBE"
	put - "$RQ/guard.sh" 0755 < "$GUARD"
	echo "  ✓ пробник и сторож доставлены в $RQ"
}

# run_probe — единственный способ выполнить нагрузку. В проводном режиме она
# уходит в `sh -s` через stdin и файлом на роутере не становится; иначе
# запускается доставленная копия.
run_probe() {
	if [ -n "$WIRED" ]; then
		sh_ "sh -s -- $WIRED $*" < "$PROBE"
	else
		sh_ "cd $RQ && sh rq03-apply.sh $*"
	fi
}

# Дублёр возврата на ноутбуке. В проводном режиме на роутере не остаётся
# сторожа, поэтому команды возврата снимаются ДО замера и живут здесь. Если
# прогон умрёт молча — trap ниже применит их сам.
arm_local_restore() {
	[ -n "$WIRED" ] || return 0
	RESTORE_LOCAL=$(mktemp /tmp/rq03-restore-XXXXXX)
	if ! run_probe --emit-restore > "$RESTORE_LOCAL" 2>/dev/null || [ ! -s "$RESTORE_LOCAL" ]; then
		echo "  ✗ не удалось снять команды возврата — без них замер запускать нельзя" >&2
		exit 1
	fi
	echo "  ✓ возврат снят на ноутбук ($(wc -l <"$RESTORE_LOCAL" | tr -d ' ') строк)"
}

# collect_wired — разбор потока на файлы. В проводном режиме забирать с
# роутера нечего: он ничего не хранит. Всё приехало сюда размеченными блоками.
# Поток передаётся аргументом, а не глобальной переменной: пока это была
# глобальная, её удалось обнулить в ветке успеха, и сбор молча пропускал
# ровно те прогоны, которые закончились хорошо.
collect_wired() {
	SRC=$1
	[ -s "$SRC" ] || { echo "  ✗ пробник ничего не выгрузил" >&2; return 1; }
	stamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	part() {
		awk -v want="$1" '
			/^===8<=== /{ cur = $2; next }
			cur == want { print }
		' "$SRC"
	}
	{
		echo "# snapshot: grenderRouter, замер RQ-03/RQ-04 (проводной режим), $stamp"
		echo "# command: scripts/probe-rq03.sh --wired --go"
		echo "# на роутере не создано ни одного файла: скрипт ушёл в sh -s через stdin"
		echo
		echo "== summary =="; part summary.txt
		echo; echo "== inventory =="; part inventory.txt
		echo; echo "== results =="; part results.tsv
		echo; echo "== rq04 scan =="; part rq04-scan.txt
	} > "$RAW"/27-rq03-apply-verbs.txt
	echo "  ✓ $RAW/27-rq03-apply-verbs.txt"

	{
		echo "# snapshot: grenderRouter, сэмплер замера RQ-03, $stamp"
		echo
		part samples.tsv
	} > "$RAW"/28-rq03-samples.tsv
	echo "  ✓ $RAW/28-rq03-samples.tsv"

	{
		echo "# snapshot: grenderRouter, отметки этапов замера RQ-03, $stamp"
		echo
		part events.tsv
		echo; echo "== probe.log =="; part probe.log
	} > "$RAW"/29-rq03-events.txt
	echo "  ✓ $RAW/29-rq03-events.txt"
}

local_restore_now() {
	[ -n "$RESTORE_LOCAL" ] && [ -s "$RESTORE_LOCAL" ] || return 0
	echo
	echo "Применяю возврат с ноутбука — прогон закончился не по-хорошему." >&2
	sh_ "sh -s" < "$RESTORE_LOCAL" >/dev/null 2>&1 &&
		echo "  ✓ конфигурация возвращена" ||
		echo "  ✗ возврат НЕ УДАЛСЯ. Команды лежат в $RESTORE_LOCAL, примените вручную" >&2
}

# ─────────── режимы ───────────

# split — разбор ранее снятого потока в raw/, без роутера и без ssh. Нужен
# двоим: песочнице, которой иначе нечем проверить сбор, и человеку, у которого
# сбор однажды не сработал, а сырой поток остался.
if [ "$MODE" = split ]; then
	collect_wired "$SPLIT_FILE"
	exit $?
fi

say "Подключение к $HOST — введите пароль (спросят один раз)"
ssh -M -S "$CTL" -o ControlPersist=180 -fN "$HOST"

case "$MODE" in

abort)
	say "Аварийный возврат"
	sh_ "[ -x $RQ/guard.sh ] && sh $RQ/guard.sh abort" >/dev/null 2>&1 || true
	sh_ "cat $RQ/guard.log 2>/dev/null | tail -5" || true
	if sh_ "[ -f $RQ/RESTORED ]"; then
		echo "  ✓ конфигурация возвращена"
	elif sh_ "[ -f $RQ/RESTORE-FAILED ]"; then
		echo "  ✗ возврат НЕ УДАЛСЯ — смотрите $RQ/guard.log и HOWTO-RESTORE.txt на роутере" >&2
		exit 1
	else
		echo "  · возвращать было нечего (замер не начинал мутировать)"
	fi
	sh_ "rm -rf $RQ" || true
	echo "  ✓ убрано за собой"
	;;

check)
	deliver
	say "Проверка предусловий (ничего не трогаем)"
	if run_probe --check-only $NO_CRON $BLUNT $WITNESS; then
		cat <<EOF

Предусловия сошлись. Замер НЕ запущен.

Что произойдёт при запуске: роутер несколько раз переключит внешнюю сеть
туда и обратно, замеряя каждый кандидат глагола применения. Домашняя точка
может при этом моргнуть — ровно это и выясняется.

Чем это грозит лично вам, зависит от того, как вы подключены. По кабелю в
LAN — ssh уцелеет при любом исходе. По wifi через домашнюю точку — вы, скорее
всего, потеряете связь, и это ожидаемый исход, а не сбой.

Свою связь и свидетеля пробник определяет сам, по факту, уже в боевом прогоне
(здесь, в проверке предусловий, до этого шага дело не доходит) — и печатает
обе строки в сводку. Если придёте по кабелю, оставьте на домашней точке
телефон: без клиента на ней живость точки измерится только структурно, и в
сводке будет прямо написано, что прямого ответа нет.

Запуск:

    ./scripts/probe-rq03.sh --go $NO_CRON $BLUNT

После обрыва связи, когда wifi вернётся:

    ./scripts/probe-rq03.sh --collect

Если что-то пошло не так:

    ./scripts/probe-rq03.sh --abort
EOF
	else
		echo
		echo "Предусловия НЕ сошлись — замер запускать нельзя. Причина выше." >&2
		exit 1
	fi
	;;

go)
	deliver
	say "Запуск замера"
	echo "Если вы по wifi — дальше связь может оборваться. Это ожидаемо."
	echo "Если по кабелю — связь уцелеет, но домашняя точка может моргнуть."
	echo

	if [ -n "$WIRED" ]; then
		# Проводной режим: держим соединение и смотрим вывод живьём. Отсоединять
		# нечего и незачем — связь по кабелю переживёт всё, что замер делает с
		# радио, а trap внутри шелла вернёт конфигурацию, если что-то оборвётся.
		arm_local_restore
		SB_OUT=$(mktemp /tmp/rq03-out-XXXXXX)
		SB_OK=$(mktemp -u /tmp/rq03-ok-XXXXXX)
		trap 'local_restore_now; cleanup' EXIT INT TERM
		echo
		# Признак успеха ставит сам пробник, а не конвейер: у пайплайна статус
		# последней команды, то есть tee, и он нулевой всегда. На нём «замер
		# закончился штатно» печаталось бы и после провала, а дублёр возврата
		# снимался бы ровно тогда, когда он и нужен.
		( run_probe $NO_CRON $BLUNT $WITNESS 2>&1 && : >"$SB_OK" ) | tee "$SB_OUT"
		if [ -f "$SB_OK" ]; then
			RESTORE_LOCAL=""   # прогон закончился сам и вернул всё сам
			trap cleanup EXIT INT TERM
			rm -f "$SB_OK"
			echo "  ✓ замер закончился штатно"
		else
			echo "  ✗ замер закончился ненулевым кодом — смотрите вывод выше" >&2
		fi
		collect_wired "$SB_OUT" || echo "  · сырой поток цел: $SB_OUT" >&2
		exit 0
	fi

	# Запуск отсоединённый: команда возвращает управление сразу, а замер
	# продолжается на роутере даже если ssh умрёт следом.
	sh_ "cd $RQ && setsid sh rq03-apply.sh $NO_CRON $BLUNT </dev/null >$RQ/run.out 2>&1 &" >/dev/null 2>&1 ||
		sh_ "cd $RQ && nohup sh rq03-apply.sh $NO_CRON $BLUNT </dev/null >$RQ/run.out 2>&1 &" >/dev/null 2>&1 ||
		{ echo "  ✗ не удалось запустить замер" >&2; exit 1; }

	echo "  ✓ замер идёт на роутере"
	echo

	i=0
	while [ "$i" -lt 180 ]; do
		if sh_soft "[ -f $RQ/DONE ]"; then
			echo "  ✓ замер закончился сам"
			break
		fi
		if ! sh_soft "true"; then
			cat <<EOF

Связь потеряна. Это ОЖИДАЕМЫЙ исход замера, а не сбой: применение
конфигурации к станционному радио задело домашнюю точку.

Замер продолжается на роутере и вернёт конфигурацию сам. Когда wifi
вернётся, заберите результат:

    ./scripts/probe-rq03.sh --collect
EOF
			exit 0
		fi
		sleep 10
		i=$((i + 1))
	done

	exec "$0" --collect --host "$HOST"
	;;

collect)
	say "Сбор результата"
	if ! sh_ "[ -d $RQ ]"; then
		echo "  ✗ на роутере нет $RQ — замер не запускали либо уже убрали" >&2
		exit 1
	fi

	# Состояние возврата — первое, что владелец обязан узнать.
	for m in RESTORED RESTORE-FAILED RESTORE-MISSING ABORTED; do
		sh_ "[ -f $RQ/$m ]" && echo "  · отметка: $m" || true
	done
	if sh_ "[ -f $RQ/RESTORE-FAILED ] || [ -f $RQ/RESTORE-MISSING ]"; then
		echo "  ✗ ВОЗВРАТ НЕ ОТРАБОТАЛ. Не убирайте $RQ, смотрите guard.log" >&2
	fi

	stamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	{
		echo "# snapshot: grenderRouter, замер RQ-03/RQ-04, $stamp"
		echo "# command: scripts/probe-rq03.sh --go   (docs/recon/probes/)"
		echo "# цель: существует ли глагол применения, трогающий только станционное радио"
		echo
		echo "== summary =="; sh_ "cat $RQ/summary.txt 2>/dev/null" || echo "(нет)"
		echo; echo "== inventory =="; sh_ "cat $RQ/inventory.txt 2>/dev/null" || echo "(нет)"
		echo; echo "== results =="; sh_ "cat $RQ/results.tsv 2>/dev/null" || echo "(нет)"
		echo; echo "== rq04 scan =="; sh_ "cat $RQ/rq04-scan.txt 2>/dev/null" || echo "(нет)"
		echo; echo "== guard.log =="; sh_ "cat $RQ/guard.log 2>/dev/null" || echo "(нет)"
	} > "$RAW"/27-rq03-apply-verbs.txt
	echo "  ✓ $RAW/27-rq03-apply-verbs.txt"

	{
		echo "# snapshot: grenderRouter, сэмплер замера RQ-03, $stamp"
		echo "# command: scripts/probe-rq03.sh --collect"
		echo
		sh_ "cat $RQ/samples.tsv 2>/dev/null" || echo "(нет)"
	} > "$RAW"/28-rq03-samples.tsv
	echo "  ✓ $RAW/28-rq03-samples.tsv"

	{
		echo "# snapshot: grenderRouter, отметки этапов замера RQ-03, $stamp"
		echo
		sh_ "cat $RQ/events.tsv 2>/dev/null" || echo "(нет)"
		echo; echo "== probe.log =="; sh_ "cat $RQ/probe.log 2>/dev/null" || echo "(нет)"
	} > "$RAW"/29-rq03-events.txt
	echo "  ✓ $RAW/29-rq03-events.txt"

	say "Готово"
	sh_ "cat $RQ/summary.txt 2>/dev/null" || echo "(сводки нет — смотрите 29-rq03-events.txt)"
	cat <<EOF

Файлы забраны. Пробник на роутере НЕ убран намеренно: если возврат не
отработал, там лежат guard.log и HOWTO-RESTORE.txt. Убрать вручную:

    ssh $HOST 'sh $RQ/guard.sh abort; rm -rf $RQ'
EOF
	;;
esac
