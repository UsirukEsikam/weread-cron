# 09: 自动选书与 books CLI

**What to build:** 纯 Cookie 书架访问（端点待实测确认，见 `docs/protocol-validation-checklist.md` #1）→ `weread-cron books` 列出当前书架 bookId 与 title（复用 daemon 的 Login Session 恢复/初始化机制，可正常 renewal 并持久化，但不产生 Task、不读取或修改终态、不发送通知）；未配置候选书时 Task 自动选书：Shelf 元数据过滤优先"未读完且可建 Reader Context"的书 → 有界探测验证 → 全部不可用时回退"已读完但可用"的书。

**Blocked by:** 04

**Status:** resolved

- [x] books 在 fake shelf 上输出正确 bookId/title
- [x] books 会话失效时报错并提示更新初始 Cookie（不静默）
- [x] books 不写终态、不产生 Task、不发通知
- [x] 无候选书时 Task 从书架自动选书（未读完优先）
- [x] 候选探测有界（失败换下一个，次数上限为内部默认）
- [x] 未读完候选全部不可用 → 回退已读完可用书（progress=100% 计费行为待实测，冲突则提出讨论，验证清单 #3）
- [x] 探测全部失败 → Task 失败走失败通知
- [x] 显式指定的候选书不因 progress=100% 被排除

## Answer

实现「纯 Cookie Shelf 访问 + `weread-cron books` + Task 自动选书」，全部经应用 seam（ADR-0006）线上行为断言。

### Shelf 端点（checklist #1）

按设计假设实施，端点与假设一致（Cookie-only 接口存在）：

- `GET {base}/web/shelf/sync`，纯 Cookie 鉴权。无 Cookie 访问返回 `{"errCode":-2010,"errMsg":"用户不存在"}`（按登录身份鉴权、非公开接口）；与我方 `renewal`/Reader 页同一 `/web/*` 接口族。
- 响应按 skill gateway `/shelf/sync` 同构假设解析：`books[]` 数组，条目字段 `bookId`/`title`/`author`/`finishReading`（`finishReading==1` = 已读完；0/1、布尔、`"1"` 字符串宽容解析）；`errCode!=0` 信封报错；`albums`/`mp` 等非普通书条目忽略。
- **响应结构未经真实账号实测确认**（见 Comments）：若实测字段与假设不一致，按验证清单处理原则提出讨论，不静默替换字段语义。

### books CLI（spec 决策 #12）

- `internal/weread`：`Client.Shelf(ctx)` + `ParseShelfResponse`（纯逻辑解析，shelf_test.go 单测）。
- `internal/app`：`App.ListBooks(ctx)` —— 复用 daemon 的 Login Session 机制（`session.New` 恢复/初始化 → renewal 新 Cookie 并入会话并持久化）；凭明确证据（renewal 200 + succ!=1）判定登录失效时从初始 Cookie 重建一次并重试（与 Task 同一姿态）；仍失败 → 返回包装 `weread.ErrLoginInvalid` 的错误（CLI 提示更新初始 Cookie，不静默）。**边界**：不产生 Task、不读写 Terminal State、不发送任何通知；并发守卫与 Task 共用 `runMu`（Task 运行中返回 `ErrTaskRunning`，避免会话文件写竞争）。
- `internal/cli`：books 路由打印每行 `bookId<TAB>title`（脚本可直接 cut）；会话失效 → ExitConfig + 明确提示；Task 运行中 → ExitRunRejected。

### 自动选书（spec 决策 #6；ticket 09 在 task 层）

无候选书时流程：renewal（选书需登录后访问 Shelf，故在 renewal 之后）→ 抓取 Shelf → 元数据过滤（未读完 `finishReading!=1` 优先，已读完为回退层；无 bookId/title 的条目过滤）→ 有界探测：逐本抓取 Reader 页验证可建 Reader Context，每层上限 `DefaultMaxSelectionProbes = 20`（内部默认，spec 决策 #13），失败换下一本——**选定书命中 Reader TTL 缓存**，进入上报直接用探测时的 Context（零额外请求）。全部不可用 → `failBookSelection`：failed 终态先落盘、再发失败通知（失败阶段 = 自动选书；已尝试动作 = Shelf 元数据过滤 → Reader Context 有界探测）。显式指定候选书保持原语义（随机取一、不因 progress=100% 被排除）。

### 失败语义边界（与 ticket 05 的暂时性失败姿态一致）

- Shelf 抓取失败（传输/HTTP/解析/errCode 信封错误）→ 暂时性失败：不写终态、不通知，当日可再次调度；不套登录失效文案（checklist #2 只认 renewal 明确证据）。
- Shelf 有数据但无可用书（含空书架）→ 本 Task 的失败：failed 终态 + 失败通知（重跑大概率依旧失败，属数据/配置问题）。

### 测试（主 seam）

- `internal/weread/shelf_test.go`：解析字段三形态、忽略非 books 条目、errCode 打包/0、空书架、畸形响应。
- `internal/app/autoselect_test.go`：未读完优先、未读完不可用回退已读完（HTTP 500 / 无 `__INITIAL_STATE__` 两种不可用形态）、每层探测有界（30 本未读完只探 20 本 → 回退层第 4 本命中，恰 24 次抓取）、探测全部失败 → failed 终态（通知时序校验先落盘）+ 失败通知、空书架失败、Shelf 暂时性失败不写终态不通知、显式书 progress=100% 不被排除且 pr=100 如实上报、选定书 enter 使用探测时的 Context。
- `internal/app/books_test.go`：happy path（renewal → shelf → bookId/title、renewal 新 Cookie 落盘）、会话失效重建一次后报错（错误包装 `ErrLoginInvalid`；无初始 Cookie 仅 1 笔 renewal）、暂时性 renewal 失败/HTTP 500/Shelf errCode 均不套登录失效文案、空书架、Task 运行中被拒绝（`ErrTaskRunning`）、全程无终态/无通知/无 Task 请求（`/web/book/read`、Reader 页 0 请求）。
- `internal/cli` 与 `cmd/weread-cron`：books 进程级呈现（成功输出格式、会话失效提示、运行中拒绝、查询失败）；无候选书不再于 CLI 层快速失败（合法输入）；缺 Cookie 快速失败仍由 requireAuth 覆盖。

## Comments

- **checklist #1（Shelf 端点/响应结构）**：端点选用 `GET /web/shelf/sync`（与 Web 端 `/web/shelf/*` 接口族一致；无 Cookie 实测返回 errCode -2010 信封，确认按身份鉴权存在）。响应字段（`books[].bookId/title/finishReading`）按 skill gateway `/shelf/sync` 同构假设实现，**尚未用真实账号 + 有效 Cookie 实测**。真机验证时若结构与假设不一致（字段名、类型、finishReading 语义），应在验证清单更新后于本票 Comments 讨论，不静默替换另一套接口/字段。
- **checklist #3（progress=100% 计费行为）**：回退"已读完但可用"的书按设计假设实施（结构上可上报，无护栏）。服务端是否接受并计入阅读时长未经实测：若已读完不计时，回退逻辑应在本票讨论（调整方向：回退层仅当服务器确认不计费时禁用，或保持现状并接受"已读完只保证完成当天任务、不保证计时"）。显式指定的书不因 progress=100% 被排除（issue 验收），且 payload 如实上报 pr=100。
- **books 的"不静默"边界**：会话失效的判别只认 renewal 明确证据（checklist #2 与 Task 同一判别集）；Shelf 返回 errCode!=0（如 -2010 用户不存在）在实现中按普通查询错误报告，**不**套"请更新初始 Cookie"提示——该形态是否为登录失效的证据未经实测。若真机验证发现"renewal 成功但 Shelf -2010"即会话失效，需先更新验证清单再讨论扩大提示范围。
- **books 并发守卫**：与 Task 共用 `runMu`（TryLock 非阻塞拒绝，`ErrTaskRunning` → CLI ExitRunRejected）。理由：books 的 renewal/会话写与运行中 Task 写同一 `login_session.json`，原子写不防丢失更新；拒绝查询优于并发写竞争。若希望 Task 运行中也可查询，需另一方案（如独立会话句柄 + 写锁粒度细化），不在 V1 范围。
- **自动选书确定性**：无候选书时按书架顺序首选（探测命中即停），不消耗 RNG（显式候选路径的随机语义与 seeded RNG 重放不变）；"优先未读完"由元数据分层保证，不额外随机化。
- **探测失败的分类（待讨论）**：探测中 Reader 页的**任何**失败（含传输/HTTP 500 等暂时性形态）一律视为"该书不可用"，换下一本——按 issue 验收口径"探测全部失败 → Task 失败走失败通知"实现（失败终态 + 失败通知；failed 终态下可用 `run` 手动重试）。与 task 开始阶段 fetchReaderState 的暂时性失败（不写终态）姿态不同：探测阶段未区分"书结构性不可用"与"暂时性故障"，若全体候选都因暂时性故障（如服务端短暂 5xx）失败，当天会被判定失败并通知。该差异是 issue 验收口径与"暂时性失败"姿态的显式取舍：若真机验证发现探测期间的暂时性故障把当天误判为失败属于常见情形，应在本票讨论（如探测全失败后先按暂时性处理不写终态，或整体重试一轮探测），不静默改判。
- **进度快照**：自动选书的探测即抓取 Reader 页（建立 Context 并缓存），选定书的上报使用该快照——多一本"探测"少一次"正式抓取"，线上请求数不变（1 次 Reader 页）。
