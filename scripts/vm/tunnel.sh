#!/bin/sh
# ssh-туннель к мордам в VM: веб-морда b4, Clash API с дашбордом nikki и
# LuCI (порты — common.sh). Панель строит ссылки на них от своего адреса,
# 127.0.0.1, а пробросов для этих портов у VM нет — desktop/ держит этот
# туннель, пока VM запущена, и переписывает ссылки на его порты (ADR-0047).
#
#   ./scripts/vm/tunnel.sh   — в переднем плане, до Ctrl-C или обрыва ssh
set -eu
. "$(dirname "$0")/common.sh"
# ExitOnForwardFailure: занятый на Mac порт — ошибка сразу, а не туннель,
# в котором молча нет одной из трёх морд. ServerAlive — чтобы упавшая VM
# роняла и туннель, а не оставляла висящий ssh.
# shellcheck disable=SC2086 # VM_SSH_OPTS — список опций
exec ssh -N -p "$VM_SSH_PORT" $VM_SSH_OPTS \
	-o BatchMode=yes -o ExitOnForwardFailure=yes \
	-o ServerAliveInterval=5 -o ServerAliveCountMax=2 \
	-L "127.0.0.1:$VM_B4_UI_PORT:127.0.0.1:$VM_GUEST_B4_UI" \
	-L "127.0.0.1:$VM_NIKKI_UI_PORT:127.0.0.1:$VM_GUEST_NIKKI_UI" \
	-L "127.0.0.1:$VM_LUCI_PORT:$VM_GUEST:$VM_GUEST_LUCI" \
	root@127.0.0.1
