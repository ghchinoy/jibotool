BIN := ./bin

.PHONY: all build arm64 armv7 test clean deploy help

all: build

## build: compile jibotool for the host architecture into ./bin/jibotool
build:
	@mkdir -p $(BIN)
	go build -o $(BIN)/jibotool .
	@echo "  built $(BIN)/jibotool"

## arm64: cross-compile for linux/arm64 (e.g. a Coral Dev Board)
arm64:
	@mkdir -p $(BIN)
	GOOS=linux GOARCH=arm64 go build -o $(BIN)/jibotool-linux-arm64 .
	@echo "  built $(BIN)/jibotool-linux-arm64"

## armv7: cross-compile for linux/armv7 (e.g. running directly on Jibo's own Tegra K1)
armv7:
	@mkdir -p $(BIN)
	GOOS=linux GOARCH=arm GOARM=7 go build -o $(BIN)/jibotool-linux-armv7 .
	@echo "  built $(BIN)/jibotool-linux-armv7"

## test: run the test suite
test:
	go test ./...

## clean: remove ./bin
clean:
	rm -rf $(BIN)
	@echo "  cleaned $(BIN)"

## deploy: push the linux/arm64 binary to a Linux host with USB access to Jibo
## Usage: make deploy HOST=192.168.1.50 HOST_USER=mendel SSH_KEY=~/.ssh/id_ed25519
HOST ?= 192.168.1.50
HOST_USER ?= mendel
SSH_KEY ?= $(HOME)/.ssh/id_ed25519
deploy: arm64
	scp -i $(SSH_KEY) $(BIN)/jibotool-linux-arm64 $(HOST_USER)@$(HOST):~/jibotool
	ssh -i $(SSH_KEY) $(HOST_USER)@$(HOST) "chmod +x ~/jibotool && ~/jibotool version"
	@echo "  deployed jibotool to $(HOST_USER)@$(HOST)"

## help: list available targets
help:
	@grep -E '^## ' Makefile | sed 's/## /  /'
