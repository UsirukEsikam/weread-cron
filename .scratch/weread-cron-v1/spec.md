# weread-cron V1：微信读书自动阅读服务

Status: ready-for-agent

## Problem Statement

我需要在微信读书账号上持续累积阅读时长，但手动打开浏览器阅读耗时且无法无人值守。现有开源方案（`wxread`、`weread.koplugin`）要么依赖抓包静态模板和过时 workaround，要么功能范围远大于需求；且长期运行需要处理登录状态维护、随机化行为、失败恢复与通知，这些手工运维成本高、不可靠。

## Solution

一个单账号、轻量、无 Web UI 的 Go 服务 `weread-cron`，以 Docker 常驻部署，长期无人值守运行。每天在可配置的 Run Window 内随机时刻启动一次 Task：renew Login Session → 选书（指定候选或自动从真实 Shelf 选择）→ 读取真实 Reading Progress 与 Reader Context → 通过 enter report + 周期 timed report 上报微信读书阅读时长 → 本地累计达标（当天随机 Target Duration）→ 落盘 Terminal State → 通过 Bark/企业微信机器人通知 → 排定次日。提供 `weread-cron run`（手动执行当天 Task）与 `weread-cron books`（书架查询）两个 CLI 子命令。

## User Stories

1. 作为部署者，我希望用 `docker compose` 在 Apple Silicon Mac mini（arm64）上常驻运行 weread-cron，以便长期无人值守地累积微信读书阅读时长。
2. 作为部署者，我希望同一镜像也能在 x86_64 Linux VPS/NAS 上运行，以便在不同硬件上部署（GitHub Actions 构建 amd64+arm64 并推送 GHCR）。
3. 作为部署者，我希望只通过环境变量完成全部配置（Cookie、候选书籍、运行窗口、时长范围、通知渠道、时区），以便无需维护配置文件。
4. 作为部署者，我希望首次启动未提供 Cookie 时报错退出并给出明确提示，以便立即发现配置缺失而不是空转。
5. 作为部署者，我希望非法配置（时长 min>max、窗口 start>end 且不相等、时间格式错误）在启动时立即失败，以便错误能被及时发现。
6. 作为部署者，我希望 `TZ` 环境变量（默认 Asia/Shanghai，二进制内嵌 tzdata）决定一切时间语义，以便在任意主机上按本地时间运行。
7. 作为部署者，我希望容器重启后自动恢复持久化的 Login Session，以便无需每次重启重新提供 Cookie。
8. 作为部署者，我希望首次运行时使用初始 Cookie 建立 Login Session，以便从零开始部署。
9. 作为部署者，我希望运行期间 renewal 产生的新 Cookie 被原子持久化到 `/data`，以便重启不会回退到旧 Cookie。
10. 作为部署者，我希望每天在 Run Window 内随机时刻启动一次 Task，以便阅读行为不呈现固定规律。
11. 作为部署者，我希望 Run Window 只约束开始时间（启动后读满目标时长、允许越过窗口结束点），以便阅读时长优先于窗口边界。
12. 作为部署者，我希望 `start == end` 的窗口表示固定启动时刻，以便需要固定时间时可以表达。
13. 作为部署者，我希望错过整天的执行不补跑，以便行为可预期、无历史欠账。
14. 作为部署者，我希望服务启动时若当天窗口尚未结束且当天尚无终态，在 `[now, 窗口结束]` 内随机安排一次，以便部署/重启不浪费当天。
15. 作为部署者，我希望当天已形成终态后不再自动执行，以便成功或失败都不会被重复执行（除非手动 `run`）。
16. 作为部署者，我希望每条 Task 开始时生成当天独立的 Target Duration（在配置范围内随机），以便每日时长有适度变化。
17. 作为部署者，我希望 Task 运行中进程异常退出后，重启可在当天剩余窗口内重新随机安排（无终态时），以便崩溃不丢整天。
18. 作为微信读书账号持有者，我希望指定多本候选书、每天随机取一本完成当天阅读，以便控制阅读对象。
19. 作为微信读书账号持有者，我希望未指定书籍时从真实 Shelf 自动选书，以便无需手动维护书单。
20. 作为微信读书账号持有者，我希望自动选书优先"未读完且能建立 Reader Context"的书，以便优先正常的阅读对象。
21. 作为微信读书账号持有者，我希望没有可用未读完书时回退到已读完但可用的书，以便书架只剩已读毕书籍时仍能运行。
22. 作为微信读书账号持有者，我希望显式指定的书不因 progress=100% 被排除，以便我指定的书总是可用。
23. 作为微信读书账号持有者，我希望自动选书对候选做有界探测（元数据过滤 + Reader Context 验证），以便不会无限尝试。
24. 作为微信读书账号持有者，我希望 Task 使用真实 Reading Progress 与 Reader Context 构造上报，以便不依赖抓包静态模板。
25. 作为微信读书账号持有者，我希望阅读位置固定不推进，以便只累计时长、不伪造阅读进度。
26. 作为微信读书账号持有者，我希望每个 Reading Session 以 enter report 开始、再进入周期 timed report，以便贴近真实 Web Reader 行为。
27. 作为微信读书账号持有者，我希望每次 timed report 的 `rt` 为距上次成功接受上报的实际间隔（非累计），以便累计时长由本地汇总得到。
28. 作为微信读书账号持有者，我希望正常的网络延迟/短暂重试可并入下一次 `rt`，以便偶发抖动不影响会话连续性。
29. 作为微信读书账号持有者，我希望超过内部阈值的异常间隔不作为一次大 `rt` 上报，而是重建 Reading Session（Task 继续、累计保留），以便主机 suspend 等不会产生虚假的大时长上报。
30. 作为微信读书账号持有者，我希望 Reader Context 到期（参考默认约 15 分钟）时主动刷新，以便长阅读会话平稳持续。
31. 作为微信读书账号持有者，我希望 timed report 被拒时按"刷新 Context → 重试 → renewal → 刷新 Context → 重试"的有界顺序恢复，以便短暂故障可自愈。
32. 作为微信读书账号持有者，我希望有界恢复耗尽后 Task 判定失败并形成 failed 终态，以便不无限重试。
33. 作为微信读书账号持有者，我希望判定 Login Session 失效时先尝试从初始 Cookie 重建一次，以便更新一次 Cookie 即可自愈。
34. 作为微信读书账号持有者，我希望重建也失败时发出明确的登录失效通知（提示更新初始 Cookie），以便我知道下一步动作。
35. 作为收到通知的用户，我希望 Task 成功时收到包含计划/实际时长、report 次数、书名的通知（Bark 和/或企业微信），以便确认当天阅读完成。
36. 作为收到通知的用户，我希望 Task 失败时收到包含失败阶段、主要错误、已尝试恢复动作的通知，以便判断是暂时性还是配置问题。
37. 作为部署者，我希望通知渠道按配置独立启用（Bark / 企业微信 / 两者 / 都无），以便按需接入。
38. 作为部署者，我希望通知发送失败不影响 Task 结果与终态，以便通知渠道故障不破坏阅读任务。
39. 作为部署者，我希望终态先落盘再发通知，以便通知期间重启不会导致当天重复执行。
40. 作为部署者，我希望 Task success 终态下 `weread-cron run` 拒绝重复执行（V1 无 force），以便不会意外双跑。
41. 作为部署者，我希望 Task failed 终态下可以用 `weread-cron run` 手动重试并更新当天结果为 success，以便人工修复后当天不空过。
42. 作为部署者，我希望当天无终态时 `weread-cron run` 立即执行完整 Task（不受窗口限制、正常随机时长），以便随时手动触发。
43. 作为部署者，我希望已有 Task 运行时第二个 Task（自动或手动）被拒绝，以便不会并发上报。
44. 作为部署者，我希望 `weread-cron books` 输出书架 bookId 与 title，以便选择 `WEREAD_CRON_BOOKS` 和验证 Cookie/书架访问。
45. 作为部署者，我希望 `weread-cron books` 使用与 daemon 相同的 Login Session 恢复机制并可正常 renewal/持久化，以便查询结果真实可靠。
46. 作为部署者，我希望 `weread-cron books` 不产生 Task、不读取或修改终态、不发送通知，以便它是纯查询工具。
47. 作为开发者，我希望协议实现（payload、签名）以独立源码调查为依据并明确标注未验证项，以便不被参考项目的静态 workaround 带偏。
48. 作为开发者，我希望内部参数（report 节奏、异常阈值、重试次数、UA、超时、日志级别、Context TTL 默认值）不暴露为配置，以便保持配置面最小。

## Implementation Decisions

1. **二进制与子命令**：`weread-cron`（无子命令 = daemon）、`weread-cron run`、`weread-cron books`；`--help` 与未知子命令/flag 以非零退出码报错。二进制名同 ADR-0005。
2. **用户可见配置（全部环境变量，前缀 `WEREAD_CRON_`）**：`WEREAD_CRON_COOKIE`（初始 Cookie header 字符串）、`WEREAD_CRON_BOOKS`（逗号分隔 bookId，可选）、`WEREAD_CRON_RUN_WINDOW_START`/`_END`（HH:MM）、`WEREAD_CRON_READ_MINUTES_MIN`/`_MAX`、`WEREAD_CRON_BARK_URL`、`WEREAD_CRON_WECOM_WEBHOOK_URL`（均可选，按配置启用）、`WEREAD_CRON_DATA_DIR`（默认 `/data`）、`TZ`（默认 `Asia/Shanghai`）。校验失败即启动失败：窗口 `start>end` 且不相等非法（`start==end` = 固定时刻）、`min>max` 非法、首次启动无 Cookie 快速失败。内部参数一律不暴露（ADR-0005）。
3. **模块职责（按职责划分，不指定文件路径）**：config（解析+校验）；session（Cookie Jar 管理、序列化/恢复、原子持久化）；weread protocol（`_e` 编码、`sg`/`s`/appId 生成、payload 构造）；reader context（抓取 `__INITIAL_STATE__`、TTL 缓存、强制刷新）；report（enter/timed 发送与成功判定）；task orchestrator（Task 编排、恢复链、异常间隔重建 Reading Session）；scheduler（`nextStart` 决策、daemon 循环、终态门控）；notify（Bark、企业微信机器人，渠道独立）。
4. **依赖注入与应用边界（ADR-0006）**：clock、RNG、HTTP client/base URL、session store、notifier 均经构造器注入；生产 `main` 组装真实实现。clock/RNG 是领域真实依赖（随机化是产品特性），不是 test hook。
5. **协议契约（基于两参考项目源码调查，实施时独立复核，未经实测项不视为事实）**：
   - timed report payload：`appId, b, c, ci, co, sm, pr, rt, ts, rn, sg, ct, ps, pc, s`；enter report 为位置字段 + `s`，**不含** `rt/ts/rn/sg`（已由两项目源码确认）。
   - `sg = sha256(ts + rn + token)`，token 优先 `reader.token`；固定值 `3c5c8717f3daf09iop3423zafeqoi` 仅是参考实现的兼容 fallback，不作为核心协议假设。
   - `s` = 对除 `s` 外按键排序的 `key=urlencode(value)` 连接串做 `0x15051505` 滚动哈希、hex 小写；精确移位/边界细节需独立复核。
   - `b`/`c` = `_e(bookId)`/`_e(chapterUid)`（MD5 派生的自定义编码，非 base36）；`pc` 缺失时 = `_e(ct)`；`appId` 由 UA 生成（`web_app_id`）。
   - 成功判定：响应含 `succ==1` 或 `synckey` 即视为接受；`rt` 语义按 ADR-0004（每次上报的实际间隔、非累计、异常间隔重建会话）。
   - renewal：`POST /web/login/renewal`，body `{"rq":"%2Fweb%2Fbook%2Fread","ql":false}`，成功 `{"succ":1}`；Set-Cookie 并入 Cookie Jar（`wr_*` 等）并原子持久化；renewal 节流为内部默认（参考 10 分钟 cooldown）。
   - Reader Context TTL 默认 ≈15 分钟：**仅参考实现默认值，不是已确认的服务器协议常量**，正式复核可修正（表述同 ADR-0004）。
6. **Task 编排**：renew → 选书（指定集合随机；自动 = Shelf 元数据过滤优先未读完 → 有界探测 Reader Context → 回退已读完可用书）→ 读真实进度（位置不推进）→ 建/刷新 Reader Context → enter report → 循环 timed report（节奏 ~30s 内部默认；`rt` = 距上次成功接受的实际间隔；异常间隔超内部阈值 → 重建 Reading Session，Task 继续、累计保留）→ 本地汇总有效 `rt` 达标 → 终态落盘 → 通知 → 排次日。
7. **恢复链（有界）**：report 被拒 → refresh Reader Context → retry → 仍败则 renewal → refresh Reader Context → retry → 仍败 → failed 终态。Login Session 失效仅凭明确证据判定（如 renewal 失败），判别特征清单实施时实测确认；判定失效后从初始 Cookie 重建一次，仍失败 → 登录失效终态，通知明确提示更新初始 Cookie。
8. **调度（ADR-0001/0002）**：`nextStart(now, window, lastTerminal, tz)` 纯决策函数：窗口只约束开始时间；存在终态 → 明天；无终态且窗口未过 → `[now, 窗口结束]` 随机；窗口已过 → 明天；不补跑；`start==end` 固定时刻。daemon 在任务实际启动前再次校验当天终态。
9. **持久化（/data）**：Login Session 序列化（Cookie 属性完整保存）+ Terminal State `{last_task_date, last_task_result}`；均原子写（临时文件 + rename）；终态先落盘、再发通知。
10. **通知内容**：成功 = 完成、计划时长、实际累计时长（本地汇总 `rt`）、report 次数、书名；失败 = 失败阶段、主要错误、已尝试恢复（refresh/renewal）；登录失效 = 明确提示更新初始 Cookie 的固定文案。通知发送失败不影响 Task 结果；渠道互不影响。
11. **并发**：进程内单 Task 互斥（运行中拒绝第二个 Task，含 `run`）；多实例防重不在 V1 范围。
12. **CLI 行为**：`run` 不受 Run Window 限制、复用正常 Task 逻辑与终态规则（无终态 → 执行并形成终态；failed → 可重试更新为 success；success → 拒绝，无 force）；`books` 只读查询（可 renewal 并持久化 Login Session，不碰终态、不产生 Task、不通知）。
13. **内部默认值清单（不暴露配置）**：report 节奏 ~30s、异常间隔阈值（~90s）、重试次数、renewal 节流、Reader Context TTL 默认、UA、HTTP 超时、日志级别、通知重试。

## Testing Decisions

- **主 seam — 应用边界（ADR-0006）**：进程内使用生产代码，构造注入 clock、RNG、HTTP endpoints、session store、notifier。`httptest.Server` 扮演微信读书服务端（服务 Reader 页 `__INITIAL_STATE__`、progress、shelf、chapterInfos、`/web/book/read`、`/web/login/renewal`，可脚本化拒绝序列）；fake Bark/企业微信端点记录通知消息体；临时目录充当 `/data`。
- **次 seam — 薄 CLI**：子进程运行真实二进制，验证仅 executable 边界可观察的行为：配置解析/校验失败（exit code + stderr 文案）、子命令路由（`--help`、未知子命令）、daemon 启动失败（缺 Cookie、非法窗口）。不要求该层覆盖长 Task 与全部协议恢复路径。
- **纯逻辑**：`nextStart`、配置校验、终态规则等纯函数在应用 seam 内直接单元测试（注入 fake clock/RNG），不形成额外 seam。
- **好的测试标准**：只断言外部行为——线上收到的请求（payload 字段、重算的 `s`/`sg`、enter vs timed 字段集、`rt` 演进）、写入 `/data` 的文件内容（Login Session、Terminal State）、通知端点收到的消息体、进程 exit code 与 stdout/stderr；不断言内部函数调用或实现细节；通过注入 clock/RNG 保证确定性。
- **协议断言依据**：以已确认的设计/源码调查结论为准——`rt` 按 ADR-0004 语义验证（间隔演进、非累计、异常间隔 → 重建 Reading Session）；enter/timed 字段集差异；签名重算验证。Reader Context 约 15 分钟按"参考默认值"断言刷新行为，**不**作为服务器协议常量断言。
- **被测模块**：task orchestrator / report 状态机、scheduler 决策、session 持久化、config 校验、notify 内容——全部经主 seam；executable 边界经 CLI seam。
- **先例**：仓库内无（绿场项目）。概念先例为 `weread.koplugin` 的 `spec/read_report_progress_spec.lua`（对 fake client 断言 enter/report 时序与字段集），跨语言借鉴其断言思路。
- 不拆 `internal/weread` HTTP client、通知渠道等更低 seam，除非实施中出现应用 seam 无法清晰覆盖的逻辑。

## Out of Scope

V1 不做：Web UI、多账号、用户名密码登录、自动浏览器登录、自动抓浏览器 Cookie、二维码登录、数据库、HTTP API、Prometheus、EPUB 下载、完整书籍正文获取、内容解密、阅读器、模拟翻页、主动推进阅读进度、Reading Session 内换书、复杂推荐算法、完整 cron expression、补跑、断点续跑、`run --force`、多实例防重、官方 Skill/API Key 认证、公众号/MP 文章、Telegram/PushPlus/WxPusher/ServerChan 等其他通知平台、复杂配置文件与环境变量双配置体系。

## Further Notes

- **实施前需实测验证的事项**（调研缺口，实施时逐项验证，未验证前采用保守默认）：纯 Cookie 可用的 Shelf 端点（调研仅确认需 API Key 的官方 Skill 网关）、已读完（progress=100%）书籍服务端是否接受并计入阅读时长、`wr_skey`/`wr_gid` 凭证偏好、`reader.token` 轮换行为、Login Session 失效的响应特征（登录失效判别清单）、`s` 哈希的边界行为、renewal 返回 Cookie 的持久化属性。
- **许可与独立性**：参考项目 `weread.koplugin`（AGPL-3.0-only）与 `findmover/wxread`（无 LICENSE）仅作独立源码调查依据，不复制、不衍生其代码。
- **决策记录**：本轮设计决策见 `docs/adr/0001–0006`；术语以 `CONTEXT.md` 为准；前期调研记录见 `docs/v1-requirements-and-research.md`（保持调研记录原貌，不以本文档覆盖）。
- **测试命名**：按 `docs/agents/domain.md` 的约定，测试与 issue 命名使用 `CONTEXT.md` 术语表词汇（Task、Reading Session、Terminal State、Login Session、Reader Context、Enter/Timed report、Run Window、Target Duration、Shelf、Reading Progress）。