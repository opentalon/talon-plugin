# talon-plugin — build, test, lint.
# Standalone repo; requires github.com/opentalon/opentalon (pkg/plugin)
# and github.com/opentalon/talon-language (pkg/talon).

.PHONY: build test lint

BINARY_NAME ?= talon-plugin

build:
	go build -o $(BINARY_NAME) .
	@echo "Built: $(BINARY_NAME)"

test:
	go test -race -count=1 ./...

lint:
	golangci-lint run
