# 05: 有界恢复链与失败通知

**What to build:** timed report 被拒时的有界恢复链——refresh Reader Context → retry → renewal → refresh Reader Context → retry → 仍失败则 failed 终态；失败通知内容（失败阶段、主要错误、已尝试恢复动作）；通知渠道独立性（Bark 与企业微信互不影响）与通知失败不影响 Task 结果。

**Blocked by:** 04

**Status:** resolved

- [x] fake server 拒绝场景下按序观察到 refresh/retry/renewal/refresh/retry 请求，顺序与次数有界
- [x] 恢复链中途成功 → Task 继续并最终成功
- [x] 恢复链耗尽 → failed 终态（阻止当天再次自动执行）
- [x] 失败通知含失败阶段、主要错误、已尝试恢复动作
- [x] Bark 与企业微信独立发送；单个渠道故障不影响另一渠道与 Task 结果
- [x] 登录失效导致的失败走 T4 的判别与明确提示（不混入普通失败文案）

## Answer

实现「timed report 被拒时的有界恢复链 + 失败通知 + 渠道独立性」，全部经应用 seam（ADR-0006）线上行为断言。

### 恢复链语义（spec 决策 #7 的实现与补充裁定）

- **入口**：只有服务器"明确拒绝"（`report.ErrRejected`：响应已完成但不被接受，见 report 包）才进入恢复链；传输错误、HTTP 非 200、解析失败不进入（沿用 ticket 04 判别原则）。
- **有界序列**：refresh Reader Context → retry → renewal → refresh Reader Context → retry（`recoverSend`，`recoverySteps=5`）；任何一步成功即恢复、Task 继续；最后一步重试仍被拒绝 → 恢复链耗尽 → `failRecoveryExhausted`：failed 终态**先落盘**、再发失败通知。
- **enter 同样走恢复链**（用户故事 #26 的 enter 被拒与 timed 被拒语义一致；issue 以 timed 为主要场景，enter 场景有独立测试）。
- **链中步骤的非拒绝失败**（refresh/retry/renewal 的传输/HTTP/解析失败）：按"暂时性失败"处理——Task 失败但不写终态、不发通知（当日可再次调度）。裁定依据：ticket 04 的先例（无明确证据不写终态，`TestRunTransientRenewalFailureDoesNotTriggerRebuild`）；spec 只对"拒绝→恢复→仍拒绝"定义了 failed 终态，传输类失败不是证据形态。
- **renewal 步骤复用 ticket 04 的 `renew()`**：出现登录失效明确证据（succ 存在且非 true/1）→ 从初始 Cookie 重建一次 → 重试；仍失败 → 走 `failLoginInvalid`（failed 终态 + 登录失效通知，固定提示更新初始 Cookie）。登录失效路径不进入普通失败通知（"不混入普通失败文案"）。
- **rt 语义（ADR-0004）**：恢复链的每次重试重新计算 rt（= 距上次被成功接受上报的实际间隔，重试延迟自然并入，用户故事 #28）；恢复成功后的 acceptedAt 成为新的 lastSent 基准。

### 模块改动

- `internal/readercontext`（新，spec 决策 #3 的 reader context 模块）：`Provider.Fetch`（Task 建立）/ `Refresh`（恢复链强制刷新）。当前无缓存、两者等价；ticket 06 将在此包引入 TTL 缓存与主动刷新，对上接口不变。Node 注释明确 TTL 表述遵守验证清单 #7/#8（不写成服务器协议常量）。
- `internal/task`：`recoverSend`（恢复链核心，reportSend 闭包每步重试重算 rt/ts/rn）+ `sendEnter` + `failRecoveryExhausted`；`Options.Reader` 注入 provider（nil = 由 Client 组装）；失败阶段/已尝试动作常量（`StageEnterReport`/`StageTimedReport`、`ActionRefreshContext`/`ActionRetryReport`/`ActionRenewal`）。
  - code-review 修正：移除 `Options.Rhythm/AnomalyThreshold`（无调用方设置，虚设注入面；内部默认直接使用 `DefaultRhythm`/`DefaultAnomalyThreshold`，贴合 spec 决策 #13"内部参数不暴露"）；抽 `rtSeconds` 消除两处重复的间隔秒计算。
- `internal/notify`：`Failure{Date, Stage, Error, Actions}` + `NotifyFailure`（Bark / 企业微信 / Multi，渠道独立、错误聚合）；`formatFailure` 输出失败阶段/主要错误/已尝试恢复（动作按序 `→` 连接）；`FailureTitle = "微信读书阅读任务失败"` 与成功/登录失效文案明确区分。
- `internal/app`：客户端装配时注入 `readercontext.NewProvider(client)`。

### 测试（主 seam，全部线上行为断言）

fake WereRead 扩展：timed/enter 选择性拒绝（`timedRejects`/`timedRejectAll`/`enterRejects`）、renewal 选择性拒绝（`renewRejectFrom`/`renewRejectOnceAt`，任务开始的 renewal 必须成功、链中/重建后重试的选择性拒绝）、Reader Context 轮换（`rotateReaderState`：第 N 次抓取 = fake-reader-token-N / psvts-N，断言刷新后 payload 用新 Context）。

- `TestRunReportRejectedRecoveryChainExhausted`：acceptance 1+3+4——按序 `renewal, refresh, enter, timed, refresh, timed, renewal, refresh, timed`（恢复链 5 步、次数有界：reader 3 次、renewal 2 次、report 4 次）→ failed 终态（通知端点请求时校验已先落盘）→ Bark/企业微信失败通知各 1 条，含"失败阶段：timed report"、"主要错误"、完整动作序列；无成功文案。错误 `errors.Is(report.ErrRejected)`。
- `TestRunRecoveryChainFirstRetrySucceeds`：acceptance 2——refresh 后重试即接受；断言刷新后 payload 的 sg/ps 使用新 token/psvts（新 Context 真实生效）；无 renewal；success 终态 + 成功通知（无失败通知）。
- `TestRunRecoveryChainViaRenewalSucceeds`：acceptance 2——走到 renewal 环节恢复；按序 11 请求、reader 3 次、renewal 2 次（有界）；Task 最终成功。
- `TestRunEnterRejectedRecoveryChainSucceeds` / `TestRunEnterRejectedRecoveryChainExhausted`：enter 同样走恢复链（中途成功 / 耗尽 → 失败通知阶段 = enter report）。
- `TestRunRecoveryChainNotifyChannelIndependence`：acceptance 5——Bark 端点 500 时企业微信仍收到完整失败通知；Task 结果（failed 终态）不受通知故障影响。
- `TestRunRecoveryChainLoginInvalidGoesThroughT4`：acceptance 6——链中 renewal 出现 succ:0 证据 → 恰好 1 次重建 + 1 次重试（renewal 总数 = 任务开始 1 + 证据 + 重建重试）；重建重试携带初始 Cookie 重建的会话；failed 终态 + 登录失效通知（固定提示）；通知不含"失败阶段/已尝试恢复/阅读任务失败"（不混入普通失败文案）。
- `TestRunRecoveryChainLoginInvalidRebuildSucceedsContinues`：链中证据 → 重建成功 → 恢复链继续（refresh → retry）→ Task 最终成功；无任何失效/失败通知。
- 既有 T3 测试 `TestRunReportRejectedFailsTask` 被本票取代（其注释明确"恢复链由 ticket 05 交付"）：拒绝场景现在产生 failed 终态 + 失败通知，而非裸失败。
- 其余既有测试（happy path、异常间隔重建、T4 各用例、重启恢复会话）全部保持通过。

### 交接说明

- "failed 终态**阻止当天再次自动执行**"的调度门控属 ticket 07（scheduler 已有 TODO）；本票交付 failed 终态落盘这一前提。
- Reader Context TTL 缓存与主动刷新（含验证清单 #7 的"刷新后是否必须重新 enter"裁定）属 ticket 06；本票的 `Fetch`/`Refresh` 为其提供原语。
- 自动选书（Shelf）属 ticket 09；`run` 的 failed-重试规则与并发互斥属 ticket 08。

## Comments

- **code-review（双轴）结论**：OK with notes。Spec 轴指出"恢复链中途非拒绝失败 → 不写终态"是 spec 未明确的行为——本票按 ticket 04 先例裁定并在此记录：传输/HTTP/解析失败不是"明确证据"，不写终态、当天可再调度（有拒绝证据后仍会重新走恢复链并收敛到 failed 终态，若服务器持续拒绝）。若真机/运维观察认为该姿态过保守（例如希望任何链中失败都立即 failed 终态），在本票 Comments 提出讨论。
- **rt 在恢复链重试中的取值**：重试 rt = 距上次被成功接受上报的实际间隔（重试延迟并入）。链中各步骤均为快速 HTTP 往返且被 30s 超时约束，正常情况下重试 rt 不会超过异常阈值（90s）；若未来恢复步骤变慢导致大 rt，可讨论在重试前做异常间隔判定（与 ADR-0004 重建路径合并）。
