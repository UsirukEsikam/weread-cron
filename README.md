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

镜像发布在 GHCR：`ghcr.io/UsirukEsikam/weread-cron`（本仓库发布的部署镜像），为
多架构清单（`linux/amd64` + `linux/arm64`），Apple Silicon、x86_64 Linux 均自动
拉取对应架构。

```bash
# 1. 复制部署模板为本地 compose.yaml（本地部署状态，.gitignore 已忽略，不入 Git）
cp compose.yaml.example compose.yaml
# 2. 复制环境变量示例并填写（至少 WEREAD_CRON_COOKIE，见下节）
cp .env.example .env
# 3. 启动（首次自动拉取镜像；可先 docker compose config 校验配置）
docker compose up -d
# 4. 查看日志确认 daemon 启动（配置非法会在启动时快速失败）
docker compose logs -f
```

`docker compose` 默认读取 `compose.yaml`。模板更新后，将新模板与本地
`compose.yaml` diff 合并即可（本地修改保留）。

运行中的常驻 daemon 内手动触发当天 Task（无需进入容器）：

```bash
docker compose exec weread-cron /weread-cron run
```

列出当前 Shelf 的书（`bookId` 与 `title` 每行一条，用于挑选
`WEREAD_CRON_BOOKS` 候选书）：

```bash
# daemon 未启动时（一次性容器，ENTRYPOINT 已是 /weread-cron，命令执行完自动退出）：
docker compose run --rm weread-cron books
# daemon 已启动时（进入运行中容器，exec 不经 ENTRYPOINT，需完整二进制路径）：
docker compose exec weread-cron /weread-cron books
```

> 注意：镜像基于 scratch，**不含 shell**，`exec` 必须给出完整二进制路径
> （`/weread-cron`），不能像常规镜像那样直接执行 `sh`。`docker compose run`
> 则会经过镜像 ENTRYPOINT（已经是 `/weread-cron`），直接跟子命令名即可。

### 镜像更新

```bash
# 拉取最新镜像并重建容器（/data 卷不受影响，Login Session 与 Terminal State 保留）
docker compose pull
docker compose up -d
```

`docker compose up -d` 检测到镜像 ID 变化时自动 recreate 容器；如需强制重建可用
`docker compose up -d --force-recreate weread-cron`。容器重建后自动从 `/data`
恢复 Login Session，无需重新提供 Cookie。

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
Cookie 后重启即可自愈（`login_session.json` 文件本身损坏时的恢复步骤见
「/data 卷要求」）。

### `/data` 卷要求

- `/data` 是**必需**的持久卷（compose 示例为 Docker named volume
  `weread-cron-data`，卷名固定、不随 compose 项目名变化），存放：
  - `login_session.json` — Login Session（完整 Cookie 集合，renewal 后原子更新）；
  - `terminal_state.json` — Terminal State（当天 Task 结果：`success`/`failed`）；
  - `task.lock` — 跨进程 Task 互斥锁文件（flock；内容不使用，锁随进程退出自动释放）。
- 容器重启自动从 `/data` 恢复 Login Session，无需重新提供 Cookie（用户故事 #7）；
  终态先落盘再通知，保证重建后不会重复执行当天任务（用户故事 #39）。
- **卷丢失的后果**：Login Session 回退为无，重启时再次要求提供
  `WEREAD_CRON_COOKIE`；当天终态丢失，daemon 可能在当天剩余窗口重新安排一次（行为
  可接受但不预期）。请勿把 `/data` 挂载到临时目录（如容器重建即清空的路径）。
- **named volume vs 本地目录**：`/data` 是应用内部状态而非用户工作目录，默认用
  named volume 持久化，无需在宿主目录查看或编辑。开发/定制部署可改挂本地目录
  （如 `./data:/data`；目录需可写，容器内以 root 运行），但 `data/` 已在
  `.gitignore` 中，不得提交入库。
- **持久化 Login Session 损坏的恢复**：`login_session.json` 存在但解码失败/为空
  时，启动报「恢复 Login Session 失败」并以 exit 1 退出，**不回退**使用
  `.env` 中的初始 Cookie（V1 保守失败决策；此时更新 `.env` 中的 Cookie 无效——
  损坏文件仍会遮挡初始 Cookie）。人工恢复：删除损坏文件（或恢复备份）后重启，
  容器以初始 Cookie 重新建立会话：

  ```bash
  # 停止容器（named volume 不删除）
  docker compose down
  # 删除损坏文件（镜像为 scratch 无 shell/rm，借助临时 alpine 容器挂载卷操作）
  docker run --rm -v weread-cron-data:/data alpine rm /data/login_session.json
  # 或从备份恢复：
  # docker run --rm -v weread-cron-data:/data \
  #   -v "$PWD/login_session.json.bak:/backup.json" alpine cp /backup.json /data/login_session.json
  # 若整卷删除（docker volume rm weread-cron-data）同样可恢复，但会连带丢失当天
  # Terminal State（后果见上）。
  docker compose up -d
  ```

  与「登录已失效」通知场景的区别：「登录已失效」时 `login_session.json` **恢复
  成功**、只是登录态过期，更新 `.env` 中的 Cookie 后重启即可自愈；损坏场景是
  文件本身无法解码/为空，必须先删除或恢复该文件，容器才会以初始 Cookie 重建
  会话。

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
  `linux/amd64,linux/arm64` → 推送 GHCR；`main` 分支推送 `latest`（及 `main`），
  `v*` tag 仅推送版本标签（如 `v1.0.0`，不触碰 `latest`）。镜像路径为
  `ghcr.io/UsirukEsikam/weread-cron`（工作流由仓库推导，无需配置）。
- 不含 shell 与调试工具；排查问题用 `docker compose logs`，或
  `docker run --rm ghcr.io/UsirukEsikam/weread-cron:latest --help` 查看用法。

### Fork 发布（可选）

fork 仓库后自行发布镜像时，release.yml 按 **fork 的仓库**推导镜像路径（即
`ghcr.io/<你的 GitHub 用户名>/weread-cron`），与本仓库路径不同。此时把本地
`compose.yaml` 的 `image:` 改为你 fork 的镜像路径即可；不发布镜像的 fork 也可
`docker build -t <你的镜像名>:local .` 本地构建后改指本地镜像。

默认部署路径始终是 `ghcr.io/UsirukEsikam/weread-cron`：仅 clone 本仓库的普通用户
直接使用，无需替换任何 OWNER 占位符。

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
