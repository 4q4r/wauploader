# wauploader — build and quality-gate targets.
#
# `make gate` runs the full quality gate: fmt → vet → lint → test → vuln,
# each step as its OWN blocking make invocation (separate recipe lines, never
# one && -chained shell line) — a failed step makes the rest unreachable.

BINARY  := wauploader
PKG     := github.com/4q4r/wauploader
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOTMP   := $(HOME)/.cache/go-tmp

.PHONY: help build fmt vet lint test vuln gate

help: ## Показать список целей Makefile
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "} ; {printf "  \033[36m%-6s\033[0m %s\n", $$1, $$2}'

build: ## Собрать бинарник (CGO_ENABLED=0, -trimpath, -ldflags "-s -w" + версия)
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X $(PKG)/internal/version.Version=$(VERSION)" -o $(BINARY) .

fmt: ## Форматирование: gofmt -l -w, затем go vet ./...
	gofmt -l -w .
	go vet ./...

vet: ## go vet ./...
	go vet ./...

lint: ## golangci-lint run (версия пинится в docs/lint.md; гейт = 0 находок)
	golangci-lint run

test: ## Все тесты: -race -p 2 -count=1 (GOTMPDIR вне общего /tmp)
	mkdir -p $(GOTMP)
	GOTMPDIR=$(GOTMP) go test -race -p 2 -count=1 ./...

vuln: ## govulncheck ./... — проверка зависимостей на известные уязвимости
	govulncheck ./...

check: ## Гейт форматирования: gofmt -l должен быть пуст; иначе список файлов и ошибка
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		printf '%s\n' "$$unformatted" >&2; \
		echo "неотформатированные файлы — запустите make fmt" >&2; \
		exit 1; \
	fi

gate: ## ПОЛНЫЙ ГЕЙТ = check → vet → lint → test → vuln, каждый шаг — отдельная блокирующая команда
	$(MAKE) check
	$(MAKE) vet
	$(MAKE) lint
	$(MAKE) test
	$(MAKE) vuln
