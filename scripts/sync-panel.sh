#!/bin/sh
# Копирует панель туда, откуда её забирает go:embed.
#
# Копия, а не symlink: go:embed не ходит по ссылкам, и сборка на чужой
# машине молча получила бы пустой каталог.
set -eu
dst=internal/httpapi/panel
rm -rf "$dst"
mkdir -p "$dst/vendor"
cp web/index.html web/app.js web/app.css web/i18n.js "$dst/"
cp web/vendor/htm-preact-standalone.module.js "$dst/vendor/"
echo "-- панель синхронизирована: $(find "$dst" -type f | wc -l | tr -d ' ') файлов"
