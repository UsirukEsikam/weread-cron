# 08: run 终态规则与并发守卫

**What to build:** `weread-cron run` 与 Terminal State 的交互规则：当天无终态 → 执行并形成终态；已有 success 终态 → 拒绝重复执行（V1 无 force）；已有 failed 终态 → 允许手动重试，成功后当天结果更新为 success；Task 运行中 → 拒绝并发启动第二个 Task。failed 终态是 run 的合法输入状态，测试可直接 seed 持久化终态文件，无需经由 T5 的真实失败路径。

**Blocked by:** 03

**Status:** resolved

- [x] 无终态 → run 完整执行并写 success 终态
- [x] success 终态 → run 拒绝，stdout 说明原因并返回约定退出码
- [x] failed 终态（seed）→ run 重试成功 → 终态更新为 success
- [x] run 执行中第二个 Task（run 或 daemon 触发）被拒绝
- [x] 无 force 选项
- [x] run 不受 Run Window 限制（任意时刻可执行）

## Answer

实现「run 终态规则与并发守卫」（spec 决策 #11/#12；用户故事 #40–#43），两处新代码面 + 两个测试面：

1. **终态规则门控 + 并发守卫（internal/app/app.go）**：`RunTask` 前置 `runMu.TryLock()`（进程内单 Task 互斥，spec 决策 #11；锁未获得方立即返回 `ErrTaskRunning`，不等待、不中断运行中的 Task）→ 锁内执行 `checkTerminalGate`：当天（cfg.TZ）已有 success 终态 → `ErrTerminalSuccess`（锁内校验避免并发双过门）；failed/无/昨日终态放行；终态不可读时保守失败。新增 `Deps.Terminal` 注入（零值 = 文件存储，同 Sessions 模式）。
2. **进程级呈现（internal/cli/cli.go）**：新增约定退出码 `ExitRunRejected = 3`；success 拒绝 → 原因输出到 **stdout**（issue 验收口径）+ 退出码 3；运行中拒绝 → stderr + 退出码 3；无 force 选项（`run --force` 落入"不接受额外参数"用法错误，ExitUsage）。
3. **终态日期匹配去重（internal/terminal）**：日期格式 `DayLayout` 与 `TodayKey`/`IsToday` 谓词收归 terminal 包（终态日期匹配的唯一定义），scheduler（nextStart 门控、todayTerminal、日志日期）与 task（终态/通知日期落盘）改用共享定义——修复 review 发现的 app/scheduler 双处实现。
4. **Test 面**：应用 seam（internal/app/app_test.go）直接 seed 终态文件（issue 口径：failed 终态不经 T5 失败路径）——无终态执行、success 拒绝（零网络请求/零通知/终态不被改写）、failed 重试成功更新为 success、昨日终态不约束、损坏终态保守失败、并发第二个 Task 拒绝（blockTimed 挂起首 Task 后 TryLock 立即拒绝、首个 Task 不被中断、完成后第三个调用落入终态规则）、窗口已过仍执行（run 不受 Run Window 限制）；CLI seam（internal/cli/cli_test.go）——退出码/输出方（stdout vs stderr）、`--force` 用法错误、run 忽略窗口；terminal 包单测覆盖 `IsToday`/`TodayKey` 的 TZ 日期边界。`TestRunRestartRestoresRenewedSession` 改为次日重启（同天 success 终态下再 run 恰是终态规则的拒绝场景）。

### 范围说明

并发守卫为**进程内**单 Task 互斥（spec 决策 #11：多实例防重不在 V1 范围）；daemon 与 run 生产上是独立进程，跨进程不互斥，符合规格。daemon 路径不受影响：它已有启动前终态再校验（ticket 07），App 层门控是防御性复检（daemon 误收 `ErrTerminalSuccess` 时按"未形成终态重排"循环处理，下一轮即排次日）。

**Commit:** a5af6c2
