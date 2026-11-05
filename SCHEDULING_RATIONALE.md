# 配额调度与会话粘性优化依据 (Scheduling & Affinity Rationale)

本文档记录 `agy-pool-go` 针对 Google 上游配额双窗口（5-Hour & Weekly）物理机制的量化分析，以及本次对调度器（Scheduler）与会话粘性（Affinity）的核心优化依据。

---

## 1. 上游配额换算关系与物理机制

根据在生产环境（`sjc` 节点）抓取 Google 原生配额接口（`v1internal:retrieveUserQuotaSummary`）与累计分析 **2,570 条真实配额变更历史（`quota-history.jsonl`）**，得出以下核心结论：

### 1.1 容量换算恒等式：$C_{weekly} = 3.5 \times C_{5h}$
在严格排除 Reset 时间漂移的连续单次生成请求中，**72.1% 的采样精准锁定在 $\Delta g5 / \Delta gw = 3.5000$**（实测样本均值 **3.500009**）：

$$\frac{\Delta g5}{\Delta gw} = \frac{C_{weekly}}{C_{5h}} = 3.5 = \frac{7}{2}$$

* **周总预算是 5 小时限额的 3.5 倍**。
* **消耗 1 个满血 5H 额度（100%），等价于永久消耗周额度的 $\frac{2}{7} \approx 28.57\%$**。
* **单账号极限耐受**：在 7 天周期内，一个账号即使每次都等 5 小时回满，**全周最多只能打满 3.5 次**（累计 17.5 小时峰值并发）。打满第 4 次前，周配额会先被彻底击穿归零。

### 1.2 Google 双窗口设计初衷
> *"Within each group, models share a weekly limit and a 5-hour limit. Quota is consumed proportionally to the cost of the tokens... The 5-hour limit smooths out aggregate demand to fairly distribute global capacity across all users, while your weekly limit is tied directly to your individual tier."*

* **5 小时限额（$C_{5h}$）**：并发削峰缓冲器（Peak Buffer），防止突发流量冲垮集群算力。
* **周限额（$C_{weekly}$）**：真实总预算墙（Hard Budget Ceiling），直接绑定用户订阅等级。
* **稳态安全消耗率**：1 周有 33.6 个 5 小时窗口，安全平摊消耗速率为 $\frac{3.5}{33.6} \approx 10.42\%$。单账号每 5 小时消耗超过 10.4%（对应周额度下降约 3%），即处于透支未来周预算的状态。

### 1.3 配额重置（Reset）规律
* **静默期浮动（Floating Idle）**：满血未用时，`g5_reset` 始终浮动在 `now + 5h`，`gw_reset` 浮动在 `now + 7d`。
* **首击锚定（First-Hit Freeze）**：首次产生消耗的瞬间，Reset 时间戳立刻冻结锁定在 `首次请求时刻 + 5小时` 与 `首次请求时刻 + 7天`。
* **到点离散跃升（Discrete Jump Reset）**：到达 Reset 时间点的瞬间，配额一次性跳变回 100%（例如实测日志记录 `0.3319 -> 1.0000`），而非漏桶平滑流出。

---

## 2. 生产环境实测痛点（以 `sjc` 节点为例）

在优化前，`sjc` 节点运行 5 个账号的配额严重失衡：
* `Main`: Weekly 82%, 5H 100%
* `Work`: Weekly 74%, 5H 100%
* `Life`: Weekly 92%, 5H 100%
* **`Backup`: Weekly 14% (濒危), 5H 99%**
* `Hike`: Weekly 84%, 5H 100%

### 根因分析：
1. **5H 收割模式（Harvest Mode）过度透支周预算**：
   原逻辑中，只要账号 5H 窗口在 90 分钟内重置，且周额度 $\ge 25\%$，就会无条件触发收割，优先将流量引流给该账号。但因为 $1 \times 5H = 28.6\% \times Weekly$，在周预算已经落后于时间进度（OverWeeklyPace）的情况下继续“收割 5H”，加速了周预算的毁灭。
2. **会话粘性（Affinity）导致单账号被“薅穿”**：
   在长上下文、多轮智能体任务（如 `/teamwork /goal /plan`）中，高频生成请求不断刷新 5 分钟的 Affinity TTL。原 `affinityHealthy` 仅在账号跌落至 Reserve 阈值（5H $\le 15\%$, Weekly $\le 10\%$）或 Depleted 时才放弃粘性。这导致单个账号被单个高强度会话连续消耗几千次，池内其他满血账号（如 `Life` 92%）只能干坐着。

---

## 3. 本次代码优化方案

### 3.1 5H 收割周预算保护锁（Harvest Quota Guard）
修改位置：`internal/scheduler/scheduler.go`

* **调整门槛**：增加 `SmartHarvestMinWeeklyHeadroom = 0.30`（30% 周储备门槛）。
* **进度校验**：增加 `!OverWeeklyPaceCapacity(capState)` 约束。
* **核心逻辑**：如果账号的周配额剩余不足 30%，或者当前周消耗速度已经快于自然时间流逝（`Pace7 < -0.30`），**坚决禁止触发 5H 收割**。宁可让临近到期的 5H 临时额度自然重置，也绝不透支不可逆的周总预算。

### 3.2 会话粘性配额跌幅解绑（Quota-Drop Affinity Spillover）
修改位置：`internal/proxy/affinity.go`

* **定义低水位线**：
  * `AffinityMin5HFloor = 0.35`（35% 5H 门槛）
  * `AffinityMinWeeklyFloor = 0.25`（25% 周门槛）
* **差值决策函数 `shouldYieldAffinityForQuota`**：
  * 当会话绑定的账号跌入低水位（5H < 35% 或 Weekly < 25%），且池中排在首位的调度候选人处于健康状态（5H $\ge 60\%$ 或 Weekly $\ge 50\%$）时，**主动解除亲和粘性（Affinity Yields）**。
  * **平滑转移**：让请求正常路由至最健康的账号接力；下一个请求即可在健康账号上建立新的会话粘性，旧账号得以休眠恢复。
  * **全池低额度保护**：若全池账号均处于低水位，保留粘性，防止 prompt cache 发生毫无意义的反复颠簸。
* **修正 `HeldByLoad`**：
  * 明确区分“因并发负载暂避（HeldByLoad）”与“因配额不足主动让权（Quota Yield）”。配额主动让权时不将状态标记为 `spill`，允许在响应成功后即刻重新绑定新账号。

---

## 4. 验证与回归测试

新增并完善了回归单元测试，确保逻辑完全闭环：
1. `TestSmartHarvestCandidate_BlockedOverWeeklyPace`：验证周消耗超前或周额度低于 30% 时，5H 收割模式被严格阻断。
2. `TestApplyAffinity_YieldsOnQuotaDepletion`：
   * 验证 5H < 35% 且候选账号健康时主动解绑。
   * 验证 Weekly < 25% 且候选账号健康时主动解绑。
   * 验证全池低额度时保持粘性不颠簸。
3. 全模块自动化回归测试（`internal/scheduler`、`internal/proxy`、`internal/quota`、`internal/storage`、`internal/forecast`、`internal/diagnostics`）全部 100% 通过。
