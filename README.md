# agy-pool-go: Antigravity Multi-Account Quota Pool & Reverse Proxy Gateway

[![CI](https://github.com/vlxlv/agy-pool-go/actions/workflows/ci.yml/badge.svg)](https://github.com/vlxlv/agy-pool-go/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/badge/go-1.26.7-blue.svg)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Platform](https://img.shields.io/badge/platform-Linux%20%7C%20Darwin%20%7C%20Termux-green.svg)](#)
[![CGO](https://img.shields.io/badge/CGO-disabled%20(0)-brightgreen.svg)](#)

A high-performance, zero-CGO Go implementation and reverse proxy gateway for **Antigravity CLI (`agy`)** on Linux, Termux, and macOS.

---

## Key Capabilities

- **Zero-CGO Pure Go (`CGO_ENABLED=0`)**: Built strictly on the Go standard library and modern transpiled SQLite (`modernc.org/sqlite`). Fully static binaries cross-compile out-of-the-box for `linux/amd64`, `linux/arm64`, `darwin/amd64`, and `darwin/arm64`.
- **High-Performance HTTP Reverse Proxy**: Non-blocking concurrent reverse proxy gateway listening locally (`127.0.0.1:8899`) with real-time SSE token streaming, request header filtering, and dynamic multi-account rotation.
- **Ultra-Low Memory Single-Pass Request Inspection**: Single-pass JSON request inspection without copying conversational turns, reducing per-request memory allocation from 573 KB to 5 KB on 500 KB payloads.
- **Intelligent Upstream Connection Pooling**: High-frequency, zero-risk metadata calls (`retrieveUserQuotaSummary`, `fetchAvailableModels`, etc.) reuse pooled HTTP/1.1 connections to eliminate TLS handshake latency.
- **Smart Prompt Cache Affinity (Cascade Pinning)**: Binds conversation `cascade_id`s to healthy accounts with 5-minute warm windows, achieving 90%+ upstream prompt-cache hit rates with automatic load-spill fallback when in-flight capacity is reached.
- **Pre-Connect Failover (`httptrace`)**: Uses `httptrace.ClientTrace` to verify whether connection bytes reached upstream; transport errors occurring prior to connection (DNS, dial timeout, TLS handshake) safely fail over to the next account without violating zero-replay guarantees.
- **Deadlock-Free Model Capacity Cooldown**: Per-model cooldowns on 503/capacity limits isolate busy models without disabling accounts. If all accounts in the pool are cooling down for a model, the proxy probes the soonest-expiring account and automatically unpauses the model across the pool upon success.
- **Strict No-Replay Transport Guarantees**: Enforces fail-safe upstream routing. Non-idempotent or in-flight POST streaming requests are never replayed across accounts upon ambiguous post-connect network failures, preventing duplicate generation charges.
- **Automated Storage & Log Lifecycle**: Built-in 5MB log rotation (`agy-pool.log`), 5MB JSONL history rotation (`quota-history.jsonl`), and explicit `ReadHeaderTimeout` protection.
- **Intelligent Load Balancing & Failover**: Dynamically routes CLI generation requests based on configurable strategies (`smart` by default, and `max_quota`). Automatically fails over on HTTP 429 rate limits or quota depletion while respecting upstream `Retry-After`.
- **Native Systemd & Debian Integration**: Includes native `.deb` packaging with automatic per-user and system-wide systemd units, clean permission isolation, and downgrade-safe version ordering.
- **Distinct CLI Domain Responsibilities**:
  - `agy-pool status`: Fast, script-friendly runtime summary (PID, memory, uptime, state.db schema, pool counts) without slow network quota refreshes.
  - `agy-pool list`: Compact account inventory and cached quota overview.
  - `agy-pool quota`: Dedicated quota dashboard with live progress bars and reset countdowns.
  - `agy-pool doctor`: Comprehensive system diagnostic checking binary discovery, daemon health, process table, database integrity, and Google TLS connectivity.
  - `agy-pool update`: Safe, self-updating CLI with cryptographic SHA-256 verification and atomic binary replacement.
- **Go-Native Persistent State**: Stores authoritative pool and runtime state in SQLite (`state.db`) under `~/.local/share/agy-pool/state.db` with transactional locking and schema versioning.
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

### Installation & Native `agy` Integration

#### Via Debian/Ubuntu Package (.deb)
Download the `.deb` package from [Releases](https://github.com/vlxlv/agy-pool-go/releases) and install:
```bash
sudo dpkg -i agy-pool_<version>_amd64.deb
```
The package automatically installs `/usr/bin/agy-pool` and systemd service files. Run the gateway per user with `systemctl --user enable --now agy-pool`, or system-wide with `sudo systemctl enable --now agy-pool` (runs as the `agy-pool` system user created by the package, state in `/var/lib/agy-pool`, optional config in `/etc/agy-pool/config.json`). Without the package, a user install can use the same unit: copy `contrib/systemd/user/agy-pool.service` to `~/.config/systemd/user/` and run `systemctl --user enable --now agy-pool` (it runs `~/.local/bin/agy-pool`). `agy-pool stop` and `agy-pool restart` act through `systemctl` when the gateway runs under one of these units. Upgrade with a newer `.deb`; `agy-pool update` refuses to replace a package-owned binary.

If upgrading from an earlier beta package, install the official `v0.1.0` release package with `sudo apt install --allow-downgrades ./agy-pool_<version>_<arch>.deb` or `sudo dpkg -i agy-pool_<version>_<arch>.deb`.

#### Via agy-pool installer
To install `agy-pool` and set up transparent integration with native Antigravity CLI:

```bash
# User-mode installation (recommended)
./bin/agy-pool install

# Or system-mode installation
sudo ./bin/agy-pool install --system
```

#### Intended User Experience

Once installed, **normal operation requires zero special flags, aliases, or manual environment exports**:

```bash
# Normal use: automatically routes through agy-pool quota load balancing
agy -p "explain this code"
agy -c
agy --mode accept-edits --dangerously-skip-permissions
```

To bypass the pool and execute native Antigravity directly against upstream Google:

```bash
# Explicit bypass: runs native Antigravity directly with its current native credentials
# No CLI Base synchronization or agy-pool routing is performed.
agy-raw -p "direct prompt"

# Or using the agy-orig alias
agy-orig -p "direct prompt"
```

#### Managed Installation Layout

The installer establishes a deterministic, robust layout:

| Path | Description |
|------|-------------|
| `~/.local/bin/agy-pool` | Go `agy-pool` binary |
| `~/.local/bin/agy` | Thin POSIX managed shim (default entrypoint invoking `agy-pool run`) |
| `~/.local/bin/agy-raw` | Managed shim executing direct native bypass via `agy-pool raw` |
| `~/.local/bin/agy-orig` | Symlink alias to `agy-raw` |
| `~/.local/libexec/agy-pool/agy-native` | Preserved real native Antigravity executable |
| `~/.config/agy-pool/config.json` | Static configuration (see tracked [`config.example.json`](config.example.json)) |
| `~/.local/share/agy-pool/state.db` | Authoritative dynamic account pool state database |

The installer safely validates and preserves the native `agy` binary into `~/.local/libexec/agy-pool/agy-native` before publishing or updating the entrypoint shim. It never overwrites the only copy of native `agy`.

#### Updating `agy-pool`

```bash
agy-pool update --check   # Report whether a newer release exists
agy-pool update           # Download, verify, replace, and restart a running gateway
```

`update` (alias `upgrade`) fetches the newest GitHub release (prereleases included), verifies the platform binary against the release `SHA256SUMS`, atomically replaces the installed `agy-pool` binary, and restarts the gateway if it was running. A failed download or checksum mismatch leaves the current binary untouched. The repository is private, so a token is required: `GH_TOKEN`, `GITHUB_TOKEN`, or an existing `gh auth login` session. System-mode installs (`/usr/local/bin`) need `sudo` with `GH_TOKEN` set.

#### Official `agy` Upgrades & Repair Workflow

If an official Google Antigravity update overwrites `~/.local/bin/agy` with a new native ELF binary:

1. Running `agy-pool doctor` will detect that `~/.local/bin/agy` is a native binary bypassing the pool.
2. Re-running `agy-pool install` repairs the integration:
   - Detects the new native binary at `~/.local/bin/agy`.
   - Atomically updates `~/.local/libexec/agy-pool/agy-native` with the upgraded binary.
   - Restores the managed shim at `~/.local/bin/agy`.

#### Uninstallation

To remove `agy-pool` integration:

```bash
agy-pool uninstall
```

Uninstall safely removes `agy-pool`-owned shims (`agy`, `agy-raw`, `agy-orig`), the `agy-pool` binary, and the managed native copy in `libexec/agy-pool`, while leaving any external native binaries completely untouched.


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

# Standalone test execution
go test ./... -count=1
```

### Self-Contained & Zero Dependencies

The Go implementation is 100% standalone and self-contained with no Python runtime or external script dependencies:
- Zero Python dependencies for building, running, testing, or updating
- 100% offline standard library test suite (`go test ./...`)
- Comprehensive race condition detection (`go test -race ./...`)
- Verified failover, prompt caching, quota tracking, and lifecycle isolation

---

## Release & CI Architecture

- **Continuous Integration (`ci.yml`)**: Triggered on push and PR to `main` with minimal permissions (`contents: read`). Runs formatting checks (`gofmt`), static analysis (`go vet`), offline unit tests, and race detector tests across standard platforms.
- **Tag-Driven Releases (`release.yml`)**: Triggered on tag pushes (`v*`) or manual `workflow_dispatch`. Re-verifies all test gates, cross-compiles static binaries for all 4 supported architectures with build-time version injection via `-ldflags`, generates and verifies unified `SHA256SUMS`, and publishes the GitHub Release (`contents: write`).
- **Production Deployment**: Production deployment remains an independent, manual operational procedure completely decoupled from CI and release publishing.

---

## License

MIT
