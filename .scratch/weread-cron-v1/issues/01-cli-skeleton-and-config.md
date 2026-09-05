# 01: CLI 骨架与配置校验

**What to build:** weread-cron 可执行文件可启动并按子命令路由（无子命令 = daemon、`run`、`books`、`--help`）；`WEREAD_CRON_*` 环境变量完整解析与校验；非法配置以非零退出码 + 明确 stderr 消息失败；首次启动缺 Cookie 快速失败；go vet/test 最小 CI 就绪。`run`/`books` 在本票只是路由占位，真实行为由后续票交付。

**Blocked by:** None（可立即开始）

**Status:** ready-for-agent

- [ ] `weread-cron --help` 与未知子命令/flag 输出清晰用法并以非零退出码结束
- [ ] 配置校验失败即启动失败：窗口 `start>end` 且不相等（`start==end` 合法 = 固定时刻）、时长 `min>max`、时间/时区格式非法——stderr 指明具体违规项
- [ ] 首次启动（无持久化 Login Session）且未配置初始 Cookie → 启动失败并明确提示
- [ ] 合法配置下 daemon/run/books 三个路由可进入各自占位路径（不崩溃）
- [ ] CI（最小集：go vet + go test）全绿
- [ ] 校验与退出码行为经真实二进制（薄 CLI seam）验证