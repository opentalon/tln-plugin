# tln-plugin build. Core's plugin fetcher (internal/bundle CloneAndBuild) runs
# `make build BINARY_NAME=<name>` after cloning. When a mod.tln is present in
# the checkout (Core writes it from config.yaml's `bundle:`), `make build`
# composes those tln plugins into the binary via cmd/bundle; with no mod.tln it
# is a plain build (no extensions). mod.tln is never committed here.
BINARY_NAME ?= tln-plugin

.PHONY: build
build:
	@if [ -f mod.tln ]; then \
		echo "tln-plugin: composing bundle from mod.tln"; \
		go run ./cmd/bundle; \
	fi
	go build -o $(BINARY_NAME) .

.PHONY: test
test:
	go test ./...
