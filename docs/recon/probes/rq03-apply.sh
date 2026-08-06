#!/bin/sh
# rq03-apply.sh — измерительная нагрузка RQ-03/RQ-04. Исполняется НА РОУТЕРЕ.
#
# Это не продуктовый код. Это одноразовый пробник: он выясняет, существует ли
# глагол применения конфигурации, который трогает ТОЛЬКО станционное радио, и
# что при этом происходит с домашней точкой доступа — вторым диапазоном той же
# phy0. Плюс проверяет, рвёт ли `ubus call iwinfo scan` активную ассоциацию.
#
# ГЛАВНОЕ ОБСТОЯТЕЛЬСТВО: владелец может сидеть на той самой домашней точке,
# которую замер способен уронить. Тогда его ssh умрёт в середине прогона — это
# не сбой, а один из возможных ОТВЕТОВ. Придя по кабелю, он этого не увидит,
# но замер обязан работать одинаково в обоих случаях, и связь выясняется по
# факту (см. «свидетель»), а не по флагу. Поэтому пробник:
#   * снимает копию конфигурации и генерирует ЯВНЫЙ возврат до первой мутации;
#   * взводит отсоединённого сторожа (guard.sh), который вернёт конфигурацию
#     сам, если главный процесс умрёт, залипнет или роутер перезагрузится;
#   * живёт в /root/rq03 (overlay, переживает перезагрузку), а не в /tmp.
#
# РЕЖИМ --wired. Когда владелец пришёл по кабелю, его связь не зависит от
# того, что замер делает с радио, и платить за страховку следами на роутере
# незачем. Тогда скрипт приезжает через stdin (`ssh host 'sh -s' < …`), рабочий
# каталог берётся в tmpfs, сторож и crontab не заводятся вовсе, а возврат
# висит на trap этого же шелла: SIGHUP при смерти ssh, INT, TERM, EXIT.
# ЦЕНА НАЗВАНА ПРЯМО: перезагрузку роутера посреди прогона такой возврат не
# переживёт — вернуть сможет только человек. Дублёр — на ноутбуке, в обёртке.
#
# ИМЕНА ЖЕЛЕЗА НЕ ПИШУТСЯ ЛИТЕРАЛОМ. Станционное и AP радио выводятся из
# `ubus call network.wireless status` тем же правилом, что и демон
# (internal/wireless/wireless.go, ResolveRadios): interfaces[].config.mode ==
# "sta" — станция, "ap" — точка; имена радио сортируются, чтобы ответ не
# зависел от порядка обхода.
#
# КАНДИДАТЫ ГЛАГОЛА НЕ УГАДЫВАЮТСЯ. Пробник инвентаризует, что умеет ЭТА
# сборка (ubus -v list, /sbin/wifi), и меряет только подтверждённое, по
# возрастанию грубости. Грубые — только если узкие провалились.
#
#   rq03-apply.sh [--no-cron] [--include-blunt] [--budget СЕК]
#   rq03-apply.sh --wired          НИ ОДНОГО файла на роутере (см. ниже)
#   rq03-apply.sh --witness MAC    назначить свидетеля вручную
#   rq03-apply.sh --check-only     только предусловия, ноль мутаций
#   rq03-apply.sh --emit-restore   напечатать возврат и выйти, ноль мутаций
#
# Коды возврата:
#   0  прогон закончен, конфигурация возвращена
#   2  неверный аргумент
#   3  прогон уже идёт (замок занят)
#   4  нет обязательной утилиты
#   5  не выведены радио или их ifname
#   6  нечем отсоединить процесс (нет setsid/start-stop-daemon/nohup)
#   7  Selection не Single — мерить нечего
#   8  нет достижимой целевой сети
#   9  включён слой 3, но crond не запущен
#  10  нет /root/rq03/guard.sh — сторожа доставить забыли
#  11  сработал аварийный порог, прогон прекращён досрочно
#  12  не удалось снять снимок конфигурации
set -u

# ── пути. Их и только их подменяет песочница scripts/check-probe-rq03.sh ──
RQ_DIR=/root/rq03
WIRELESS_CFG=/etc/config/wireless
UPTIME_FILE=/proc/uptime
WIFI_SBIN=/sbin/wifi
INITD_NETWORK=/etc/init.d/network

TAG=rq03

BUDGET=1500        # весь прогон, с (25 минут) — дальше сторож всё вернёт
WAIT_ASSOC=45      # ждать ассоциации и L3, с
WAIT_CLIENT=30     # аварийный порог отсутствия СВИДЕТЕЛЯ на домашней точке, с
PAUSE=5            # пауза между переключением туда и обратно, с
NEED_PER_CAND=150  # столько бюджета нужно, чтобы начинать нового кандидата, с

NO_CRON=no
WIRED=no
WITNESS_PIN=
INCLUDE_BLUNT=no
CHECK_ONLY=no
EMIT_RESTORE=no

LOG=$RQ_DIR/probe.log
EVENTS=$RQ_DIR/events.tsv
SAMPLES=$RQ_DIR/samples.tsv
RESULTS=$RQ_DIR/results.tsv
SUMMARY=$RQ_DIR/summary.txt
INVENTORY=$RQ_DIR/inventory.txt
SCANLOG=$RQ_DIR/rq04-scan.txt
PHASE=$RQ_DIR/phase
BEAT=$RQ_DIR/beat
DEADLINE=$RQ_DIR/deadline
DONEF=$RQ_DIR/DONE
RESTORE=$RQ_DIR/restore.sh
GUARD=$RQ_DIR/guard.sh
TMPD=$RQ_DIR/tmp

while [ $# -gt 0 ]; do
	case "$1" in
		--no-cron)       NO_CRON=yes ;;
		--wired)         WIRED=yes ;;
		--witness)       WITNESS_PIN=$(printf '%s' "${2:-}" | tr 'a-z' 'A-Z'); shift ;;
		--include-blunt) INCLUDE_BLUNT=yes ;;
		--check-only)    CHECK_ONLY=yes ;;
		--emit-restore)  EMIT_RESTORE=yes ;;
		--budget)        BUDGET=${2:-}; shift ;;
		-h|--help)       sed -n '2,55p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
		*) echo "rq03-apply: неизвестный аргумент '$1'" >&2; exit 2 ;;
	esac
	shift
done

case "$BUDGET" in
	'' | *[!0-9]*) echo "rq03-apply: --budget хочет число секунд" >&2; exit 2 ;;
esac

# ── проводной режим: на роутере не остаётся НИ ОДНОГО файла ──
#
# Скрипт приезжает через stdin (`ssh host 'sh -s' < …`) и на флеш не ложится
# вовсе. Рабочий каталог — в tmpfs: временные файлы нужны, потому что ubus
# отвечает в файл, но /tmp это RAM, флеша он не касается и переживает ровно
# до перезагрузки. Убирается trap'ом на выходе.
#
# Слой 2 (отсоединённый сторож) и слой 3 (crontab) в этом режиме ОТКЛЮЧЕНЫ, и
# это осознанный размен, а не упрощение. Возврат держится на trap внутри этого
# же шелла: он отработает на обрыве ssh (SIGHUP), на Ctrl-C и на падении
# скрипта. Чего он НЕ переживёт — перезагрузки роутера и пропажи питания:
# тогда конфигурация останется переключённой, и вернуть её сможет только
# человек. Режим годится, когда владелец пришёл по кабелю и связь не зависит
# от того, что замер делает с радио. Второй рубеж — на ноутбуке: обёртка
# помнит команды возврата и применит их сама, если этот шелл умрёт молча.
if [ "$WIRED" = yes ]; then
	RQ_DIR=$(mktemp -d /tmp/rq03-XXXXXX) || { echo "rq03-apply: нет mktemp" >&2; exit 4; }
	LOG=$RQ_DIR/probe.log
	EVENTS=$RQ_DIR/events.tsv
	SAMPLES=$RQ_DIR/samples.tsv
	RESULTS=$RQ_DIR/results.tsv
	SUMMARY=$RQ_DIR/summary.txt
	INVENTORY=$RQ_DIR/inventory.txt
	SCANLOG=$RQ_DIR/rq04-scan.txt
	PHASE=$RQ_DIR/phase
	BEAT=$RQ_DIR/beat
	DEADLINE=$RQ_DIR/deadline
	DONEF=$RQ_DIR/DONE
	RESTORE=$RQ_DIR/restore.sh
	GUARD=$RQ_DIR/guard.sh
	TMPD=$RQ_DIR/tmp
fi

# ────────────────────────── время, журнал, отметки ──────────────────────────
#
# Время — из /proc/uptime: монотонно, есть всегда, не зависит от ntp. Дата на
# роутере без интернета врёт, а интернет мы как раз и передёргиваем.

now()  { awk '{printf "%d\n", $1}' "$UPTIME_FILE" 2>/dev/null; }
nowf() { awk '{printf "%.2f\n", $1}' "$UPTIME_FILE" 2>/dev/null; }

say() {
	printf '%s\n' "$*"
	printf '[%s] %s\n' "$(nowf)" "$*" >>"$LOG" 2>/dev/null
	logger -t "$TAG" "$*" 2>/dev/null
	return 0
}

die() {
	code=$1
	shift
	printf 'rq03-apply: ОТКАЗ(%s): %s\n' "$code" "$*" >&2
	printf '[%s] ОТКАЗ(%s): %s\n' "$(nowf)" "$code" "$*" >>"$LOG" 2>/dev/null
	logger -t "$TAG" "ОТКАЗ($code): $*" 2>/dev/null
	exit "$code"
}

beat() { now >"$BEAT" 2>/dev/null; return 0; }

# nap — единственный способ ждать в главном процессе: он ОБЯЗАН при этом
# трогать отметку живости, иначе сторож примет ожидание за смерть.
nap() {
	i=0
	while [ "$i" -lt "$1" ]; do
		sleep 1
		beat
		i=$((i + 1))
	done
}

ev() {
	printf '%s\t%s\t%s\n' "$(nowf)" "$1" "$2" >>"$EVENTS" 2>/dev/null
	return 0
}

phase() { printf '%s\n' "$1" >"$PHASE" 2>/dev/null; ev phase "$1"; }

# ────────────────────────────── ubus и json ─────────────────────────────────

jf() { jsonfilter -i "$1" -e "$2" 2>/dev/null; }

# ucall кладёт ответ в файл и возвращает код ubus. Пустой ответ и ошибка
# вызова — РАЗНЫЕ вещи, и путать их нельзя: «нет ассоциации» и «не смог
# спросить» отвечают на разные вопросы.
ucall() {
	out=$1
	shift
	if [ $# -ge 3 ]; then
		ubus call "$1" "$2" "$3" >"$out" 2>>"$LOG"
	else
		ubus call "$1" "$2" >"$out" 2>>"$LOG"
	fi
}

# Ключи верхнего уровня в ответе ubus. jsonfilter умеет доставать значение по
# пути, но не умеет перечислять имена ключей, а имена радио нам нужны именно
# как данные. Разбор идёт по СТРУКТУРЕ (глубина скобок), а не по отступам.
# Кавычка внутри значения сбила бы счёт, поэтому список ниже объединяется с
# именами секций wifi-device из UCI: лишнее имя безвредно (роли у него не
# найдётся), пропущенное — нет.
json_top_keys() {
	awk '
	{
		line = $0
		if (d == 1 && match(line, /^[ \t]*"[^"]+"[ \t]*:[ \t]*\{/)) {
			k = line
			sub(/^[ \t]*"/, "", k)
			sub(/".*$/, "", k)
			print k
		}
		n = gsub(/\{/, "{", line)
		m = gsub(/\}/, "}", line)
		d += n - m
	}' "$1"
}

# ─────────────────────────────── UCI-разбор ─────────────────────────────────

uci_show() { uci -q show wireless 2>/dev/null; }

uci_opt() { uci -q get "wireless.$1.$2" 2>/dev/null; }

# Секции wifi-iface с заданным mode. Если задан второй аргумент — ещё и с
# заданным device (так работает `ours` в internal/wireless/wireless.go).
sections_by_mode() {
	uci_show | awk -F= -v want="$1" -v dev="${2:-}" -v q="'" '
	{
		key = $1
		val = $2
		gsub(q, "", val)
		if (key ~ /\.mode$/) {
			s = key; sub(/^wireless\./, "", s); sub(/\.mode$/, "", s)
			mode[s] = val
			if (!(s in seen)) { seen[s] = 1; order[++n] = s }
		}
		if (key ~ /\.device$/) {
			s = key; sub(/^wireless\./, "", s); sub(/\.device$/, "", s)
			device[s] = val
			if (!(s in seen)) { seen[s] = 1; order[++n] = s }
		}
	}
	END {
		for (i = 1; i <= n; i++) {
			s = order[i]
			if (mode[s] != want) continue
			if (dev != "" && device[s] != dev) continue
			print s
		}
	}'
}

wifi_device_names() {
	uci_show | awk -F= '$2 == "wifi-device" { s = $1; sub(/^wireless\./, "", s); print s }'
}

# disabled_state повторяет правило демона (internal/uci/parse.go): отсутствие
# опции — ВКЛЮЧЕНО, нераспознанное значение — отдельный ответ, а не «включено».
disabled_state() {
	v=$(uci_opt "$1" disabled)
	case "$(printf '%s' "$v" | tr 'A-Z' 'a-z')" in
		'')                                 echo enabled ;;
		1 | on | true | yes | enabled)      echo disabled ;;
		0 | off | false | no | disabled)    echo enabled ;;
		*)                                  echo unparseable ;;
	esac
}

# ───────────────────────── генератор возврата ───────────────────────────────
#
# Возврат — это ЯВНЫЙ список команд, а не «откатим как-нибудь». Секции, у
# которой опции disabled не было, возвращается ЧИТАЕМЫЙ '0', а не удаление
# опции: на возврате важна гарантия и понятность, а не побайтовая точность.
# Последний глагол — самый тупой из существующих: `wifi` целиком. Узость нужна
# замеру, а не возврату.

gen_restore() {
	echo '#!/bin/sh'
	echo '# Возврат конфигурации wireless в состояние ДО пробника RQ-03.'
	echo '# Сгенерирован rq03-apply.sh. Запускать можно сколько угодно раз.'
	echo 'set -u'
	echo 'logger -t rq03-restore "возврат конфигурации wireless" 2>/dev/null'
	for s in $(sections_by_mode sta); do
		v=$(uci_opt "$s" disabled)
		[ -n "$v" ] || v=0
		echo "uci set wireless.$s.disabled='$v'"
	done
	echo 'uci commit wireless'
	echo '# Самый тупой глагол из существующих — на возврате важна гарантия.'
	echo 'wifi'
}

# ─────────────────────────── предусловия ────────────────────────────────────

need_tools() {
	miss=
	for t in "$@"; do
		command -v "$t" >/dev/null 2>&1 || miss="$miss $t"
	done
	[ -z "$miss" ] || die 4 "нет обязательных утилит:$miss"
}

# Отсоединение проверяется, а не предполагается. Тихий запуск в переднем плане
# был бы худшим исходом: обрыв ssh убил бы замер посреди применённого наполовину
# конфига, и вернуть его стало бы некому.
detach_kind() {
	if command -v setsid >/dev/null 2>&1; then
		echo setsid
	elif command -v start-stop-daemon >/dev/null 2>&1; then
		echo ssd
	elif command -v nohup >/dev/null 2>&1; then
		echo nohup
	else
		echo none
	fi
}

detach() {
	tag=$1
	shift
	case "$DETACH" in
		setsid) setsid "$@" </dev/null >>"$RQ_DIR/$tag.out" 2>&1 & DETACHED_PID=$! ;;
		nohup)  nohup "$@" </dev/null >>"$RQ_DIR/$tag.out" 2>&1 & DETACHED_PID=$! ;;
		ssd)
			prog=$1
			shift
			start-stop-daemon -S -b -x "$prog" -- "$@"
			DETACHED_PID=""
			;;
		*) die 6 "нечем отсоединить процесс" ;;
	esac
	return 0
}

# ──────────────────────────── шаг 1: предусловия ────────────────────────────

mkdir -p "$RQ_DIR" "$TMPD" 2>/dev/null

need_tools awk uci

if [ "$EMIT_RESTORE" = yes ]; then
	gen_restore
	# В проводном режиме за собой убираем сразу: каталог в tmpfs заводился
	# только ради журнала, а обещание «ни одного следа» распространяется и на
	# режимы, которые ничего не мутируют.
	[ "$WIRED" = yes ] && rm -rf "$RQ_DIR"
	exit 0
fi

need_tools flock jsonfilter ubus logger wifi

# Замок: два пробника одновременно — это две встречные мутации одного файла.
exec 9>"$RQ_DIR/lock" 2>/dev/null
flock -n 9 || die 3 "прогон уже идёт (занят $RQ_DIR/lock)"

# В проводном режиме сторожа нет по замыслу: он живёт файлом, а файлов мы не
# оставляем. Требовать его здесь значило бы отказать в режиме, ради которого
# всё и делалось.
[ "$WIRED" = yes ] || [ -f "$GUARD" ] || die 10 "нет $GUARD — доставьте сторожа вместе с нагрузкой"

DETACH=$(detach_kind)
[ "$DETACH" = none ] && die 6 "нет ни setsid, ни start-stop-daemon, ни nohup: отсоединить сторожа и сэмплер нечем, а без них обрыв ssh оставит конфигурацию применённой наполовину"

if [ "$NO_CRON" = no ] && [ "$WIRED" = no ]; then
	command -v crontab >/dev/null 2>&1 || die 9 "нет crontab, а слой 3 включён (снять: --no-cron)"
	pgrep crond >/dev/null 2>&1 || pidof crond >/dev/null 2>&1 ||
		die 9 "crond не запущен — строка в crontab никого не поднимет (снять: --no-cron)"
fi

WSTATUS=$TMPD/wireless-status.json
ucall "$WSTATUS" network.wireless status ||
	die 5 "не отвечает ubus call network.wireless status"

STA_RADIO=; AP_RADIO=; STA_IF=; AP_IF=; STA_SEC=; AP_SEC=; STA_NET=; AP_NET=

RADIO_NAMES=$( { json_top_keys "$WSTATUS"; wifi_device_names; } | sort -u )

for r in $RADIO_NAMES; do
	i=0
	while [ "$i" -lt 8 ]; do
		m=$(jf "$WSTATUS" "@['$r'].interfaces[$i].config.mode")
		[ -n "$m" ] || break
		ifn=$(jf "$WSTATUS" "@['$r'].interfaces[$i].ifname")
		sec=$(jf "$WSTATUS" "@['$r'].interfaces[$i].section")
		net=$(jf "$WSTATUS" "@['$r'].interfaces[$i].config.network[0]")
		case "$m" in
			sta) [ -n "$STA_RADIO" ] || { STA_RADIO=$r; STA_IF=$ifn; STA_SEC=$sec; STA_NET=$net; } ;;
			ap)  [ -n "$AP_RADIO" ]  || { AP_RADIO=$r;  AP_IF=$ifn;  AP_SEC=$sec;  AP_NET=$net; } ;;
		esac
		i=$((i + 1))
	done
done

[ -n "$STA_RADIO" ] || die 5 "станционное радио не выведено из network.wireless status (нет интерфейса с config.mode=sta). Гадать нельзя: индекс радио ничего не означает"
[ -n "$AP_RADIO" ]  || die 5 "радио домашней точки не выведено (нет интерфейса с config.mode=ap)"
[ -n "$STA_IF" ]    || die 5 "у станционного радио $STA_RADIO нет ifname — мерить нечем"
[ -n "$AP_IF" ]     || die 5 "у радио точки $AP_RADIO нет ifname — живость AP померить нечем"

SAME_PHY=нет
[ "$STA_RADIO" = "$AP_RADIO" ] && SAME_PHY="да (одно радио)"

# Selection: ровно одна станционная секция включена. Всё прочее — не «почини
# и меряй», а «мерить нечего»: непонятно, что считать исходным состоянием.
STA_SECS=$(sections_by_mode sta "$STA_RADIO")
[ -n "$STA_SECS" ] || die 7 "на $STA_RADIO нет ни одной станционной секции"

ENABLED=; UNPARSEABLE=
for s in $STA_SECS; do
	case "$(disabled_state "$s")" in
		enabled)     ENABLED="$ENABLED $s" ;;
		unparseable) UNPARSEABLE="$UNPARSEABLE $s" ;;
	esac
done

[ -z "$UNPARSEABLE" ] || die 7 "значение disabled не распознано в секциях:$UNPARSEABLE — Selection не Single"

set -- $ENABLED
case $# in
	1) ACTIVE_SEC=$1 ;;
	0) die 7 "ни одна станционная секция не включена (all_disabled) — исходного состояния нет, мерить нечего" ;;
	*) die 7 "включено несколько станционных секций:$ENABLED — Selection не Single, мерить нечего" ;;
esac

ACTIVE_SSID=$(uci_opt "$ACTIVE_SEC" ssid)
[ -n "$ACTIVE_SSID" ] || die 7 "у активной секции $ACTIVE_SEC нет ssid"

# Целевая сеть: вторая сохранённая станционная сеть. Без неё переключать
# нечего, а значит и мерить нечего.
SCAN=$TMPD/scan.json
SCAN_OK=no
if ucall "$SCAN" iwinfo scan "{\"device\":\"$STA_IF\"}"; then SCAN_OK=yes; fi
SCAN_SSIDS=$(jf "$SCAN" '@.results[*].ssid')

TARGET_SEC=; TARGET_SSID=; TARGET_WHY=
for s in $STA_SECS; do
	[ "$s" = "$ACTIVE_SEC" ] && continue
	ssid=$(uci_opt "$s" ssid)
	enc=$(uci_opt "$s" encryption)
	key=$(uci_opt "$s" key)
	if [ -z "$ssid" ]; then
		TARGET_WHY="$TARGET_WHY; $s: нет ssid"
		continue
	fi
	if [ "$enc" != none ] && [ -z "$key" ]; then
		TARGET_WHY="$TARGET_WHY; $s ($ssid): encryption=$enc, но key пуст — подключиться заведомо не выйдет"
		continue
	fi
	if [ "$SCAN_OK" = yes ] && ! printf '%s\n' "$SCAN_SSIDS" | grep -qxF "$ssid"; then
		TARGET_WHY="$TARGET_WHY; $s ($ssid): не видно в скане на $STA_IF"
		continue
	fi
	TARGET_SEC=$s
	TARGET_SSID=$ssid
	break
done

[ -n "$TARGET_SEC" ] || die 8 "нет достижимой целевой сети для переключения${TARGET_WHY:-; других станционных секций нет}"

cat <<EOF
Предусловия сошлись:
  станция:        $STA_RADIO / $STA_IF / секция $ACTIVE_SEC (ssid '$ACTIVE_SSID', сеть $STA_NET)
  домашняя точка: $AP_RADIO / $AP_IF / секция $AP_SEC (сеть $AP_NET)
  общее радио:    $SAME_PHY
  целевая сеть:   $TARGET_SEC (ssid '$TARGET_SSID')
  отсоединение:   $DETACH
  слой 3 (cron):  $([ "$NO_CRON" = yes ] && echo "выключен (--no-cron)" || echo включён)
  бюджет:         $BUDGET с
EOF

# ── адреса и свидетель: читаем ДО выхода по --check-only ──
#
# Раньше этот разбор стоял после снимка и сторожа, то есть уже в боевом
# прогоне, и сухой прогон о свидетеле не говорил ни слова. А это ровно то,
# что владелец обязан знать ДО запуска: кто именно будет доказывать живость
# домашней точки и будет ли вообще. Блок ничего не мутирует — только
# netstat, ubus и ip neigh, — поэтому его место здесь.

# ──────────────────── шаг 4: адреса для прямых измерений ────────────────────
#
# Пинг до владельца — это и есть прямое измерение того, моргнула ли домашняя
# точка для НАСТОЯЩЕГО клиента. Второго человека с ping у нас нет.

OWNER_IP=$(netstat -tn 2>/dev/null | awk '$4 ~ /[:.]22$/ && $6 == "ESTABLISHED" { ip = $5; sub(/[:.][0-9]+$/, "", ip); print ip }' | head -1)

LAN_DEV=
if [ -n "$AP_NET" ]; then
	if ucall "$TMPD/apnet.json" "network.interface.$AP_NET" status; then
		LAN_DEV=$(jf "$TMPD/apnet.json" '@.l3_device')
	fi
fi

OWNER_MAC=
OWNER_SEEN_IN_NEIGH=нет
if [ -n "$OWNER_IP" ] && [ -n "$LAN_DEV" ] && command -v ip >/dev/null 2>&1; then
	OWNER_MAC=$(ip neigh show dev "$LAN_DEV" 2>/dev/null | awk -v want="$OWNER_IP" '
		$1 == want { for (i = 1; i <= NF; i++) if ($i == "lladdr") print toupper($(i + 1)) }' | head -1)
	[ -n "$OWNER_MAC" ] && OWNER_SEEN_IN_NEIGH=да
fi

GW_IP=
if [ -n "$STA_NET" ] && ucall "$TMPD/stanet.json" "network.interface.$STA_NET" status; then
	GW_IP=$(jf "$TMPD/stanet.json" '@.route[*].nexthop' | head -1)
fi

# Форма аргументов ping выясняется вызовом, а не верой в наличие бинарника.
# busybox в разных сборках отвергает дробный -i и уходит с подсказкой по
# использованию; фоновый пинг тогда умирает на старте, а в сводке остаётся NA
# — неотличимое от «потерь не было». Признаком годности берётся ровно та
# строка, которую потом разбирает ping_stop: если её нет, разбирать нечего.
HAVE_PING=no
PING_ARGS=
PING_WHY=
if command -v ping >/dev/null 2>&1; then
	for form in "-i 0.2 -W 1" "-i 1 -W 1" "-i 1" ""; do
		ping -c 1 $form "${OWNER_IP:-127.0.0.1}" >"$TMPD/ping-probe.txt" 2>&1 || true
		if grep -q 'packet loss' "$TMPD/ping-probe.txt" 2>/dev/null; then
			HAVE_PING=yes
			PING_ARGS=$form
			break
		fi
	done
	[ "$HAVE_PING" = yes ] || PING_WHY=$(head -2 "$TMPD/ping-probe.txt" 2>/dev/null | tr '\n' ' ')
else
	PING_WHY="ping не найден"
fi

say "владелец: ip=${OWNER_IP:-НЕ ВЫВЕДЕН} mac=${OWNER_MAC:-НЕ ВЫВЕДЕН} (в neigh на ${LAN_DEV:-?}: $OWNER_SEEN_IN_NEIGH); upstream-шлюз: ${GW_IP:-НЕ ВЫВЕДЕН}; ping: $HAVE_PING${PING_ARGS:+ (ping -c N $PING_ARGS)}"
[ "$HAVE_PING" = yes ] || say "ВНИМАНИЕ: пинги не измеряются ($PING_WHY). Все потери_* будут NA — это ОТСУТСТВИЕ данных, а не отсутствие потерь."


ap_macs() {
	if ucall "$TMPD/al.json" iwinfo assoclist "{\"device\":\"$AP_IF\"}"; then
		jf "$TMPD/al.json" '@.results[*].mac' | tr 'a-z' 'A-Z' | sort | tr '\n' ',' | sed 's/,$//'
	else
		echo ERR
	fi
}

# ── кто у нас свидетель живости домашней точки ──
#
# Аварийный порог и главное измерение RQ-03 опираются на НАСТОЯЩЕГО клиента
# домашней точки: только он отвечает на вопрос «моргнула ли она».
#
# Владелец годится на эту роль, ТОЛЬКО если сам сидит на этой точке. Придя по
# кабелю, он остаётся в том же br-lan и виден в `ip neigh`, но в assoclist
# домашней точки его нет и не будет. Проверка «владелец на точке» тогда ложна
# всегда, счётчик отсутствия натикает WAIT_CLIENT, и аварийный порог оборвёт
# исправный прогон на первом же кандидате. Поэтому связь владельца выясняется
# ОДИН раз, до мутаций, и по факту, а не по флагу.
#
# Если владелец на кабеле, свидетелем становится любой другой клиент точки
# (телефон, ноутбук). Нет ни одного — живость точки меряется только
# структурно (ap_up, объект hostapd), и это прямо пишется в сводку: разница
# между «клиенты не переассоциировались» и «радио не гасло» существенная, и
# выдавать второе за первое нельзя.

BASE_AP_MACS=$(ap_macs)
WITNESS_MAC=
WITNESS_IP=
OWNER_LINK=неизвестно

case ",$BASE_AP_MACS," in
	*,"${OWNER_MAC:-нетмака}",*) OWNER_LINK=wifi; WITNESS_MAC=$OWNER_MAC; WITNESS_IP=$OWNER_IP ;;
	*)
		[ -n "$OWNER_MAC" ] && OWNER_LINK=провод
		# Свидетель, назначенный владельцем, главнее автовыбора: «первый из
		# списка» — выбор произвольный, а на точке может сидеть устройство,
		# которое вот-вот уснёт и выпадет из assoclist. Тогда аварийный порог
		# оборвал бы исправный прогон.
		if [ -n "$WITNESS_PIN" ]; then
			case ",$BASE_AP_MACS," in
				*,"$WITNESS_PIN",*) WITNESS_MAC=$WITNESS_PIN ;;
				*) die 8 "свидетель $WITNESS_PIN не найден на домашней точке; сейчас там: ${BASE_AP_MACS:-никого}" ;;
			esac
		else
			# Первый клиент точки, который не владелец.
			for m in $(printf '%s' "$BASE_AP_MACS" | tr ',' ' '); do
				[ "$m" = ERR ] && continue
				[ "$m" = "$OWNER_MAC" ] && continue
				WITNESS_MAC=$m
				break
			done
		fi
		if [ -n "$WITNESS_MAC" ] && [ -n "$LAN_DEV" ] && command -v ip >/dev/null 2>&1; then
			WITNESS_IP=$(ip neigh show dev "$LAN_DEV" 2>/dev/null | awk -v want="$WITNESS_MAC" '
				{ for (i = 1; i <= NF; i++) if ($i == "lladdr" && toupper($(i + 1)) == want) print $1 }' | head -1)
		fi
		;;
esac

if [ "$OWNER_LINK" = провод ] && [ -z "$WITNESS_MAC" ]; then
	say "ВНИМАНИЕ: владелец по кабелю, на домашней точке нет ни одного клиента — живость точки будет измерена только структурно. Подключите к ней телефон и перезапустите, если нужен прямой ответ."
fi

say "связь владельца: $OWNER_LINK; свидетель на домашней точке: ${WITNESS_MAC:-НЕТ} ${WITNESS_IP:+($WITNESS_IP)}; клиентов на точке при старте: ${BASE_AP_MACS:-нет}"

# witness_present — «настоящий клиент всё ещё на домашней точке». Именно на
# ней держится аварийный порог. Нет свидетеля — судить не о чем, и порог не
# срабатывает: обрывать прогон по отсутствию того, чего нет, нельзя.
witness_present() {
	[ -n "$WITNESS_MAC" ] || return 0
	m=$(ap_macs)
	[ "$m" = ERR ] && return 0   # не смогли спросить — это не доказательство пропажи
	case ",$m," in
		*,"$WITNESS_MAC",*) return 0 ;;
	esac
	return 1
}

if [ "$CHECK_ONLY" = yes ]; then
	cat <<EOF

  связь владельца: $OWNER_LINK
  свидетель:       ${WITNESS_MAC:-НЕТ}${WITNESS_IP:+ ($WITNESS_IP)}${WITNESS_PIN:+ [назначен вручную]}
  клиенты точки:   ${BASE_AP_MACS:-никого}
EOF
	if [ -z "$WITNESS_MAC" ]; then
		echo "  ВНИМАНИЕ: свидетеля нет — живость домашней точки будет измерена только"
		echo "  структурно. Подключите к ней устройство, если нужен прямой ответ."
	fi
	echo
	echo "--check-only: ничего не тронуто."
	[ "$WIRED" = yes ] && rm -rf "$RQ_DIR"
	exit 0
fi

# ─────────────── шаг 2: снимок и возврат ДО первой мутации ──────────────────

cp "$WIRELESS_CFG" "$RQ_DIR/wireless.orig" 2>/dev/null ||
	die 12 "не удалось скопировать $WIRELESS_CFG — без снимка мутировать нельзя"

gen_restore >"$RESTORE.new" || die 12 "не удалось сгенерировать возврат"
mv "$RESTORE.new" "$RESTORE"
chmod 0755 "$RESTORE" 2>/dev/null

say "снимок: $RQ_DIR/wireless.orig, возврат: $RESTORE"
sed 's/^/    /' "$RESTORE"

# ───────────────────── шаг 3: сторож и слой 3 ───────────────────────────────

START=$(now)
echo $((START + BUDGET)) >"$DEADLINE"
beat
: >"$EVENTS"
phase idle

if [ "$NO_CRON" = yes ]; then
	cat >"$RQ_DIR/HOWTO-RESTORE.txt" <<EOF
Пробник RQ-03 запущен БЕЗ строки в crontab (--no-cron). Это значит: если
роутер перезагрузится посреди замера, конфигурация останется переключённой
и вернуть её будет некому. Вернуть руками:

    ssh root@ROUTER 'sh $RESTORE'

Если ssh не пускает (домашняя точка не поднялась) — с кабелем или через
failsafe:

    sh $RESTORE

Исходная копия файла лежит здесь: $RQ_DIR/wireless.orig
Она НЕ применяется автоматически: восстанавливать надо читаемыми командами,
а не подкладыванием файла мимо uci.
EOF
	echo
	echo "=== СЛОЙ 3 ВЫКЛЮЧЕН (--no-cron). Возврат руками, если что: ==="
	cat "$RQ_DIR/HOWTO-RESTORE.txt"
	echo "==========================================================="
	echo
fi

if [ "$WIRED" = yes ]; then
	# Возврат вешаем на сигналы ЭТОГО шелла. HUP приходит, когда умирает ssh, —
	# ради него всё и затевалось.
	trap 'say "trap: возврат конфигурации"; sh "$RESTORE" >>"$LOG" 2>&1; rm -rf "$RQ_DIR"' EXIT
	trap 'say "trap: прервано сигналом"; sh "$RESTORE" >>"$LOG" 2>&1; rm -rf "$RQ_DIR"; exit 11' INT TERM HUP
	say "проводной режим: сторожа нет, возврат на trap; рабочий каталог $RQ_DIR (tmpfs)"
else

detach guard /bin/sh "$GUARD" loop
GUARD_LAUNCH_PID=${DETACHED_PID:-}

# Проверяем, что сторож ЖИВ, а не что мы его «наверное» запустили.
armed=no
i=0
while [ "$i" -lt 20 ]; do
	if [ -f "$RQ_DIR/guard.pid" ]; then armed=yes; break; fi
	if [ -n "$GUARD_LAUNCH_PID" ] && kill -0 "$GUARD_LAUNCH_PID" 2>/dev/null; then armed=yes; break; fi
	sleep 0.2
	i=$((i + 1))
done
[ "$armed" = yes ] || die 6 "сторож не поднялся ($DETACH) — мутировать конфигурацию без него нельзя"
say "сторож взведён ($DETACH)"
fi

if [ "$NO_CRON" = no ] && [ "$WIRED" = no ]; then
	CRONLINE="* * * * * $GUARD once"
	cur=$(crontab -l 2>/dev/null || true)
	case "$cur" in
		*"$GUARD"*) ;;
		*) printf '%s\n%s\n' "$cur" "$CRONLINE" | grep -v '^$' | crontab - 2>/dev/null ;;
	esac
	say "слой 3: строка в crontab добавлена ($CRONLINE)"
fi

# ───────────────────────────── шаг 5: сэмплер ───────────────────────────────
#
# Шаг выясняется вызовом, а не верой. Дробный sleep есть не во всех сборках
# busybox; где его нет, `sleep 0.25` отваливается МГНОВЕННО с ошибкой, а цикл
# начинает крутиться на полной скорости. Ровно так и вышло в прогоне
# 2026-08-05: задумано было 4 Гц, фактический шаг вышел 0.05 с — впятеро чаще,
# то есть вчетверо больше нагрузки на ubus, чем предполагалось.

SLEEP_TICK='sleep 1'
if sleep 0.25 2>/dev/null; then
	SLEEP_TICK='sleep 0.25'
elif command -v usleep >/dev/null 2>&1 && usleep 250000 2>/dev/null; then
	SLEEP_TICK='usleep 250000'
fi
say "шаг сэмплера: $SLEEP_TICK"

cat >"$RQ_DIR/sampler.sh" <<EOF
#!/bin/sh
# Сэмплер пробника RQ-03. Сгенерирован rq03-apply.sh, живёт отдельным
# процессом: главный в это время занят применением конфигурации.
set -u
RQ_DIR='$RQ_DIR'
UPTIME_FILE='$UPTIME_FILE'
SAMPLES='$SAMPLES'
TMPD='$TMPD'
STA_IF='$STA_IF'
AP_IF='$AP_IF'
AP_RADIO='$AP_RADIO'
STA_NET='$STA_NET'
SLEEP_TICK='$SLEEP_TICK'
EOF

cat >>"$RQ_DIR/sampler.sh" <<'SAMPLER'
echo $$ >"$RQ_DIR/sampler.pid"
T=$TMPD/sample

j() { jsonfilter -i "$1" -e "$2" 2>/dev/null | tr '\n' ',' | sed 's/,$//'; }

printf '# t\tphase\tsta_if\tassoc_ssid\tap_up\tap_clients\tap_macs\tap_conn_time\tnet_up\tnet_ipv4\tnet_gw\tnet_uptime\n' >>"$SAMPLES"

while :; do
	[ -f "$RQ_DIR/DONE" ] && break
	t=$(awk '{printf "%.2f\n", $1}' "$UPTIME_FILE" 2>/dev/null)
	ph=$(cat "$RQ_DIR/phase" 2>/dev/null)
	[ -n "$ph" ] || ph=-

	# ОШИБКА ВЫЗОВА и «нет ассоциации» — разные значения. ERR значит «не
	# смогли спросить», прочерк — «спросили, ассоциации нет».
	if ubus call iwinfo info "{\"device\":\"$STA_IF\"}" >"$T.info" 2>/dev/null; then
		ssid=$(j "$T.info" '@.ssid')
		[ -n "$ssid" ] || ssid=-
	else
		ssid=ERR
	fi

	if ubus call network.wireless status >"$T.wl" 2>/dev/null; then
		apup=$(jsonfilter -i "$T.wl" -e "@['$AP_RADIO'].up" 2>/dev/null)
		[ -n "$apup" ] || apup=-
	else
		apup=ERR
	fi

	if ubus call iwinfo assoclist "{\"device\":\"$AP_IF\"}" >"$T.al" 2>/dev/null; then
		macs=$(j "$T.al" '@.results[*].mac')
		[ -n "$macs" ] || macs=-
		n=$(jsonfilter -i "$T.al" -e '@.results[*].mac' 2>/dev/null | grep -c .)
		# connected_time есть не во всех сборках iwinfo. Отсутствие поля —
		# это NA, а не ноль: ноль означал бы «только что переассоциировался».
		ct=$(j "$T.al" '@.results[*].connected_time')
		[ -n "$ct" ] || ct=NA
	else
		macs=ERR; n=ERR; ct=ERR
	fi

	if ubus call "network.interface.$STA_NET" status >"$T.net" 2>/dev/null; then
		nup=$(j "$T.net" '@.up');       [ -n "$nup" ] || nup=-
		nip=$(j "$T.net" "@['ipv4-address'][0].address"); [ -n "$nip" ] || nip=-
		ngw=$(j "$T.net" '@.route[0].nexthop');           [ -n "$ngw" ] || ngw=-
		nut=$(j "$T.net" '@.uptime');   [ -n "$nut" ] || nut=-
	else
		nup=ERR; nip=ERR; ngw=ERR; nut=ERR
	fi

	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
		"$t" "$ph" "$STA_IF" "$ssid" "$apup" "$n" "$macs" "$ct" "$nup" "$nip" "$ngw" "$nut" >>"$SAMPLES"
	$SLEEP_TICK
done
SAMPLER

chmod 0755 "$RQ_DIR/sampler.sh" 2>/dev/null
detach sampler /bin/sh "$RQ_DIR/sampler.sh"
say "сэмплер запущен, пишет $SAMPLES"

# ───────────────────────── измерительные примитивы ──────────────────────────

assoc_ssid() {
	if ucall "$TMPD/info.json" iwinfo info "{\"device\":\"$STA_IF\"}"; then
		s=$(jf "$TMPD/info.json" '@.ssid')
		[ -n "$s" ] && printf '%s\n' "$s" || echo '-'
	else
		echo ERR
	fi
}

net_up() {
	if ucall "$TMPD/net.json" "network.interface.$STA_NET" status; then
		jf "$TMPD/net.json" '@.up'
	else
		echo ERR
	fi
}

net_uptime() {
	if ucall "$TMPD/net.json" "network.interface.$STA_NET" status; then
		jf "$TMPD/net.json" '@.uptime'
	else
		echo ERR
	fi
}


PING_OWNER_PID=
PING_GW_PID=
PING_WIT_PID=

ping_start() {
	[ "$HAVE_PING" = yes ] || return 0
	# Пинг до владельца. По кабелю это КОНТРОЛЬ, а не измерение точки доступа:
	# он обязан не терять ни пакета при любом исходе, и если потерял — значит
	# задело br-lan целиком, а это уже другой разговор.
	if [ -n "$OWNER_IP" ]; then
		ping -c 600 $PING_ARGS "$OWNER_IP" >"$TMPD/ping-owner.txt" 2>&1 &
		PING_OWNER_PID=$!
	fi
	# Пинг до свидетеля — то самое прямое измерение «моргнула ли точка для
	# настоящего клиента». Когда владелец сам на точке, свидетель это он, и
	# второй пинг не нужен.
	if [ -n "$WITNESS_IP" ] && [ "$WITNESS_IP" != "$OWNER_IP" ]; then
		ping -c 600 $PING_ARGS "$WITNESS_IP" >"$TMPD/ping-wit.txt" 2>&1 &
		PING_WIT_PID=$!
	fi
	if [ -n "$GW_IP" ]; then
		ping -c 600 $PING_ARGS "$GW_IP" >"$TMPD/ping-gw.txt" 2>&1 &
		PING_GW_PID=$!
	fi
	return 0
}

# SIGINT, а не SIGTERM: по нему ping печатает итоговую строку со статистикой,
# ради которой он и запускался.
ping_stop() {
	LOSS_OWNER=NA
	LOSS_GW=NA
	LOSS_WIT=NA
	if [ -n "$PING_WIT_PID" ]; then
		kill -INT "$PING_WIT_PID" 2>/dev/null
		wait "$PING_WIT_PID" 2>/dev/null
		LOSS_WIT=$(awk '/packet loss/ { for (i = 1; i <= NF; i++) if ($i ~ /%$/) print $i }' "$TMPD/ping-wit.txt" 2>/dev/null | head -1)
		[ -n "$LOSS_WIT" ] || LOSS_WIT=NA
		PING_WIT_PID=
	fi
	[ "$LOSS_WIT" = NA ] && [ "$OWNER_LINK" = wifi ] && LOSS_WIT=$LOSS_OWNER
	[ "$HAVE_PING" = yes ] || return 0
	if [ -n "$PING_OWNER_PID" ]; then
		kill -INT "$PING_OWNER_PID" 2>/dev/null
		wait "$PING_OWNER_PID" 2>/dev/null
		LOSS_OWNER=$(awk '/packet loss/ { for (i = 1; i <= NF; i++) if ($i ~ /%$/) print $i }' "$TMPD/ping-owner.txt" 2>/dev/null | head -1)
		[ -n "$LOSS_OWNER" ] || LOSS_OWNER=NA
		PING_OWNER_PID=
	fi
	if [ -n "$PING_GW_PID" ]; then
		kill -INT "$PING_GW_PID" 2>/dev/null
		wait "$PING_GW_PID" 2>/dev/null
		LOSS_GW=$(awk '/packet loss/ { for (i = 1; i <= NF; i++) if ($i ~ /%$/) print $i }' "$TMPD/ping-gw.txt" 2>/dev/null | head -1)
		[ -n "$LOSS_GW" ] || LOSS_GW=NA
		PING_GW_PID=
	fi
	ev ping "$1 владелец=$LOSS_OWNER свидетель=$LOSS_WIT шлюз=$LOSS_GW"
	return 0
}

stop_bg() {
	for f in "$RQ_DIR/sampler.pid"; do
		[ -f "$f" ] || continue
		p=$(cat "$f" 2>/dev/null)
		case "$p" in '' | *[!0-9]*) continue ;; esac
		kill "$p" 2>/dev/null
	done
	[ -n "$PING_OWNER_PID" ] && kill "$PING_OWNER_PID" 2>/dev/null
	[ -n "$PING_GW_PID" ] && kill "$PING_GW_PID" 2>/dev/null
	return 0
}

finish() {
	: >"$DONEF"
	stop_bg
	sh "$GUARD" once >/dev/null 2>&1
	say "прогон закончен, код $1"
	exit "$1"
}

emergency() {
	say "АВАРИЯ: $*"
	ev emergency "$*"
	phase emergency
	sh "$RESTORE" >>"$LOG" 2>&1
	say "аварийный restore.sh код $?"
	: >"$RQ_DIR/ABORTED"
	EMERGENCY_REASON=$*
	write_summary
	dump_wired
	finish 11
}

# wait_assoc ждёт ассоциации с нужным ssid И живого L3. Внутри же сидит
# аварийный порог: если клиент владельца пропал с домашней точки дольше
# WAIT_CLIENT — долбить дальше нельзя, владелец отрезан.
wait_assoc() {
	want=$1
	lim=$2
	t0=$(now)
	miss0=
	while :; do
		beat
		s=$(assoc_ssid)
		u=$(net_up)
		if [ "$s" = "$want" ] && [ "$u" = "true" ]; then
			return 0
		fi
		if witness_present; then
			miss0=
		else
			[ -n "$miss0" ] || miss0=$(now)
			if [ $(( $(now) - miss0 )) -gt "$WAIT_CLIENT" ]; then
				emergency "свидетель $WITNESS_MAC не вернулся на домашнюю точку за $WAIT_CLIENT с"
			fi
		fi
		if [ $(( $(now) - t0 )) -ge "$lim" ]; then
			return 1
		fi
		sleep 1
	done
}

switch_to() {
	uci set "wireless.$1.disabled=1" 2>>"$LOG"
	uci set "wireless.$2.disabled=0" 2>>"$LOG"
	uci commit wireless 2>>"$LOG"
	ev switch "$1 -> $2"
}

# ─────────────────────────── инвентаризация ─────────────────────────────────

WL_METHODS=$TMPD/wl-methods.txt
NET_METHODS=$TMPD/net-methods.txt

inventory() {
	ubus -v list network.wireless >"$WL_METHODS" 2>&1
	ubus -v list network >"$NET_METHODS" 2>&1
	{
		echo "# ubus -v list network.wireless"
		cat "$WL_METHODS"
		echo
		echo "# ubus -v list network"
		cat "$NET_METHODS"
		echo
		echo "# grep -n 'network.wireless|reconf|reload' $WIFI_SBIN"
		grep -n 'network\.wireless\|reconf\|reload' "$WIFI_SBIN" 2>&1
		echo
		echo "# ubus list | grep '^hostapd'"
		ubus list 2>/dev/null | grep '^hostapd'
		echo
		echo "# station: $STA_RADIO/$STA_IF   ap: $AP_RADIO/$AP_IF"
	} >"$INVENTORY"
}

has_method() { grep -q "\"$2\"[ ]*:" "$1"; }

# Метод, не принимающий device, узким кандидатом быть не может: он трогает
# всё сразу. Это и есть половина ответа на RQ-03, поэтому фиксируется явно.
method_takes_device() { grep "\"$2\"[ ]*:" "$1" | grep -q '"device"'; }

wifi_supports_reconf() { grep -q 'reconf' "$WIFI_SBIN" 2>/dev/null; }

# ─────────────────────────── кандидаты глагола ──────────────────────────────

do_verb() {
	case "$1" in
		ubus_reconf_dev)
			ubus call network.wireless reconf "{\"device\":\"$STA_RADIO\"}" >>"$LOG" 2>&1
			;;
		wifi_reconf_radio)
			wifi reconf "$STA_RADIO" >>"$LOG" 2>&1
			;;
		ubus_downup_dev)
			ubus call network.wireless down "{\"device\":\"$STA_RADIO\"}" >>"$LOG" 2>&1
			rc1=$?
			ubus call network.wireless up "{\"device\":\"$STA_RADIO\"}" >>"$LOG" 2>&1
			rc2=$?
			[ "$rc1" = 0 ] && [ "$rc2" = 0 ]
			;;
		wifi_all)
			wifi >>"$LOG" 2>&1
			;;
		network_reload)
			"$INITD_NETWORK" reload >>"$LOG" 2>&1
			;;
		*) return 127 ;;
	esac
}

budget_left() { echo $(( START + BUDGET - $(now) )); }

result_row() {
	printf '%s\n' "$*" >>"$RESULTS"
}

# measure — протокол одного кандидата целиком.
measure() {
	cid=$1
	label=$2
	pre=$3

	left=$(budget_left)
	if [ "$left" -lt "$NEED_PER_CAND" ]; then
		say "кандидат $label пропущен: бюджета осталось $left с, нужно $NEED_PER_CAND"
		result_row "$cid	$pre	SKIP	бюджет"
		return 1
	fi

	say "── кандидат: $label (network reload заранее: $pre)"
	phase "begin:$cid:$pre"
	ev begin "$cid pre=$pre"

	# туда
	switch_to "$ACTIVE_SEC" "$TARGET_SEC"
	phase "apply:$cid:$pre"
	ping_start
	prerc=-
	if [ "$pre" = yes ]; then
		ubus call network reload >>"$LOG" 2>&1
		prerc=$?
	fi
	t0=$(nowf)
	do_verb "$cid"
	rc1=$?
	t1=$(nowf)
	d1=$(awk -v a="$t0" -v b="$t1" 'BEGIN { printf "%.2f", b - a }')
	ev apply "$cid pre=$pre rc=$rc1 dur=$d1"

	fwd=нет
	wait_assoc "$TARGET_SSID" "$WAIT_ASSOC" && fwd=да
	ping_stop "$cid/$pre/туда"
	lo1=$LOSS_OWNER
	lg1=$LOSS_GW

	phase "settle:$cid:$pre"
	nap "$PAUSE"

	# обратно — ТЕМ ЖЕ глаголом
	phase "revert:$cid:$pre"
	switch_to "$TARGET_SEC" "$ACTIVE_SEC"
	ping_start
	[ "$pre" = yes ] && ubus call network reload >>"$LOG" 2>&1
	t2=$(nowf)
	do_verb "$cid"
	rc2=$?
	t3=$(nowf)
	d2=$(awk -v a="$t2" -v b="$t3" 'BEGIN { printf "%.2f", b - a }')
	ev revert "$cid pre=$pre rc=$rc2 dur=$d2"

	back=нет
	wait_assoc "$ACTIVE_SSID" "$WAIT_ASSOC" && back=да
	ping_stop "$cid/$pre/обратно"
	lo2=$LOSS_OWNER
	lg2=$LOSS_GW

	if [ "$back" = нет ]; then
		emergency "исходная сеть '$ACTIVE_SSID' не поднялась за $WAIT_ASSOC с после кандидата $label — остальные кандидаты не меряются"
	fi

	macs_now=$(ap_macs)
	same=да
	[ "$macs_now" = "$AP_MACS_BASE" ] || same=нет

	phase idle
	result_row "$cid	$pre	rc_туда=$rc1	${d1}s	ассоц=$fwd	rc_обратно=$rc2	${d2}s	вернулась=$back	AP_клиенты_как_в_начале=$same	потери_владелец=$lo1/$lo2	потери_шлюз=$lg1/$lg2	prerc=$prerc"
	say "── кандидат $label: туда rc=$rc1 (${d1}s, ассоциация: $fwd), обратно rc=$rc2 (${d2}s, вернулась: $back), клиенты AP как в начале: $same, потери до владельца: $lo1 / $lo2"

	[ "$fwd" = да ] && return 0
	return 1
}

# ───────────────────────────── шаг 6: RQ-04 ─────────────────────────────────

measure_scan() {
	say "── RQ-04: скан при живой ассоциации"
	{
		echo "# RQ-04: ubus call iwinfo scan {\"device\":\"$STA_IF\"} при живой ассоциации"
		echo "# до: ssid=$(assoc_ssid) net_uptime=$(net_uptime) ap_macs=$(ap_macs)"
	} >"$SCANLOG"

	k=1
	while [ "$k" -le 3 ]; do
		phase "scan:$k"
		ping_start
		u0=$(net_uptime)
		t0=$(nowf)
		ucall "$TMPD/scan$k.json" iwinfo scan "{\"device\":\"$STA_IF\"}"
		rc=$?
		t1=$(nowf)
		d=$(awk -v a="$t0" -v b="$t1" 'BEGIN { printf "%.2f", b - a }')
		n=$(jf "$TMPD/scan$k.json" '@.results[*].bssid' | grep -c .)
		nap 3
		u1=$(net_uptime)
		s1=$(assoc_ssid)
		ping_stop "scan$k"
		# Сброс uptime = L3 падал. Нечитаемое значение — НЕИЗВЕСТНО, а не
		# «сброса не было»: это разные ответы.
		reset=нет
		case "$u0$u1" in
			'' | *[!0-9]*) reset=НЕИЗВЕСТНО ;;
			*) [ "$u1" -ge "$u0" ] || reset=да ;;
		esac
		echo "скан $k: rc=$rc длительность=${d}s сетей=$n ssid_после='$s1' uptime $u0 -> $u1 (сброс: $reset) потери_владелец=$LOSS_OWNER потери_свидетель=$LOSS_WIT потери_шлюз=$LOSS_GW" >>"$SCANLOG"
		say "RQ-04 скан $k: rc=$rc ${d}s, сетей=$n, ассоциация после: '$s1', uptime wwan $u0 -> $u1 (сброс: $reset), потери до владельца: $LOSS_OWNER"
		ev scan "$k rc=$rc dur=$d n=$n ssid=$s1 uptime=$u0->$u1"
		k=$((k + 1))
	done
	phase idle
}

# dump_wired — выгрузка артефактов в поток. В проводном режиме рабочий каталог
# лежит в tmpfs и стирается trap'ом на выходе: файлов, которые можно было бы
# забрать потом, не существует. Поэтому всё, что нужно сохранить, уходит на
# ноутбук здесь и сейчас, размеченными блоками.
dump_wired() {
	[ "$WIRED" = yes ] || return 0
	for f in "$SUMMARY" "$INVENTORY" "$RESULTS" "$SCANLOG" "$EVENTS" "$SAMPLES" "$LOG"; do
		printf '===8<=== %s\n' "$(basename "$f")"
		cat "$f" 2>/dev/null || echo "(нет)"
	done
	printf '===8<=== END\n'
	return 0
}

# ──────────────────────────────── сводка ────────────────────────────────────

write_summary() {
	{
		echo "RQ-03/RQ-04 — сводка пробника"
		echo
		echo "станция:        $STA_RADIO / $STA_IF / секция $ACTIVE_SEC (ssid '$ACTIVE_SSID', сеть $STA_NET)"
		echo "домашняя точка: $AP_RADIO / $AP_IF / секция $AP_SEC (сеть $AP_NET)"
		echo "целевая сеть:   $TARGET_SEC (ssid '$TARGET_SSID')"
		echo "владелец:       ip=${OWNER_IP:-НЕ ВЫВЕДЕН} mac=${OWNER_MAC:-НЕ ВЫВЕДЕН}, связь: ${OWNER_LINK:-неизвестно}"
		echo "свидетель на домашней точке: ${WITNESS_MAC:-НЕТ} ${WITNESS_IP:+($WITNESS_IP)}"
		if [ -z "${WITNESS_MAC:-}" ]; then
			echo "  ВНИМАНИЕ: свидетеля не было — живость домашней точки измерена ТОЛЬКО структурно"
			echo "  (ap_up и объект hostapd). «Клиенты не переассоциировались» этим НЕ доказано."
		fi
		echo "upstream-шлюз:  ${GW_IP:-НЕ ВЫВЕДЕН}"
		echo "клиенты AP на старте: ${AP_MACS_BASE:-НЕТ}"
		echo "connected_time в assoclist: ${HAS_CONN_TIME:-неизвестно}"
		echo
		echo "== кандидаты (по возрастанию грубости) =="
		if [ "$HAVE_PING" != yes ]; then
			echo "  ВНИМАНИЕ: пинги не измерялись ($PING_WHY)."
			echo "  Все потери_* ниже = NA. Это ОТСУТСТВИЕ данных, а не отсутствие потерь;"
			echo "  живость домашней точки читайте по ap_clients и connected_time в samples.tsv."
		fi
		if [ -f "$RESULTS" ]; then cat "$RESULTS"; else echo "нет ни одного измеренного кандидата"; fi
		echo
		echo "== доступность глаголов в этой сборке =="
		cat "$TMPD/verbs.txt" 2>/dev/null
		echo
		if [ -n "${EMERGENCY_REASON:-}" ]; then
			echo "== ПРОГОН ПРЕКРАЩЁН ДОСРОЧНО =="
			echo "$EMERGENCY_REASON"
			echo
		fi
		echo "== RQ-04 =="
		cat "$SCANLOG" 2>/dev/null
		echo
		echo "Читать вместе с $SAMPLES (шаг: $SLEEP_TICK) и $EVENTS."
	} >"$SUMMARY"
}

# ──────────────────────────────── прогон ────────────────────────────────────

AP_MACS_BASE=$(ap_macs)
HAS_CONN_TIME=нет
if ucall "$TMPD/al0.json" iwinfo assoclist "{\"device\":\"$AP_IF\"}"; then
	ct=$(jf "$TMPD/al0.json" '@.results[*].connected_time')
	[ -n "$ct" ] && HAS_CONN_TIME=да
fi
say "клиенты домашней точки на старте: ${AP_MACS_BASE:-нет}; поле connected_time: $HAS_CONN_TIME"
EMERGENCY_REASON=

inventory
say "инвентаризация записана в $INVENTORY"

{
	echo "reconf в network.wireless:      $(has_method "$WL_METHODS" reconf && echo есть || echo нет)"
	echo "  принимает device:             $(method_takes_device "$WL_METHODS" reconf && echo да || echo нет)"
	echo "up/down в network.wireless:     $(has_method "$WL_METHODS" up && has_method "$WL_METHODS" down && echo есть || echo нет)"
	echo "  down принимает device:        $(method_takes_device "$WL_METHODS" down && echo да || echo нет)"
	echo "notify в network.wireless:      $(has_method "$WL_METHODS" notify && echo есть || echo нет)"
	echo "reload в network:               $(has_method "$NET_METHODS" reload && echo есть || echo нет)"
	echo "reconf в $WIFI_SBIN:            $(wifi_supports_reconf && echo есть || echo нет)"
} >"$TMPD/verbs.txt"
sed 's/^/    /' "$TMPD/verbs.txt"

measure_scan

# Узкие кандидаты, по возрастанию грубости, каждый в двух вариантах:
# без предварительного `ubus call network reload` и с ним. Если без него
# применение молча ничего не делает — это половина ответа на RQ-03.
NARROW=
has_method "$WL_METHODS" reconf && method_takes_device "$WL_METHODS" reconf && NARROW="$NARROW ubus_reconf_dev"
wifi_supports_reconf && NARROW="$NARROW wifi_reconf_radio"
has_method "$WL_METHODS" down && has_method "$WL_METHODS" up && method_takes_device "$WL_METHODS" down &&
	NARROW="$NARROW ubus_downup_dev"

[ -n "$NARROW" ] || say "ВНИМАНИЕ: узких кандидатов в этой сборке нет вовсе — это уже ответ на RQ-03"

NARROW_OK=no
for cid in $NARROW; do
	for pre in no yes; do
		if measure "$cid" "$cid" "$pre"; then NARROW_OK=yes; fi
	done
done

if [ "$NARROW_OK" = yes ]; then
	say "узкий кандидат сработал — грубые (wifi целиком, $INITD_NETWORK reload) не меряем"
else
	say "узкие кандидаты не дали ассоциации — переходим к грубым"
	measure wifi_all "wifi целиком" no || true
	if [ "$INCLUDE_BLUNT" = yes ]; then
		measure network_reload "$INITD_NETWORK reload" no || true
	else
		say "$INITD_NETWORK reload не меряется: нужен флаг --include-blunt"
	fi
fi

# Возврат в исходное состояние делаем явно и на своих условиях, а не надеемся,
# что последний кандидат оставил всё как было.
say "финальный возврат конфигурации"
sh "$RESTORE" >>"$LOG" 2>&1
say "restore.sh код $?"
nap 5

write_summary
sed 's/^/    /' "$SUMMARY"
dump_wired
finish 0
