.PHONY: build build-windows dev clean

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

build:
	cd web && KOJO_VERSION="$(VERSION)" npm run build
	go build -ldflags "-X main.version=$(VERSION)" -o kojo ./cmd/kojo

build-windows:
	cd web && KOJO_VERSION="$(VERSION)" npm run build
	GOOS=windows GOARCH=amd64 go build -ldflags "-X main.version=$(VERSION)" -o kojo.exe ./cmd/kojo

dev-server:
	go run ./cmd/kojo --dev

watch:
	air

dev-web:
	cd web && npm run dev

clean:
	rm -f kojo kojo.exe
	rm -rf web/dist
