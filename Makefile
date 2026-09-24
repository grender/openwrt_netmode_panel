BIN      := netmoded
BUILDDIR := build
TARGET   := $(BUILDDIR)/$(BIN)

GOFLAGS_TARGET := GOOS=linux GOARCH=arm64 CGO_ENABLED=0
LDFLAGS        := -s -w

.PHONY: all verify fmt vet test api panel panel-next dev preview build size checks probe-check geometry vm-up vm-qemu vm-deploy vm-ssh vm-down vm-destroy clean help

all: verify

## verify — полный гейт фазы. Всё, что можно проверить на ноуте без роутера.
verify: fmt vet test build size checks
	@echo "== verify OK =="

fmt:
	@out=$$(gofmt -l . 2>/dev/null); \
	if [ -n "$$out" ]; then echo "gofmt требуется для:"; echo "$$out"; exit 1; fi
	@echo "-- gofmt clean"

vet:
	@go vet ./...
	@echo "-- go vet clean"

test:
	@go test -race -count=2 ./...

## api — перегенерирует клиентскую часть контракта (пути и таксономию
## причин) из docs/api/openapi.yaml. Нужен node и разборщик YAML; результат
## коммитится, поэтому в verify этой цели нет — там сверяется только
## свежесть, и без node она печатает пропуск.
api:
	@node scripts/gen-api.mjs

## panel — собирает панель туда, откуда её забирает go:embed, и пишет
## манифест происхождения.
##
## НЕ зависимость build и ничего внутри verify. Пока панель была копией пяти
## файлов, `build: panel` был безобиден и лишь вырождал второй ярус сверки
## внутри verify. Со сборкой он означал бы: `go build` требует node, а
## `make verify` перестаёт работать на машине, где есть только Go и шелл.
## Вместо зависимости — гейт: check-panel-build.sh краснеет, если панель
## отстала от своего исходника, и называет команду.
panel:
	@scripts/build-panel.sh

## dev — авторская петля: Vite отдаёт панель, API проксируется в мок.
## Cookie-рукопожатие тут НЕ задействовано: токен подставляется заголовком
## явно. Отгружаемый путь авторизации проверяется только через preview и
## cmd/netmoded-dev — см. web/README.md.
dev:
	@MOCK_STATIC= node web/mock-server.mjs 8088 & \
	cd web/panel && npm run dev; \
	kill %1 2>/dev/null || true

## preview — то, что ОТГРУЖАЕТСЯ: собранный бандл из мока, настоящий путь
## cookie. Гонять обязательно перед коммитом, трогающим index.html, имена
## ассетов или withToken.
preview: panel-next
	@node web/mock-server.mjs 8088

## panel-next — сборка НОВОЙ панели. Кладёт результат в web/panel/dist и
## никуда его не копирует: пока встроенный артефакт остаётся старой панелью,
## а собранная новая существует только для того, чтобы её было видно.
## Исчезнет вместе с переключением — тогда её соберёт сама цель panel.
panel-next:
	@cd web/panel && npm ci --silent && npm run build

build:
	@mkdir -p $(BUILDDIR)
	@$(GOFLAGS_TARGET) go build -trimpath -ldflags="$(LDFLAGS)" -o $(TARGET) ./cmd/netmoded
	@echo "-- built $(TARGET)"

size: build
	@scripts/check-size.sh $(TARGET)

## checks — механические инварианты плана. Каждый скрипт стоит на конкретном
## решении владельца и валит сборку, если решение начали отменять в коде.
checks:
	@scripts/check-stdlib.sh
	@scripts/check-no-rollback.sh
	@scripts/check-b4json.sh
	@scripts/check-no-hardcoded-if.sh
	@scripts/check-evidence.sh
	@scripts/check-wireless-write.sh
	@scripts/check-routes.sh
	@scripts/check-fail-reasons.sh
	@scripts/check-netmode-seed.sh
	@scripts/check-netmode-apply.sh
	@scripts/check-netmode-wifi.sh
	@scripts/check-netmode-bridge.sh
	@scripts/check-panel-deps.sh
	@scripts/check-panel-build.sh
	@scripts/check-vm-scripts.sh
	@scripts/check-engines.sh

## probe-check — песочница измерительной оснастки RQ-03. Не в checks намеренно:
## пробник не инвариант плана, он одноразовый и на роутере не остаётся. Но
## гонять его ОБЯЗАТЕЛЬНО перед тем, как везти замер на железо.
probe-check:
	@scripts/check-probe-rq03.sh

## geometry — замер геометрии панели (протокол web/README.md, но машиной).
## Не в verify и не в checks намеренно: требует установленного Google Chrome,
## которого нет ни в CI, ни у того, кто правит только Go. Гонять ОБЯЗАТЕЛЬНО
## до и после любой правки web/app.css и web/app.js: числа «карточка не
## дёрнулась» и «документ не шире окна» проверяются только замером, глазами
## скачок в 74px за один кадр пропускается.
geometry: panel
	@node scripts/measure-geometry.mjs

## vm-up — тестовая VM с OpenWrt в UTM (Mac на Apple Silicon): образ,
## VM, первичная настройка, deploy.sh --install. Панель — 127.0.0.1:18088,
## ssh — 127.0.0.1:18022. См. docs/vm-utm.md.
vm-up:
	@scripts/vm/up.sh --utm

## vm-qemu — то же, но VM запускается чистым QEMU в фоне, без UTM.
vm-qemu:
	@scripts/vm/up.sh --qemu

## vm-deploy — перезалить только бинарь в уже поднятую VM.
vm-deploy:
	@scripts/vm/deploy.sh

vm-ssh:
	@scripts/vm/ssh.sh

vm-down:
	@scripts/vm/down.sh

## vm-destroy — удалить VM и её диск (скачанный образ остаётся в кэше).
vm-destroy:
	@scripts/vm/destroy.sh

clean:
	@rm -rf $(BUILDDIR)

help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //'
