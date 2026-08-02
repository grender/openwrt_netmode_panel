#!/bin/sh
# Имена интерфейсов И привязка «радио ↔ роль» выводятся, а не хардкодятся.
#
# Две стороны одного правила, потому что ловятся одним грепом:
#
# 1. `ubus call network.wireless status` отдаёт связь «секция ↔ ifname» прямо
#    в ответе (interfaces[].section и interfaces[].ifname), поэтому phy0.0-sta0
#    и phy0.1-ap0 нельзя писать литералом: они меняются с конфигурацией радио,
#    а на другом железе выглядят иначе.
#
# 2. Тот же ответ говорит, какое радио чем работает (interfaces[].config.mode)
#    и в каком диапазоне (config.band), поэтому `radio0`/`radio1` литералом
#    тоже нельзя. Индекс радио задаётся порядком регистрации драйверов, а не
#    диапазоном: radio0 и radio1 — два диапазона ОДНОЙ phy0
#    (docs/recon/raw/10-uci-show-wireless.txt, raw/30-leds-and-phy.txt).
#    После перепрошивки или смены ревизии железа они меняются местами, и
#    инвариант фазы 1 («трогаем только станционное радио», ADR-0009) молча
#    начинает применяться не к тому радио. Порядок разрешения — ADR-0019.
set -eu

hits=$(grep -rnE '"(phy[0-9]+\.[0-9]+-[a-z]+[0-9]*|wlan[0-9]+|radio[0-9]+)' \
	--include='*.go' internal/ cmd/ 2>/dev/null \
	| grep -v '_test\.go:' || true)

if [ -n "$hits" ]; then
	echo "check-no-hardcoded-if: имя интерфейса или радио захардкожено:"
	echo "$hits" | sed 's/^/  /'
	echo ""
	echo "Выводи из network.wireless status (mode, band, ifname), при"
	echo "выключенном радио — из wifi-iface с mode=sta в UCI. Не вывелось —"
	echo "отвечай 503 radio_unknown, а не угадывай (ADR-0019). В тестах"
	echo "литерал допустим — там он приходит из записанной фикстуры."
	exit 1
fi

echo "-- check-no-hardcoded-if: имена интерфейсов и роли радио выводятся"
