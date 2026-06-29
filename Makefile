BINARY_NAME = weatherstation
PKG = main

BUILD_DATE = $(shell date -u '+%Y-%m-%d %H:%M:%S UTC')
BUILD_USER = $(shell whoami)
UNAME_INFO = $(shell uname -a)
GIT_COMMIT = $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
GIT_BRANCH = $(shell git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "unknown")

LDFLAGS = -ldflags "\
	-X '$(PKG).BuildDate=$(BUILD_DATE)' \
	-X '$(PKG).BuildUser=$(BUILD_USER)' \
	-X '$(PKG).UnameInfo=$(UNAME_INFO)' \
	-X '$(PKG).GitCommit=$(GIT_COMMIT)' \
	-X '$(PKG).GitBranch=$(GIT_BRANCH)'"

.PHONY: build build-raspi build-pi32 build-all clean test deps

build:
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p bin
	@go build $(LDFLAGS) -o bin/$(BINARY_NAME) . || { echo "FAILED: go build -o bin/$(BINARY_NAME) ."; exit 1; }

build-raspi:
	@echo "Building $(BINARY_NAME)-linux-arm64..."
	@mkdir -p bin
	@GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o bin/$(BINARY_NAME)-linux-arm64 . || { echo "FAILED: GOOS=linux GOARCH=arm64 go build -o bin/$(BINARY_NAME)-linux-arm64 ."; exit 1; }

build-pi32:
	@echo "Building $(BINARY_NAME)-linux-armv7..."
	@mkdir -p bin
	@GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build $(LDFLAGS) -o bin/$(BINARY_NAME)-linux-armv7 . || { echo "FAILED: GOOS=linux GOARCH=arm GOARM=7 go build -o bin/$(BINARY_NAME)-linux-armv7 ."; exit 1; }

build-all: build build-raspi build-pi32

clean:
	rm -rf bin/

test:
	go test -v ./...

deps:
	go mod download && go mod tidy
