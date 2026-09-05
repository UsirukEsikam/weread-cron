# 03: 最小 Task happy path

**What to build:** `weread-cron run` 在配置指定候选书（`WEREAD_CRON_BOOKS`）下完整跑通一次 Task 直到成功：初始 Cookie 建立 Login Session → 任务开始 renewal → 读真实 Reading Progress → 抓取 Reader 页建立 Reader Context → enter report → 周期 timed report（`rt` 为距上次成功接受的实际间隔、本地累计）→ 达到当天随机 Target Duration → success 终态落盘 → 成功通知 → stdout 摘要、exit 0。应用 seam（注入 clock/RNG/HTTP/notify/session store + fake WeRead server + fake 通知端点 + 临时 /data，ADR-0006）随本票建立。

**Blocked by:** 01, 02

**Status:** resolved

- [x] fake server 线上断言：enter → timed 时序、payload 字段集、`s`/`sg` 可重算验证
- [x] `rt` 按 ADR-0004：每次 timed report = 距上次成功接受的实际间隔，非累计；本地汇总在达标时停止
- [x] renewal 请求在 Task 开始时发出且新 Cookie 并入会话
- [x] Target Duration 在配置范围内随机且一次 Task 只生成一次（seeded RNG 下确定性）
- [x] 达标后 success 终态先落盘、再发成功通知（计划/实际时长、report 次数、书名）
- [x] 成功通知经 fake Bark/企业微信端点断言内容
- [x] 通知端点故障不影响 Task 结果与终态
- [x] `run` 退出码 0，stdout 摘要含关键信息

## Answer

实现「指定候选书下的完整 Task happy path」+ ADR-0006 应用 seam，全部经线上行为断言。

### 模块（新文件）

- `internal/clock`：`Clock`（Now/Sleep）+ `Fake`（Sleep 立即推时、`Advance` 注入 suspend 跳变）
- `internal/atomicfile`：原子写（临时文件 + rename）
- `internal/session`：`Jar`（Cookie 属性完整保存）、`Store`/`FileStore`（原子持久化）、`New`（有持久化 → 恢复；否则初始 Cookie 建立并落盘）→ 本包扩展自 ticket 01 的占位
- `internal/weread/client.go`：HTTP 客户端（renewal / Reader 页 / `/web/book/read`），请求形态按 koplugin `client.lua`：renewal body `{"rq":"%2Fweb%2Fbook%2Fread","ql":false}`、JSON POST 带 `Origin`/`Referer=Reader 页地址`、payload 数字字段（ci/co/pr/ct/rt/ts/rn）线上输出为数字
- `internal/weread/readerstate.go`：`__INITIAL_STATE__` 提取与解析（结构/字段优先级按 koplugin `reader_state.lua`：currentChapter 优先、回退 progress.book；chapterOffset 兼容数字/字符串）
- `internal/report`：enter/timed 发送；被服务器拒绝 → `ErrRejected`（ticket 05 恢复链据此区分拒绝与传输故障），本票直接失败
- `internal/notify`：Bark / 企业微信机器人；`Multi` 渠道独立、失败聚合
- `internal/terminal`：Terminal State 原子持久化
- `internal/task`：编排（选书（RNG）→ Target Duration（首次 Intn 后）→ renewal → Reader 页 → enter → 循环 timed → 终态落盘 → 成功通知）
- `internal/app`：装配点（Deps 注入 clock/RNG/HTTP/base URL/session store/notifier；零值 = 生产默认）

### 与 spec 流程的两处说明

1. **选书与 Target Duration 先于 renewal**（spec 决策 #6 写 renew → 选书）：本地决策先行，候选未配置时快速失败（不发起网络请求；CLI 薄 seam 可无网络断言）。renewal 仍是 Task 的首个线上请求。
2. **rt 的基准时刻**：`lastSent`（上次被接受上报的发送时刻，而非响应时刻）——异常间隔（suspend）会出现在下一次 rt 中并触发重建，符合 ADR-0004；首次 timed 的 rt = 距 enter 成功（会话开始）的间隔，与官方 JS 的 startReadingTime 语义在首拍一致。

### 测试（应用 seam 为主）

- `internal/app/app_test.go`（主 seam）：fake WeRead server（请求记录 + 《三体全集》`__INITIAL_STATE__` 页 + renewal Set-Cookie）+ fake Bark/企业微信 + fake clock + 种子 RNG：
  - happy path：renewal → reader 页 → enter → timed×N 时序；enter/timed 字段集差异；`s`/`sg` 重算；数字字段线上类型；rt=30 逐次演进、达标即停；renewal 新 Cookie 在后续请求与 `login_session.json` 中；Terminal State 先落盘（通知端点请求时校验文件）再通知；Bark/WeCom 消息体含书名/计划/实际/次数；`run` 摘要信息
  - 确定性：seeded RNG 重放 pickBook→target→rn 顺序，target 一次生成且在范围内
  - 异常间隔：响应期间 `Advance(120s)` → 二次 enter（重建 Reading Session）、无 rt>90、累计仍 60s
  - 通知端点故障（Bark 500）：Task 结果与终态不受影响、企业微信渠道正常
- 纯逻辑：`readerstate`/`session`/`terminal`/`atomicfile` 单测；CLI 进程内（退出码/stdout 摘要/失败路径）与薄二进制 seam（`run` 无候选快速失败、books 占位）

> 说明：`run` 成功路径的退出码 0 在薄二进制 seam 无法覆盖（需真实微信读书端点），故在 cli 层可注入 App 的进程内测试 + 应用 seam 覆盖；二进制 seam 覆盖失败/快速失败路径。

### 未实现（交接后续票）

- 恢复链（refresh/renewal/retry）、失败终态与失败通知 → ticket 05；Reader Context TTL 缓存/刷新 → ticket 06；自动选书（`WEREAD_CRON_BOOKS` 未配置时快速失败）→ ticket 09；run 的终态规则（success 拒绝、failed 重试）与并发互斥 → ticket 08。
- 协议未实测项延续 ticket 02 标注（token fallback / s 校验边界 / synckey 语义），`x-wr-ticket`/`x-wrpa-0`（仅 MP 内容使用，V1 Out of Scope）未采集。

经双轴 `/code-review` 审查后修正：`Enter`/`Timed` 拒绝返回改为 `ErrRejected` 错误（消除不可达分支；ticket 05 以 `errors.Is` 区分）、`succ` 判定收敛至 `protocol.IsSucc`（去除重复实现）、Task 文件注释明确异常间隔重建属本票（ADR-0004）而 refresh/renewal 恢复属 ticket 05。