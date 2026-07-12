.PHONY: build build-windows dev clean

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

# Without this, a failed npm ci can leave a fresh-mtime stamp file
# behind and the next make would skip the install.
.DELETE_ON_ERROR:

# npm writes node_modules/.package-lock.json on every install, so it
# doubles as the stamp file: missing on a fresh clone or after
# rm -rf node_modules (npm ci runs), refreshed whenever package.json /
# the lockfile changes (npm ci re-runs — and fails fast if the two
# are out of sync), untouched otherwise (make skips the step).
# --include=dev keeps tsc/vite installed even under NODE_ENV=production.
web/node_modules/.package-lock.json: web/package.json web/package-lock.json
	cd web && npm ci --include=dev

build: web/node_modules/.package-lock.json
	cd web && KOJO_VERSION="$(VERSION)" npm run build
	go build -ldflags "-X main.version=$(VERSION)" -o kojo ./cmd/kojo

build-windows: web/node_modules/.package-lock.json
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
