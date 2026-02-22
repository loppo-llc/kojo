.PHONY: build dev clean

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

build:
	cd web && npm run build
	go build -ldflags "-X main.version=$(VERSION)" -o kojo ./cmd/kojo

dev-server:
	go run ./cmd/kojo --dev

watch:
	air

dev-web:
	cd web && npm run dev

clean:
	rm -f kojo
	rm -rf web/dist
