#!/bin/sh
# ssh в тестовую VM: ./scripts/vm/ssh.sh [команда…]
set -eu
. "$(dirname "$0")/common.sh"
vm_ssh "$@"
