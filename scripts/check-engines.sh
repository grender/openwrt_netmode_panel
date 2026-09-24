#!/bin/sh
# Установка движков (ADR-0046) не разошлась со своими обещаниями.
#
# Настоящая установка проверяется только на VM (docs/vm-utm.md), поэтому
# здесь — то, что ломается молча и без неё:
#   1. engines.lock разбирается: пять полей, 64-значный sha256, у nikki есть
#      строки под обе архитектуры (роутер и VM), у b4 — arm64;
#   2. init.d b4 и engines.sh синтаксически целы, init.d b4 службу НЕ
#      включает (включением владеет netmode-apply);
#   3. шаблон профиля выполняет контракт демона
#      (docs/contracts/nikki-mixin-rulesets.md): провайдеры sub/sub-auto из
#      ./providers/sub.yaml без override, группы AUTO/PROXY/BYPASS,
#      empty-fallback REJECT, правила кончаются MATCH;
#   4. установщик не пишет b4.json (ADR-0007) и кладёт профиль только в
#      ветке «профиля нет»;
#   5. deploy.sh подключает engines.sh и зовёт фазу движков под --install.
set -eu

fail=0
bad() { echo "check-engines: $*"; fail=1; }

LOCK=scripts/engines.lock
rows=$(grep -v '^#' "$LOCK" | grep -v '^[[:space:]]*$' || true)
[ -n "$rows" ] || bad "$LOCK пуст"
printf '%s\n' "$rows" | while read -r e v a f h extra; do
	[ -z "$extra" ] || { echo "check-engines: лишние поля в строке '$e $v $a $f'"; exit 1; }
	case "$e" in nikki|b4) ;; *) echo "check-engines: неизвестный движок '$e'"; exit 1 ;; esac
	printf '%s' "$h" | grep -qE '^[0-9a-f]{64}$' || { echo "check-engines: не sha256 у $f: '$h'"; exit 1; }
	case "$v" in v[0-9]*) ;; *) echo "check-engines: версия '$v' у $f — ждём тег вида v1.2.3"; exit 1 ;; esac
done || fail=1
for a in aarch64_cortex-a53 aarch64_generic; do
	printf '%s\n' "$rows" | awk -v a="$a" '$1 == "nikki" && $3 == a' | grep -q . ||
		bad "в $LOCK нет nikki под $a (роутер — cortex-a53, VM — generic)"
done
printf '%s\n' "$rows" | awk '$1 == "b4" && $3 == "arm64"' | grep -q . || bad "в $LOCK нет b4 под arm64"

for f in scripts/engines.sh scripts/engines-plan.sh files/etc/init.d/b4; do
	sh -n "$f" || bad "синтаксис $f"
done
grep -qE '^[^#]*\benable\b' files/etc/init.d/b4 && bad "files/etc/init.d/b4 включает службу — этим владеет netmode-apply"

P=files/etc/nikki/profiles/netmoded.yaml
for want in '^  sub:' '^  sub-auto:' 'path: \./providers/sub\.yaml' 'name: AUTO' 'name: PROXY' \
	'name: BYPASS' 'empty-fallback: REJECT' 'use: \[sub-auto\]' 'use: \[sub\]'; do
	grep -qE "$want" "$P" || bad "в $P нет «$want» — контракт профиля (docs/contracts/nikki-mixin-rulesets.md)"
done
[ "$(grep -c 'path: \./providers/sub\.yaml' "$P")" -eq 2 ] || bad "в $P провайдеры sub и sub-auto должны читать один файл"
grep -qE '^[^#]*override' "$P" && bad "в $P есть override — демон сверяет имена узлов и отвергнет обновление"
last=$(grep -E '^[[:space:]]*- ' "$P" | tail -1)
case "$last" in *MATCH,*) ;; *) bad "последнее правило в $P — не MATCH: '$last'" ;; esac

code=$(sed 's/#.*//' scripts/engines.sh)
printf '%s\n' "$code" | grep -q 'b4\.json' && bad "engines.sh обращается к b4.json — запрещено (ADR-0007)"
n=$(printf '%s\n' "$code" | grep -c 'profiles/netmoded.yaml' || true)
[ "$n" -ge 1 ] || bad "engines.sh не кладёт шаблон профиля"
printf '%s\n' "$code" | grep -q 'E_PROFILES:-0}" -eq 0' || bad "engines.sh кладёт профиль без проверки «своего профиля нет»"

grep -qE '^\. scripts/engines\.sh' scripts/deploy.sh || bad "deploy.sh не подключает scripts/engines.sh"
grep -qE '^[[:space:]]*engines_bootstrap' scripts/deploy.sh || bad "deploy.sh не зовёт engines_bootstrap"

[ "$fail" -eq 0 ] || exit 1
echo "-- check-engines: пины, шаблон профиля и init.d b4 согласованы с ADR-0046"
