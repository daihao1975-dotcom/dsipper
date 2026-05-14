BINARY  := dsipper
PKG     := dsipper
VERSION ?= 0.11.2
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build build-mac build-linux-amd64 build-linux-arm64 build-linux-386 build-windows-amd64 cross clean test test-race test-regression demo-html guide-html fmt

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

# 32-bit Linux x86 — old/embedded x86 boxes that don't run amd64
build-linux-386:
	GOOS=linux GOARCH=386 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-linux-386 .

# Windows x86_64 — `.exe` suffix mandatory so File Explorer + cmd.exe
# accept it without "execution policy" complaints. `dsipper-windows-amd64.exe`
# == "Windows x86 (64-bit)" in lay terms.
build-windows-amd64:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-windows-amd64.exe .

cross: build-mac build-linux-amd64 build-linux-arm64 build-linux-386 build-windows-amd64
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

# Generate the usage manual in English + 中文 (bilingual). Each page is
# self-contained, embeds the live `<cmd> -h` output so docs track the binary,
# and links to the other language via a chip in the sidebar.
guide-html: build
	@python3 test/build-guide.py
	@command -v open >/dev/null && open outputs/dsipper-guide.html outputs/dsipper-guide.zh-CN.html || true

fmt:
	go fmt ./...
	go vet ./...

clean:
	rm -rf bin/
