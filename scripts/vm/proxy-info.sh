#!/bin/sh
# Логин прокси nikki и токен панели из VM — одним ssh (desktop/, ADR-0047):
#   user=… password=… token=…
# Это секреты: вывод не для логов.
set -eu
. "$(dirname "$0")/common.sh"
vm_ssh -o ConnectTimeout=3 -o BatchMode=yes '
	printf "user=%s\n" "$(uci -q get nikki.@authentication[0].username)"
	printf "password=%s\n" "$(uci -q get nikki.@authentication[0].password)"
	printf "token=%s\n" "$(cat /etc/netmoded/token 2>/dev/null)"
' </dev/null
