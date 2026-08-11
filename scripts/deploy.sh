#!/bin/sh
# Сборка и доставка netmoded на роутер.
#
#   ./scripts/deploy.sh                 собрать, залить бинарь и перезапустить
#   ./scripts/deploy.sh --install       полная установка: бинарь, netmode-apply,
#                                       netmode-wifi, init.d, сид, автозапуск
#   ./scripts/deploy.sh --run           залить и запустить в консоли
#   ./scripts/deploy.sh --no-restart    залить и оставить демон погашенным
#   ./scripts/deploy.sh --host root@10.0.0.1 --port 8089
#
# Вход по паролю: ssh спросит его ОДИН раз. Дальше все команды идут через
# то же соединение (ControlMaster), поэтому повторных запросов не будет.
#
# Залить бинарь можно только поверх остановленного демона, поэтому деплой
# всегда сначала гасит его. Перезапуск идёт через init.d, а тот вызывает
# netmode-apply — на пару секунд гаснет туннель и перезапускается
# firewall. Если сейчас так нельзя, деплойте с --no-restart.
#
# ЧТО ВЫ СЕЙЧАС СТАВИТЕ. Это фаза 2, и прежняя строка «правит только
# ВЫКЛЮЧЕННЫЕ секции, активную не трогает» больше не верна — не смягчена, а
# именно неверна (ADR-0026 заменил инвариант записи фазы 1).
#
# Демон фазы 2 умеет ПЕРЕКЛЮЧАТЬ ВНЕШНЮЮ СЕТЬ: по нажатию в панели он пишет
# wireless.<секция>.disabled ('0' целевой, '1' всем прочим включённым
# станционным), делает uci commit wireless и применяет узким глаголом через
# netmode-wifi (ADR-0025). Это первая операция в проекте, способная оборвать
# канал, по которому её запустили: если вы ходите на роутер через ту самую
# станционную сеть, переключение уронит и ssh, и панель. Домашняя точка
# grenderNet при этом не моргает — так выбран глагол, и это измерено
# (docs/recon/apply-verbs.md), — поэтому обратная дорога есть.
#
# Чего демон по-прежнему НЕ делает: не правит и не удаляет ВКЛЮЧЁННУЮ секцию
# (409 enabled_network_readonly), не трогает секции домашней точки и не
# трогает секции чужого радио вовсе, не откатывает конфиг (ADR-0006), не
# читает /etc/b4/b4.json.
#
# Сам деплой сетей не переключает. Он гасит демон, льёт бинарь и
# перезапускает — с побочным эффектом netmode-apply (см. выше про туннель).
#
# БЕЗ --install ЛЬЁТСЯ ТОЛЬКО БИНАРЬ. Скрипты слоя 2 (netmode-apply,
# netmode-wifi) остаются те, что уже на роутере, и если их там нет — деплой
# теперь отказывает пре-флайтом, а не рапортует успех. Раньше рапортовал:
# демон фазы 2 приехал без netmode-wifi, панель отдавалась с рабочей на вид
# кнопкой, а при нажатии конфигурация коммитилась и повисала неприменённой.
set -eu

HOST=root@192.168.9.1
PORT=8088
RUN=no
INSTALL=no
RESTART=yes
REMOTE=/usr/local/bin/netmoded
APPLY=/usr/local/bin/netmode-apply
WIFI=/usr/local/bin/netmode-wifi
INITD=/etc/init.d/netmoded

while [ $# -gt 0 ]; do
	case "$1" in
		--run)        RUN=yes ;;
		--install)    INSTALL=yes ;;
		--no-restart) RESTART=no ;;
		--host)       HOST=$2; shift ;;
		--port)       PORT=$2; shift ;;
		-h|--help) sed -n '2,21p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
		*) echo "неизвестный аргумент: $1" >&2; exit 2 ;;
	esac
	shift
done

cd "$(dirname "$0")/.."

# Что уедет на роутер при --install, проверяем ДО сборки и до запроса пароля.
# Половинчатая установка — демон новый, а скрипта применения нет — выглядит
# как удачный деплой и ломается только при первом нажатии в панели, кодом
# «нет исполнителя».
if [ "$INSTALL" = yes ]; then
	for f in files/usr/local/bin/netmode-apply files/usr/local/bin/netmode-wifi \
		files/etc/init.d/netmoded files/etc/config/netmode; do
		[ -f "$f" ] || { echo "нет $f — устанавливать нечего" >&2; exit 1; }
	done
fi

VER="0.2.0-phase2-$(date +%Y%m%d)"
BIN=build/netmoded

say() { printf '\n\033[1m%s\033[0m\n' "$*"; }

# ─────────── сборка ───────────

say "Сборка $VER"
scripts/sync-panel.sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath \
	-ldflags="-s -w -X main.version=$VER" -o "$BIN" ./cmd/netmoded
scripts/check-size.sh "$BIN"

# ─────────── одно соединение на всё ───────────

CTL=$(mktemp -u /tmp/netmoded-ssh-XXXXXX)
cleanup() { ssh -S "$CTL" -O exit "$HOST" 2>/dev/null || true; }
trap cleanup EXIT INT TERM

say "Подключение к $HOST — введите пароль (спросят один раз)"
ssh -M -S "$CTL" -o ControlPersist=180 -fN "$HOST"

sh_() { ssh -S "$CTL" "$HOST" "$@"; }
put() { sh_ "cat > $2.new && chmod $3 $2.new && mv $2.new $2"; }

# ─────────── проверки до заливки ───────────

say "Проверки на роутере"

ARCH=$(sh_ 'uname -m')
[ "$ARCH" = aarch64 ] || { echo "  ✗ архитектура $ARCH, бинарь под aarch64" >&2; exit 1; }
echo "  ✓ архитектура: $ARCH"

FREE=$(sh_ "df -k /usr/local 2>/dev/null || df -k /" | awk 'NR==2{print $4}')
[ "${FREE:-0}" -ge 7168 ] || { echo "  ✗ свободно ${FREE} КБ, нужно ~7168" >&2; exit 1; }
echo "  ✓ свободно: $((FREE / 1024)) МБ"

# Порт проверяем, только если демон ещё не наш: иначе он же его и держит.
#
# ОСТОРОЖНО с pgrep -f: удалённая команда сама содержит подстроку netmoded,
# и на некоторых оболочках процесс-обёртка матчит сам себя — тогда проверка
# всегда решает «занят нами» и пропускает чужую службу. На busybox роутера
# оболочка делает exec и лишнего процесса не остаётся, но подтверждено это
# не на нём. Ошибка здесь безопасна в одну сторону: если пре-флайт промахнётся,
# health-check после перезапуска пойдёт с токеном и по коду возврата, и чужая
# служба на том же адресе даст не-200 — деплой упадёт внятно, а не соврёт.
# В health-check этот приём поэтому не используется вовсе (см. wait_listening).
if sh_ "netstat -lnt 2>/dev/null | grep -q ':$PORT '" && ! sh_ "pgrep -f netmoded >/dev/null 2>&1"; then
	echo "  ✗ порт $PORT занят не нами:" >&2
	sh_ "netstat -lntp 2>/dev/null | grep ':$PORT '" >&2
	echo "    укажите другой: --port 8089" >&2
	exit 1
fi
echo "  ✓ порт $PORT свободен либо занят нами"

LAN=$(sh_ "uci get network.lan.ipaddr 2>/dev/null" | sed 's#/.*##')
[ -n "$LAN" ] || { echo "  ✗ демон не стартует без адреса — это защита, а не сбой" >&2; exit 1; }
echo "  ✓ LAN-адрес: $LAN"

# flock проверяется ДО заливки, потому что без него не работает ни одно
# применение: и netmode-apply, и netmode-wifi берут им замок и без него
# отказывают кодом 7 (ADR-0020, ADR-0027). Узнать об этом при первом нажатии
# «переключить сеть» — значит узнать в худший момент: конфигурация к тому
# времени уже записана и применения ждёт.
#
# Отказ здесь жёсткий, а не предупреждение: роутер без flock не умеет ни
# сменить режим, ни сменить внешнюю сеть, то есть панель на нём — витрина.
sh_ "command -v flock >/dev/null 2>&1" || {
	echo "  ✗ на роутере нет flock — без него ни смена режима, ни смена сети не применяются" >&2
	echo "    поставьте его: opkg update && opkg install flock  (или включите апплет в busybox)" >&2
	exit 1
}
echo "  ✓ flock есть — применение сможет взять замок"

# Скрипты слоя 2 проверяются на РОУТЕРЕ и при ЛЮБОМ деплое, кроме --install.
#
# Проверка выше по файлу (`[ -f "$f" ]` под INSTALL=yes) смотрит на локальное
# дерево и отвечает на другой вопрос — «есть ли что заливать». Что лежит на
# роутере, она не знает.
#
# Дыра стоила живого отказа. Обычный деплой льёт один бинарь: демон фазы 2
# приехал на роутер, где netmode-wifi никогда не устанавливался, и деплой
# отрапортовал успех. Панель отдавалась с рабочей на вид кнопкой
# переключения сети; при нажатии демон писал и КОММИТИЛ конфигурацию, а
# применить её было нечем — станция висела не подключённой ни к старой сети,
# ни к новой, пока владелец не передёрнул радио руками из LuCI.
#
# Отказ жёсткий, а не предупреждение, и довод тот же, что тремя строками
# выше про flock: роутер без этих скриптов умеет записать намерение и не
# умеет его исполнить, то есть панель на нём — витрина. Предупреждение в
# зелёном выводе деплоя не читают.
#
# При INSTALL=yes пропускается: там они как раз и заливаются ниже.
if [ "$INSTALL" = no ]; then
	sh_ "[ -x $WIFI ] && [ -x $APPLY ]" || {
		echo "  ✗ на роутере нет скриптов применения (нужны $APPLY и $WIFI):" >&2
		sh_ "ls -l $APPLY $WIFI 2>&1" | sed 's/^/      /' >&2
		echo "    демон запишет намерение и не применит его — переустановите пакет целиком:" >&2
		echo "        ./scripts/deploy.sh --install" >&2
		exit 1
	}
	echo "  ✓ скрипты применения на месте"
fi

# ─────────── заливка ───────────

say "Заливка"

# Останавливаем прошлый экземпляр: поверх работающего файла не записать.
sh_ "[ -x $INITD ] && $INITD stop >/dev/null 2>&1; killall netmoded 2>/dev/null; sleep 0.3; mkdir -p $(dirname $REMOTE)" || true

put - "$REMOTE" 0755 < "$BIN"
echo "  ✓ $REMOTE"

GOT=$(sh_ "$REMOTE -version")
[ "$GOT" = "$VER" ] || { echo "  ✗ версия на роутере '$GOT' ≠ собранной '$VER'" >&2; exit 1; }
echo "  ✓ версия совпала: $GOT"

if [ "$INSTALL" = yes ]; then
	put - "$APPLY" 0755 < files/usr/local/bin/netmode-apply
	echo "  ✓ $APPLY"

	# Рядом с netmode-apply и теми же правами: это второй скрипт слоя 2. Без
	# него смена внешней сети отказывает причиной executor_missing, а панель
	# показывает баннер «на роутере не хватает скриптов применения» ещё до
	# нажатия (missing_executors в /api/status).
	put - "$WIFI" 0755 < files/usr/local/bin/netmode-wifi
	echo "  ✓ $WIFI"

	put - "$INITD" 0755 < files/etc/init.d/netmoded
	echo "  ✓ $INITD"

	# Сид конфига НЕ перезаписывает существующий: там уже может быть
	# выбранный режим и правки владельца.
	if sh_ "[ -f /etc/config/netmode ]"; then
		echo "  · /etc/config/netmode уже есть, не трогаем"
	else
		sed "s/option listen '192.168.9.1'/option listen '$LAN'/; s/option port '8088'/option port '$PORT'/" \
			files/etc/config/netmode | sh_ "cat > /etc/config/netmode.new && chmod 0600 /etc/config/netmode.new && mv /etc/config/netmode.new /etc/config/netmode"
		echo "  ✓ /etc/config/netmode (listen=$LAN, port=$PORT, права 0600)"
	fi

	sh_ "$INITD enable" >/dev/null 2>&1 || true
	echo "  ✓ автозапуск включён"
fi

# ─────────── перезапуск и проверка, что панель ожила ───────────

# Слушает демон то, что записано в его конфиге, а не то, что передали
# в --port: --port нужен пре-флайту и первому сиду. Проверять надо по
# настоящему адресу, иначе health-check соврёт на нестандартном порту.
health_target() {
	RPORT=$(sh_ "uci get netmode.main.port 2>/dev/null" || true)
	[ -n "$RPORT" ] || RPORT=$PORT
	BIND=$(sh_ "uci get netmode.main.listen 2>/dev/null" || true)
	[ -n "$BIND" ] || BIND=$LAN
}

# Ждём, пока сокет появится. pgrep -f netmoded здесь не годится:
# удалённая команда сама содержит эту подстроку и матчит саму себя.
wait_listening() {
	i=0
	while [ "$i" -lt 20 ]; do
		if sh_ "netstat -lnt 2>/dev/null | grep -q '$BIND:$RPORT'"; then
			return 0
		fi
		sleep 0.5
		i=$((i + 1))
	done
	return 1
}

# Открытый сокет ещё не значит «панель отдаётся»: запрашиваем её всерьёз,
# с токеном, и судим по КОДУ ВОЗВРАТА клиента.
#
# Заголовки не разбираем. На OpenWrt wget — это busybox или
# uclient-fetch: GNU-шный -S они не понимают и падают на разборе
# аргументов, так и не сходив на сервер, а про 401 каждый пишет своё.
# С токеном ответ ровно 200, и curl -f / wget возвращают 0 только на нём.
#
# Токен идёт ЗАГОЛОВКОМ, а не в query, и читается на роутере — не приезжает
# от нас аргументом. Три причины, и все три настоящие: в query он попал бы
# в argv клиента (виден в ps на роутере), в текст ошибки (uclient-fetch
# печатает URL целиком, и токен уехал бы в терминал разработчика), и в наш
# собственный вызов ssh. Проект держит секреты строго: на ответ с секретом
# ставится no-store, а TestNikkiPanelNeverLogsSecret стережёт журнал, — деплою
# нет причин быть исключением.
http_alive() {
	PROBE=$(sh_ "T=\$(cat /etc/netmoded/token 2>/dev/null)
	if command -v curl >/dev/null 2>&1; then
		curl -fsS -m 5 -o /dev/null -H \"Authorization: Bearer \$T\" '$1'
	else
		wget -q -T 5 --header=\"Authorization: Bearer \$T\" -O /dev/null '$1'
	fi" 2>&1) && return 0
	[ -z "$PROBE" ] || echo "    $PROBE" >&2
	return 1
}

tail_log() { sh_ "logread -e netmoded 2>/dev/null | tail -20" >&2 || true; }

print_panel() {
	[ -n "${TOKEN:-}" ] || TOKEN=$(sh_ "cat /etc/netmoded/token 2>/dev/null" || true)
	if [ -n "$TOKEN" ]; then
		echo "Панель: http://${BIND:-$LAN}:${RPORT:-$PORT}/?token=$TOKEN"
		echo
		echo "Открытую вкладку перезагрузите жёстко (Cmd-Shift-R): статика зашита"
		echo "в бинарь и отдаётся без ETag, браузер может держать прежний app.js."
	else
		echo "Демон запущен, но токен ещё не создан. Посмотрите:"
		echo
		echo "    ssh $HOST logread -e netmoded"
	fi
}

if [ "$RUN" = yes ]; then
	say "Запуск в консоли — Ctrl-C останавливает"
	echo
	ssh -S "$CTL" -t "$HOST" "$REMOTE" || true
	echo

	# Сюда мы попадаем ровно тогда, когда демон УМЕР: ssh -t держит консоль,
	# пока он жив, и возвращается на Ctrl-C или на падении. Печатать здесь
	# «Готово» и ссылку на панель значило бы рапортовать успех над трупом и
	# дать владельцу мёртвый адрес — а роутер при этом остаётся без демона:
	# заливка сделала stop, init.d в этой ветке не звался, procd поднимет
	# его только на перезагрузке.
	say "Демон остановлен"
	printf '%s\n' \
		"Консольный запуск закончился, на роутере демон не работает. Поднять:" \
		"" \
		"    ssh $HOST $INITD start" \
		"" \
		"Или перезалить с перезапуском: ./scripts/deploy.sh"
elif [ "$RESTART" = no ]; then
	say "Готово"
	printf '%s\n' \
		"Бинарь залит, демон оставлен погашенным (--no-restart). Поднять:" \
		"" \
		"    ssh $HOST $INITD start"
elif ! sh_ "[ -x $INITD ]"; then
	say "Готово"
	printf '%s\n' \
		"Бинарь залит, но $INITD на роутере нет — перезапускать нечего." \
		"Полная установка (init.d, конфиг, автозапуск):" \
		"" \
		"    ./scripts/deploy.sh --install" \
		"" \
		"Запустить вручную и посмотреть лог:" \
		"" \
		"    ssh $HOST $REMOTE"
else
	say "Перезапуск"

	# restart, а не start: заливка уже сделала stop, но procd мог успеть
	# поднять старый экземпляр по respawn.
	sh_ "$INITD restart" >/dev/null 2>&1 || true

	health_target

	if wait_listening; then
		echo "  ✓ слушает $BIND:$RPORT"
	else
		echo "  ✗ демон не поднялся за 10 с — последние строки лога:" >&2
		tail_log
		exit 1
	fi

	TOKEN=$(sh_ "cat /etc/netmoded/token 2>/dev/null" || true)

	if [ -z "$TOKEN" ]; then
		echo "  · токена ещё нет, панель не проверена"
	elif ! sh_ "command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1"; then
		echo "  · ни curl, ни wget на роутере, панель не проверена"
	elif http_alive "http://$BIND:$RPORT/"; then
		echo "  ✓ панель отвечает"
	else
		echo "  ✗ порт слушается, но панель не отдаётся — последние строки лога:" >&2
		tail_log
		exit 1
	fi

	say "Готово"
	print_panel
fi

if [ "$INSTALL" = yes ]; then
	cat <<EOF

Осталось сделать РУКАМИ — демон чужой crontab не правит:

    ssh $HOST 'crontab -l | grep -v happ2clash | crontab -'

Расписание обновления подписки теперь внутри демона (SPEC §9), и две
копии дёргали бы конвертер вдвое чаще, чем нужно.
EOF
fi
