GO ?= go
BINARY := bin/viam-firmata
ENTRYPOINT := ./cmd/module

.PHONY: all build module.tar.gz test clean

all: build

build:
	mkdir -p bin
	$(GO) build -o $(BINARY) $(ENTRYPOINT)

module.tar.gz: build
	tar -czf module.tar.gz $(BINARY) meta.json

test:
	$(GO) test -race ./...

clean:
	rm -rf bin module.tar.gz
