BIN      := netmoded
BUILDDIR := build
TARGET   := $(BUILDDIR)/$(BIN)

GOFLAGS_TARGET := GOOS=linux GOARCH=arm64 CGO_ENABLED=0
LDFLAGS        := -s -w

.PHONY: all verify fmt vet test panel build size checks probe-check clean help

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

## panel — копирует web/ туда, откуда её забирает go:embed
panel:
	@scripts/sync-panel.sh

build: panel
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
	@scripts/check-netmode-seed.sh
	@scripts/check-netmode-apply.sh
	@scripts/check-netmode-wifi.sh
	@scripts/check-web-size.sh
	@scripts/check-panel-sync.sh

## probe-check — песочница измерительной оснастки RQ-03. Не в checks намеренно:
## пробник не инвариант плана, он одноразовый и на роутере не остаётся. Но
## гонять его ОБЯЗАТЕЛЬНО перед тем, как везти замер на железо.
probe-check:
	@scripts/check-probe-rq03.sh

clean:
	@rm -rf $(BUILDDIR)

help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //'
