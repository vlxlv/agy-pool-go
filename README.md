# agy-pool-go: Antigravity Multi-Account Quota Pool & Reverse Proxy Gateway

[![CI](https://github.com/vlxlv/agy-pool-go/actions/workflows/ci.yml/badge.svg)](https://github.com/vlxlv/agy-pool-go/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/badge/go-1.26.7-blue.svg)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Platform](https://img.shields.io/badge/platform-Linux%20%7C%20Darwin%20%7C%20Termux-green.svg)](#)
[![CGO](https://img.shields.io/badge/CGO-disabled%20(0)-brightgreen.svg)](#)

A high-performance, zero-CGO Go implementation and reverse proxy gateway for **Antigravity CLI (`agy`)** on Linux, Termux, and macOS.

---

## Key Capabilities

- **Zero-CGO Pure Go (`CGO_ENABLED=0`)**: Built on pure Go standard library and modern transpiled SQLite (`modernc.org/sqlite`). Fully static binaries cross-compile out-of-the-box for `linux/amd64`, `linux/arm64`, `darwin/amd64`, and `darwin/arm64`.
- **High-Performance HTTP Reverse Proxy**: Non-blocking concurrent reverse proxy gateway listening locally (`127.0.0.1:8899`) with real-time SSE token streaming, request header filtering, and dynamic multi-account rotation.
- **Strict No-Replay Transport Guarantees**: Enforces fail-safe upstream routing. Non-idempotent or in-flight POST streaming requests are never replayed across accounts upon ambiguous network failures, preventing duplicate generation charges.
- **Intelligent Load Balancing & Failover**: Dynamically routes CLI generation requests based on configurable strategies (`max_quota`, `least_used`, `round_robin`). Automatically fails over on HTTP 429 rate limits or quota depletion while respecting upstream `Retry-After`.
- **Distinct CLI Domain Responsibilities**:
  - `agy-pool status`: Fast, script-friendly runtime summary (PID, memory, uptime, state.db schema, pool counts) without slow network quota refreshes.
  - `agy-pool list`: Compact account inventory and cached quota overview.
  - `agy-pool quota`: Dedicated quota dashboard with live progress bars, reset countdowns, and `--refresh` capability.
  - `agy-pool doctor`: Comprehensive system diagnostic checking binary discovery, daemon health, process table, database integrity, and Google TLS connectivity.
- **Cross-Platform Session Continuity**: Resolves and continues directory sessions across account rotations using `conversation_summaries.db`.
- **Encrypted Backup & Migration**: Securely export and import account pools with PBKDF2-HMAC-SHA256 and CTR keystream encryption.

---

## Building & Installation

### Requirements
- Go 1.22+ (tested with Go 1.26.7)
- Standard POSIX environment (Linux, Termux, macOS)

### Build from Source
```bash
# Clone the repository
git clone https://github.com/vlxlv/agy-pool-go.git
cd agy-pool-go

# Build native binary
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/agy-pool ./cmd/agy-pool
```

### Cross-Compilation Matrix
Zero external C toolchains are required:
```bash
# Linux AMD64
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/agy-pool-linux-amd64 ./cmd/agy-pool

# Linux ARM64
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o dist/agy-pool-linux-arm64 ./cmd/agy-pool

# macOS AMD64
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/agy-pool-darwin-amd64 ./cmd/agy-pool

# macOS ARM64 (Apple Silicon)
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o dist/agy-pool-darwin-arm64 ./cmd/agy-pool
```

---

## Testing & Verification

All standard Go unit and race detector tests run completely offline without external network or token requirements:

```bash
# Run unit tests
go test ./... -count=1

# Run race detector
go test -race ./... -count=1

# Format and vet
test -z "$(gofmt -l .)"
go vet ./...
```

To run differential comparison against the reference Python implementation:
```bash
PYTHONPATH=/path/to/agy-pool ./scripts/dual-test.sh
```

---

## License

MIT
