BINARY  := dsipper
PKG     := dsipper
VERSION ?= 0.11.0
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build build-mac build-linux-amd64 build-linux-arm64 cross clean test test-race test-regression fmt

all: build

build:
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) .

build-mac:
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-darwin-arm64 .
	GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-darwin-amd64 .

build-linux-amd64:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-linux-amd64 .

build-linux-arm64:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-linux-arm64 .

cross: build-mac build-linux-amd64 build-linux-arm64
	@echo "--- artifacts ---"
	@ls -lh bin/

test:
	go test ./...

test-race:
	go test -race ./...

# Full end-to-end black-box regression (13 cases). Re-builds first.
test-regression: build
	./test/regression.sh

fmt:
	go fmt ./...
	go vet ./...

clean:
	rm -rf bin/
