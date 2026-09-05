# 06: Reading Session 维护

**What to build:** 长阅读会话的两个可靠性行为：(1) Reader Context 到期（参考默认约 15 分钟 TTL）时主动重新抓取 Reader 页刷新 Context，Reading Session 继续（不中断、无多余 enter）；(2) 异常墙钟间隔超过内部阈值时不形成大 `rt` 上报，而是重新 enter、重建 Reading Session——同一本书继续，Task 已累计时长保留。TTL 与阈值为内部默认值，不做配置。

**Blocked by:** 03

**Status:** resolved

- [x] 注入 clock 推进越过 Context TTL → 观察到主动重新抓取 Reader 页（新 token/psvts），后续 timed report 继续，无 enter 重复
- [x] clock 跳变超过内部阈值 → 无大 `rt` 上报（最后一次 timed 的 `rt` 不包含缺口）
- [x] 异常间隔后观察到新的 enter report（Reading Session 重建）
- [x] 重建后同一本书、Task 已累计时长保留
- [x] 行为与 ADR-0004 一致；TTL 表述为参考默认值而非服务器协议常量（验证清单 #7/#8）
- [x] 若实测表明服务器要求刷新后重新 enter（参考实现如此），在 Comments 提出讨论

## Answer

实现「Reader Context TTL 主动刷新（无 enter）」与「异常间隔重建 Reading Session」，全部经应用 seam（ADR-0006）线上行为断言。

### TTL 缓存与主动刷新（spec 决策 #5/#6；用户故事 #30）

- **`internal/readercontext`**：Provider 引入 TTL 缓存（ticket 05 预留的语义落地，`Fetch`/`Refresh` 签名对上层不变）：
  - `Fetch`：TTL 内命中缓存（零网络请求、不换 token/psvts）；过期或换书时重新抓取并更新缓存。Task 建立与周期 timed report 前的主动刷新共用。
  - `Refresh`：总是重新抓取并更新缓存（恢复链的强制刷新语义保持）。
  - 缓存按单书槽位保存（一个 Task 只使用一本书）；`Options{Clock}` 注入时间源（ADR-0006：clock 是领域真实依赖；生产 Real、测试 Fake），**TTL 值不暴露为配置**——`DefaultContextTTL = 15 * time.Minute`，注释明确为参考默认值（weread.koplugin 的 `CONTEXT_TTL_SECONDS`），非服务器协议常量（验证清单 #7/#8 口径，同 ADR-0004）。
- **`internal/task`**：timed report 循环每笔上报前经 `Reader.Fetch` 取 Context——TTL 内命中缓存不发请求；到期时自动重新抓取。刷新**不重置 rt 基准**：Reading Session 继续（无 enter、不中断），rt 仍 = 距上次被成功接受上报的实际间隔（ADR-0004）。
- **刷新失败（暂时性）的裁定**：沿用现有 Context 继续，下次周期重试（与 ticket 05 对恢复链中 refresh 失败的先例一致——不写终态、Task 不中断）；若服务器因此拒绝上报，恢复链会先 refresh 再 retry，已覆盖该情形。
- **异常间隔路径不变（ADR-0004）**：`rt` 未超过阈值正常上报；超过 `DefaultAnomalyThreshold`（90s，内部默认）→ 不作为大 `rt` 上报，重新 enter 重建 Reading Session（Task 继续、同一本书、累计保留）。重建路径的 enter 使用上方 Fetch 给出的 Context（跳变越过 TTL 时为刚重抓的新 Context；未越 TTL 时命中缓存、无额外请求）。

### 测试（主 seam，全部线上行为断言）

- `TestRunContextTTLExpiryRefreshesWithoutEnter`（acceptance 1）：20 分钟目标 + `rotateReaderState`——Reader 页恰好被抓取 2 次（初始 + TTL 到期主动刷新；中间 39 笔周期性上报命中缓存）；enter 数 = 1（无 enter 重复）；第 30 笔 timed（TTL 900s / 节奏 30s 边界）起 payload 使用新 token/psvts（`sg` 按 `fake-reader-token-2` 重算、`ps=fake-psvts-2`），此前为初始 Context；所有 `rt=30`（刷新不重置 rt 基准）。
- `TestRunContextTTLRefreshFailureContinuesWithExistingContext`（新增裁定）：Reader 页第 2 次抓取（TTL 到期那次）返回 HTTP 500 → Task 继续完成（40 笔 timed 全部接受、enter 数 = 1）；失败周期沿用旧 Context（psvts-1），下次周期重试成功（psvts-3/token-3；抓取总数 3）。
- `TestRunAbnormalIntervalRebuildsSession`（强化，acceptance 2-4）：挂起期间时钟跳变 120s → 两次 enter、所有 `rt=30`（最后 timed 不含缺口）；新增 Result 断言：`BookID == testBookID`（同一本书）、`Actual == 1 分钟`（累计保留）、`Reports == 2`。
- 既有测试（T3 happy path、T5 恢复链全序列、T4 登录失效各用例）全部保持通过——恢复链的 `Refresh` 更新缓存后，后续周期 Fetch 命中缓存，请求序列不变。

### 模块改动

- `internal/readercontext/readercontext.go`：`DefaultContextTTL` 常量、`Options{Clock}`、TTL 缓存（`Fetch` 命中缓存 / `Refresh` 强制）、并发互斥。
- `internal/task/task.go`：timed report 循环引入每笔上报前的 `Reader.Fetch`（TTL 主动刷新；刷新失败按暂时性继续）；异常间隔路径注释更新（enter 使用已刷新的 Context）。
- `internal/app/app.go`：注入 `readercontext.Options{Clock: deps.Clock}`（生产 Real、测试 Fake）。
- `internal/app/app_test.go`：fakeWeread 新增 `readerFailAt`（第 N 次 Reader 页抓取返回 500）。

## Comments

- **参考实现（weread.koplugin）刷新后确实重新 enter——已在 Comments 提出讨论（checklist #7）**。源码核查（`weread/lib/read_report.lua`，v1.4.0）：`_build_context` 在 Context 刷新路径（TTL 到期或 force）上无条件 `book.read_session_entered_at = nil`（L979），随后的 `_send`（L1077-1079）因 `read_session_entered_at` 为空先发 enter report 再发 timed report——即参考实现每 15 分钟刷新一次 Context 就重新 enter 一次；其 rt 也采用固定 30s（`DEFAULT_INTERVAL_SECONDS = 30`，L11）而非实际间隔。本票按 ticket 验收与 spec 决策 #6 实现"刷新不 enter、rt 按 ADR-0004 实际间隔"，两者对"刷新后是否必须重新 enter"存在冲突，需真实账号实测裁决（验证清单 #7）。**风险提示**：若服务器确实要求 Context 变更后重新 enter，TTL 刷新后的首笔 timed report 会被拒绝，而恢复链的重试是 timed（非 enter）——refresh → retry → renewal → refresh → retry 全部被拒 → ~15 分钟处恢复链耗尽、failed 终态（有界、可见，但长会话必失败）。届时两个可选方向：(a) TTL 刷新后先 enter 再继续（对齐参考实现）；(b) 恢复链在 refresh 后先 enter 再 retry。待协议验证后裁定。
- **官方 JS 的 rt 语义**（protocol.go 注释）：官方 Web Reader 脚本中 timed report 的 `rt` = 距阅读开始（startReadingTime）的累计秒数，与 ADR-0004 选择的"距上次被成功接受上报的实际间隔"不同——按 ADR-0004 的声明，这是 V1 在两个参考实现之间做出的设计选择（非已确认的服务器协议事实），本票维持 ADR-0004 语义（每笔间隔、异常间隔重建），差异留给协议验证复核。
- **TTL 表述**：`DefaultContextTTL` 注释明确"参考默认值 ≈15 分钟（weread.koplugin 的 CONTEXT_TTL_SECONDS），不是已确认的服务器协议常量"，与 ADR-0004 及验证清单 #7/#8 口径一致（acceptance 5）。
- **刷新失败按暂时性继续的裁定**：TTL 主动刷新失败（传输/HTTP/解析）不中断 Task、不写终态——沿用现有 Context 继续、下次周期重试；与 ticket 05 对恢复链中 refresh 失败的先例一致（两者姿态不同：链内 refresh 失败时上报已被拒且无法刷新修复，只能按暂时性失败放弃；发送前刷新失败时仍可带上现有 Context 尝试上报）。若运维观察认为应更严格（例如刷新失败即 Task 失败），在本票 Comments 提出讨论。
- **code-review（双轴）结论**：OK with notes（无 P0/P1）。Standards 轴修正：① 术语漂移——「Reader state」是 CONTEXT.md 明确回避的表述，已改为「Reader Context 解析结果」；② 两个 TTL 测试的公共骨架（20 分钟目标、40 笔上报、enter=1、rt 全 30）抽出 `runTTLScenario` 断言辅助，消除 Duplicated Code。Spec 轴补充：新增 `TestRunAbnormalIntervalBeyondTTLReentersWithFreshContext`（时钟跳变 1200s 同时越过异常阈值与 TTL），验证“重建 enter 使用越过 TTL 后重抓的新 Context”这一此前无测试路径。
- **未采纳的提示**（judgement call，裁定记录）：① `cacheEntry` 类型（Data Clumps）——缓存按单书槽位是设计决策（一个 Task 只使用一本书），抽类型不增加价值；② `mu` 互斥（Speculative Generality）——Provider 是对外 API，Fetch/Refresh 可被任意并发调用，保留防御性互斥；③ Options.TTL 注入面未提供（T5 移除虚设注入面的同一原则，测试用默认 900s 驱动）。
- **code-review 提示**：`Options.TTL` 未提供（无调用方需要覆盖 TTL 默认值；测试用默认 900s + 20 分钟目标驱动，避免虚设注入面——沿用 T5 移除 `Rhythm/AnomalyThreshold` 注入面的同一原则）；`Options.Clock` 有真实调用方（app 注入 fake clock 驱动 TTL 行为），保留。
