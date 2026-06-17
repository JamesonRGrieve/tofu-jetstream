# SPDX-License-Identifier: AGPL-3.0-or-later
BINARY      := terraform-provider-jetstream
VERSION     ?= dev
# Local dev install path used by the dev_overrides .tfrc (see README).
DEV_BIN_DIR ?= $(HOME)/.local/bin

.PHONY: build install fmt vet test tidy check clean

build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BINARY) .

# Install the provider binary where the dev_overrides .tfrc points (no registry
# round-trip). Point OpenTofu at it with a CLI config like:
#   provider_installation { dev_overrides { "jamesonrgrieve/jetstream" = "<DEV_BIN_DIR>" } direct {} }
install: build
	mkdir -p $(DEV_BIN_DIR)
	install -m 0755 $(BINARY) $(DEV_BIN_DIR)/$(BINARY)

fmt:
	gofmt -w .

vet:
	go vet ./...

test:
	go test ./...

tidy:
	go mod tidy

# Full local gate (mirrors CI / pre-commit).
check: tidy fmt vet test build
	@test -z "$$(gofmt -l .)" || (echo "gofmt: files need formatting" && gofmt -l . && exit 1)

clean:
	rm -f $(BINARY)
	rm -rf dist
