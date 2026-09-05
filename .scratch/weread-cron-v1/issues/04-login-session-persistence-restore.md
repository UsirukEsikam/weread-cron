# 04: Login Session 持久化、恢复与失效重建

**What to build:** Login Session 的跨重启生命周期：原子持久化到 `/data`、启动时恢复优先于初始 Cookie、renewal 更新的 Cookie 落盘（重启不回退旧会话）；登录失效凭明确证据判别（判别清单见 `docs/protocol-validation-checklist.md` #2/#4）→ 从初始 Cookie 重建一次 → 仍失败 → failed 终态 + 明确提示"更新初始 Cookie"的登录失效通知。

**Blocked by:** 03

**Status:** resolved

- [x] 运行中 renewal 后重启 → 恢复的是最新会话而非初始 Cookie
- [x] 无持久化会话 → 从初始 Cookie 初始化
- [x] 持久化为原子写（写入中途崩溃不留半写状态）
- [x] 凭明确证据判定登录失效 → 自动从初始 Cookie 重建一次；重建成功则当天 Task 继续
- [x] 重建仍失败 → failed 终态 + 登录失效通知（明确提示更新初始 Cookie）
- [x] 登录失效判别特征与验证清单一致；发现假设冲突时在 Comments 提出讨论，不静默替换判别方案

## Answer

实现「Login Session 跨重启生命周期 + 登录失效重建」，全部经应用 seam（ADR-0006）线上行为断言。

### 判别特征（checklist #2 一致性）

本票把**唯一**明确证据实现为：renewal 请求完成（HTTP 200）、响应为合法 JSON、**succ 字段存在且不为 true/1**——服务器明确拒绝 renewal。分类与清单假设一致（"renewal 失败是明确证据之一"），未新增任何未经实测的判别特征：传输错误、HTTP 非 200、响应解析失败、**响应不含 succ 字段**（如 errCode 错误体）均**不**归为登录失效（暂时性失败，不重建、不写终态，当日可再次调度）。其余候选特征（404/401、重定向到登录页等）在清单 #2 中标注待实测；若实测发现新的证据形态，应在验证清单更新后按处理原则在本票 Comments 讨论，不在本票静默替换。

### 模块改动

- `internal/weread/client.go`：`ErrLoginInvalid` 哨兵 + `Renewal` 分类（如上）；非证据失败保持普通错误。
- `internal/session`：`Rebuild(store, initialCookie)`——用初始 Cookie 覆盖持久化会话并原子落盘；初始 Cookie 为空时失败（无重建种子，部署无 `WEREAD_CRON_COOKIE` 时也能正确走失效终态）。
- `internal/notify`：`Notifier.NotifyLoginInvalid`（Bark/企业微信/Multi）；登录失效通知固定文案 = `LoginInvalidPrompt`（明确提示更新初始 Cookie，spec 决策 #10），附加重建原因摘要。
- `internal/task`：`Options.RebuildLoginSession`（登录失效重建入口，App 装配层注入）；`renew()`——证据出现 → 重建 → 重试一次；仍失败 → `failLoginInvalid()`：failed 终态**先落盘**再发登录失效通知（通知失败不影响结果），返回包装 `ErrLoginInvalid` 的错误（含提示）。有界：恰好 1 次重建 + 1 次重试。
- `internal/app`：客户端 Cookie/Merge 回调改读**可变会话句柄**（`current`），`RebuildLoginSession` 重建后切换句柄——之后的请求自动携带初始 Cookie 重建的会话。

### 测试（主 seam）

- `TestRunLoginInvalidRebuildsFromInitialCookieAndContinues`：预置旧会话（wr_gid=OLDSTALE）→ 首笔 renewal 被拒（`succ:0` 证据）→ 重建后重试携带初始 Cookie（断言第二笔 renewal 的 Cookie header）→ Task 完整成功、success 终态、无登录失效通知。
- `TestRunLoginInvalidRebuildFailsWritesFailedTerminalAndNotifies`：renewal 持续拒绝 → 恰好 2 笔 renewal（有界）→ failed 终态（通知端点请求时校验已落盘）→ Bark/企业微信登录失效通知含固定提示 + 日期；无成功通知；错误 `errors.Is(ErrLoginInvalid)`。
- `TestRunTransientRenewalFailureDoesNotTriggerRebuild`：renewal HTTP 500 → 1 笔 renewal、无重建、无终态、无任何通知（判别证据边界）。
- `TestRunRenewalNoSuccIsNotLoginInvalid`：renewal 200 但不含 succ 字段（errCode 错误体）→ 同为非证据（无重建/无终态/无通知）——code-review 修正：`IsSucc` 对缺失 succ 的响应返回 false，若不显式检查 succ 存在会误判为证据，已改为仅 succ 存在且不为 true/1 才归为证据。
- `TestRunLoginInvalidRebuildWithoutInitialCookieFails`：无初始 Cookie（只有失效的持久化会话）→ 重建失败路径同样 failed 终态 + 通知。
- `TestRunRestartRestoresRenewedSession`：同一 `/data` 上第二个 App（重启模拟）+ 更换后的初始 Cookie → 首笔 renewal 携带上次落盘会话（new123），初始 Cookie 未生效、未覆盖文件。
- `internal/session`：`TestRebuildOverwritesPersistedSession`（覆盖 + 原子落盘无残留）、`TestRebuildWithoutInitialCookieFails`（失败不破坏既有会话）。

### 交接说明

- 恢复链中间环节的 renewal（report 被拒后的有界恢复，spec 决策 #7）属 ticket 05；本票提供判别哨兵（`ErrLoginInvalid`）与重建原语可供其复用——ticket 05 的"登录失效导致的失败走 T4 的判别与明确提示"由此衔接。
- 终态语义（failed 阻止自动执行、`run` 手动重试更新为 success）属 ticket 07/08，本票只按 spec 决策 #9/#10 落盘与通知，不在 CLI 层改行为。

## Comments

- **实施的判别证据边界（checklist #2）**：只把 renewal 200 + 合法 JSON + **succ 存在且不为 true/1** 当作"明确证据"；HTTP 非 200（含 401/403/5xx）、响应解析失败、响应不含 succ 字段（errCode 错误体等）按暂时性失败处理，不触发重建。理由：清单假设仅确认"renewal 失败"是证据之一，其余形态未实测；宁可不重建（当日可再调度）也不误判登录失效。若真机验证发现 401/重定向登录页等也构成明确证据，需先更新验证清单，再在本票 Comments 讨论扩展判别集——不在未验证情况下静默加入。
- **官方 JS 的 rt 累计语义与 ADR-0004 差异**（protocol.go 包注释）与本次改动无关，不在本票处理。