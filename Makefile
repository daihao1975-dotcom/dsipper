BINARY  := dsipper
PKG     := dsipper
VERSION := 0.9.0
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build build-mac build-linux-amd64 build-linux-arm64 cross clean test fmt

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

fmt:
	go fmt ./...
	go vet ./...

clean:
	rm -rf bin/
