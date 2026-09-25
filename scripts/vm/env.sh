#!/bin/sh
# Настройки стенда для внешних программ (desktop/, ADR-0047): KEY=value по
# строке. Порты не дублируются в приложении — берутся из common.sh.
set -eu
. "$(dirname "$0")/common.sh"
for v in VM_NAME VM_SSH_PORT VM_HTTP_PORT VM_PROXY_PORT VM_DIR UTMCTL; do
	eval "printf '%s=%s\n' $v \"\$$v\""
done
