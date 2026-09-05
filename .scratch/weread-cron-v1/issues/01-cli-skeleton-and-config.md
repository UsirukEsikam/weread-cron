# 01: CLI 骨架与配置校验

**What to build:** weread-cron 可执行文件可启动并按子命令路由（无子命令 = daemon、`run`、`books`、`--help`）；`WEREAD_CRON_*` 环境变量完整解析与校验；非法配置以非零退出码 + 明确 stderr 消息失败；首次启动缺 Cookie 快速失败；go vet/test 最小 CI 就绪。`run`/`books` 在本票只是路由占位，真实行为由后续票交付。

**Blocked by:** None（可立即开始）

**Status:** resolved

- [x] `weread-cron --help` 与未知子命令/flag 输出清晰用法并以非零退出码结束
- [x] 配置校验失败即启动失败：窗口 `start>end` 且不相等（`start==end` 合法 = 固定时刻）、时长 `min>max`、时间/时区格式非法——stderr 指明具体违规项
- [x] 首次启动（无持久化 Login Session）且未配置初始 Cookie → 启动失败并明确提示
- [x] 合法配置下 daemon/run/books 三个路由可进入各自占位路径（不崩溃）
- [x] CI（最小集：go vet + go test）全绿
- [x] 校验与退出码行为经真实二进制（薄 CLI seam）验证

## Answer

实现（连同一并确立了 Go 工程骨架）：

- `cmd/weread-cron/main.go`（薄 main：信号 ctx + cli.Run；`_ "time/tzdata"` 按 ADR-0003）
- `internal/cli`：子命令路由（无子命令 = daemon、`run`、`books`）、用法输出、退出码（0 成功 / 1 配置与启动前置失败 / 2 用法错误）、缺 Cookie 且无持久化 Login Session 时的快速失败提示（Login Session 存在性检查经 `session.Store` 注入）
- `internal/config`：全部 `WEREAD_CRON_*` + `TZ` 解析校验（窗口 HH:MM 与 start>end 规则、时长 min>max、时区、URL、整体为空 BOOKS = 自动选书）；违规项聚合为多行清单、逐项指明
- `internal/scheduler`：daemon 占位路径（校验通过后阻塞至信号，ticket 07 交调度循环）
- `internal/session`：最小 `Store.HasLoginSession`（文件存在性），完整 Login Session 持久化由 ticket 04 交付
- 测试：config 纯逻辑表驱动、cli 进程内路由、真实二进制 seam（`cmd/weread-cron/seam_test.go`，TestMain 构建二进制，覆盖 usage/退出码/非法配置/缺 Cookie 快速失败/daemon 启停/占位路由）
- `.github/workflows/ci.yml`：最小 CI（go vet + go test），`.gitignore`

执行约定（按 spec 逐条落实）：`--help` 以非零退出码结束（spec Implementation Decisions #1 明确）；run/books 为占位路由，明确报“尚未实现”并以退出码 1 结束，不假装成功。

经 `/code-review` 双轴审查后修正：BOOKS 整体为空视为自动选书而非配置错误（User Story 19）；go.mod 降到 1.22（无更高特性需求，避免 CI 工具链风险）；测试命名改用术语表“Login Session”；修复 `-race` 数据竞争。