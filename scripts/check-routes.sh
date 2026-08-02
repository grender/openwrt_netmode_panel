#!/bin/sh
# OpenAPI — единственный источник правды о маршрутах.
#
# Бэкенд и фронтенд идут параллельно от одного контракта, поэтому расхождение
# должно валить гейт обоих треков, а не всплывать при первом клике в панели.
# Сверяются три списка:
#
#   docs/api/openapi.yaml   ←→   маршруты, зарегистрированные в Go
#                           ←→   литералы fetch() в web/app.js
#
# Правка контракта возвращается к контракту, а не латается по краям: маршрут
# без записи в openapi.yaml — это расширение объёма мимо ADR.
set -eu

SPEC=docs/api/openapi.yaml
[ -f "$SPEC" ] || { echo "check-routes: нет $SPEC" >&2; exit 1; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Пути верхнего уровня в openapi: две ведущих пробела, затем /api/...
sed -n 's/^  \(\/api[A-Za-z0-9_/{}-]*\):.*/\1/p' "$SPEC" | sort -u > "$tmp/spec"

fail=0

# --- Go ---
if [ -d internal/httpapi ]; then
	grep -rhoE '"(GET|POST|PUT|DELETE) /api[A-Za-z0-9_/{}-]*"' \
		--include='*.go' internal/httpapi 2>/dev/null \
		| tr -d '"' | awk '{print $2}' | sort -u > "$tmp/go" || true
	grep -rhoE 'HandleFunc\("\/api[A-Za-z0-9_/{}-]*"' \
		--include='*.go' internal/httpapi 2>/dev/null \
		| sed 's/.*("//; s/"$//' | sort -u >> "$tmp/go" || true
	sort -u -o "$tmp/go" "$tmp/go"

	if [ -s "$tmp/go" ]; then
		extra=$(comm -13 "$tmp/spec" "$tmp/go")
		if [ -n "$extra" ]; then
			echo "check-routes: маршрут в Go, которого нет в openapi.yaml:"
			echo "$extra" | sed 's/^/  /'
			fail=1
		fi
	else
		echo "   (маршрутов Go пока нет)"
	fi
fi

# --- Панель ---
if [ -f web/app.js ]; then
	grep -ohE "['\"\`]/api[A-Za-z0-9_/-]*" web/app.js 2>/dev/null \
		| sed "s/^['\"\`]//" | sort -u > "$tmp/web" || true

	if [ -s "$tmp/web" ]; then
		# Путь панели может быть конкретным экземпляром шаблона {id};
		# сверяем по префиксу до первой фигурной скобки.
		sed 's/{[^}]*}.*//' "$tmp/spec" | sed 's#/$##' | sort -u > "$tmp/specpfx"
		while IFS= read -r u; do
			[ -z "$u" ] && continue
			# Хвостовой слэш срезается: панель строит путь как
			# '/api/wifi/networks/' + id, и это тот же маршрут, что
			# '/api/wifi/networks/{id}' в контракте.
			base=${u%/}
			if ! grep -qxF "$base" "$tmp/spec" && ! grep -qxF "$base" "$tmp/specpfx"; then
				echo "check-routes: панель дёргает путь вне контракта: $u"
				fail=1
			fi
		done < "$tmp/web"
	fi
fi

[ "$fail" -ne 0 ] && exit 1

echo "-- check-routes: маршруты сходятся с openapi.yaml"
