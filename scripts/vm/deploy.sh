#!/bin/sh
# scripts/deploy.sh, нацеленный на тестовую VM: хост, ssh-порт, свой
# known_hosts и адрес панели за пробросом подставлены. Остальные аргументы
# уходят как есть:
#
#   ./scripts/vm/deploy.sh             перезалить бинарь
#   ./scripts/vm/deploy.sh --install   полная установка (её зовёт up.sh)
set -eu
. "$(dirname "$0")/common.sh"

DEPLOY_SSH_OPTS=$VM_SSH_OPTS exec "$ROOT/scripts/deploy.sh" \
	--host root@127.0.0.1 --ssh-port "$VM_SSH_PORT" \
	--panel-url "http://127.0.0.1:$VM_HTTP_PORT" "$@"
