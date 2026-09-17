# agy-pool: Python to Go Formal Migration Contract & Parity Specification

**Document Version:** 1.0.0
**Status:** PROPOSED & FROZEN
**Reference Baseline:** `agy-pool-python` release `v0.1.0-beta.2`
**Reference Commit SHA:** `c91765907006954d9b7f29faa61dbefaec2c284d`
**Target Implementation:** `agy-pool-go` (`v0.1.0-alpha.1` candidate)
**Authoritative Source of Truth:** This document governs all development, verification, candidate gating, and eventual production cutover for the Go rewrite of `agy-pool`.

> [!NOTE]
> **Post-Cutover Implementation Status (Historical Specification)**:
> - Python-to-Go production cutover has been completed.
> - Go is now the primary/live implementation.
> - Python is retained only as parity, migration, and rollback reference.
> - The historical migration contract below is intentionally preserved for audit/history.


---

## Classification Schema

Every requirement, data model, and interface in this contract is marked with one of five authoritative labels:

- `[FROZEN]`: Non-negotiable parity requirement. The Go implementation MUST match Python `v0.1.0-beta.2` observable behavior exactly. Any deviation is a blocking defect.
- `[COMPATIBILITY REQUIRED]`: External contract (file formats, network protocols, CLI arguments, environment variables) that must remain interoperable with existing systems, scripts, and persistent state.
- `[IMPLEMENTATION DETAIL]`: Internal Python mechanism that Go may implement differently using Go idioms (e.g., goroutines instead of daemon threads), provided all observable contracts are preserved.
- `[FUTURE DECISION]`: Explicitly documented design question deferred to a later phase. MUST NOT be unilaterally decided during the initial parity rewrite.
- `[NON-GOAL]`: Explicitly out of scope for the initial Go implementation. Speculative expansions are strictly prohibited.

---

## 1. Migration Principles & Self-Hosting Topology

### Core Principles `[FROZEN]`

1. **Python `v0.1.0-beta.2` is the Behavior Oracle**: The Python codebase at commit `c917659` defines correct behavior. In any dispute between theoretical design and Python implementation, the Python behavior is authoritative.
2. **Behavioral Parity Before Replacement**: Go must prove functional, protocol, and storage parity before replacing Python in any production role.
3. **No Big-Bang Cutover**: The rewrite will proceed through staged, testable phases (G1, G2, G3) with side-by-side candidate gates.
4. **No Shared Writable Production State During Development**: Go development and test runs MUST NEVER write to Python production state paths (`~/.gemini/agy-pool-accounts.json`).
5. **Python Remains the Production Gateway During Go Development**: The Python gateway continues serving user and agent requests throughout development.
6. **Isolated Candidate Instances**: Go candidate instances run on a dedicated port (`8901`) with an isolated copy of state.
7. **Rigorous Offline Verification**: Parity claims must be substantiated by JSON golden fixtures, dual-runner differential tests, and controlled live candidate verification.
8. **Preserve No-Replay Semantics**: Ambiguous transport failures (timeouts, broken pipes, network drops) MUST NOT be replayed across accounts.
9. **Pre-Migration Backups Mandatory**: Real user state must be snapshot-backed up before any candidate test or state migration.
10. **Fail-Closed Test Safety**: Tests must abort immediately if a write target resolves to a protected production path.

### Self-Hosting Topology `[FROZEN]`

The development environment operates under an active self-hosting loop:

```
┌────────────────────────────────────────────────────────┐
│                   Development Agent                    │
│            (Coding / Building / Verifying)             │
└──────────────────────────┬─────────────────────────────┘
                           │ Active HTTP session
                           ▼
┌────────────────────────────────────────────────────────┐
│               agy-pool-python (PID 625954)             │
│            Port 8899 (Production Gateway)              │
│       State: ~/.gemini/agy-pool-accounts.json          │
└──────────────────────────┬─────────────────────────────┘
                           │ Upstream API calls
                           ▼
┌────────────────────────────────────────────────────────┐
│          Google Cloud Code / Antigravity PaaS          │
└────────────────────────────────────────────────────────┘

                           ... in parallel ...

┌────────────────────────────────────────────────────────┐
│                 agy-pool-go (Candidate)                │
│            Port 8901 (Isolated Candidate)              │
│       State: ~/.gemini-go-dev/ or /tmp/state           │
└────────────────────────────────────────────────────────┘
```

> [!CAUTION]
> **CRITICAL SELF-HOSTING INVARIANT**: The stable Python gateway on port 8899 carries the active development session itself. The Python gateway MUST NOT be stopped, signaled, killed, or replaced by any autonomous agent session. Production cutover must be performed exclusively from a human-controlled terminal outside any active agent session.

---

## 2. Project Roles & Development Isolation

### Role Definition `[FROZEN]`

| Attribute | `agy-pool-python` | `agy-pool-go` |
|---|---|---|
| **Role** | Reference implementation & Behavior Oracle | Candidate implementation |
| **Status** | Production active; carries live agent | Experimental development target |
| **State Authority** | Owns `~/.gemini/agy-pool-accounts.json` | READ-ONLY access to production copies; writes only to isolated root |
| **Network Port** | `8899` (or `$AGY_PORT`) | `8901` during candidate phase |
| **Process State** | Owns `~/.gemini/agy-pool.pid` | Owns separate candidate PID file |
| **Log Target** | `~/.gemini/agy-pool.log` | Dedicated candidate log file |

### Isolation Policy `[FROZEN]`

During all development phases (G1 through G3):
- Go source code will reside in a distinct package/directory tree.
- Go binaries MUST NOT be symlinked to `agy-pool` or `agy` in system bin paths.
- Go development state root defaults to `~/.gemini-go-dev/` or a temporary directory via `AGY_GEMINI_DIR`.
- Go test suites MUST enforce `AGY_TEST_MODE=1` with fail-closed write guards.

---

## 3. Target Go Repository Architecture

### Directory Layout `[FROZEN]`

The Go codebase must follow standard Go project structure without extraneous framework overhead:

```text
agy-pool-go/
├── cmd/
│   └── agy-pool/
│       └── main.go                 # Thin executable entrypoint & CLI dispatcher
│
├── internal/
│   ├── config/                     # Constants, path resolution, test guards
│   ├── storage/                    # Atomic JSON persistence, file locking
│   ├── auth/                       # OAuth client, token refresh, single-flight
│   ├── accounts/                   # Account lifecycle, target matching, import/export
│   ├── quota/                      # Quota parsing, freshness classification, formatting
│   ├── scheduler/                  # max_quota, least_used, round_robin engine
│   ├── proxy/                      # HTTP/1.1 reverse proxy, SSE streaming, failover
│   ├── daemon/                     # Process supervisor, PID management, log rotation
│   ├── diagnostics/                # Doctor health checks, environment inspection
│   ├── cli/                        # Subcommand definitions, table rendering
│   └── conversation/               # SQLite session continuity, CWD discovery, presence
│
├── testdata/
│   ├── pool/                       # JSON state fixtures (empty, multi, legacy)
│   ├── quota/                      # Upstream quota response fixtures
│   ├── scheduler/                  # Ranking input/output vectors
│   ├── proxy/                      # HTTP request/response transcripts
│   └── cli/                        # Expected CLI stdout/stderr golden files
│
├── scripts/
│   ├── dual-test.sh                # Differential Python vs Go test runner
│   └── candidate-smoke.sh          # Live candidate verification harness
│
├── go.mod
├── go.sum
└── README.md
```

### Dependency Discipline `[FROZEN]`

- **Standard Library First**: Utilize Go stdlib (`net/http`, `net/url`, `os`, `sync`, `encoding/json`, `crypto`, `syscall`, `time`, `database/sql`).
- **Zero Heavyweight Frameworks**:
  - NO web frameworks (no Gin, Echo, Fiber). Use `net/http`.
  - NO ORMs (no Gorm, Ent). Use `database/sql` with a pure-Go SQLite driver (e.g. `modernc.org/sqlite`).
  - NO dependency injection frameworks (no Wire, Dig). Explicit constructor injection only.
  - NO CLI frameworks (no Cobra, Urfave). Use stdlib `flag` or a lightweight POSIX argument parser that preserves existing CLI syntax.
  - NO structured logging frameworks (no Zap, Zerolog) for gateway logs. Preserve the exact line-oriented format required by parsers.

---

## 4. State Layout Contract

The persistent files in `~/.gemini/` represent both external contracts and internal state:

| Path | Owner | Permissions | Access | Locking Semantics | Compatibility Type |
|---|---|---|---|---|---|
| `~/.gemini/agy-pool-accounts.json` | Storage | `0600` | Read/Write | Exclusive via `.lock` | `[COMPATIBILITY REQUIRED]` Byte/JSON schema compatible |
| `~/.gemini/agy-pool-accounts.json.lock` | Storage | `0600` | Lock only | Sidecar `flock` inode | `[COMPATIBILITY REQUIRED]` Stable inode flock |
| `~/.gemini/agy-pool.pid` | Daemon | `0600` | Read/Write | Exclusive via `.lock` | `[COMPATIBILITY REQUIRED]` JSON schema compatible |
| `~/.gemini/agy-pool.pid.lock` | Daemon | `0600` | Lock only | Sidecar `flock` inode | `[COMPATIBILITY REQUIRED]` Stable inode flock |
| `~/.gemini/agy-pool.log` | Daemon | `0600` | Append | Mutex + file lock | `[COMPATIBILITY REQUIRED]` Parser token compatible |
| `~/.gemini/agy-pool.log.<N>` | Daemon | `0600` | Read-only | Rotated under lock | `[IMPLEMENTATION DETAIL]` Shuffled backups |
| `~/.gemini/agy-pool-refresh-<hash>.lock`| Auth | `0600` | Lock only | Per-account single-flight | `[COMPATIBILITY REQUIRED]` SHA256-derived lock name |
| `~/.gemini/antigravity-cli/antigravity-oauth-token` | Native agy | `0600` | Read/Write | Managed by sync | `[COMPATIBILITY REQUIRED]` JSON token compatibility |
| `~/.gemini/antigravity-cli/conversation_summaries.db`| Native agy | User rw | Read-Only | SQLite read-only mode | `[COMPATIBILITY REQUIRED]` Schema read compatibility |
| `~/.gemini/antigravity-cli/presence/<cid>.lock` | Native agy | User rw | Non-blocking flock | `flock` probe | `[COMPATIBILITY REQUIRED]` Native agy lock detection |

---

## 5. Pool JSON Schema Contract

### Top-Level Schema `[COMPATIBILITY REQUIRED]`

The file `~/.gemini/agy-pool-accounts.json` contains a JSON object:

```json
{
  "version": 1,
  "strategy": "max_quota",
  "active_account_id": "acc_1",
  "round_robin_last_account_id": "acc_2",
  "accounts": []
}
```

- `version` (int, required): Schema version (currently `1`).
- `strategy` (string, required): One of `"max_quota"`, `"least_used"`, `"round_robin"`. Default: `"max_quota"`.
- `active_account_id` (string or null, required): ID of the currently designated active account.
- `round_robin_last_account_id` (string or null, optional): Persisted reservation cursor for round-robin.
- `accounts` (array of objects, required): Ordered list of account records.

### Account Record Schema `[COMPATIBILITY REQUIRED]`

```json
{
  "id": "acc_1",
  "name": "Work Account",
  "email": "user@example.com",
  "access_token": "ya29....",
  "refresh_token": "1//04....",
  "id_token": "eyJhbGciOi...",
  "token_expiry": 1789563600.0,
  "updated_at": 1789560000,
  "status": "ready",
  "validation_url": null,
  "rate_limited_until": 0.0,
  "request_count": 120,
  "gen_count": 45,
  "error_count": 0,
  "last_used_at": 1789560037,
  "last_quota": {
    "gemini_5h": {
      "fraction": 0.85,
      "reset_time": "2026-09-16T15:16:55Z"
    },
    "gemini_weekly": {
      "fraction": 0.92,
      "reset_time": "2026-09-23T04:10:43Z"
    },
    "third_party_5h": {
      "fraction": 1.0,
      "reset_time": "2026-09-16T17:00:00Z"
    },
    "third_party_weekly": {
      "fraction": 1.0,
      "reset_time": "2026-09-23T12:00:00Z"
    },
    "remaining_fraction": 0.85,
    "reset_time": "2026-09-16T15:16:55Z",
    "updated_at": 1789560008
  }
}
```

### Field Specification Rules `[FROZEN]`

1. **Unknown Field Preservation**: The Go parser MUST preserve unrecognized fields on account objects or the root object during read-modify-write transactions.
2. **Empty Field Serialization**: Fields with `null` or absent values must not break Python deserialization. Keys with explicit `null` in Python must serialize cleanly.
3. **Legacy Fallback Compatibility**:
   - If `last_quota` contains only `remaining_fraction` (alpha.9 schema), Go MUST treat both `gemini_5h` and `gemini_weekly` as having that fraction (`legacy_fraction_known = true`).
   - If legacy fields `quota`, `gemini_5h_pct`, or `gemini_weekly_pct` are present without `last_quota`, Go must compute capacity using those fallback values.
4. **Timestamps**:
   - `token_expiry` and `rate_limited_until`: Unix epoch float/integer seconds.
   - `updated_at`, `last_used_at`, `last_quota.updated_at`: Unix epoch integer seconds.
   - `reset_time`: ISO 8601 UTC string (e.g. `2026-09-16T15:16:55Z`) or epoch number.

---

## 6. Atomic Storage Contract

### Read-Modify-Write Persistence `[FROZEN]`

All pool modifications must pass through an atomic transaction equivalent to Python's `pool_transaction(mutator)`:

1. **Process & Thread/Goroutine Mutual Exclusion**:
   - Acquire an in-memory lock (`sync.RWMutex` / `sync.Mutex`).
   - Acquire an exclusive POSIX `flock(LOCK_EX)` on `~/.gemini/agy-pool-accounts.json.lock`.
2. **Crash-Safe Unlocked Read**:
   - Read and parse `~/.gemini/agy-pool-accounts.json`.
   - If JSON is malformed or missing the `accounts` array, **ABORT IMMEDIATELY**. Under NO circumstances should corrupted state be overwritten with an empty pool.
3. **In-Memory Mutation**:
   - Execute the mutation closure.
4. **Atomic File Replacement**:
   - Create a temporary file in the *same directory* (`~/.gemini/agy-pool-accounts.json.<pid>.<rand>.tmp`).
   - Set file permissions strictly to `0600` via `fchmod`.
   - Write formatted JSON (2-space indent, UTF-8).
   - Call `fsync()` on the temporary file descriptor.
   - Close the temporary file.
   - Atomically replace the destination file via `rename(tmp, dest)` (`os.Rename`).
   - Open the parent directory (`~/.gemini/`) and call `fsync()` on the directory file descriptor (POSIX metadata durability).
5. **Cleanup**:
   - If any error occurs prior to rename, remove the temporary file.
   - Release the sidecar `flock` and in-memory lock.

### Concurrent Failure Invariants `[FROZEN]`

- **Crash before rename**: Original file untouched, temp file unlinked.
- **Crash after rename**: New state completely written and durable; no partial JSON.
- **Concurrent processes**: Block on `.lock` inode; execute sequentially.
- **Concurrent goroutines**: Block on in-memory mutex; execute sequentially.

---

## 7. Persistent-State Safety Contract

### Incident History & Root Cause `[FROZEN]`

During Python modularization, test-harness path isolation failed when entrypoint globals were patched while modules read authoritative constants. Synthetic test accounts destroyed real user OAuth state. This must be prevented in Go by architectural design, not conventions.

### Safety Invariants `[FROZEN]`

1. **Authoritative Central Configuration**: All storage and state paths must be derived from a single configuration package (`internal/config`). No package may construct independent paths to `~/.gemini`.
2. **Immutable Production Path Detection**:
   - The real OS user home must be detected via OS user databases (`os/user` or `pwd.getpwuid`) rather than relying purely on the mutable `HOME` environment variable.
   - The production directory (`<real_home>/.gemini`) is permanently registered in `_FORBIDDEN_WRITE_DIRS`.
3. **Fail-Closed Write Guard**:
   - Every file write or directory creation primitive in Go must call `AssertSafeWritePath(path)`.
   - When running in test mode (`AGY_TEST_MODE=1` or test build tag), if the canonical resolved path (`filepath.EvalSymlinks`) matches or is a child of any forbidden directory, the process must **PANIC/FATAL IMMEDIATELY** before any file descriptor is opened.
4. **Pre-Test Snapshot Protocol**:
   - Automated scripts must create a timestamped backup (`~/.gemini/agy-pool-accounts.json.<ts>.bak`) with `0600` permissions prior to running any test suites.
   - Post-test checks must verify the SHA-256 hash and mtime of the production accounts file.

---

## 8. Authentication & OAuth Contract

### OAuth Specifications `[COMPATIBILITY REQUIRED]`

- **Client ID & Secret**: Sourced by default from obfuscated XOR-decoded bytes matching Python `_DEFAULT_CLIENT_ID` and `_DEFAULT_CLIENT_SECRET`, overrideable via `AGY_CLIENT_ID` and `AGY_CLIENT_SECRET`.
- **OAuth Scopes**:
  ```text
  openid email profile https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/userinfo.profile https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/cclog https://www.googleapis.com/auth/experimentsandconfigs https://www.googleapis.com/auth/aicode
  ```
- **Loopback Callback Server**:
  - Binds to `127.0.0.1` on the first free port in range `8085` to `8135`.
  - Handles `GET /auth/callback` or `GET /`.
  - Extracts `code` or `error` query parameter.
  - Responds with UTF-8 HTML confirmation page and closes connection.
- **Token Refresh Protocol**:
  - Endpoint: `POST https://oauth2.googleapis.com/token`
  - Content-Type: `application/x-www-form-urlencoded`
  - Parameters: `client_id`, `client_secret`, `refresh_token`, `grant_type=refresh_token`.
  - Threshold: Refresh if `token_expiry - now <= 120` seconds.
  - Refresh Lock: Single-flight per account using sidecar file lock `~/.gemini/agy-pool-refresh-<sha256(key)[:24]>.lock` (where `key` is `id`, `email`, or `refresh_token`).
  - Double-Check: After acquiring lock, re-read stored pool state; if another process already refreshed the token, return the new token without calling Google.
  - Token Persistence: Update `access_token`, `token_expiry`, `updated_at`, and optionally `refresh_token` (if rotated by Google) under `pool_transaction`.

### Failure Semantics `[FROZEN]`

- **Missing Refresh Token**: Return error immediately; do not attempt HTTP.
- **HTTP 400 `invalid_grant` / Revoked Token**: Mark account `status = "auth_error"`, set `rate_limited_until = now + 3600`, increment `error_count`.
- **Network / Timeout Error**: Return error without mutating persistent status.
- **Zero Token Leakage**: Tokens and refresh tokens MUST NEVER appear in log files, CLI outputs, or error strings.

---

## 9. Account Identity & Privacy Contract

### Separation of Identity Layers `[FROZEN]`

1. **Internal Identity**: Used strictly for storage keys, token refresh locks, and upstream OAuth routing:
   - Account ID: Stable string `acc_1`, `acc_2`, etc.
   - Account Email: Real user email stored in `agy-pool-accounts.json`.
2. **User-Facing Display Identity**: Used across CLI tables, status listings, doctor output, and gateway logs:
   - Friendly Name: Explicit custom label (`name` field) set via `agy-pool rename`.
   - Fallback 1: `"Account N"` if account ID matches `acc_N`.
   - Fallback 2: `"Account"` generic safe fallback.

> [!IMPORTANT]
> **PRIVACY INVARIANT**: The user's real email address MUST NEVER be printed to standard output, standard error, proxy log lines, or diagnostics. Only friendly names or safe fallbacks (`Account 1`, etc.) are permitted in visible outputs.

---

## 10. Account Lifecycle Management Contract

### Command Behaviors `[COMPATIBILITY REQUIRED]`

- **Target Resolution (`find_account_by_target`)**:
  Accepts a selector string and resolves in order:
  1. 1-based numerical index corresponding to the current `agy-pool list` display order.
  2. Exact match against `id` (e.g. `acc_1`).
  3. Exact match against `email`.
  4. Exact match against friendly `name`.
- **ID Allocation (`_next_account_id`)**:
  Scans existing accounts and allocates `acc_N` using the lowest positive integer `N` not currently in use. Account IDs are immutable once assigned.
- **Operations**:
  - `login` / `add`: Initiates browser OAuth flow, exchanges code for tokens, decodes email/name from JWT id_token payload, allocates ID, adds to pool, sets as active if first account.
  - `import-current`: Reads `~/.gemini/antigravity-cli/antigravity-oauth-token`, imports if not present in pool.
  - `rename <target> <name>`: Updates friendly label under transaction. Rejects empty name.
  - `remove <target>`: Deletes account from pool. If removed account was `active_account_id`, resets active to first available or null.
  - `switch [target]`: Explicitly sets `active_account_id`. If `target` is `"auto"`, clears explicit selection and lets scheduler choose.
  - `verify [target]`: Displays Google security verification URL for accounts in `validation_required` status.
  - `export [file]`: Exports pool JSON. Supports `-e / --encrypt` with passphrase using PBKDF2 (SHA-256, 100,000 iterations) + AES-GCM (256-bit). Default output file: `agy-pool-backup-<timestamp>.json` with mode `0600`.
  - `import <file>`: Restores pool. Flags: `--replace` (overwrites existing accounts) vs merge (updates existing by ID/email, appends new). Supports `--skip-existing`.

---

## 11. Quota Data & Capacity Representation Contract

### Quota Structure `[COMPATIBILITY REQUIRED]`

The `last_quota` object tracks capacity across distinct rate-limit horizons:

- `gemini_5h`: 5-hour rolling Gemini window (`fraction` float `[0.0, 1.0]`, `reset_time` ISO8601).
- `gemini_weekly`: 7-day rolling Gemini window (`fraction` float `[0.0, 1.0]`, `reset_time` ISO8601).
- `third_party_5h`: 5-hour rolling Claude/GPT window.
- `third_party_weekly`: 7-day rolling Claude/GPT window.
- `remaining_fraction`: Derived compat floor (`min(gemini_5h, gemini_weekly)`).
- `reset_time`: Reset time corresponding to the minimum fraction window.
- `updated_at`: Unix epoch seconds when snapshot was recorded.

### Parsing & Probing Rules `[FROZEN]`

1. **Fraction Normalization**: Fractions must be clamped to `[0.0, 1.0]`. `NaN` and `Inf` must be rejected as invalid/unknown.
2. **Primary Quota Endpoint**: `POST https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary` with empty JSON `{}`.
3. **Fallback Quota Endpoint**: If primary returns HTTP error, call `POST https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels` and parse `quotaInfo.remainingFraction` from candidates (`gemini-3.8-flash-high`, `gemini-3.6-flash-high`, etc.).
4. **Preserve Known Cached Windows**: If a refresh returns only one window (e.g. 5h without weekly), Go MUST preserve the previously cached weekly window rather than deleting it.
5. **Depleted Threshold**: An account is classified as depleted if its raw quota floor satisfies `raw_floor <= 0.005` (0.5%).

---

## 12. Quota Freshness & Background Refresh Contract

### Freshness Classification `[FROZEN]`

Given snapshot age `age = now - last_quota.updated_at`:

| Freshness Class | Condition | Freshness Rank (Scheduling) | Action Required |
|---|---|---|---|
| **fresh** | `age <= 60s` AND all window reset times are in the future | `2` | None |
| **aging** | `61s <= age <= 300s` AND all reset times in future | `2` | Async refresh in background |
| **stale** | `age > 300s` OR any window reset time has passed (`reset_time <= now`) | `1` | Async refresh required |
| **unknown** | Missing `last_quota` OR `updated_at <= 0` | `0` | Immediate refresh required |

### Single-Flight Background Refresh `[FROZEN]`

- Asynchronous background refreshes are launched when a request selects an account whose quota is not `fresh`.
- Deduplication: Managed via in-memory set (`_QUOTA_REFRESH_IN_FLIGHT`). Only one goroutine per account key may probe quota at a time.
- Exponential Backoff on Error: If refresh fails, schedule next retry after:
  `[30s, 60s, 120s, 240s, 300s]`, capped at `300s`.
- On success: Clear backoff entry.

---

## 13. 429 & Rate Limit / Retry-After Contract

### Error Classification `[FROZEN]`

- **HTTP 429**: Treated unconditionally as rate-limit / quota exhaustion.
- **HTTP 403 Quota Markers**: Treated as quota exhaustion if error body contains any of:
  `resource_exhausted`, `quota_exceeded`, `rate_limit_exceeded`, `quota exceeded`, `quota exhausted`, `exceeded your current quota`, `insufficient quota`.
- **HTTP 403 Validation Markers**: Treated as security check if body contains:
  `validation_required`, `verify your account`, `verify your account to continue`.

### Retry-After Parsing `[COMPATIBILITY REQUIRED]`

The `Retry-After` header must be parsed from upstream response headers:
1. Integer seconds (e.g. `"120"`).
2. RFC 2822 / RFC 7231 HTTP-date (e.g. `"Wed, 21 Oct 2026 07:28:00 GMT"`). Convert to delta seconds against `now`.
3. Clamped: `delay = max(5, min(86400, parsed_seconds))`.
4. Fallback Default: `300` seconds (5 minutes) if header is absent or invalid.

### Mutation on Quota Failure `[FROZEN]`

Under `pool_transaction`:
- `rate_limited_until = now + delay`
- `last_quota.updated_at = 0` (invalidates freshness; forces immediate refresh on next use)
- `error_count = error_count + 1`
- Cached quota fractions are PRESERVED (not zeroed out), maintaining historical telemetry.

---

## 14. Scheduler & Load Balancing Engine Contract

### Eligibility Tiering `[FROZEN]`

For any generation request, accounts are grouped into four strictly prioritized tiers:

1. **Tier 1 - Eligible**: Healthy capacity (`raw_floor > 0.005`), not rate-limited (`rate_limited_until <= now`), status not restricted (`validation_required` or `auth_error`).
2. **Tier 2 - Depleted**: Raw quota floor depleted (`raw_floor <= 0.005`), not rate-limited, not restricted.
3. **Tier 3 - Cooldown**: Rate-limited (`rate_limited_until > now`).
4. **Tier 4 - Restricted**: Status is `"validation_required"` or `"auth_error"`.

Ordering: All Eligible accounts precede Depleted accounts, which precede Cooldown accounts, which precede Restricted accounts.

### Reset-Aware Capacity Math (`max_quota`) `[FROZEN]`

For each account, compute reset-aware capacity parameters:

$$\text{Constants: } W_5 = 18000.0 \text{ seconds (5 hours)}, \quad W_7 = 604800.0 \text{ seconds (7 days)}$$

$$\text{Reset Ratios: } r_5 = \min\left(1.0, \frac{\max(0.0, \text{reset}_5 - \text{now})}{W_5}\right), \quad r_7 = \min\left(1.0, \frac{\max(0.0, \text{reset}_7 - \text{now})}{W_7}\right)$$

$$\text{Headroom Paces: } \text{pace}_5 = q_5 - r_5, \quad \text{pace}_7 = q_7 - r_7$$

$$\text{Aggregates: } \text{worst\_pace} = \min(\text{known paces}), \quad \text{total\_pace} = \sum(\text{known paces}), \quad \text{raw\_floor} = \min(\text{known fractions})$$

$$\text{known\_window\_count} = \begin{cases} 1 & \text{if legacy combined fraction} \\ (q_5 \ne \text{nil}) + (q_7 \ne \text{nil}) & \text{otherwise} \end{cases}$$

#### Sort Tuple for `max_quota` (Descending) `[FROZEN]`

```python
(
    known_window_count,         # Prefer accounts with 2 measured windows over 1
    quota_freshness_rank,       # fresh/aging (2) > stale (1) > unknown (0)
    worst_pace,                 # Safest remaining pace headroom (float64)
    total_pace,                 # Aggregate pace across windows
    raw_floor,                  # Absolute lowest fraction
    -hits                       # Lowest generation hits tie-break
)
```

### Strategy: `least_used` `[FROZEN]`

Primary criterion is generation count (`gen_count`, falling back to `request_count`), breaking ties by reset-aware capacity:

#### Sort Tuple for `least_used` (Ascending) `[FROZEN]`

```python
(
    hits,                       # Lowest generation count first
    -known_window_count,        # Prefer dual-window evidence
    -quota_freshness_rank,      # Prefer fresh telemetry
    -worst_pace,                # Higher worst pace tie-break
    -total_pace,                # Higher total pace tie-break
    -raw_floor,                 # Higher raw floor tie-break
    id                          # Deterministic string ID tie-break
)
```

### Strategy: `round_robin` `[FROZEN]`

1. **Persistent Reservation Cursor**: Stored at `pool["round_robin_last_account_id"]`.
2. **Atomic Selection-at-Reservation**: Under `pool_transaction`, the scheduler rotates the eligible account list starting immediately *after* `round_robin_last_account_id`.
3. **Cursor Advancement**: The cursor is updated to the chosen account's ID **BEFORE** upstream network dispatch begins.
4. **No Rollback on Upstream Failure**: If upstream dispatch fails, the cursor remains advanced. Concurrent generation requests cannot reserve the same next account.
5. **Non-RR Strategies**: `max_quota` and `least_used` MUST NOT mutate `round_robin_last_account_id`.

---

## 15. Proxy HTTP Protocol & Request Forwarding Contract

### Protocol Handling `[FROZEN]`

- **Listen Address**: `127.0.0.1:8899` (or `$AGY_PORT`).
- **HTTP Version**: HTTP/1.1 reverse proxy.
- **Request Body Reading**:
  - If `Transfer-Encoding: chunked`, decode chunks boundedly.
  - If `Content-Length` present, read exact byte count.
  - If `POST` with empty body, ensure `Content-Length: 0` header is forwarded.
- **Hop-by-Hop Header Sanitization**:
  Strip headers listed in `Connection` value plus: `connection`, `keep-alive`, `proxy-authenticate`, `proxy-authorization`, `proxy-connection`, `te`, `trailer`, `transfer-encoding`, `upgrade`.
- **Header Rewriting**:
  - `Host`: Rewrite to upstream backend host (`daily-cloudcode-pa.googleapis.com`).
  - `Authorization`: Replace with `Bearer <access_token>` of selected account.
  - `User-Agent`: Pass client User-Agent if prefixed with `antigravity/`; otherwise inject default `antigravity/cli/...`.
  - `Accept-Encoding`: Set to `"identity"` (prevent unexpected upstream gzip framing on relayed streams).

---

## 16. No-Replay Transport Reliability Contract

### Strict At-Most-Once Generation Safety `[FROZEN]`

This is a non-negotiable safety invariant. Upstream generation requests (`*generateContent*`) consume AI compute and may mutate upstream session state.

```
                  Upstream Request Dispatched
                              │
             ┌────────────────┴────────────────┐
             ▼                                 ▼
   Explicit HTTP Response             Ambiguous Transport Failure
   (429 / 403 / 401 received)        (Timeout / EOF / Broken Pipe / SSL)
             │                                 │
             ▼                                 ▼
   FAILOVER PERMITTED                 STRICT NO-REPLAY
   - Record error on account          - DO NOT try next account
   - Switch to next candidate         - Return 504 Gateway Timeout (if timeout)
   - Re-dispatch to next account      - Return 502 Bad Gateway (other errors)
                                      - Terminate downstream client call
```

### Specific Transport Failure Behaviors `[FROZEN]`

1. **Connection Timeout / Read Timeout**: Return `504 Gateway Timeout` immediately. **DO NOT REPLAY**.
2. **`RemoteDisconnected` / Upstream EOF**: Return `502 Bad Gateway` immediately. **DO NOT REPLAY**.
3. **`IncompleteRead` / Truncated Stream**: Return `502 Bad Gateway` immediately. **DO NOT REPLAY**.
4. **TLS / SSL Transport Errors**: Return `502 Bad Gateway` immediately. **DO NOT REPLAY**.
5. **Started SSE Stream**: Once downstream headers are committed, any subsequent error truncates the stream with log entry `[STREAM TRUNCATED]`. **NEVER REPLAY**.

> [!CAUTION]
> Remote branch commit `ab2baae` attempted broad replay on `ssl.SSLError`. That behavior was **REJECTED** in Python `v0.1.0-beta.2` because transport drops can occur *after* the upstream server has already committed generation tokens. Go MUST preserve Python beta.2's strict no-replay policy.

---

## 17. Streaming & Server-Sent Events (SSE) Contract

### SSE Relay Mechanics `[FROZEN]`

- **Detection**: Request is SSE if path contains `generatecontent` or `stream`, or `Accept: text/event-stream`.
- **Response Commitment**: Once upstream headers arrive, send downstream headers:
  - `Transfer-Encoding: chunked`
  - `Connection: close`
  - Upstream content type and non-hop-by-hop headers.
- **Relay Loop**:
  - Buffer chunks (up to 4096 bytes).
  - Write chunk size in hex + `\r\n` + chunk payload + `\r\n`.
  - Call `Flush()` on response writer after every chunk.
- **Completion**: Write terminating zero chunk (`0\r\n\r\n`) and flush.
- **Client Disconnect**: Handle broken pipe / reset gracefully without crashing the server.

---

## 18. Request & Generation Counting Contract

### Invariant Counters `[FROZEN]`

Under `pool_transaction` on upstream success:

- `request_count`: Incremented by `1` on **EVERY** successful upstream request (generation, quota probe, model listing).
- `gen_count`: Incremented by `1` **ONLY** when `generatecontent` is present in the request path.
- `last_used_at`: Updated to `int(time.time())` **ONLY** on successful generation.
- Auxiliary metadata requests (e.g. `v1internal:retrieveUserQuotaSummary`) MUST NOT increment `gen_count` or update `last_used_at`.

---

## 19. Live-Test Traffic Classification Contract

### Traffic Classes `[COMPATIBILITY REQUIRED]`

The live test parser classifies session traffic into four categories:

1. **`CLEAN`**: Exactly one generation request dispatched and succeeded on the expected candidate account.
2. **`INTERNAL_MULTIDISPATCH`**: One user-level `agy` CLI command triggered multiple internal generation dispatches (e.g. agent thought step + response synthesis) through the gateway within a single session invocation.
3. **`INCONCLUSIVE`**: Upstream API latency or external quota resets obscured the delta during verification.
4. **`CONCURRENT`**: Genuinely unrelated external clients sent traffic during the test window.

> [!NOTE]
> Go test harnesses MUST recognize that native Antigravity CLI invocations frequently execute multiple internal generation calls for a single prompt. This is expected behavior and must not be flagged as a concurrency defect.

---

## 20. Daemon Lifecycle & Process Management Contract

### PID File Schema `[COMPATIBILITY REQUIRED]`

The PID file `~/.gemini/agy-pool.pid` contains a JSON object:

```json
{
  "pid": 625954,
  "version": "0.1.0-beta.2",
  "script_mtime": 1789558575
}
```

Legacy format (single integer PID string) must also be accepted on read for backwards compatibility.

### Process Management Invariants `[FROZEN]`

- **Stale PID Detection**:
  1. Read PID from file.
  2. Send signal 0 (`kill -0 <pid>`). If process does not exist, PID is stale.
  3. Inspect `/proc/<pid>/cmdline` (on Linux). If command does not contain `agy-pool`, PID is stale.
- **Port Checking**: TCP connect probe to `127.0.0.1:<port>` with 300ms timeout.
- **Signal Handling**:
  - `SIGTERM` / `SIGINT`: Gracefully stop HTTP server, close listening socket, unlink PID file if matching own PID, exit 0.
- **Outdated Code Detection**:
  Compare binary mtime or version against running PID metadata. Trigger hot-reload when outdated.

---

## 21. Log Management & Rotation Contract

### Logging Specifications `[COMPATIBILITY REQUIRED]`

- **Path**: `~/.gemini/agy-pool.log` (mode `0600`).
- **Policy**: Size-based copytruncate rotation under lock `~/.gemini/agy-pool.log.lock`.
- **Max Bytes**: Default 5 MB (`5 * 1024 * 1024`), configurable via `AGY_LOG_MAX_BYTES`.
- **Backups**: Default 1, configurable via `AGY_LOG_BACKUP_COUNT`.
- **Parser-Sensitive Log Tokens**:
  Automation and tests rely on the exact presence of these tokens in `agy-pool.log`:
  - `[PROXY]` - Successful request forwarding
  - `[FAILOVER]` - Account failover notification
  - `[PROXY ERROR]` - Upstream HTTP error
  - `[PROXY WARN]` - Recoverable warning
  - `[PROXY EXCEPTION]` - Transport exception
  - `[STREAM TRUNCATED]` - Mid-stream truncation
  - `[ALL EXHAUSTED]` - Complete pool exhaustion

---

## 22. System Doctor & Diagnostics Contract

### Diagnostic Checks `[COMPATIBILITY REQUIRED]`

`agy-pool doctor` executes 8 diagnostic checks:

1. **Runtime Environment**: Go Runtime version (Go 1.22+ required).
2. **POSIX Concurrency**: Verify OS file locking (`flock`) capability.
3. **Native Binary**: Locate real `agy` executable in PATH or standard candidate directories.
4. **Gateway Daemon**: Probe PID, port listening status, and code currency.
5. **Account Pool Health**: Verify file presence, `0600` permissions, and account counts (Ready, Cooldown, Exhausted, Restricted).
6. **Upstream TLS Connectivity**: Perform TCP+TLS handshake to `daily-cloudcode-pa.googleapis.com:443`.
7. **Session Database**: Query `~/.gemini/antigravity-cli/conversation_summaries.db` in read-only mode.
8. **Gateway Log**: Check active log file size and rotation limit.

> [!NOTE]
> **MIGRATION DECISION**: The Python version prints `Python Runtime: 3.11.x`. The Go version will print `Go Runtime: go1.x.y`. This wording change is explicitly approved for Go v0.1.0-alpha.1.

---

## 23. Native Binary Discovery & Anti-Recursion Contract

### Search Order `[FROZEN]`

To locate the real underlying `agy` binary:
1. `$AGY_BIN` environment variable (if set and exists).
2. `$PREFIX/bin/agy` (Termux).
3. `/data/data/com.termux/files/usr/bin/agy`.
4. `/usr/local/bin/agy`.
5. `/usr/bin/agy`.
6. `~/.local/bin/agy`.
7. System `PATH` search (`exec.LookPath`).

### Anti-Recursion Guard `[FROZEN]`

To prevent infinite loops where the wrapper executes itself:
- Resolve canonical path (`filepath.EvalSymlinks`) of candidate binaries.
- If the resolved candidate matches the current executable, entrypoint script, or `os.Args[0]`, discard it and continue searching.

---

## 24. Command-Line Interface (CLI) Specification

### Subcommand Matrix `[COMPATIBILITY REQUIRED]`

| Command | Aliases | Arguments | Exit Code | Network Used | Description |
|---|---|---|---|---|---|
| `--version` | `version` | None | 0 | No | Print version string |
| `list` | `ls` | `[target]` | 0 | No | Display compact account inventory table without forced quota refresh |
| `quota` | None | `[target]` | 0 | No | Display per-account visual quota cards, progress bars, and reset countdowns |
| `status` | None | None | 0 | No | Show gateway process, configuration, and runtime status (compact, local-only, no quota bars) |
| `doctor` | `check`, `health` | None | 0 (warn), 1 (fail) | Yes (TLS check) | Run system health diagnostics |
| `strategy` | `strat` | `[name]` | 0 (ok), 1 (invalid) | No | View or set load balancing strategy |
| `rename` | None | `<target> <name>` | 0 (ok), 1 (err) | No | Update friendly display name |
| `remove` | `rm` | `<target>` | 0 (ok), 1 (err) | No | Delete account from pool |
| `switch` | None | `[target]` | 0 (ok), 1 (err) | No | Manually set active account |
| `verify` | None | `[target]` | 0 | No | Display Google security verification URL |
| `login` | `add` | None | 0 (ok), 1 (err) | Yes | Add new Google account via OAuth |
| `import-current`| None | None | 0 (ok), 1 (err) | No | Import existing token file |
| `export` | `backup` | `[file] [-e] [-p pass]` | 0 (ok), 1 (err) | No | Export pool backup |
| `import` | `restore` | `<file> [--replace]` | 0 (ok), 1 (err) | No | Import pool backup |
| `start` | None | None | 0 (ok), 1 (err) | No | Start background gateway daemon |
| `stop` | None | None | 0 | No | Stop background gateway daemon |
| `restart` | None | None | 0 (ok), 1 (err) | No | Restart background gateway daemon |
| `logs` | `log` | `[-n] [-f] [--clear] [--rotate]`| 0 | No | View, tail, clear, or rotate logs |
| `run` | None | `[args...]` | Propagated | Indirect | Wrap and execute native `agy` |

> **Note on CLI Command Separation & Runtime Statistics**: In Go `agy-pool-go`, command responsibilities between `status`, `list`, `quota`, and `doctor` are intentionally decoupled from legacy Python behavior:
> - `status`: Compact gateway and runtime summary only. Reads lightweight runtime snapshot metadata (daemon uptime, goroutine count, Alloc/Heap/Sys memory, GC cycles, Go version) recorded by the running daemon process (without calling CLI runtime stats), and pool health summary without progress bars.
> - `list` (`ls`): Fast tabular account inventory (ID, friendly name, status, hits, cached 5H/weekly quota, last used) without network calls and strictly protecting user privacy (never printing email addresses).
> - `quota`: Dedicated full quota dashboard with 68-column cards, progress bars, percentages, and reset countdowns.
> - `doctor`: Comprehensive diagnostic suite covering locks, TLS, binary integrity, and database health.

---

## 25. "agy" CLI Wrapper Contract

### Execution Flow `[FROZEN]`

When the user runs `agy` (via alias `alias agy='agy-pool run'`):
1. **Resolve Conversation Continuity**: Intercept `-c` / `--continue` and resolve to `--conversation <cid>` if applicable.
2. **Ensure Gateway Running**: Check if daemon is active. If stopped or running outdated code, start/reload it.
3. **Set Environment**: Set `CLOUD_CODE_URL=http://127.0.0.1:8899`.
4. **Sync Token File**: Sync active account credentials into `~/.gemini/antigravity-cli/antigravity-oauth-token` for CLI bootstrap compatibility.
5. **Locate Native Executable**: Discover real `agy` via anti-recursion resolver.
6. **Process Replacement**: Execute native `agy` replacing current process (`syscall.Exec`) with forwarded arguments and environment.

---

## 26. Conversation Continuity & Session Resume Contract

### SQLite Continuity Resolution `[FROZEN]`

When resolving `agy -c`:
1. **Open Session Database**: Open `~/.gemini/antigravity-cli/conversation_summaries.db` in strict read-only mode:
   `PRAGMA query_only = ON; PRAGMA busy_timeout = 2000;`
2. **Scan Conversations**:
   Query `conversation_id, title, workspace_uris, last_modified_time` ordered by `last_modified_time DESC`.
3. **Hierarchical Path Matching**:
   Resolve current working directory realpath. Walk upward from current directory to root, searching for the first conversation whose `workspace_uris` matches the path.
4. **Presence Lock Inspection**:
   Inspect `~/.gemini/antigravity-cli/presence/<cid>.lock`. Test non-blocking `flock(LOCK_EX | LOCK_NB)`. If held by another process, log a warning notice.
5. **Argument Replacement**:
   Replace `-c` / `--continue` with `--conversation <cid>`. If user passed explicit `--conversation`, pass through untouched.

---

## 27. Installer & Upgrade Lifecycle Contract

### Installer Contract `[COMPATIBILITY REQUIRED]`

- **Installation Directory Precedence**:
  1. `$PREFIX/bin` (Termux)
  2. `/usr/local/bin` (if writable)
  3. `$HOME/.local/bin`
- **Installed Artifacts**:
  - `agy-pool` -> main binary or symlink
  - `agy-raw` -> direct native launcher script
  - `agy-orig` -> alias symlink to `agy-raw`
- **Shell Profiles**: Add alias marker block to `~/.bashrc` and `~/.zshrc`:
  ```bash
  # >>> agy-pool integration >>>
  alias agy='agy-pool run'
  alias agy-orig='agy-raw'
  # <<< agy-pool integration <<<
  ```
- **State Preservation**: Reinstall or uninstall MUST NEVER delete `~/.gemini/agy-pool-accounts.json`.

> [!NOTE]
> **FUTURE DECISION**: Whether Go installs as a single standalone static binary in `$TARGET_DIR` or continues using symlinks will be decided during Phase G3 packaging.

---

## 28. Exit-Code & Error Handling Contract

### Exit Codes `[FROZEN]`

- `0`: Success / Normal completion / Non-fatal doctor warnings.
- `1`: User error, invalid argument, unknown command, fatal doctor check, corrupt storage state, daemon launch failure.
- `130`: Interrupted via `SIGINT` (Ctrl+C).

---

## 29. Golden Fixture Plan

### Directory Structure & Test Fixtures `[FROZEN]`

To guarantee parity, both Python and Go will execute against shared JSON fixtures:

```text
testdata/
├── pool/
│   ├── empty.json                  # Blank initial pool structure
│   ├── valid_3_accounts.json       # Standard 3-account baseline
│   ├── legacy_alpha9.json          # Combined fraction without windows
│   └── corrupt_syntax.json         # Truncated JSON to test abort guard
│
├── quota/
│   ├── normal_dual_window.json     # Standard 5h + weekly response
│   ├── single_window_5h.json       # 5h only (weekly preserved from cache)
│   ├── exhausted_5h.json           # 0.0% fraction with reset time
│   └── retry_after_variants.json   # Integer, RFC 2822, and RFC 7231 headers
│
├── scheduler/
│   ├── max_quota_vectors.json      # Inputs and expected rankings
│   ├── least_used_tie_breaks.json  # Hits + pace tie-break vectors
│   └── round_robin_rotations.json  # Multi-cycle reservation sequences
│
├── proxy/
│   ├── normal_generation.json      # Request/response transcript
│   ├── 429_failover_flow.json      # Account 1 429 -> Account 2 200
│   ├── sse_stream_chunks.json      # Chunked framing validation
│   └── ambiguous_timeout.json      # 504 no-replay assertion
│
└── cli/
    ├── list_table_golden.txt       # Aligned Unicode box table output
    └── doctor_healthy_golden.txt   # Clean doctor output
```

---

## 30. Differential Testing Strategy & Dual-Runner Harness

### Dual-Runner Architecture `[FROZEN]`

A differential test script (`scripts/dual-test.sh`) executes identical operations against both implementations and verifies state equivalence:

```
                      Test Scenario Input
                               │
               ┌───────────────┴───────────────┐
               ▼                               ▼
      agy-pool-python                    agy-pool-go
     (State: /tmp/py)                  (State: /tmp/go)
               │                               │
               ▼                               ▼
      Python Result State               Go Result State
               │                               │
               └───────────────┬───────────────┘
                               ▼
                    State Differential Normalizer
                    - Mask PID
                    - Mask mtime / timestamps
                    - Sort JSON object keys
                               ▼
                     diff -u py.json go.json
                               ▼
                     ASSERT IDENTICAL (0 diff)
```

---

## 31. Implementation Phase G1: Pure Domain Parity

**Objective**: Implement core non-network packages in Go.

### Scope:
1. `internal/config`: Path resolution, test mode, fail-closed guards.
2. `internal/storage`: Atomic JSON persistence, sidecar `.lock`, crash resilience.
3. `internal/quota`: Quota normalization, reset time parsing, capacity state calculations.
4. `internal/scheduler`: `max_quota`, `least_used`, `round_robin` candidate ranking and reservation.

### Exit Gate:
- 100% pass on `testdata/pool/`, `testdata/quota/`, and `testdata/scheduler/` golden fixtures.
- Storage crash-safety tests pass (aborted writes, corrupt state refusal).
- Zero production state access.

---

## 32. Implementation Phase G2: Runtime & Networking Parity

**Objective**: Implement networking, proxying, auth, and daemon lifecycle.

### Scope:
1. `internal/auth`: OAuth loopback handler, token refresh, single-flight locking.
2. `internal/accounts`: Account lifecycle, selectors, import/export, AES-GCM encryption.
3. `internal/proxy`: HTTP/1.1 proxy, SSE streaming, header rewrites, failover loop.
4. `internal/daemon`: Supervisor, PID file JSON management, log rotation.

### Exit Gate:
- Mocked upstream proxy tests verify failover on 429/403/401.
- Strict no-replay tests verify timeouts and transport errors return 504/502 without second dispatches.
- Daemon background launch, status, stop, and restart work reliably in an isolated state directory.

---

## 33. Implementation Phase G3: CLI, Diagnostics & Wrapper Parity

**Objective**: Implement CLI interface, user-facing tools, and launcher.

### Scope:
1. `internal/cli`: Argument parsing, table rendering, command dispatch.
2. `internal/diagnostics`: Full 8-point system doctor.
3. `internal/conversation`: SQLite session discovery and `-c` resolution.
4. `cmd/agy-pool`: Main entrypoint.

### Exit Gate:
- CLI golden test outputs match Python formatting and exit codes.
- `agy-pool doctor` passes on clean test environment.
- Session resume matches Python against simulated SQLite conversation databases.

---

## 34. Go Live Candidate Strategy

### Side-by-Side Validation Topology `[FROZEN]`

Once Phases G1–G3 pass offline tests:

```text
Production (Python):   http://127.0.0.1:8899  (State: ~/.gemini/agy-pool-accounts.json) [UNTOUCHED]
Candidate (Go):       http://127.0.0.1:8901  (State: /tmp/agy-candidate/accounts.json)
```

### Candidate Verification Checklist:
1. Initialize candidate state with a copied snapshot of real accounts.
2. Start Go daemon on candidate port `8901`.
3. Run `agy-pool-go doctor`.
4. Run candidate quota check.
5. Execute one live AI generation dispatch through candidate port `8901`.
6. Assert candidate max_quota prediction matched actual account dispatch.
7. Verify SSE streaming relayed properly without truncation.
8. Stop Go candidate daemon cleanly.
9. Assert production port `8899` remained completely undisturbed.

---

## 35. Shadow / Read-Only Compatibility Validation

Before Go is permitted to write production state, an optional read-only audit stage may run:
- Go reads the production accounts file in read-only mode (`O_RDONLY`).
- Decodes all 3 production accounts.
- Evaluates capacity scoring and verifies scheduler order matches Python.
- Performs zero writes.

---

## 36. Production Cutover Contract & Gate Checklist

### Mandatory Pre-Cutover Conditions `[FROZEN]`

The cutover replaces Python with Go on the production port `8899`. It MUST satisfy every condition:

- [ ] Go implementation passed Phases G1, G2, G3 offline suites.
- [ ] Go candidate passed live candidate verification on port `8901`.
- [ ] Full timestamped backup of `~/.gemini/agy-pool-accounts.json` verified (`0600` mode, hash recorded).
- [ ] Git repository committed and tagged.
- [ ] **CURRENT AGENT SESSION TERMINATED**: The cutover MUST NOT be executed from within an AI agent whose network route traverses port 8899.
- [ ] Cutover executed manually by a human operator in a standard shell.

### Cutover Execution Steps (Manual Shell):
```bash
# 1. Stop Python daemon
agy-pool stop

# 2. Switch binary symlink to Go executable
ln -sf /path/to/agy-pool-go /usr/local/bin/agy-pool

# 3. Start Go daemon on standard port 8899
agy-pool start

# 4. Verify system health
agy-pool doctor
agy-pool list

# 5. Run test generation
agy "Respond with PONG"

# 6. Verify session continuity
agy -c "Confirm session resumed"
```

---

## 37. Rollback Protocol & Disaster Recovery

### Rollback Procedure `[FROZEN]`

If any defect is detected during or immediately following cutover:

```bash
# 1. Stop Go daemon immediately
kill $(cat ~/.gemini/agy-pool.pid | grep -o '"pid":[0-9]*' | cut -d: -f2) 2>/dev/null || agy-pool stop

# 2. Re-point symlink to Python implementation
ln -sf /home/codex/agy-pool/bin/agy-pool ~/.local/bin/agy-pool
# (or /usr/local/bin/agy-pool)

# 3. Inspect production state
# If state was corrupted by Go, restore pre-cutover backup:
# cp -p ~/.gemini/agy-pool-accounts.json.<timestamp>.bak ~/.gemini/agy-pool-accounts.json
# chmod 600 ~/.gemini/agy-pool-accounts.json

# 4. Restart Python production daemon
agy-pool start

# 5. Validate recovery
agy-pool doctor
agy "Ping Python recovery"
```

---

## 38. Performance Objectives & Resource Budgets

Performance goals are secondary to correctness. Parity comes first, optimization second.

| Metric | Python Reference (Baseline) | Go Target Budget | Verification Method |
|---|---|---|---|
| **Idle Resident Memory (RSS)** | ~28 MB | `< 12 MB` | `ps -o rss -p <pid>` |
| **CLI Invocation Overhead** | ~120 ms | `< 15 ms` | `time agy-pool list` |
| **Proxy Request Latency Overhead** | ~1.5 ms | `< 0.2 ms` | Local benchmark |
| **Concurrent Request Capacity** | Limited by GIL / threads | Goroutine concurrency | Load test harness |
| **Binary Size** | N/A (Scripts) | `< 25 MB` (unstripped) | File size inspection |

---

## 39. Security Boundaries & Attack Surface Analysis

1. **File Permissions**: All state, pid, and log files MUST be created with `0600` permissions (`fchmod`). Directories must be `0700`.
2. **Credential Redaction**: `access_token`, `refresh_token`, and user emails must never appear in log files.
3. **Loopback Binding**: Gateway proxy and OAuth callback listeners MUST bind strictly to `127.0.0.1`. Never bind to `0.0.0.0`.
4. **Path Traversal Resistance**: SQLite database and file path operations must evaluate canonical symlinks and validate bounds.
5. **TLS Invariants**: Upstream connections must use system TLS certificates with standard verification. Never disable TLS verification (`InsecureSkipVerify: false`).
6. **Bounded Request Parsing**: HTTP proxy body readers must enforce maximum length limits to prevent denial-of-service memory exhaustion.

---

## 40. Explicit Non-Goals for Go Initial Release (`v0.1.0-alpha.1`)

The following features are explicitly **OUT OF SCOPE** for the initial Go rewrite:
- Adding new load balancing strategies beyond `max_quota`, `least_used`, and `round_robin`.
- Live in-flight token consumption estimation.
- Redesigning the terminal UI, progress bars, or color palettes.
- Re-architecting storage from JSON to SQLite or relational databases.
- Multi-node distributed clustering or cloud account synchronization.
- Modifying upstream transport failover rules (no broad network error replay).
- Plugin architectures or dynamic module loading.

---

## 41. Versioning & Tagging Scheme

- **Python Reference**: Remains tagged at `v0.1.0-beta.2` (`release commit c917659`).
- **Go Development Tree**:
  - Development builds: `v0.1.0-alpha.1`, `v0.1.0-alpha.2`.
  - Candidate for cutover: `v0.1.0-beta.3` or `v0.2.0-go.1`.
  - Python production tags must NEVER be overwritten or reused for unproven Go binaries.

---

## 42. Python Reference Implementation Freeze

Effective immediately upon acceptance of this contract:
1. **Freeze Active**: The Python implementation in `agy_pool/` is frozen as the reference standard.
2. **Permitted Changes**: Only critical security vulnerabilities, severe state-corruption bugs, or breaking upstream Google API changes may be patched in Python.
3. **Contract Synchronization**: If an emergency fix is made to Python, this migration contract and all shared golden fixtures MUST be updated and verified before continuing Go development.

---

## 43. Final Parity Acceptance Gates

Before Go replaces Python, it must achieve clean sign-off across all five gates:

| Gate | Scope | Acceptance Criteria |
|---|---|---|
| **Gate 1** | Golden Fixtures | 100% pass on all `testdata/` vectors across storage, quota, scheduler, proxy, and CLI. |
| **Gate 2** | Crash Durability | Zero corrupted states across 1,000 simulated process interruptions during storage transactions. |
| **Gate 3** | Transport Safety | Asserted zero re-dispatch on timeouts, EOF, broken pipes, and mid-stream SSE terminations. |
| **Gate 4** | Candidate Live Test | Side-by-side verification on port `8901` with real generation and quota prediction parity. |
| **Gate 5** | Operator Review | Human verification of independent manual cutover script and rollback readiness. |
