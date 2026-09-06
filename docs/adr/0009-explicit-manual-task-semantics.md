# 显式手动 Task 语义、参数化与终态防降级

明确 `weread-cron run` 为用户显式发起的按需 Task，引入参数化时长与可选书籍，解耦当天终态门控，确立终态防降级规则。本 ADR 澄清并部分取代（supersede）ADR-0002 中关于 `weread-cron run` 的终态规则决策。

## 背景

ADR-0002 曾规定：手动 `weread-cron run` 与自动执行共用同一终态规则——今天已有 success 终态时拒绝重复执行，且运行时沿用配置的随机 Target Duration 与选书配置。

在实际使用中，显式调用 `weread-cron run` 代表用户明确发起的即时阅读需求。现有的"当天已成功则跳过"门控剥夺了用户的按需执行能力；随机时长无法满足特定的阅读时长需求；且未能指定特定书籍阅读。

## 决策

1. **显式 CLI 接口与参数化**：
   - 用法：`weread-cron run --minutes <N> [--book <bookId>]`。
   - `--minutes` 为必需正整数（`N > 0`），缺失、非整数或非正数直接作为用法错误退出（ExitUsage = 2）。
   - `--minutes` 设定本次 Task 的精确 Target Duration，覆盖配置的 `[ReadMinutesMin, ReadMinutesMax]` 随机范围。
   - `--book` 为可选参数。指定时直接使用该书籍 ID，绕过配置的候选书列表与书架自动选书；未指定时保留原有的候选配置或书架自动选书回退。

2. **按需独立执行**：
   - 手动 Task 不受 Run Window 约束，立即启动。
   - 手动 Task 不受当天已形成的 Terminal State（无论是 success、failed 还是无）门控阻拦。
   - 并发互斥严格保留：若同一 deployment 内已有 Task 运行中（进程内守卫或跨进程 flock），手动 Task 立即被拒绝（ExitRunRejected = 3），不中断运行中的 Task。

3. **终态参与与防降级规则（Anti-Downgrade Invariant）**：
   - 手动 Task 依然参与每日 Terminal State 的持久化：
     - 手动 Task 成功：记录或保留当天的 `success` 终态。
     - 手动 Task 失败：仅在当天**尚无** `success` 终态时记录为 `failed`；若当天已存在 `success` 终态，持久化终态保持 `success`（绝不降级）。
   - 通知与退出状态忠实反映单次执行结果：
     - 不论终态是否防降级，配置的通知渠道（Bark/企业微信等）如实发送本次手动 Task 的成功或失败通知。
     - CLI 调用方如实接收结果：成功时输出摘要并以 0 退出；失败时输出错误并以 ExitConfig (1) 退出。

4. **对自动调度的影响**：
   - daemon 自动调度的终态门控逻辑保持不变。若当天因手动 Task 成功形成了 `success` 终态，daemon 在到点启动前检查到终态，当天不再执行自动任务。

## 取代关系

- **部分取代 ADR-0002**：取消"已有 success 终态时拒绝 `weread-cron run` 执行"的限制。ADR-0002 中关于"跨重启无状态、不续跑、不补跑、自动调度受当天终态约束"的决策保持有效。
