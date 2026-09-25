#!/bin/sh
# Настройки стенда для внешних программ (desktop/, ADR-0047): KEY=value по
# строке. Порты не дублируются в приложении — берутся из common.sh.
set -eu
. "$(dirname "$0")/common.sh"
for v in VM_NAME VM_SSH_PORT VM_HTTP_PORT VM_PROXY_PORT VM_DIR UTMCTL \
	VM_B4_UI_PORT VM_NIKKI_UI_PORT VM_LUCI_PORT VM_GUEST_B4_UI VM_GUEST_NIKKI_UI VM_GUEST_LUCI; do
	eval "printf '%s=%s\n' $v \"\$$v\""
done
