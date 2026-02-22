# Contributing

Thanks for your interest in contributing to kojo!

## Development Setup

1. Install prerequisites: Go 1.25+, Node.js 20+, [Tailscale](https://tailscale.com/)
2. Clone the repository
3. Start development servers:

```bash
make dev-server   # Go server with --dev flag (proxies to Vite)
make dev-web      # Vite dev server
```

Or use hot reload:

```bash
make watch        # Requires air (go install github.com/air-verse/air@latest)
```

## Building

```bash
make build
```

## Pull Requests

- Keep PRs focused on a single change
- Make sure `go vet ./...` passes
- Make sure `make build` succeeds
- Write a clear description of what changed and why
