#!/bin/sh
# Сухой прогон фазы движков: что сделал бы deploy.sh --install с nikki, b4 и
# flock на этом роутере. Только чтение — ничего не качает и не меняет.
#
#   ./scripts/engines-plan.sh                                 роутер по умолчанию
#   ./scripts/engines-plan.sh --host root@127.0.0.1 --ssh-port 18022   тестовая VM
#
# Смысл — посмотреть ДО настоящего --install на живом роутере, что там всё
# «уже есть» и деплой движки не тронет (ADR-0046). DEPLOY_SSH_OPTS — как у
# deploy.sh.
set -eu

HOST=root@192.168.9.1
SSHPORT=
while [ $# -gt 0 ]; do
	case "$1" in
		--host)     HOST=$2; shift ;;
		--ssh-port) SSHPORT=$2; shift ;;
		-h|--help)  sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
		*) echo "неизвестный аргумент: $1" >&2; exit 2 ;;
	esac
	shift
done

cd "$(dirname "$0")/.."
. scripts/engines.sh

CTL=$(mktemp -u /tmp/netmoded-plan-XXXXXX)
trap 'ssh -S "$CTL" -O exit "$HOST" 2>/dev/null || true' EXIT
# shellcheck disable=SC2086 # DEPLOY_SSH_OPTS — список опций, делится намеренно
ssh -M -S "$CTL" -o ControlPersist=60 ${SSHPORT:+-p "$SSHPORT"} ${DEPLOY_SSH_OPTS:-} -fN "$HOST"
sh_() { ssh -S "$CTL" "$HOST" "$@"; }

step "Движки на $HOST — что сделал бы deploy.sh --install"
engines_detect
engines_decide

if [ "$D_FLOCK" != install ] && [ "$D_NIKKI" != install ] && [ "$D_B4" != install ]; then
	printf '\n%s\n' "Ставить нечего: --install движки не тронет, apk звать не будет."
else
	printf '\n%s\n' "--install поставит отмеченное «будет поставлен»; свободно на /: $((E_FREE_KB / 1024)) МБ."
	[ "$D_NIKKI" != install ] || [ "${E_PROFILES:-0}" -ne 0 ] ||
		echo "Профиля nikki нет — будет положен шаблон files/etc/nikki/profiles/netmoded.yaml."
fi
