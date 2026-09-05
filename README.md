# weread-cron

微信读书自动阅读服务：单账号、Docker 常驻、长期无人值守地累积微信读书阅读时长。
每天在可配置的 Run Window 内随机时刻启动一次 Task（renew → 选书 → 真实进度上报 →
本地累计达标 → 落盘 Terminal State → 通知），失败自动恢复、当天失败可手动重试，不发
送虚假阅读进度（位置固定不推进，仅累计时长）。协议实现基于独立源码调查（见
[docs/protocol-validation-checklist.md](docs/protocol-validation-checklist.md)）。

## 子命令

| 命令 | 说明 |
| --- | --- |
| `weread-cron`（无参数） | daemon 常驻：每天在 Run Window 内随机安排一次 Task |
| `weread-cron run` | 手动执行当天 Task（不受窗口限制；当天 success 终态拒绝重复执行） |
| `weread-cron books` | 列出当前 Shelf 的 `bookId` 与 `title`（每行一条，制表符分隔） |

配置全部通过环境变量提供（前缀 `WEREAD_CRON_`，见 ADR-0005），无配置文件。

## 部署（Docker Compose 示例）

镜像发布在 GHCR：`ghcr.io/<你的 GitHub 用户名>/weread-cron`，为多架构清单
（`linux/amd64` + `linux/arm64`），Apple Silicon、x86_64 Linux 均自动拉取对应架构。

```bash
# 1. 复制环境变量示例并填写（至少 WEREAD_CRON_COOKIE，见下节）
cp .env.example .env
# 2. 把 docker-compose.yml 中 image 的 OWNER 替换为你的 GitHub 用户名/组织
# 3. 启动（首次自动拉取镜像）
docker compose up -d
# 4. 查看日志确认 daemon 启动（配置非法会在启动时快速失败）
docker compose logs -f
```

手动触发当天 Task（无需进入容器）：

```bash
docker compose exec weread-cron /weread-cron run
```

> 注意：镜像基于 scratch，**不含 shell**，`exec` 必须给出完整二进制路径
> （`/weread-cron`），不能像常规镜像那样直接执行 `sh`。

### 首次 Cookie 配置

`WEREAD_CRON_COOKIE` 是微信读书的 Cookie header 字符串（形如
`wr_vid=1111111; wr_skey=abcdef...; wr_gid=123456`），是登录态的初始种子：

1. 浏览器登录 <https://weread.qq.com>；
2. 打开开发者工具（F12）→ Network，刷新页面，选择任一指向 `weread.qq.com`
   的请求；
3. 在请求头中找到 `Cookie:` 字段，复制整段值；
4. 填入 `.env`：

   ```bash
   WEREAD_CRON_COOKIE='wr_vid=1111111; wr_skey=abcdef...; wr_gid=123456'
   ```

   值内含 `;`、`=`、`#` 等字符时务必用单引号包裹整行（compose 的 env_file 解析）。

5. `docker compose up -d`（或 `docker compose restart weread-cron`）生效。

首次启动未提供 Cookie 且 `/data` 无持久化 Login Session 时，进程快速失败（exit 1），
stderr 明确提示设置 `WEREAD_CRON_COOKIE`。登录态在运行期间由 renewal 自动续期并
持久化，日常无需再次提供 Cookie；若收到「登录已失效」通知，更新 `.env` 中的
Cookie 后重启即可自愈。

### `/data` 卷要求

- `/data` 是**必需**的持久卷，存放：
  - `login_session.json` — Login Session（完整 Cookie 集合，renewal 后原子更新）；
  - `terminal_state.json` — Terminal State（当天 Task 结果：`success`/`failed`）。
- 容器重启自动从 `/data` 恢复 Login Session，无需重新提供 Cookie（用户故事 #7）；
  终态先落盘再通知，保证重建后不会重复执行当天任务（用户故事 #39）。
- **卷丢失的后果**：Login Session 回退为无，重启时再次要求提供
  `WEREAD_CRON_COOKIE`；当天终态丢失，daemon 可能在当天剩余窗口重新安排一次（行为
  可接受但不预期）。请勿把 `/data` 挂载到临时目录（如容器重建即清空的路径）。
- 目录需可写：容器内以 root 运行，挂载到 `./data`（compose 示例）即可读写。
  使用其他挂载源时请保证写权限，否则启动后持久化会报错。

## 配置清单

| 环境变量 | 必填 | 默认 | 说明 |
| --- | --- | --- | --- |
| `WEREAD_CRON_COOKIE` | 首次部署 | 无 | 微信读书 Cookie header 字符串 |
| `WEREAD_CRON_BOOKS` | 否 | 无 | 候选 bookId 逗号分隔；留空 = 自动从 Shelf 选书 |
| `WEREAD_CRON_RUN_WINDOW_START` | 是 | 无 | Run Window 开始，`HH:MM` |
| `WEREAD_CRON_RUN_WINDOW_END` | 是 | 无 | Run Window 结束，`HH:MM`；`start==end` = 固定启动时刻 |
| `WEREAD_CRON_READ_MINUTES_MIN` | 是 | 无 | Target Duration 随机下限（分钟） |
| `WEREAD_CRON_READ_MINUTES_MAX` | 是 | 无 | Target Duration 随机上限（分钟） |
| `WEREAD_CRON_BARK_URL` | 否 | 无 | Bark 通知地址（配置即启用） |
| `WEREAD_CRON_WECOM_WEBHOOK_URL` | 否 | 无 | 企业微信机器人 Webhook 地址（配置即启用） |
| `WEREAD_CRON_DATA_DIR` | 否 | `/data` | 持久化目录 |
| `TZ` | 否 | `Asia/Shanghai` | 一切时间语义的时区（二进制内嵌 tzdata，任意 IANA 时区名） |

约束（校验失败即启动失败，exit 1）：窗口 `start>end` 且不相等非法；`min>max` 非法；
时间格式非法。内部参数（report 节奏、重试次数、UA 等）不暴露为配置。

## 镜像

- 极简基础：scratch + 静态二进制（`CGO_ENABLED=0`）+ CA 证书；时区数据库内嵌于
  二进制（ADR-0003），因此 `TZ` 可覆盖（默认 `Asia/Shanghai`），镜像体积小且无系统
  依赖，主机为任意 Linux/多架构均可运行。
- 镜像内二进制即完整 V1（daemon / run / books 全部子命令），每次发布为当前 main
  全量代码。
- 构建与发布（`.github/workflows/release.yml`）：test 全绿 → buildx 构建
  `linux/amd64,linux/arm64` → 推送 GHCR；`main` 分支推送 `latest`，`v*` tag 推送版本
  标签。镜像路径为 `ghcr.io/<仓库 owner>/<仓库名>`（工作流由仓库推导，无需配置）。
- 不含 shell 与调试工具；排查问题用 `docker compose logs`，或
  `docker run --rm ghcr.io/<owner>/weread-cron:latest --help` 查看用法。

## 本地构建与测试

```bash
# 构建二进制（开发）
go build ./cmd/weread-cron

# 测试（仓库要求：应用 seam + CLI seam + 纯逻辑单测，主 seam 见 ADR-0006）
go vet ./...
go test ./...

# 构建本地镜像
docker build -t weread-cron:local .
```
