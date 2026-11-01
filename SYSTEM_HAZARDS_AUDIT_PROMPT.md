# System Hazards & Reliability Audit: Peer Review Prompt

You are acting as a Principal Systems & Backend Architect. Please review the following architectural reliability and hazard audit of **`agy-pool-go`** (`v0.1.0-beta.41`), a Go reverse proxy and multi-account gateway for Google Antigravity / Gemini CLI.

We performed a deep code inspection and live runtime verification of the proxy, scheduler, state storage, and error-handling subsystems. Below are five identified operational hazards, ranging from catastrophic pool deadlocks to resource leaks, along with empirical evidence and proposed remediations.

Please provide your critique, risk assessment, and concrete architectural recommendations for each item.

---

## 1. System Overview & Context

`agy-pool-go` routes requests from IDE agents (`localhost:8899`) to upstream Google Cloud / Gemini endpoints across multiple accounts:
* **Generation Traffic (~34%)**: Streaming and non-streaming inference calls (`v1internal:streamGenerateContent`, `generateContent`).
* **Non-Generation Traffic (~66%)**: Metadata, session discovery, and quota summary calls.
* **Key Invariants**:
  1. **Zero Silent Replay**: In-flight requests must never be unsafely retried if upstream may have partially executed or consumed quota.
  2. **Standard Library First**: Minimal external dependencies; native platform mechanisms preferred.
  3. **High Availability**: A localized failure on one account should degrade or failover gracefully without disrupting the entire pool.

---

## 2. Identified Operational Hazards & Bottlenecks

### [P0] Model Cooldown Deadlock & Full Pool Paralyzation
* **Code Reference**:
  * [`internal/proxy/proxy.go:1038-1046`](file:///home/codex/work/agy-pool-go/internal/proxy/proxy.go#L1038-L1046)
  * [`internal/proxy/model_cooldown.go:16-25, 97-120, 192-211`](file:///home/codex/work/agy-pool-go/internal/proxy/model_cooldown.go#L16-L25)
* **Mechanism**:
  1. **Hypersensitive Trigger**: If upstream returns HTTP 503 or `MODEL_CAPACITY_EXHAUSTED`, or if an account answers with Time-To-First-Byte (TTFB) $> 30\text{s}$ twice in a row (`modelSlowTTFB = 30 * time.Second`), the proxy pauses that account for that model.
  2. **Disproportionate Duration**: The pause begins at **1 hour minimum** (`modelCooldownMin = time.Hour`) and doubles up to 24 hours.
  3. **Hard Exclusion (Zero Fallback)**: Unlike account-level quota cooldowns (`RateLimitedUntil`), which the scheduler retains at the bottom of the candidate list as a last resort, `ExcludeModelCooldowns` unconditionally drops all cooling accounts.
* **The Failure Mechanism**:
  * Modern reasoning models (e.g., `gemini-2.5-pro` with thinking mode enabled) routinely take 30–50 seconds of thinking time before emitting the first token. Two complex prompts in a row on the same account will flag it as "stalled" and pause it for 1 hour.
  * Alternatively, a transient 10-second upstream 503 spike across accounts causes all accounts in the pool to enter model cooldown.
  * Once all accounts are paused, `candidates` becomes empty. The proxy immediately fails fast with:
    ```
    All accounts in pool exhausted or unavailable: 5 account(s) paused for gemini-2.5-pro after 503/capacity errors
    ```
  * **Persistent Deadlock**:
    - The pause map is serialized to SQLite `state.db` under `runtime_global` (`key = 'model_pauses'`), so **restarting the daemon does not restore service**.
    - An account can only clear its cooldown (`ClearModelCooldown`) by successfully answering a request fast, but because it is hard-excluded from scheduling, **it is never selected to serve a request**.
    - There is currently no CLI command (e.g. `agy-pool cooldown clear`) to manually unpause models. The entire pool remains dead for that model for 1 to 24 hours.
* **Empirical Verification**:
  Inspecting the live `state.db` revealed an active pause persisted on disk:
  ```json
  {"acc_3\u0000gemini-3.8-flash-medium":{"until":"2026-10-24T02:11:14.819191041-07:00","backoff":3600000000000}}
  ```
* **Proposed Remedies**:
  1. **Fallback as Last Resort**: If `ExcludeModelCooldowns` drops all candidates, return the cooled candidates sorted by soonest cooldown expiration instead of an empty slice.
  2. **Adaptive TTFB Threshold**: Increase `modelSlowTTFB` from 30s to 90s or 120s (or disable TTFB-based pausing for reasoning models where deep thinking is expected).
  3. **Shorter Initial Backoff**: For transient upstream 503s, start backoff at 30s or 60s instead of a punitive 1 hour.
  4. **CLI Management**: Expose `agy-pool cooldown [list|reset]` to inspect and clear model pauses.

---

### [P1] Log Rotation Dead Code & Unbounded Disk Expansion
* **Code Reference**:
  * [`internal/proxy/proxy.go:35-36, 350-354, 570`](file:///home/codex/work/agy-pool-go/internal/proxy/proxy.go#L35-L36)
  * [`internal/daemon/log.go:36-75, 149-163`](file:///home/codex/work/agy-pool-go/internal/daemon/log.go#L36-L75)
  * [`internal/quota/history.go:21-47`](file:///home/codex/work/agy-pool-go/internal/quota/history.go#L21-L47)
* **Mechanism**:
  * `internal/daemon/log.go` implements an effective copytruncate log rotation mechanism (`RotateLogIfNeeded`, `DefaultMaxLogBytes = 5 MB`).
  * On every proxy request, `maybeRotateLog()` is called in `internal/proxy/proxy.go:570`:
    ```go
    func maybeRotateLog() {
        if LogRotator != nil {
            LogRotator()
        }
    }
    ```
  * **The Problem**: `LogRotator` is a package-level function pointer initialized to `nil`. In the entire repository, **`LogRotator` is never assigned a value** in either `cmd/agy-pool/main.go` or `internal/daemon/server.go`.
* **Empirical Verification**:
  * The production log file [`~/.local/share/agy-pool/agy-pool.log`](file:///home/codex/.local/share/agy-pool/agy-pool.log) has already swollen to **6.5 MB**, exceeding the 5 MB limit without ever rotating.
  * Furthermore, [`quota-history.jsonl`](file:///home/codex/.local/share/agy-pool/quota-history.go#L21) explicitly notes in code comments:
    ```go
    // shortcut: the file grows unbounded (~250 KB/day per busy account); add rotation if kept for months.
    ```
    This file has grown to **1.9 MB** and has no rotation or pruning logic.
* **Proposed Remedies**:
  1. Wire `proxy.LogRotator = func() { daemon.MaybeRotateLog() }` during daemon initialization.
  2. Implement bounded retention (e.g., sliding window or 5 MB cap) for `quota-history.jsonl`.

---

### [P1] Overly Eager Transport Error Termination (Lost Failover on Pre-Send Network Glitches)
* **Code Reference**:
  * [`internal/proxy/proxy.go:986-1014`](file:///home/codex/work/agy-pool-go/internal/proxy/proxy.go#L986-L1014)
* **Mechanism**:
  * When `client.Do(upstreamReq)` returns an error (`err != nil`), the proxy immediately classifies it as `AmbiguousTransportFailure`:
    ```go
    return DispatchOutcome{
        Action: ActionTerminal,
        Reason: "AmbiguousTransportFailure",
        Err:    ErrAmbiguousTransport,
    }
    ```
  * In `HandleProxy`, any outcome other than `ActionFailoverNext` immediately terminates the attempt loop and returns a 502/504 Bad Gateway downstream.
* **The Problem**:
  * While preventing replay after request bytes have been sent is a sound principle, many transport errors occur **strictly prior to transmitting any bytes**:
    - DNS resolution failure on one interface/DNS server
    - TCP dial timeout or connection refused
    - TLS handshake timeout
    - Local network/VPN route change
  * Even more critically, for **non-generation requests** (e.g. `retrieveUserQuotaSummary`, `fetchAvailableModels`), calls are completely idempotent and safe to retry on the next account regardless of when the error occurred.
  * Treating every pre-send error as `ActionTerminal` turns transient localized socket hiccups on one account into user-facing failures, completely bypassing the pool's remaining healthy accounts.
* **Proposed Remedies**:
  1. Distinguish pre-send transport errors: If `net.OpError` occurred during `dial` or `tls.Handshake`, failover to `ActionFailoverNext`.
  2. For `!isGeneration` requests, always allow `ActionFailoverNext` on network failure.

---

### [P2] Indiscriminate Downstream `Connection: close` Causing Local Socket Churn & TIME_WAIT Exhaustion
* **Code Reference**:
  * [`internal/proxy/proxy.go:533`](file:///home/codex/work/agy-pool-go/internal/proxy/proxy.go#L533) (`SendBuffered`)
  * [`internal/proxy/stream.go:23`](file:///home/codex/work/agy-pool-go/internal/proxy/stream.go#L23) (`SendStream`)
* **Mechanism**:
  * On every response written to the client, the proxy injects:
    ```go
    committer.Header().Set("Connection", "close")
    ```
* **The Problem**:
  * Downstream clients (Antigravity CLI, IDE language servers, diagnostic monitors) frequently issue rapid bursts of metadata and health checks.
  * Unconditionally closing connections forces a new TCP 3-way handshake on every single request.
  * **Empirical Verification**:
    Running `ss -tan 'sport = :8899'` reveals dozens of loopback sockets permanently lingering in `TIME-WAIT`. Under concurrent agent tasks (e.g. multi-subagent workflows), this generates unnecessary ephemeral socket recycling overhead and CPU churn.
* **Proposed Remedies**:
  * Allow HTTP/1.1 persistent connections (`Keep-Alive`) for buffered downstream responses. Only close when the downstream client explicitly sends `Connection: close` or during graceful shutdown.

---

### [P2] Missing HTTP Server Header Read Timeout in Daemon
* **Code Reference**:
  * [`internal/daemon/server.go:205-207`](file:///home/codex/work/agy-pool-go/internal/daemon/server.go#L205-L207)
* **Mechanism**:
  ```go
  server := &http.Server{
      Handler: handler,
  }
  ```
* **The Problem**:
  * `http.Server` is initialized with default timeouts (all 0 / indefinite).
  * While `ReadTimeout` must remain unset to support long-lived Server-Sent Events (SSE) streaming, Go's security best practices require `ReadHeaderTimeout` to be set explicitly.
  * Any local process or hung client connecting to `127.0.0.1:8899` that writes partial headers will cause the handler goroutine and socket to block indefinitely, creating a slow-loris leak.
* **Proposed Remedies**:
  * Add `ReadHeaderTimeout: 5 * time.Second` to `http.Server`.

---

## 3. Specific Questions for Peer Review

1. **Model Cooldown Fallback Strategy**:
   When all accounts in the pool are marked as cooled down for a specific model, what is the cleanest fallback pattern that preserves load balancing while avoiding hard outages?
2. **Pre-Transmission Error Classification**:
   What is the idiomatic Go approach for inspecting `net/http` errors to safely distinguish between *“bytes might have been sent (unsafe to replay)”* vs *“connection failed before byte 0 (safe to failover)”*?
3. **Log & Telemetry Compaction**:
   Should `quota-history.jsonl` be moved to a rolling SQLite table with an automatic TTL trigger (e.g. keeping the last 7 days of entries), or is a flat-file rotation sufficient?
