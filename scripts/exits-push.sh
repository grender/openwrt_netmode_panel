#!/bin/sh
# Кладёт экспорты своих выходных нод на роутер и просит демон их перечитать.
#
#   ./scripts/exits-push.sh                       все exports/*.json соседнего репо нод
#   ./scripts/exits-push.sh путь/kz-nqz-exit-01.json …
#   ./scripts/exits-push.sh --host root@127.0.0.1 --ssh-port 18022   тестовая VM
#
# Источник — exports/<нода>.json репозитория выходных нод (node-export.yml;
# формат — README там, «Формат exports/<нода>.json (schema 1)»). В файлах
# секреты клиента: скрипт их не читает и не печатает — только передаёт
# потоком по ssh (scp на роутере не работает: в dropbear нет sftp-сервера).
#
# На роутере файл ложится атомарно: временный «.<нода>.json.tmp» (его
# демон пропускает) и mv. Затем POST /api/exits/reload с токеном демона —
# изнутри роутера, токен наружу не выходит. Перечитка пересобирает
# провайдер exits и, если изменился набор SNI своих нод, перезапускает
# Nikki (docs/exit-nodes.md, ADR-0050).
#
# EXITS_SSH_OPTS — дополнительные опции ssh (например, known_hosts для VM).
set -eu

HOST=root@192.168.9.1
SSH_PORT=22
EXPORTS_DIR=$(cd "$(dirname "$0")/.." && pwd)/../wall-breaker-exit-node/exports
DIR=/etc/netmoded/exits
NAME_RE='^[a-z][a-z]-[a-z][a-z][a-z]-(exit|relay)-[0-9][0-9]\.json$'

die() { echo "exits-push: $*" >&2; exit 1; }

files=""
while [ $# -gt 0 ]; do
	case "$1" in
	--host) HOST=$2; shift 2 ;;
	--ssh-port) SSH_PORT=$2; shift 2 ;;
	-h|--help) sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
	-*) die "неизвестный ключ: $1" ;;
	*) files="$files
$1"; shift ;;
	esac
done
if [ -z "$files" ]; then
	[ -d "$EXPORTS_DIR" ] || die "нет каталога $EXPORTS_DIR — укажите файлы явно"
	files=$(find "$EXPORTS_DIR" -maxdepth 1 -name '*.json' -type f | sort)
	[ -n "$files" ] || die "в $EXPORTS_DIR нет *.json — сначала ansible-playbook node-export.yml"
fi

# shellcheck disable=SC2086
r() { ssh ${EXITS_SSH_OPTS:-} -p "$SSH_PORT" -o ConnectTimeout=10 "$HOST" "$@"; }

echo "$files" | while IFS= read -r f; do
	[ -n "$f" ] || continue
	base=$(basename "$f")
	# Имя проверяется здесь, до ssh: оно уходит в команду на роутере.
	echo "$base" | grep -Eq "$NAME_RE" || die "$base: имя не по правилу <cc>-<iata>-<exit|relay>-<NN>.json"
	[ -f "$f" ] || die "$f: нет файла"
	r "umask 077 && mkdir -p $DIR && cat > $DIR/.$base.tmp && mv $DIR/.$base.tmp $DIR/$base" < "$f"
	echo "  ✓ $base"
done

echo "Перечитка на роутере…"
r 'T=$(cat /etc/netmoded/token) || exit 3
L=$(uci -q get netmode.main.listen || uci -q get network.lan.ipaddr | cut -d/ -f1)
P=$(uci -q get netmode.main.port || echo 8088)
curl -s -o /tmp/exits-reload.json -w "%{http_code}" -X POST -H "Authorization: Bearer $T" "http://$L:$P/api/exits/reload"
echo; cat /tmp/exits-reload.json; echo; rm -f /tmp/exits-reload.json'
echo "Ход — в панели или GET /api/status (поле job)."
