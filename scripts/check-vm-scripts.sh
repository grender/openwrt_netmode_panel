#!/bin/sh
# Скрипты тестовой VM (scripts/vm/, docs/vm-utm.md) согласованы между собой.
#
# Проверить их по-настоящему можно только на Mac с UTM, поэтому здесь —
# то, что ломается молча и без Mac:
#   1. синтаксис всех scripts/vm/*.sh;
#   2. порты, подсеть и адрес гостя в utm-create.sh и qemu-run.sh берутся из
#      common.sh, а не вписаны литералами. Разойдись они — VM под UTM и под
#      QEMU слушала бы разные порты, и up.sh/deploy.sh стучались бы не туда;
#   3. deploy.sh передаёт --ssh-port мастер-соединению: без этого деплой в
#      VM за пробросом ушёл бы на порт 22 хоста.
set -eu

D=scripts/vm
fail=0

for f in "$D"/*.sh; do
	sh -n "$f" || { echo "check-vm-scripts: синтаксис $f"; fail=1; }
done

for f in "$D/utm-create.sh" "$D/qemu-run.sh"; do
	hits=$(grep -nE '18022|18088|17890|7890|192\.168\.1\.|:22\b|8088' "$f" | grep -v '^[0-9]*:[[:space:]]*#' || true)
	if [ -n "$hits" ]; then
		echo "check-vm-scripts: в $f литерал сети/порта — берите из common.sh:"
		echo "$hits" | sed 's/^/  /'
		fail=1
	fi
	for v in VM_SSH_PORT VM_HTTP_PORT VM_PROXY_PORT VM_GUEST_PROXY VM_GUEST VM_NET; do
		grep -q "\$$v" "$f" || { echo "check-vm-scripts: $f не использует \$$v"; fail=1; }
	done
done

if ! grep -qE '^ssh -M .*SSHPORT' scripts/deploy.sh; then
	echo "check-vm-scripts: мастер-соединение deploy.sh не учитывает --ssh-port"
	fail=1
fi

[ "$fail" -eq 0 ] || exit 1
echo "-- check-vm-scripts: скрипты VM согласованы с common.sh"
