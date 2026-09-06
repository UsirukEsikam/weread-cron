# weread-cron

微信读书自动阅读（单账号）。

每天在设定的运行时间窗口（Run Window）内随机挑一个时刻自动执行阅读任务。

如果任务执行失败，程序会记录当天失败结果并支持手动重试；次日继续按计划执行新的自动任务。

## 快速部署

镜像已发布至 GitHub Container Registry：`ghcr.io/UsirukEsikam/weread-cron:latest`，支持 `linux/amd64` 与 `linux/arm64`。

使用 Docker Compose 部署步骤如下：

```bash
# 1. 复制配置文件模板
cp compose.yaml.example compose.yaml
cp .env.example .env

# 2. 获取并配置 Cookie（获取方法见下文「配置 Cookie」一节）
# 编辑 .env 文件，填入 WEREAD_CRON_COOKIE

# 3. 启动服务
docker compose up -d

# 4. 查看日志，确认服务正常运行
docker compose logs -f
```

## 常用命令

查看实时日志：

```bash
docker compose logs -f
```

如需手动执行：

```bash
# 执行指定时长的阅读任务（例如 30 分钟，自动从书架或配置候选选书）：
docker compose exec weread-cron /weread-cron run --minutes 30

# 执行指定时长并指定书籍 ID（例如阅读书籍 695233）：
docker compose exec weread-cron /weread-cron run --minutes 30 --book 695233
```

查看书架上的图书列表（用于获取书籍 ID）：

```bash
# 容器运行中执行：
docker compose exec weread-cron /weread-cron books

# 容器未运行或临时查看：
docker compose run --rm weread-cron books
```

> **提示**：镜像基于 scratch 构建，容器内部没有 shell 环境。使用 `docker compose exec` 时需填写完整绝对路径 `/weread-cron`。

## 配置 Cookie

`WEREAD_CRON_COOKIE` 是微信读书的登录凭证。首次部署时必须提供，服务启动后会自动维护并续期登录状态。

获取步骤：

1. 在电脑浏览器中访问并登录微信读书网页版：<https://weread.qq.com>。
2. 按 `F12` 打开开发者工具，切换到「网络」（Network）标签页。
3. 刷新页面，在网络请求列表中选中任意一个发往 `weread.qq.com` 的请求。
4. 在右侧「标头」（Headers）的请求标头（Request Headers）中找到 `Cookie`，复制它的完整内容。
5. 打开 `.env` 文件，粘贴到 `WEREAD_CRON_COOKIE`。如果内容包含 `;`、`=` 或 `#` 等字符，请用英文单引号包裹：

   ```bash
   WEREAD_CRON_COOKIE='wr_vid=1111111; wr_skey=abcdef...; wr_gid=123456'
   ```

6. 执行启动命令使配置生效：

   ```bash
   docker compose up -d
   ```

服务成功运行后，登录状态会保存在数据卷中并自动刷新。只要数据卷存在，后续重启容器无需重新填写 Cookie。

## 常用配置

所有配置项均通过环境变量提供（在 `.env` 中设置）：

| 环境变量 | 必填 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `WEREAD_CRON_COOKIE` | 首次部署 | 无 | 微信读书登录 Cookie |
| `WEREAD_CRON_RUN_WINDOW_START` | 是 | 无 | 时间窗口开始时间，格式 `HH:MM` |
| `WEREAD_CRON_RUN_WINDOW_END` | 是 | 无 | 时间窗口结束时间，格式 `HH:MM`（与开始时间相同时表示固定时刻启动） |
| `WEREAD_CRON_READ_MINUTES_MIN` | 是 | 无 | 单次阅读最短时长（分钟） |
| `WEREAD_CRON_READ_MINUTES_MAX` | 是 | 无 | 单次阅读最长时长（分钟） |
| `WEREAD_CRON_BOOKS` | 否 | 留空 | 指定阅读的书籍 ID（多本用逗号分隔）；留空则自动从书架挑选 |
| `WEREAD_CRON_BARK_URL` | 否 | 无 | Bark 推送通知地址（填写即启用） |
| `WEREAD_CRON_WECOM_WEBHOOK_URL` | 否 | 无 | 企业微信机器人 Webhook 地址（填写即启用） |
| `TZ` | 否 | `Asia/Shanghai` | 时区（决定任务日期划分和时间窗口计算） |
| `WEREAD_CRON_DATA_DIR` | 否 | `/data` | 数据持久化目录 |

配置注意事项：
- 时间窗口开始时间不能晚于结束时间。
- 单次阅读的最小时长不能大于最大时长。
- 时间必须使用 24 小时制 `HH:MM` 格式。若配置格式不合法，程序启动时会直接报错退出。

## 版本更新

拉取最新镜像并重新创建容器：

```bash
docker compose pull
docker compose up -d
```

数据保存在独立的数据卷中，更新镜像不会影响已有的登录状态和任务记录，无需重新提供 Cookie。

## 常见恢复操作

### 登录状态过期

如果收到登录失效通知，说明保存的登录凭证已过期：

1. 按照「配置 Cookie」一节的方法，在浏览器中重新获取最新的 Cookie。
2. 打开 `.env` 文件，更新 `WEREAD_CRON_COOKIE`。
3. 执行以下命令重新创建容器并加载新配置：

   ```bash
   docker compose up -d
   ```

> **注意**：请执行 `docker compose up -d`，不要使用 `docker compose restart`。单纯执行 `restart` 不会重新读取 `.env` 中的修改。

### 数据持久化与数据卷

服务使用 Docker 数据卷 `weread-cron-data` 挂载到容器内的 `/data` 目录。目录中保存两项内容：

- 登录状态：记录自动续期后的最新 Cookie；
- 当天任务结果：记录当天阅读任务是否已完成。任务结果保存在 `/data` 中，已经完成的当天任务不会因为容器重建或重启而再次执行。

请妥善保留该数据卷。如果数据卷被误删，服务重启后需要重新在 `.env` 中提供 `WEREAD_CRON_COOKIE`。

### 登录状态文件损坏恢复

在极少数情况下（例如宿主机非正常断电），`/data/login_session.json` 文件可能损坏并导致程序无法启动（日志提示读取或解析会话失败）。

此时程序不会直接读取 `.env` 中的初始 Cookie，需要手动清理损坏的文件：

```bash
# 1. 停止容器
docker compose down

# 2. 借助临时容器删除数据卷中的损坏文件
docker run --rm -v weread-cron-data:/data alpine rm /data/login_session.json

# 3. 确认 .env 中的 WEREAD_CRON_COOKIE 有效后重新启动服务
docker compose up -d
```

启动后，服务会读取 `.env` 中的 Cookie 重新初始化登录状态。

## 本地构建（可选）

如需在本地编译或构建镜像：

```bash
go build ./cmd/weread-cron             # 编译可执行文件
go test ./...                          # 运行测试
docker build -t weread-cron:local .    # 构建本地 Docker 镜像
```
