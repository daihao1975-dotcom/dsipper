BINARY  := dsipper
PKG     := dsipper
VERSION ?= 0.11.1
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build build-mac build-linux-amd64 build-linux-arm64 cross clean test test-race test-regression demo-html fmt

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

# Render a self-contained HTML page of the CLUI output (banner, colored
# slog, LivePanel frames, summary box). Useful for reviewers without a
# real terminal — outputs/clui-demo.html opens in the default browser.
demo-html: build
	./test/render-demo.sh

fmt:
	go fmt ./...
	go vet ./...

clean:
	rm -rf bin/
