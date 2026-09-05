# 03: 最小 Task happy path

**What to build:** `weread-cron run` 在配置指定候选书（`WEREAD_CRON_BOOKS`）下完整跑通一次 Task 直到成功：初始 Cookie 建立 Login Session → 任务开始 renewal → 读真实 Reading Progress → 抓取 Reader 页建立 Reader Context → enter report → 周期 timed report（`rt` 为距上次成功接受的实际间隔、本地累计）→ 达到当天随机 Target Duration → success 终态落盘 → 成功通知 → stdout 摘要、exit 0。应用 seam（注入 clock/RNG/HTTP/notify/session store + fake WeRead server + fake 通知端点 + 临时 /data，ADR-0006）随本票建立。

**Blocked by:** 01, 02

**Status:** ready-for-agent

- [ ] fake server 线上断言：enter → timed 时序、payload 字段集、`s`/`sg` 可重算验证
- [ ] `rt` 按 ADR-0004：每次 timed report = 距上次成功接受的实际间隔，非累计；本地汇总在达标时停止
- [ ] renewal 请求在 Task 开始时发出且新 Cookie 并入会话
- [ ] Target Duration 在配置范围内随机且一次 Task 只生成一次（seeded RNG 下确定性）
- [ ] 达标后 success 终态先落盘、再发成功通知（计划/实际时长、report 次数、书名）
- [ ] 成功通知经 fake Bark/企业微信端点断言内容
- [ ] 通知端点故障不影响 Task 结果与终态
- [ ] `run` 退出码 0，stdout 摘要含关键信息