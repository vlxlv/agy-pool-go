# Architectural Peer Review: Evaluating a Rust Rewrite for `agy-pool`

You are acting as a **Principal Distributed Systems & Performance Architect**. Please review the feasibility, architectural design, ROI, and engineering trade-offs of rewriting **`agy-pool`** from Go to Rust.

We currently operate `agy-pool-go`, a local/remote reverse proxy gateway and multi-account quota balancer for Google Cloud Code / Antigravity / Gemini CLI. While Go has served us well, the team is evaluating whether a ground-up rewrite in modern Rust—leveraging best-of-breed community crates—is commercially and architecturally justified.

Below is the complete architectural context, operational telemetry, proposed Rust stack, and key technical challenges. Please provide a rigorous, candid evaluation.

---

## 1. System Context & Workload Profile

`agy-pool` sits between local developer tooling (IDEs, CLI agents, subagent swarms) on `localhost:8899` and upstream Google endpoints (`daily-cloudcode-pa.googleapis.com`):

* **Workload Characteristics**:
  * **99.9% I/O Bound**: Relaying HTTP/1.1 and Server-Sent Events (SSE) `streamGenerateContent`.
  * **Upstream Latency Dominance**: Upstream Time-To-First-Byte (TTFB) ranges from **2.5s to 15s** (LLM inference & reasoning). Local gateway overhead in Go is already **< 0.5 ms**.
  * **Production Footprint (Measured on 5-account Linux node)**:
    * Memory RSS: **~20 MB** (live gateway, v0.1.1-alpha.2).
    * CPU Utilization: **~0.1%** average.
    * Concurrency: Typically 5–20 concurrent in-flight streams, peaking at ~50.
* **Core Responsibilities**:
  1. **Dual-Horizon Quota Scheduling**: Managing Google's dual windows: 5-hour rolling burst window vs 7-day weekly tier budget ($C_{weekly} = 3.5 \times C_{5h}$). Accounts must balance burst utilization without premature weekly lockout.
  2. **Session Affinity & Prompt Cache Optimization**: Binding multi-turn conversations (`request.labels.cascade_id`) to the same upstream account with automatic spillover when the bound account drops into low quota.
  3. **Strict No-Replay Transport Reliability**: Absolute invariant—generation requests (`*generateContent*`) must NEVER be retried if any request bytes have left the host socket, preventing duplicate token spend and mutated server state.
  4. **State Persistence & Leases**: SQLite in WAL mode managing account credentials, in-flight process reservations, and cooldown timers.
  5. **Background OAuth & Quota Refresh**: Single-flight background token refresh and quota polling without stalling active generations.

---

## 2. Proposed Rust Architecture (Leveraging Modern Community Crates)

Rather than hand-rolling low-level networking primitives, the proposed rewrite relies on battle-tested ecosystem crates to minimize boilerplate:

| Component | Go Implementation (Current) | Proposed Rust Crate Stack | Rationale |
| :--- | :--- | :--- | :--- |
| **HTTP Proxy & SSE Streaming** | Custom `net/http` reverse proxy, manual `http.Flusher` loop, chunked framing | **`axum` + `tower` + `reqwest`** | Stream piping via `Body::from_stream(upstream_res.bytes_stream())`. Native backpressure and automatic client disconnect cancellation. |
| **Persistence (SQLite)** | `modernc.org/sqlite` (pure Go; releases already build with `CGO_ENABLED=0` as static binaries for every OS/arch) | **`sqlx` (sqlite, bundled)** or **`tokio-rusqlite`** | Native async connection pooling. (No CGO or static-linking gain: Go already has both.) |
| **Session Affinity & TTL Cache** | Mutex-guarded TTL map (not an LRU): sweeps expired bindings at a size threshold, evicts the soonest-expiring binding when full, persists bindings across restarts, and applies quota-health / load / spill rules (`internal/proxy/affinity.go`, ~450 lines incl. persistence and rules) | **`moka`** (async future cache) | Replaces only the TTL/eviction part; persistence and health rules remain hand-written. |
| **Concurrency & In-Flight Tracking**| In-memory atomics + SQLite file reservation leases | **`dashmap`** + **`portable-atomic`** | Sharded, lock-free concurrent maps avoiding global lock contention across async tasks. |
| **CLI & Configuration** | `flag` package + custom validation | **`clap` (derive)** + **`serde`** | Strict, compile-time validated CLI flags, environment overrides, and schema parsing. |
| **Structured Observability** | Custom leveled logger writing to disk | **`tracing`** + **`tracing-subscriber`** + **`tracing-appender`** | Zero-allocation structured spans, non-blocking asynchronous file logging with rotation. |

---

## 3. Key Architectural Questions for Evaluation

Please evaluate our proposed rewrite across the following 5 dimensions:

### Q1: Real-World ROI & Practical Necessity
Given that the Go implementation consumes only **~20 MB RSS**, **0.1% CPU**, and introduces **< 0.5 ms** local latency on an upstream request taking 5,000 ms, is a Rust rewrite justifiable from a performance or cost standpoint?
* What concrete operational gains (if any) can actually be realized?
* Does dropping memory from ~20 MB to ~8 MB justify the migration cost, or is this a classic case of premature optimization?

### Q2: Correctness & Elegance of the Proposed Crate Stack
Critique the proposed stack (`axum` + `reqwest` + `sqlx` + `moka`):
* **Streaming Caveats**: Are there subtle pitfalls when streaming SSE through `reqwest::Response::bytes_stream()` into `axum::body::Body::from_stream()`? Specifically regarding:
  * Chunk framing / boundary preservation.
  * Ensuring the upstream connection is immediately severed if the downstream IDE client disconnects early.
  * Preventing silent buffering or gzip re-compression.
* **TLS Engine**: Should we standardize on `rustls` (pure Rust, static musl friendly) or `native-tls` (OpenSSL) when speaking to Google PA endpoints (`daily-cloudcode-pa.googleapis.com`)?

### Q3: Async SQLite Impedance Mismatch in Rust
SQLite is synchronous file I/O.
* Between **`sqlx`** (worker pool async abstraction) and **`tokio-rusqlite`** (dedicated actor thread via channels), which pattern is better suited for a high-concurrency proxy that performs short read-modify-write transactions on token leases?
* Is SQLite even the right choice for in-flight reservation tracking, or should runtime lease tracking be purely in-memory (`dashmap` / `atomic`), using SQLite only for cold credential persistence?

### Q4: Type-Safe Enforcement of the "Strict No-Replay" Invariant
The Go code already enforces "At-Most-Once" structurally, without classifying error types: all proxied traffic goes through one `client.Do` call site (`dispatchAccount`); `GetBody` is nil, idempotency headers are stripped and generation requests use `Close=true`, so `net/http` cannot auto-retry; a transport error fails over only if `httptrace.GotConn` never fired (no connection, so no request bytes), otherwise it is terminal; every outcome is terminal unless it is explicitly `ActionFailoverNext`, and the outer loop stops once the downstream `ResponseCommitter` is committed. These are runtime checks plus tests, not compile-time guarantees.
* What would Rust's type system add over this runtime design? How can it (e.g., **Typestate Pattern**, **Linear Types**, or **RAII Guards**) be leveraged to make accidental replays unrepresentable at compile time, and is that worth it given the above?
* Can we structurally enforce that an HTTP dispatch future can only transition into a retryable state *before* the first byte is committed to the wire?

### Q5: Staged Migration vs Long-Term Maintenance
If we decide to proceed, what is the cleanest, lowest-risk migration roadmap?
* Would you recommend a hybrid approach (e.g., compiling the core scheduler as a Rust cdylib/cgo library called by Go), or a complete clean-room rewrite?
* What are the primary maintenance liabilities a Rust codebase would introduce for an engineering team that predominantly writes Go and Python?

---

## 4. Desired Output Format

Please provide:
1. **Executive Verdict**: (Strongly Recommend / Feasible but Unnecessary / Not Recommended) with top 3 driving reasons.
2. **Deep-Dive Responses to Q1 – Q5**: Including concrete code snippets, architectural trade-offs, and crate recommendations.
3. **Failure Modes & Pitfalls**: Specific edge cases where an async Rust proxy might regress compared to Go's runtime model.
