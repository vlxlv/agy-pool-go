# Architectural Performance Audit & Peer Review Prompt

You are acting as a Principal Systems & Backend Architect. Please review the following architectural performance audit of **`agy-pool-go`**, a high-performance Go reverse proxy and multi-account gateway for Google Antigravity / Gemini CLI.

Read the system architecture, empirical telemetry, identified bottlenecks, and proposed mitigations below, then provide your independent critique, risk analysis, and prioritization advice.

---

## 1. System Overview & Context

`agy-pool-go` is an in-process and daemon reverse proxy (`localhost:8899`) that intercepts calls from coding agents/IDEs and routes them across a pool of developer accounts.

* **Traffic Breakdown** (from 60,000+ audited production log entries):
  * **Non-Generation Calls (~66%)**: `listExperiments`, `loadCodeAssist`, `fetchUserInfo`, `retrieveUserQuotaSummary`. These are idempotent read-only metadata calls.
  * **Generation Calls (~34%)**: `v1internal:streamGenerateContent`, `generateContent`. These are heavy, long-running streaming inference calls (TTFB 1s~5s, stream duration 2s~40s).
* **Key Constraints**:
  * **Standard Library First** (enforced in `docs/MIGRATION_CONTRACT.md`): Keep dependencies minimal, zero bloat, platform-native.
  * **Zero-Replay / Quota Protection**: Inference calls must never be silently replayed by HTTP transports (to avoid burning user quota or corrupting state).
  * **Session Affinity**: Requests belonging to the same conversation (`cascade_id` / `session_id`) must remain pinned to the same account for 5 minutes (`DefaultAffinityTTL`) to maximize upstream prompt caching (85%~98% cache hit rate).

---

## 2. Identified Architectural Bottlenecks

### [P0] SQLite Single-Writer Lock Contention & Extreme Write Amplification
* **Mechanism**:
  Every generation request executes **three independent SQLite write transactions** (`BEGIN IMMEDIATE`):
  1. **Inbound Lease Reservation** (`ReserveInFlight`): `INSERT INTO inflight_leases`
  2. **Outbound Lease Release** (`release()`): `DELETE FROM inflight_leases WHERE id = ?`
  3. **Outbound Accounting & Telemetry** (`RecordSuccess`): calls `PoolTransaction` $\rightarrow$ `SavePoolToTx`
* **The Problem**:
  `SavePoolToTx` suffers from severe **write amplification**. Even when merely incrementing a single account's `request_count` and `gen_count`:
  * It scans all accounts in `accounts`.
  * It re-executes `UPSERT` statements for every account, runtime record, and quota window across the entire pool.
  * It serializes the entire 4-hour rolling telemetry event array (`generation_events`, up to thousands of structs) into a giant JSON blob and overwrites `runtime_global`.
* **Symptom in Logs**:
  Under concurrency (e.g. multiple subagents issuing tool calls simultaneously), SQLite file-level single-writer lock contention causes `ReserveInFlight` to time out (`ErrWriteBusy`), logging fallback warnings: `in-flight lease not recorded for %s, balancing on local counts`.

---

### [P1] Upstream TCP/TLS Handshake Overhead on Zero-Risk Metadata Calls
* **Mechanism**:
  In `internal/proxy/transport.go`, both `BaseTransport` and `GenerationTransport` are hardcoded with:
  ```go
  ForceAttemptHTTP2: false,
  DisableKeepAlives: true, // Project invariant: no hidden connection-reuse retries
  ```
* **The Problem**:
  * Disabling keep-alives was intended to prevent Go's `http.Transport` from secretly retrying failed idempotent requests on dead reused connections during generation.
  * However, applying `DisableKeepAlives: true` to `BaseTransport` forces **every single request** to perform a brand-new TCP 3-way handshake + TLS 1.3 handshake (50ms ~ 200ms overhead).
  * **66.4% of all proxy traffic** consists of idempotent metadata calls (`listExperiments`, `fetchUserInfo`, `retrieveUserQuotaSummary`) that carry zero quota-waste risk, yet all suffer full TLS negotiation latency on every invocation.

---

### [P2] Broad Mutex Scope in Request Selection (`selectionMu`)
* **Mechanism**:
  In `internal/proxy/proxy.go`, `selectionMu.Lock()` serializes inbound generation requests:
  ```go
  selectionMu.Lock()
  sessionKey = ExtractSessionKey(r, body)
  candidates = scheduler.OrderCandidates(...)
  _, leaseRelease, leaseErr := storage.ReserveInFlight(pick)
  selectionMu.Unlock()
  ```
* **The Problem**:
  `selectionMu` wraps CPU parsing, memory scheduling, and the **cross-process SQLite write transaction** (`ReserveInFlight`). If disk I/O or SQLite WAL checkpointing stalls, all incoming generation requests in the process are blocked from even parsing headers or session keys.

---

### [P2] Full Request Body Memory Buffering (`io.ReadAll`)
* **Mechanism**:
  Incoming requests are fully buffered into heap memory via `ReadRequestBody` (`io.ReadAll(r.Body)`) before forwarding, in order to allow failover re-dispatch.
* **The Problem**:
  In multi-turn conversations with large contexts or multimodal payloads (high-resolution screenshots, image attachments, or code bundles of 5MB ~ 20MB), buffering full bodies across concurrent requests creates heap spikes and GC pressure.

---

### [P2] Per-Chunk Regex Scanning in Streaming Pipeline (`usageTap`)
* **Mechanism**:
  During SSE streaming (`SendStream`), incoming chunks pass through `usageTap.Read()`, which executes:
  ```go
  var usageRe = regexp.MustCompile(`"(promptTokenCount|cachedContentTokenCount)"\s*:\s*(\d+)`)
  ```
  On every chunk (multiple times per second), `usageRe.FindAllSubmatch` runs against a concatenated buffer (`append(t.carry, b...)`), allocating submatch slices and strings simply to read token counters at the end of the stream.

---

## 3. Preliminary Benchmark & Optimization Experiment (`moego`)

On branch `test/try-moego`, we evaluated replacing standard library `json.Unmarshal` with `github.com/MoeForge/moego/ojson` (zero-copy path indexing) for hot inspection paths (`requestModel` and `ExtractSessionKey`):

* **Benchmark Results** (Linux arm64, realistic 50KB 30-turn conversation payload):
  * `requestModel`: **31.7 µs $\rightarrow$ 0.084 µs (375x faster)**
  * `ExtractSessionKey`: **65.3 µs $\rightarrow$ 0.81 µs (80.4x faster)**, memory down from **12.5 KB (131 allocs) $\rightarrow$ 168 B (4 allocs) (98.7% reduction)**.
* **Trade-off Consideration**:
  `moego` is at `v0.1.0-alpha.1`. While the speedup on multi-turn bodies is massive, `agy-pool-go` has a strict "Standard Library First" guideline.

---

## 4. Specific Questions for Your Review

Please provide concrete, actionable feedback on the following questions:

1. **Prioritization & Severity**:
   Do you agree with the P0 $\rightarrow$ P2 ranking? Are there any hidden latency or reliability risks in this architecture that were overlooked?
2. **SQLite Write Amplification Fix**:
   What is the cleanest, lowest-complexity pattern to eliminate SQLite write lock contention in `RecordSuccess` and `ReserveInFlight`?
   * *Option A*: Targeted single-row atomic SQL updates (`UPDATE account_runtime SET request_count = request_count + 1 WHERE id = ?`) instead of full `SavePoolToTx`.
   * *Option B*: Split telemetry into an append-only event table, and keep in-flight counts in shared memory / atomic counters with periodic background sync.
   * *Option C*: In-memory cache layer with write-behind or batched commit.
   Which pattern best balances correctness across multiple CLI/daemon processes with minimal code?
3. **Transport Keep-Alive Strategy**:
   Is splitting transports (enabling HTTP/2 + persistent connection pooling on `BaseTransport` while keeping `GenerationTransport` unpooled with `DisableKeepAlives: true`) safe and standard practice for LLM gateways?
4. **Lock Granularity & Body Streaming**:
   How would you restructure `selectionMu` and `ReadRequestBody` so that large multimodal request bodies do not penalize concurrent request routing?
5. **Dependency Evaluation (`moego/ojson`)**:
   Given the 80x~375x benchmark gain in `ExtractSessionKey` on large conversation contexts vs the `v0.1.0-alpha.1` maturity risk and "Standard Library First" rule, would you:
   * Keep it on `test/try-moego` until the library stabilizes?
   * Implement a tiny, zero-dependency hand-rolled scanner for the top-level `labels.cascade_id` and `model` fields?
   * Merge `moego` now because the path-indexed extraction is already isolated and covered by equivalence tests?
