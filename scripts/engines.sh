# shellcheck shell=sh
# Установка движков nikki и b4 на систему, где их ещё нет (ADR-0046).
#
# Подключается через `. scripts/engines.sh` из deploy.sh (фаза «Движки» при
# --install) и из engines-plan.sh (только решение, без действий). Сам по
# себе ничего не выполняет: только определяет функции и переменные.
#
# Главное правило — «стоит — не трогаем». Решение принимается по тем же
# признакам, по которым движком пользуется netmode-apply: есть исполняемый
# /etc/init.d/<движок> — движок установлен, и ни пакеты, ни конфиг, ни
# профиль владельца не трогаются; расхождение с закреплённой версией — только
# предупреждение. Поэтому на роутере, где всё поставлено руками, фаза
# движков — пустой проход.
#
# Пакеты качаются на Mac, а не на роутере: и фид nikki, и releases GitHub с
# роутера не открываются (docs/recon/raw/98-engines-fresh-install-vm.txt).
# На роутер файлы едут через `ssh cat` — sftp-сервера в dropbear нет.
#
# Требует от подключающего скрипта: sh_ (команда на роутере через готовое
# ssh-соединение) и put (заливка stdin в файл: put - путь права).

# ─────────── источники ───────────

ENGINES_LOCK=${ENGINES_LOCK:-scripts/engines.lock}
ENGINES_CACHE=${ENGINES_CACHE:-build/engines}
# Версионные релизы на GitHub, а не скользящий фид nikkinikki.pages.dev:
# у релиза постоянный адрес и опубликованный sha256, фид же отдаёт только
# последнюю версию и с роутера недоступен. Оба адреса переопределяются —
# например, на своё зеркало.
ENGINES_NIKKI_BASE=${ENGINES_NIKKI_BASE:-https://github.com/nikkinikki-org/OpenWrt-nikki/releases/download}
ENGINES_B4_BASE=${ENGINES_B4_BASE:-https://github.com/DanielLavrushin/b4/releases/download}

# Какие пакеты из архива nikki ставятся. Архив — локальный фид целиком: в
# нём ещё mihomo-alpha (второе ядро, конфликтует с mihomo-meta) и переводы
# на китайский. Зависимости (kmod-tun, yq, …) apk берёт из штатного фида.
ENGINES_NIKKI_PKGS="nikki mihomo-meta luci-app-nikki luci-i18n-nikki-ru"

# kmod, без которых b4 не перехватывает трафик (список — установщик
# апстрима, installer/platforms/openwrt.sh). nft-tproxy и nft-socket
# приходят и с nikki, но b4 может ставиться без него.
ENGINES_B4_KMODS="kmod-nft-queue kmod-nft-nat kmod-nft-tproxy kmod-nft-socket"

# Каталог на роутере для временной заливки пакетов (в tmpfs).
ENGINES_RTMP=/tmp/netmoded-engines

# Сколько свободного места в КБ нужно на / сверх установки движков: сам
# демон и скрипты (порог — тот же, что у проверки в deploy.sh).
ENGINES_SPARE_KB=7168

# ─────────── вывод ───────────
#
# Строки одного вида на весь деплой: «  ✓» сделано, «  ·» пропущено или уже
# так, «  ⚠» предупреждение (работа идёт дальше), «  ✗» отказ (в stderr).
# Номер шага в заголовке — чтобы по обрывку вывода было видно, где встали.

STEP_N=0
STEP_TOTAL=${STEP_TOTAL:-0}
CUR_STEP=
step() {
	STEP_N=$((STEP_N + 1))
	CUR_STEP=$*
	if [ "$STEP_TOTAL" -gt 0 ]; then
		printf '\n\033[1m[%s/%s] %s\033[0m\n' "$STEP_N" "$STEP_TOTAL" "$*"
	else
		printf '\n\033[1m%s\033[0m\n' "$*"
	fi
}
ok()   { printf '  ✓ %s\n' "$*"; }
skip() { printf '  · %s\n' "$*"; }
warn() { printf '  ⚠ %s\n' "$*" >&2; }

# fail ЧТО СОСТОЯНИЕ ДЕЛАТЬ... — отказ с тремя частями и выход.
#
# Одной строки «не вышло» мало: человек у терминала должен понять, в каком
# виде остался роутер (можно ли просто повторить) и что именно набрать.
# Каждая строка после второй — отдельная команда или совет.
fail() {
	printf '  ✗ %s\n' "$1" >&2
	printf '    состояние: %s\n' "$2" >&2
	shift 2
	if [ $# -gt 0 ]; then
		printf '    что делать:\n' >&2
		for l in "$@"; do printf '        %s\n' "$l" >&2; done
	fi
	[ -z "${DEPLOY_LOG:-}" ] || printf '    полный журнал: %s\n' "$DEPLOY_LOG" >&2
	exit 1
}

# rlog ТЕКСТ — строка в системный журнал роутера (logread -e netmoded-deploy).
#
# Только для шагов, которые МЕНЯЮТ систему: если вывод на Mac потерян, по
# журналу роутера всё равно видно, что и когда поставил деплой. Текст идёт
# через stdin, а не аргументом — без кавычек внутри кавычек. Отказ logger
# не валит деплой (так же, как log() в netmode-apply).
rlog() {
	printf '%s\n' "$*" | sh_ 'logger -t netmoded-deploy 2>/dev/null || true' 2>/dev/null || true
}

# run_remote ОПИСАНИЕ КОМАНДА — выполнить на роутере, вывод придержать.
#
# Удачно — одна строка «✓ описание». Неудачно — «✗ описание (код N)» и
# хвост вывода команды с отступом. Так apk при успехе не заливает экран, а
# при отказе его текст не теряется. Код возврата — кода команды; решает, что
# делать с отказом, вызывающий (обычно — fail с состоянием и советом).
RUN_OUT=
run_remote() {
	_desc=$1
	shift
	if RUN_OUT=$(sh_ "$*" 2>&1); then
		ok "$_desc"
		return 0
	else
		_rc=$?
		printf '  ✗ %s (код %s), последние строки:\n' "$_desc" "$_rc" >&2
		printf '%s\n' "$RUN_OUT" | tail -20 | sed 's/^/      /' >&2
		return "$_rc"
	fi
}

# ─────────── закреплённые версии и загрузка на Mac ───────────

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

# engines_pin ENGINE ARCH — строка lock «version file sha256» или пусто.
# Для b4 любая aarch64_* сводится к arm64: бинарь статический, один на всех.
engines_pin() {
	_arch=$2
	[ "$1" = b4 ] && case "$_arch" in aarch64_*) _arch=arm64 ;; esac
	awk -v e="$1" -v a="$_arch" '!/^#/ && NF == 5 && $1 == e && $3 == a { print $2, $4, $5; exit }' "$ENGINES_LOCK"
}

# engines_fetch ENGINE ARCH — архив в кэше Mac, сверенный с lock.
# Печатает путь к файлу. Неверный хэш — файл удаляется, отказ: на роутере к
# этому моменту ещё ничего не менялось.
engines_fetch() {
	_pin=$(engines_pin "$1" "$2")
	[ -n "$_pin" ] || fail "для $1 нет закреплённой версии под архитектуру $2" \
		"на роутере ничего не менялось" \
		"допишите строку в $ENGINES_LOCK (архитектура роутера — /etc/apk/arch)" \
		"сейчас там: $(awk -v e="$1" '!/^#/ && $1 == e { printf "%s ", $3 }' "$ENGINES_LOCK")"
	_ver=$(printf '%s\n' "$_pin" | awk '{print $1}')
	_file=$(printf '%s\n' "$_pin" | awk '{print $2}')
	_want=$(printf '%s\n' "$_pin" | awk '{print $3}')
	case "$1" in
		nikki) _url=$ENGINES_NIKKI_BASE/$_ver/$_file ;;
		b4)    _url=$ENGINES_B4_BASE/$_ver/$_file ;;
	esac
	_dir=$ENGINES_CACHE/$1-$_ver
	_path=$_dir/$_file
	mkdir -p "$_dir"
	if [ ! -f "$_path" ]; then
		printf '  · качаю %s\n' "$_url" >&2
		curl -fL --progress-bar --connect-timeout 20 -o "$_path.part" "$_url" ||
			{ rm -f "$_path.part"; fail "не скачался $_file" \
				"на роутере ничего не менялось" \
				"проверьте с этого Mac: curl -fIL $_url" \
				"или укажите зеркало: ENGINES_$(echo "$1" | tr a-z A-Z)_BASE=https://…"; }
		mv "$_path.part" "$_path"
	fi
	_got=$(sha256 "$_path")
	if [ "$_got" != "$_want" ]; then
		rm -f "$_path"
		fail "sha256 $_file не совпал с $ENGINES_LOCK (ждали ${_want%"${_want#????????????}"}…, получили ${_got%"${_got#????????????}"}…)" \
			"скачанный файл удалён из кэша, на роутере ничего не менялось" \
			"повторите деплой — если снова не совпадёт, архив на сервере другой:" \
			"сверьте digest в релизе $_ver и обновите строку в $ENGINES_LOCK осознанно"
	fi
	printf '%s\n' "$_path"
}

# ─────────── что уже стоит на роутере ───────────

# engines_detect — одним ssh-вызовом собирает всё, что нужно для решения.
# Результат — переменные E_*; ничего на роутере не меняет (этим пользуется
# engines-plan.sh — сухой прогон).
engines_detect() {
	_facts=$(sh_ '
		echo "arch=$(cat /etc/apk/arch 2>/dev/null)"
		command -v flock >/dev/null 2>&1 && echo flock=yes || echo flock=no
		[ -x /etc/init.d/nikki ] && echo nikki_init=yes || echo nikki_init=no
		{ [ -x /usr/libexec/mihomo ] || command -v mihomo >/dev/null 2>&1; } && echo mihomo_bin=yes || echo mihomo_bin=no
		echo "nikki_pkgs=$(apk list -I nikki luci-app-nikki mihomo-meta mihomo-alpha 2>/dev/null | awk "{printf \"%s \", \$1}")"
		n=0; for p in /etc/nikki/profiles/*.yaml /etc/nikki/profiles/*.yml; do [ -f "$p" ] && n=$((n + 1)); done
		echo "profiles=$n"
		[ -x /etc/init.d/b4 ] && echo b4_init=yes || echo b4_init=no
		B=$(sed -n "s/^PROG=\"\{0,1\}\([^\"]*\)\"\{0,1\}\$/\1/p" /etc/init.d/b4 2>/dev/null | head -1)
		[ -n "$B" ] || B=/usr/bin/b4
		echo "b4_prog=$B"
		[ -x "$B" ] && echo "b4_ver=$("$B" --version 2>/dev/null | sed -n "s/^B4 version: \([^ ]*\).*/\1/p")" || echo b4_ver=
		m=""; for k in '"$ENGINES_B4_KMODS"'; do apk info -e "$k" >/dev/null 2>&1 || m="$m $k"; done
		echo "b4_kmods_missing=$m"
		echo "free_kb=$(df -k / | awk "NR==2{print \$4}")"
	') || fail "не удалось опросить роутер" "ничего не менялось" "проверьте ssh: команда выше в выводе"
	E_ARCH=$(printf '%s\n' "$_facts" | sed -n 's/^arch=//p')
	E_FLOCK=$(printf '%s\n' "$_facts" | sed -n 's/^flock=//p')
	E_NIKKI_INIT=$(printf '%s\n' "$_facts" | sed -n 's/^nikki_init=//p')
	E_MIHOMO_BIN=$(printf '%s\n' "$_facts" | sed -n 's/^mihomo_bin=//p')
	E_NIKKI_PKGS=$(printf '%s\n' "$_facts" | sed -n 's/^nikki_pkgs=//p')
	E_PROFILES=$(printf '%s\n' "$_facts" | sed -n 's/^profiles=//p')
	E_B4_INIT=$(printf '%s\n' "$_facts" | sed -n 's/^b4_init=//p')
	E_B4_PROG=$(printf '%s\n' "$_facts" | sed -n 's/^b4_prog=//p')
	E_B4_VER=$(printf '%s\n' "$_facts" | sed -n 's/^b4_ver=//p')
	E_B4_KMODS_MISSING=$(printf '%s\n' "$_facts" | sed -n 's/^b4_kmods_missing=//p')
	E_FREE_KB=$(printf '%s\n' "$_facts" | sed -n 's/^free_kb=//p')
}

# engines_decide — по E_* решает, что делать с каждым компонентом:
# D_FLOCK / D_NIKKI / D_B4 = install | have | partial. Печатает решение.
#
# «partial» — движок стоит наполовину (init.d есть, ядра нет, или наоборот).
# Это состояние владельца: чинить его молча значило бы угадывать, что он
# задумал (ADR-0010). Предупреждаем и не трогаем.
engines_decide() {
	[ -n "$E_ARCH" ] || fail "на роутере нет /etc/apk/arch — это не OpenWrt 25.12 с apk" \
		"ничего не менялось" "установка движков рассчитана на apk (OpenWrt 25.12+)"
	ok "архитектура пакетов: $E_ARCH"

	if [ "$E_FLOCK" = yes ]; then D_FLOCK=have; skip "flock: уже есть"
	else D_FLOCK=install; ok "flock: будет поставлен из штатного фида"; fi

	_npin=$(engines_pin nikki "$E_ARCH" | awk '{print $1}')
	if [ "$E_NIKKI_INIT" = yes ] && [ "$E_MIHOMO_BIN" = yes ]; then
		D_NIKKI=have
		skip "nikki: уже установлен (${E_NIKKI_PKGS% }), не трогаем"
		# Закреплённая версия — тег релиза, он же версия luci-app-nikki.
		case "$E_NIKKI_PKGS" in
			*"luci-app-nikki-${_npin#v}-"*|"") ;;
			*) warn "nikki: закреплён ${_npin:-?} (luci-app-nikki ${_npin#v}), на роутере другое — оставляем как есть" ;;
		esac
	elif [ "$E_NIKKI_INIT" = no ] && [ "$E_MIHOMO_BIN" = no ]; then
		D_NIKKI=install
		ok "nikki: будет поставлен ${_npin:-(нет пина для $E_ARCH!)}"
	else
		D_NIKKI=partial
		warn "nikki стоит наполовину: init.d=$E_NIKKI_INIT, ядро mihomo=$E_MIHOMO_BIN — не трогаем"
		warn "  доставить руками или снести (apk del nikki mihomo-meta) и повторить --install"
	fi

	_bpin=$(engines_pin b4 "$E_ARCH" | awk '{print $1}')
	# Признак — init.d, как у netmode-apply. Версию могли не узнать (init.d
	# старого установщика без строки PROG=, бинарь в /opt) — это не повод
	# считать b4 недоставленным.
	if [ "$E_B4_INIT" = yes ]; then
		D_B4=have
		if [ -n "$E_B4_VER" ]; then
			skip "b4: уже установлен ($E_B4_PROG, $E_B4_VER), не трогаем"
			[ "v$E_B4_VER" = "$_bpin" ] || warn "b4: закреплён $_bpin, на роутере $E_B4_VER — оставляем как есть"
		else
			skip "b4: уже установлен (/etc/init.d/b4), версию узнать не вышло ($E_B4_PROG --version), не трогаем"
		fi
	elif [ -z "$E_B4_VER" ]; then
		D_B4=install
		ok "b4: будет поставлен ${_bpin:-(нет пина!)}"
	else
		D_B4=partial
		warn "b4 стоит наполовину: бинарь $E_B4_PROG ($E_B4_VER) без /etc/init.d/b4 — не трогаем"
		warn "  доставить руками или снести /etc/init.d/b4 и бинарь и повторить --install"
	fi
}

# ─────────── установка ───────────

# engines_bootstrap — фаза «Движки» деплоя: решить, скачать, поставить.
# Ставит только недостающее; после неё печатается сводка.
engines_bootstrap() {
	engines_detect
	engines_decide

	S_FLOCK=$D_FLOCK S_NIKKI=$D_NIKKI S_B4=$D_B4 S_PROFILE=- S_PROVIDERS=-

	# Ставить нечего — роутер, где всё поставлено руками. apk не зовётся,
	# на Mac ничего не качается.
	if [ "$D_FLOCK" != install ] && [ "$D_NIKKI" != install ] && [ "$D_B4" != install ]; then
		engines_providers
		engines_summary
		return 0
	fi

	# Сначала всё скачать, сверить и распаковать на Mac: неверный хэш или не
	# тот архив должны остановить деплой ДО первого изменения на роутере.
	STAGE=$ENGINES_CACHE/stage.$$
	rm -rf "$STAGE"
	mkdir -p "$STAGE/nikki" "$STAGE/b4"
	if [ "$D_NIKKI" = install ]; then
		_tgz=$(engines_fetch nikki "$E_ARCH") || exit 1
		ok "архив nikki сверен: $_tgz"
		tar -xzf "$_tgz" -C "$STAGE/nikki"
	fi
	if [ "$D_B4" = install ]; then
		_tgz=$(engines_fetch b4 "$E_ARCH") || exit 1
		ok "архив b4 сверен: $_tgz"
		tar -xzf "$_tgz" -C "$STAGE/b4"
		[ -f "$STAGE/b4/b4" ] || fail "в архиве b4 нет файла b4" "на роутере ничего не менялось" \
			"архив не тот — проверьте строку в $ENGINES_LOCK"
	fi
	engines_pick_nikki

	engines_space_check

	engines_base_packages

	[ "$D_NIKKI" != install ] || engines_install_nikki
	engines_providers
	[ "$D_B4" != install ] || engines_install_b4

	rm -rf "$STAGE"
	engines_summary
}

# engines_pick_nikki — выбрать из распакованного архива nikki нужные .apk
# (NIKKI_APKS — имена файлов). Имя пакета отделено от версии дефисом перед
# цифрой: nikki-2026…, но не nikki-foo. Ровно один файл на пакет — иначе
# архив не тот, что ждали.
NIKKI_APKS=
engines_pick_nikki() {
	NIKKI_APKS=
	[ "$D_NIKKI" = install ] || return 0
	for p in $ENGINES_NIKKI_PKGS; do
		_f=$(cd "$STAGE/nikki" && ls | grep -E "^$p-[0-9].*\.apk\$" || true)
		[ "$(printf '%s\n' "$_f" | grep -c .)" -eq 1 ] ||
			fail "в архиве nikki не один файл для пакета $p: '${_f:-нет}'" \
				"на роутере ничего не менялось" "архив не тот, что ждали — проверьте строку в $ENGINES_LOCK"
		NIKKI_APKS="$NIKKI_APKS $_f"
	done
}

# Сумма размеров файлов в КБ (wc -c одинаков на macOS и Linux, в отличие
# от колонок tar -tv).
kb_of() { cat "$@" | wc -c | awk '{ printf "%d", $1 / 1024 }'; }

# Место: распакованный mihomo втрое больше своего .apk (15 МБ → 42 МБ),
# отсюда множитель; b4 — размер бинаря как есть. Плюс запас на сам демон.
engines_space_check() {
	_need=$ENGINES_SPARE_KB
	if [ -n "$NIKKI_APKS" ]; then
		_kb=$(cd "$STAGE/nikki" && kb_of $NIKKI_APKS)
		_need=$((_need + _kb * 3))
	fi
	[ "$D_B4" != install ] || _need=$((_need + $(kb_of "$STAGE/b4/b4")))
	if [ "${E_FREE_KB:-0}" -lt "$_need" ]; then
		fail "на / свободно $((E_FREE_KB / 1024)) МБ, движкам нужно ~$((_need / 1024)) МБ" \
			"на роутере ничего не менялось" \
			"освободите место или поставьте движки на внешний носитель руками" \
			"тестовая VM: образ растягивается при настройке (VM_DISK_SIZE, scripts/vm/common.sh)"
	fi
	ok "места хватает: свободно $((E_FREE_KB / 1024)) МБ, нужно ~$((_need / 1024)) МБ"
}

# Пакеты штатного фида — одним вызовом apk и только если что-то нужно:
# на системе, где всё есть, apk не зовётся вовсе.
engines_base_packages() {
	_pkgs=
	[ "$D_FLOCK" != install ] || _pkgs="flock"
	[ "$D_B4" != install ] || _pkgs="$_pkgs $E_B4_KMODS_MISSING"
	_pkgs=$(echo $_pkgs)
	[ -n "$_pkgs" ] || return 0
	rlog "apk add $_pkgs"
	run_remote "apk update" "apk update" ||
		fail "apk update не прошёл — фиды OpenWrt недоступны с роутера" \
			"ничего не поставлено" \
			"проверьте интернет на роутере: wget -O /dev/null https://downloads.openwrt.org" \
			"тестовая VM: VM_APK_MIRROR=… (docs/vm-utm.md)"
	run_remote "apk add $_pkgs" "apk add $_pkgs" ||
		fail "apk add $_pkgs не прошёл" \
			"часть пакетов могла встать — apk ставит транзакцией, обычно ничего" \
			"причина — в выводе apk выше; kmod-* должны совпадать с ядром (uname -r) и версией OpenWrt" \
			"повторите --install после исправления"
	[ "$D_FLOCK" != install ] || S_FLOCK=installed
}

engines_install_nikki() {
	rlog "установка nikki:$NIKKI_APKS"
	sh_ "rm -rf $ENGINES_RTMP && mkdir -p $ENGINES_RTMP"
	_files=
	for _f in $NIKKI_APKS; do
		put - "$ENGINES_RTMP/$_f" 0644 < "$STAGE/nikki/$_f"
		_files="$_files $ENGINES_RTMP/$_f"
	done
	ok "пакеты nikki залиты в $ENGINES_RTMP:$NIKKI_APKS"

	# --allow-untrusted: у nikki свой ключ подписи, в /etc/apk/keys его нет,
	# и класть его туда значило бы доверить третьей стороне все будущие
	# установки. Целостность даёт sha256 архива из git (ADR-0046).
	if ! run_remote "apk add nikki (зависимости — из штатного фида)" "apk add --allow-untrusted$_files"; then
		sh_ "rm -rf $ENGINES_RTMP" || true
		fail "nikki не встал" \
			"apk ставит транзакцией — пакеты nikki скорее всего не установлены; временные файлы убраны" \
			"причина — в выводе apk выше; если нет места или kmod не под ядро — это там" \
			"повторите --install после исправления"
	fi
	rlog "nikki установлен"

	# Пакет сам включает службу при установке (default_postinst). Включением
	# и запуском владеет netmode-apply: движок работает, только когда его
	# выбрали в панели.
	if run_remote "nikki: служба остановлена и выключена" "/etc/init.d/nikki stop >/dev/null 2>&1; /etc/init.d/nikki disable && ! ls /etc/rc.d | grep -q nikki"; then
		rlog "nikki: stop, disable"
	else
		warn "nikki установлен, но выключить службу не удалось — она может стартовать при загрузке"
		warn "  проверьте: ls /etc/rc.d | grep nikki; выключить: /etc/init.d/nikki disable"
	fi
	sh_ "rm -rf $ENGINES_RTMP" || true
	S_NIKKI=installed

	engines_nikki_config
}

# Настройки свежего nikki. Только на системе, где nikki поставили мы, и
# только то, без чего демон не работает; остальное — выбор владельца (LuCI).
engines_nikki_config() {
	# Профиль: шаблон, если своего нет. apk при переустановке конфиги не
	# затирает, так что профиль мог пережить удаление пакета — тогда он
	# владельца, и выбор профиля в /etc/config/nikki тоже его.
	if [ "${E_PROFILES:-0}" -eq 0 ] && ! sh_ 'ls /etc/nikki/profiles/*.yaml /etc/nikki/profiles/*.yml >/dev/null 2>&1'; then
		put - /etc/nikki/profiles/netmoded.yaml 0600 < files/etc/nikki/profiles/netmoded.yaml
		# enabled=1 — собственный выключатель nikki: при 0 его init.d start
		# молча ничего не делает, и netmode-apply nikki падал бы кодом 6.
		# Запуск по-прежнему решает netmode-apply (служба выключена выше).
		run_remote "профиль netmoded.yaml выбран в nikki" \
			"uci set nikki.config.profile='file:netmoded.yaml' && uci set nikki.config.enabled='1' && uci commit nikki" ||
			fail "профиль положен, но выбрать его в /etc/config/nikki не удалось" \
				"nikki стоит с профилем по умолчанию (подписка-заглушка) и выключен" \
				"uci set nikki.config.profile='file:netmoded.yaml'; uci set nikki.config.enabled='1'; uci commit nikki"
		rlog "nikki: положен профиль /etc/nikki/profiles/netmoded.yaml, config.profile, config.enabled"
		S_PROFILE=seeded
	else
		skip "профиль nikki уже есть в /etc/nikki/profiles — не трогаем"
		S_PROFILE=have
	fi

	# Clash API и zashboard: пакет заполняет их сам, но если пусто — демону
	# не с чем говорить. Пишем только пустое; значения не печатаются.
	_set=$(sh_ '
		c=
		[ -n "$(uci -q get nikki.mixin.api_listen)" ] || { uci set nikki.mixin.api_listen="[::]:9090"; c="$c api_listen"; }
		[ -n "$(uci -q get nikki.mixin.ui_path)" ] || { uci set nikki.mixin.ui_path=ui; c="$c ui_path"; }
		[ -z "$(uci changes nikki)" ] || uci commit nikki
		echo "$c"') || fail "не удалось проверить nikki.mixin.api_listen/ui_path" \
			"nikki поставлен и выключен, профиль выбран" \
			"проверьте руками: uci get nikki.mixin.api_listen; uci get nikki.mixin.ui_path"
	if [ -n "$_set" ]; then ok "nikki.mixin:$_set — были пусты, заполнены"; rlog "nikki.mixin:$_set заполнены"
	else skip "nikki.mixin: api_listen, ui_path уже заданы"; fi
	sh_ '[ -n "$(uci -q get nikki.mixin.api_secret)" ]' ||
		warn "nikki.mixin.api_secret пуст — демон не достучится до Clash API; задайте его в LuCI → nikki"
}

# Каталог файла провайдера. Пакет nikki создаёт его сам, но его могли
# удалить руками; atomicfile демона каталоги намеренно не создаёт, и без
# него обновление подписки отказывает. Идемпотентно, при любом nikki.
engines_providers() {
	sh_ '[ -x /etc/init.d/nikki ]' || return 0
	if sh_ '[ -d /etc/nikki/run/providers ]'; then
		S_PROVIDERS=have
		skip "/etc/nikki/run/providers на месте"
	else
		sh_ 'mkdir -p /etc/nikki/run/providers' ||
			fail "не создался /etc/nikki/run/providers" "без него обновление подписки откажет" \
				"mkdir -p /etc/nikki/run/providers"
		rlog "создан /etc/nikki/run/providers"
		S_PROVIDERS=created
		ok "/etc/nikki/run/providers создан — туда демон пишет узлы подписки"
	fi
}

engines_install_b4() {
	_ver=$(engines_pin b4 "$E_ARCH" | awk '{print $1}')
	rlog "установка b4 $_ver"
	put - /usr/bin/b4 0755 < "$STAGE/b4/b4"
	_got=$(sh_ '/usr/bin/b4 --version 2>/dev/null' | sed -n 's/^B4 version: \([^ ]*\).*/\1/p')
	[ "v$_got" = "$_ver" ] ||
		fail "залитый /usr/bin/b4 сообщает версию '${_got:-нет ответа}', закреплена $_ver" \
			"бинарь /usr/bin/b4 оставлен на роутере, init.d не ставился" \
			"rm /usr/bin/b4 на роутере и повторите --install; если повторится — архив не тот"
	ok "/usr/bin/b4 ($_got)"
	sh_ 'mkdir -p /etc/b4'
	put - /etc/init.d/b4 0755 < files/etc/init.d/b4
	# init.d у нас без enable, но проверяем, а не предполагаем: включением
	# владеет netmode-apply.
	sh_ '/etc/init.d/b4 disable >/dev/null 2>&1; true'
	ok "/etc/init.d/b4 (служба выключена — включит netmode-apply по выбору режима)"
	rlog "b4 $_got: /usr/bin/b4, /etc/init.d/b4"
	S_B4=installed
}

engines_summary() {
	_st() {
		case "$1" in
			installed) echo "поставлен" ;;
			have)      echo "уже был" ;;
			partial)   echo "стоит наполовину — не тронут" ;;
			install)   echo "НЕ поставлен" ;;
			seeded)    echo "положен шаблон netmoded.yaml" ;;
			created)   echo "создан" ;;
			-)         echo "не трогали" ;;
			*)         echo "$1" ;;
		esac
	}
	printf '  ┌ итог движков\n'
	printf '  │ flock       %s\n' "$(_st "$S_FLOCK")"
	printf '  │ nikki       %s\n' "$(_st "$S_NIKKI")"
	printf '  │ b4          %s\n' "$(_st "$S_B4")"
	printf '  │ профиль     %s\n' "$(_st "$S_PROFILE")"
	printf '  └ providers/  %s\n' "$(_st "$S_PROVIDERS")"
}
