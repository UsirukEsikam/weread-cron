# 跨进程 Task 互斥：/data 锁文件 + flock

同一 deployment（同一 `/data` 目录）内，daemon 与 `weread-cron run`/`books` 是独立操作系统进程，各自持有独立的进程内 `sync.Mutex`：CLI 每次命令调用都新建一个 App 实例。Task 运行期间不落终态，第二个进程同样能通过终态门控，导致同一 deployment 出现两个并发 Task（并发 renewal、并发 Reading Session、重复定时上报、Login Session/Terminal State 并发写）。V1 设计（spec 决策 #11）要求"已有 Task 运行时第二个 Task 被拒绝"——进程内互斥只覆盖了同一 App 实例内的并发。

本 ADR 记录跨进程互斥的载体取舍（ticket 11 实施说明）。

## 决策

并发守卫升级为两层（`internal/app/app.go` 的 `acquireTaskGuard`）：

1. **进程内守卫**（保留 `runMu sync.Mutex` + `TryLock`）：同一 App 实例内第二个调用零文件 I/O 立即拒绝——保持 ticket 08 的全部进程内语义与回归测试。
2. **跨进程守卫**：`/data/task.lock` 锁文件 + `flock(2)`（`LOCK_EX|LOCK_NB`，非阻塞）。锁随 open file description 的生命周期由内核管理：持有进程正常退出**或崩溃**（含 SIGKILL）时自动释放，不存在"残留锁导致永久卡死"。

任一守卫被持有 → `ErrTaskRunning`（非阻塞、不等待、不中断运行中的 Task）。`App.ListBooks` 与 Task 共用同一守卫（避免与运行中 Task 的会话写竞争）。跨进程锁获取的非 `ErrLocked` 失败（如 `/data` 不可写）→ 明确错误、保守失败：无法保证互斥时不得执行 Task。作用范围严格按 `/data` 隔离：不同 deployment（不同 `/data`）互不干扰；锁文件常驻 `/data`、内容不使用、不随释放删除（flock 只需要 inode）。

## 取舍

| 载体 | 结论 | 理由 |
| --- | --- | --- |
| `flock(2)`（LOCK_EX \| LOCK_NB） | **采用** | 内核自动释放（进程退出/崩溃即释放，无陈旧锁判定）；非阻塞语义与 TryLock 一致；锁绑定 open file description，同一进程内两次独立 `open` 也互相冲突（进程内回归测试依赖同一内核机制）；无第三方依赖；Linux（Docker 目标）与 macOS（开发机）语义一致。 |
| `O_CREATE\|O_EXCL` 锁文件 + 删除 | 否决 | 文件是"锁状态"本身，进程崩溃后文件残留 → 永久卡死；需要额外陈旧锁判定（PID liveness），复杂且不可靠。 |
| fcntl/POSIX 记录锁 | 否决 | 语义复杂（按进程合并/释放，fork/exec 行为特殊），对"整把锁互斥 + 进程死亡释放"的需求没有收益。 |
| 锁文件 + 写入 PID 协商 | 否决 | 需要轮询与超时判定，且无法在崩溃时可靠清理。 |

## 边界

- 锁只覆盖**单个 Task 的执行期**：daemon 的睡眠等待期不持锁（daemon 与手动 run 在同一 deployment 内可共存，仅 Task 本身互斥）。
- 分布式/多实例（跨主机的多 deployment）协调不在 V1 范围（spec 决策 #11 的"多实例防重"边界不变）——`flock` 只保证单机同一文件系统内的互斥。
- daemon 到点执行被跨进程守卫拒绝时按"窗口内重排"处理（Info 级日志，非失败语义）；运行中的手动 Task 不受影响。