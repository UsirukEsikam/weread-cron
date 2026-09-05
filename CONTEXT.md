# WeRead Cron（微信读书自动阅读）

单账号、Docker 常驻、自动累计微信读书阅读时长的轻量 Go 工具。每天在配置窗口内随机启动一次阅读任务，使用真实 Reader/Progress 状态上报阅读时长。

## Language

**Task**（任务）:
一天的完整工作单元：在运行窗口内随机启动（或由 `weread-cron run` 手动触发）、选书、建立阅读会话、周期上报直至达成目标时长、通知、排定次日。一天最多一个终态；终态形成后当天不再自动执行。
_Avoid_: job、run、定时任务

**Terminal State**（终态）:
Task 结束时落盘的最小记录：`last_task_date + last_task_result`（`success` 或 `failed`）。success 与 failed 都阻止当天再次自动执行；failed 允许 `weread-cron run` 手动重试并更新为 success。

**Reading Session**（阅读会话）:
单本书从 enter report 到结束的连续上报周期。一个 Task 只有一个 Reading Session，只使用一本书。
_Avoid_: session（裸用，有歧义）

**Login Session**（登录会话）:
可跨重启持久化/恢复的微信读书登录凭据（完整 Cookie 集合，含 `wr_skey`/`wr_gid` 等）。
_Avoid_: session（裸用）

**Reader Context**:
从 Web Reader 页面 `window.__INITIAL_STATE__` 提取的阅读器状态（bookId、`reader.token`、`psvts`、`pclts`、当前章节、progress 等），用于构造上报 payload；TTL 约 15 分钟，过期需重新抓取。
_Avoid_: reader state、initial state

**Enter report**（进入上报）:
Reading Session 开始时发送的一次 `/web/book/read` 上报，携带位置状态，不含计时字段（`rt`/`ts`/`rn`/`sg`）。

**Timed report**（周期上报）:
进入上报之后按节奏（约 30 秒）发送的上报，携带计时字段（`rt`/`ts`/`rn`/`sg`），用于累计阅读时长。
_Avoid_: report（裸用）

**Run Window**（运行窗口）:
每日可随机启动 Task 的时间区间；只约束开始时间，不约束结束时间。

**Target Duration**（目标时长）:
每次 Task 开始时在配置的 [min, max] 区间内随机生成的当天阅读目标；只在任务开始时生成一次。

**Shelf**（书架）:
当前账号的书籍集合；自动选书的来源。
_Avoid_: bookshelf

**Reading Progress**（阅读进度）:
单本书的阅读位置状态：chapterUid、chapterIdx、chapterOffset、progress(0–100)、summary 等。
