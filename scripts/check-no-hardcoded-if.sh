#!/bin/sh
# Имена сетевых интерфейсов выводятся, а не хардкодятся.
#
# `ubus call network.wireless status` отдаёт связь «секция ↔ ifname» прямо
# в ответе (interfaces[].section и interfaces[].ifname), поэтому phy0.0-sta0
# и phy0.1-ap0 нельзя писать литералом: они меняются с конфигурацией радио,
# а на другом железе выглядят иначе.
set -eu

hits=$(grep -rnE '"(phy[0-9]+\.[0-9]+-[a-z]+[0-9]*|wlan[0-9]+)' \
	--include='*.go' internal/ cmd/ 2>/dev/null \
	| grep -v '_test\.go:' || true)

if [ -n "$hits" ]; then
	echo "check-no-hardcoded-if: имя интерфейса захардкожено:"
	echo "$hits" | sed 's/^/  /'
	echo ""
	echo "Выводи из network.wireless status. В тестах литерал допустим —"
	echo "там он приходит из записанной фикстуры."
	exit 1
fi

echo "-- check-no-hardcoded-if: имена интерфейсов выводятся"
