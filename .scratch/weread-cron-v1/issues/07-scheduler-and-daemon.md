# 07: 调度与 daemon

**What to build:** 每日调度闭环：`nextStart(now, window, lastTerminal, tz)` 纯决策函数（Run Window 只约束开始时间、终态门控、`[now, 窗口结束]` 重排、错过不补跑、`start==end` = 固定时刻）+ daemon 循环（恢复 Login Session → 计算下次启动 → 睡眠到点 → 启动前再校验当天终态 → 执行 Task → 排次日）。TZ 由内嵌 tzdata + 环境变量决定（ADR-0003）。daemon 启动失败路径经薄 CLI seam 验证。

**Blocked by:** 03

**Status:** resolved

- [x] `nextStart` 决策正确：窗口只约束开始、终态（success/failed）存在 → 明天、无终态且窗口未过 → `[now, 结束]` 内随机、窗口已过 → 明天、`start==end` 固定时刻、跨午夜窗口为配置错误
- [x] daemon 在注入 clock 下到点执行 Task，完成后写终态并排定次日
- [x] 当天已有终态（seed 文件）→ daemon 不自动执行
- [x] 启动时当天窗口未过且无终态 → 当天剩余窗口内随机安排一次
- [x] 错过的整天不补跑
- [x] 缺 Cookie / 非法配置下 daemon 启动失败（薄 CLI seam：exit code + stderr）
- [x] TZ 生效：默认 Asia/Shanghai，`TZ` 可覆盖

## Answer

实现「每日调度闭环」（spec 决策 #8/#9；ADR-0001/0002/0006），两处新代码面 + 三个测试面：

1. `NextStart` 纯决策函数（internal/scheduler/nextstart.go）：窗口只约束开始、终态门控（success/failed → 次日）、[now, 窗口结束] 内随机、窗口已过 → 次日（不补跑）、start==end 固定时刻、跨午夜窗口返回错误（config 前置校验的防御性复检）、TZ 决定日期边界。表驱动单测直接覆盖全部分支（含同种子确定性）。
2. daemon 主循环（internal/scheduler/scheduler.go，`Daemon.Run`）：恢复 Login Session（失败即启动失败——空/损坏会话文件不再拖到 02:00）→ 计算下次启动 → 分片睡眠到点（`sleepChunk=2s`，SIGTERM 取消延迟 ≤ 2s）→ 启动前再校验当天终态（睡眠期间被 run 手动执行则跳过）→ 执行 Task（TaskRunner 注入；终态写与通知由 Task 自身完成，spec 决策 #9）→ 排定次日。暂时性失败（无终态）经 `replanMinGap=1min` 后在剩余窗口内重排——避免窗口末尾 `next==now` 的紧循环；failed 终态 → 排次日（不重跑今天）。
3. Test 面：NextStart 纯函数单测；daemon 循环测试（`clock.Capped` 把睡眠钉在上限，取消/终态注入时序确定——覆盖终态跳过、错过不补跑、启动前再校验、暂时性失败重排、failed 终态排次日、取消干净退出、空会话启动失败）；应用边界集成测试（internal/app/daemon_test.go：真实 App + fake weread 服务端，断言线上请求、终态文件、成功通知、排次日）。
4. CLI：daemon 路径经 `makeApp` 装配 App 为 TaskRunner（daemon 与 run 复用同一应用装配），启动失败 exit code + stderr 由薄 CLI seam & cli 包测试验证（含空会话损坏文件的新失败路径）。

### 范围说明

未实现 ticket 08 内容（run 终态规则/并发互斥）：cli 层 `cmdRun` 注释仍显式延后到 ticket 08；`NextStart`/daemon 只门控自动执行。窗口「只约束开始时间」（任务可越过窗口结束点）由 daemon 循环不截断 Task 体现，无专门测试断言（Task 内时长累计已有 03/06 覆盖）。

**Commit:** a4faef2