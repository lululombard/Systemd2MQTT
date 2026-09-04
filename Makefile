BINARY   := systemd2mqtt
PKG      := ./cmd/systemd2mqtt
MODULE   := github.com/lululombard/Systemd2MQTT
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(DATE)
CONFIG   ?= config.example.yaml

.PHONY: all build test vet fmt snapshot print-polkit-rule clean

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

# Fails when any file is not gofmt clean, and lists the offenders.
fmt:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi

snapshot:
	goreleaser build --snapshot --clean --single-target

print-polkit-rule:
	go run $(PKG) --config $(CONFIG) --print-polkit-rule

clean:
	rm -rf $(BINARY) dist
