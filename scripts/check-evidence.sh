#!/bin/sh
# Главная защита от галлюцинированных интеграций.
#
# Каждый литерал, которым код обращается наружу — путь HTTP API, порт, путь
# в sysfs, имя объекта ubus — обязан быть процитирован в docs/recon/evidence.json
# со ссылкой на сырой вывод роутера или на строку исходников b4.
#
# Проверка механическая, а не оценочная: агент не может «убедить» её, что
# эндпоинт существует. Либо литерал есть в evidence.json, либо сборка падает.
#
# Сканируются ТОЛЬКО клиентские пакеты. internal/httpapi исключён намеренно:
# там живут наши собственные маршруты, они описаны в docs/api/openapi.yaml
# и проверяются другим скриптом (check-routes.sh).
set -eu

EV=docs/recon/evidence.json
DIRS="internal/b4 internal/nikki internal/led internal/executor internal/wireless"

[ -f "$EV" ] || { echo "check-evidence: нет $EV" >&2; exit 1; }

scan=""
for d in $DIRS; do
	[ -d "$d" ] && scan="$scan $d"
done

if [ -z "$scan" ]; then
	echo "-- check-evidence: клиентских пакетов ещё нет, пропуск"
	exit 0
fi

# Литералы в двойных кавычках из неsтестовых файлов.
lits=$(grep -rhoE '"[^"]+"' --include='*.go' $scan 2>/dev/null \
	| grep -v '_test\.go' \
	| tr -d '"' \
	| sort -u || true)

missing=""
for l in $lits; do
	case "$l" in
		# Внешние поверхности, которые обязаны быть подтверждены.
		#
		# /ui* — веб-панель mihomo. Под /api/* она не подпадает (это статика
		# дашборда, а не Clash API), и без отдельного шаблона оказалась бы
		# единственным внешним путём в проекте без механической защиты.
		/api/*|/ui*|/sys/class/leds/*|network.wireless|network.interface.*|iwinfo)
			if ! grep -qF "$l" "$EV"; then
				missing="$missing $l"
			fi
			;;
		# Порты — ищем как :NNNN или host:NNNN
		*:9090*|*:7000*)
			p=$(printf '%s' "$l" | grep -oE '[0-9]{4,5}' | head -1)
			if [ -n "$p" ] && ! grep -qF "$p" "$EV"; then
				missing="$missing $l"
			fi
			;;
	esac
done

if [ -n "$missing" ]; then
	echo "check-evidence: литералы не подтверждены в $EV:"
	for m in $missing; do echo "  $m"; done
	echo ""
	echo "Каждый внешний путь, порт и объект ubus обязан быть процитирован"
	echo "со ссылкой на docs/recon/raw/ или на строку исходников b4."
	echo "НЕ дописывай литерал в evidence.json без цитаты — первопричина"
	echo "всегда «фаза начата без разведки», чинить надо гейт, а не код."
	exit 1
fi

# status=hypothesis считается НЕподтверждённым — предупреждаем явно.
if grep -q '"status": *"hypothesis"' "$EV"; then
	echo "check-evidence: ВНИМАНИЕ — в evidence.json есть status=hypothesis."
	echo "  Такие поверхности реализовывать нельзя: интерфейс + 503."
fi

echo "-- check-evidence: внешние литералы подтверждены"
