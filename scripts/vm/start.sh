#!/bin/sh
# Быстрый запуск УЖЕ созданной VM — без provision и deploy (их делает up.sh).
# Код 3 — VM ещё не создана: тогда нужен `make vm-up`.
set -eu
. "$(dirname "$0")/common.sh"
case "$(vm_backend)" in
	none) echo "  ✗ VM не создана — нужен make vm-up" >&2; exit 3 ;;
	utm) utm_start ;;
	qemu) "$(dirname "$0")/qemu-run.sh" --daemon ;;
esac
wait_ssh || die "VM запущена, но ssh на 127.0.0.1:$VM_SSH_PORT не ответил"
