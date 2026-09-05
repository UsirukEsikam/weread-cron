# 10: Docker 镜像与 GHCR 发布

**What to build:** V1 的最终部署闭环：极简镜像（内嵌 tzdata，ADR-0003）+ docker compose 示例（`/data` 持久卷、TZ 环境变量）+ GitHub Actions（测试 → 构建 `linux/amd64` 与 `linux/arm64` → 推送 GHCR）。本票等 V1 功能票（T5–T9）全部完成后交付，**Done 表示发布镜像已包含完整 V1**（daemon + run + books 全子命令），而不是 daemon 子集。

**Blocked by:** 05, 06, 07, 08, 09

**Status:** resolved

- [x] 镜像基于 scratch/distroless 极简基础，二进制内嵌 tzdata，`TZ` 可覆盖（默认 Asia/Shanghai）
- [x] compose 示例可拉取对应架构镜像并以 `/data` 持久卷常驻运行
- [x] Actions：test 全绿 → 构建 amd64 + arm64 → 推送 GHCR
- [x] 镜像内二进制支持 daemon/run/books 全部子命令（完整 V1）
- [x] 文档说明部署方式（含首次 Cookie 配置与 `/data` 卷要求）

## Answer

实现「Docker 镜像与 GHCR 发布」——部署闭环的最后一环，镜像内二进制即完整 V1（daemon/run/books）。交付物：

1. **`Dockerfile`（scratch 极简基础）**：多阶段构建——`golang:1.27` 构建 `CGO_ENABLED=0` 静态二进制（`-trimpath -ldflags="-s -w"`）；`scratch` 运行时仅含二进制 + 系统 CA 证书（`/etc/ssl/certs/ca-certificates.crt`，TLS 握手需要：weread/Bark/企业微信）。tzdata 由 `cmd/weread-cron/main.go` 的 `import _ "time/tzdata"` 内嵌（ADR-0003，ticket 01 已就位），镜像无需系统时区库；`ENV TZ=Asia/Shanghai` 显式声明默认值（可覆盖）、`WEREAD_CRON_DATA_DIR=/data`（与 config 默认一致）；`VOLUME /data`。无 shell、无包管理器（**调试须用 `docker compose logs` 或 `docker run ... --help`，`exec` 需显式 `/weread-cron` 完整路径**）。
2. **`docker-compose.yml`（仓库根，部署示例）**：`image: ghcr.io/<OWNER>/weread-cron:latest`（README 说明替换 OWNER；多架构清单由 GHCR manifest 自动选择）、`restart: unless-stopped`、`env_file: .env`、`./data:/data` 持久卷（Login Session + Terminal State）。
3. **`.env.example`**：全部 `WEREAD_CRON_*` 变量的注释模板（Cookie、Run Window、时长范围、候选书、通知渠道、DataDir、TZ），含 env_file 特殊字符（`;`/`=`/`#`）需单引号包裹的说明。
4. **`.github/workflows/release.yml`**：触发 = `main` 分支 + `v*` tag；`test` 门禁**复用 ci.yml**（ci.yml 增加 `workflow_call` 入口，同一份 test 定义）→ `image` job（`needs: test`）：setup-qemu + setup-buildx → login GHCR（`GITHUB_TOKEN`，`packages: write`）→ metadata-action（镜像名 `ghcr.io/${{ github.repository }}`，**由仓库推导，无需硬编码 owner**；tag：分支标签 + 版本标签 + main 时 `latest`）→ build-push-action `platforms: linux/amd64,linux/arm64` 推送。
5. **`README.md`（新建，部署文档）**：子命令表、Docker Compose 快速部署、**首次 Cookie 配置**（F12 → Network → 任一 weread.qq.com 请求头 `Cookie:` 全值粘贴入 `.env`，单引号包裹）、**`/data` 卷要求**（`login_session.json` + `terminal_state.json`；丢失后果 = 重启要求重新提供 Cookie + 当天可能重排；目录需可写）、配置清单表、镜像说明（scratch 极简、多架构、latest/v* 发布策略）、本地构建与测试。

### 本地验证（真实镜像，非仅代码评审）

- `docker build` 成功；镜像 10.4MB（scratch）。
- `docker run --rm weread-cron:local --help` → exit 2，用法列出 daemon/run/books 全部子命令（完整 V1 在镜像内）。
- daemon/run/books 三个入口在容器内均正确路由并触发「缺 Cookie 快速失败」（exit 1 + 指明 `WEREAD_CRON_COOKIE` 与 `/data` 无 Login Session），零网络请求。
- **ADR-0003 容器级验证**：`docker run -e TZ=America/New_York ...`（scratch 无系统时区文件）配置加载成功——内嵌 tzdata 生效、`TZ` 可覆盖（非默认时区）。
- **`/data` 持久化验证**：`--network none` + 初始 Cookie 挂载宿主临时目录 → `run` 后 `login_session.json` 按初始 Cookie 落盘（用户故事 #8/#9 持久化路径在容器内工作）；renewal 网络失败按 ticket 05 暂时性失败姿态不写终态、不通知（terminal_state.json 未生成）。
- **多架构验证**：`CGO_ENABLED=0 GOOS=linux GOARCH=amd64|arm64 go build`（与 Dockerfile 同 flags）均产出静态 ELF；`docker buildx build --platform linux/amd64,linux/arm64 --output type=cacheonly` 两平台全链路构建成功（amd64 经 qemu 仿真）。
- `docker compose config` 通过（`.env` 缺失时按预期报错；Cookie 含 `=` 的值解析正确）。

### 未在本票验证 / 说明

- **GHCR 实际推送**：需要真实 GitHub 仓库 + `GITHUB_TOKEN` 权限，本地不可验证；工作流按 GHCR 标准模式（login + build-push + packages: write）编排，`ghcr.io/${{ github.repository }}` 路径与 compose 示例的 `ghcr.io/<OWNER>/weread-cron` 一致。
- **CI 工作流执行**：release.yml 语法经 YAML 解析校验；与 ci.yml 的 test 口径保持一致（vet + test），镜像构建仅会在 test 全绿后执行。
- **容器内真实 Task 运行**（真实 weread 端点 + 有效 Cookie）属于真机验证范畴（验证清单 #2–#3），本票冒烟已覆盖路由、快速失败、持久化、时区、多架构构建。

## Comments

- **CA 证书来源**：`golang:1.27`（Debian 系）自带 `/etc/ssl/certs/ca-certificates.crt`，复制进 scratch 即得完整系统信任链；若未来切换构建镜像需保证证书路径可用。
- **VOLUME /data 与 ENTRYPOINT**：`VOLUME` 仅声明意图（compose 不挂载时 Docker 自动匿名卷，防止误写镜像层）；compose 示例仍显式绑定 `./data:/data` 以便宿主定位。
- **文档用词**：沿用 CONTEXT.md 术语（Login Session、Terminal State、Run Window、Target Duration、Shelf）；README 面向部署者，标注「镜像无 shell」等易踩坑点。
- **code-review 发现与处理**（双轴并发评审后修复）：① Spec 轴发现 `.env.example` 原样激活空 `WEREAD_CRON_COOKIE=` 行会与 config 层「显式空值=违规」矛盾——已持久化 Login Session 的容器重启会因此启动失败（破坏用户故事 #7 的恢复语义）；改为注释行 + 首次部署时取消注释（空值未设置 = 无 Cookie 快速失败路径）。② Spec 轴指出 compose 示例未呈现 ticket 口径的「TZ 环境变量」：compose 增加 `TZ: ${TZ:-Asia/Shanghai}`（.env 可覆盖，已验证）。③ Spec 轴指出 README「10 秒内快速失败」无 spec 依据（10s 仅是 CLI seam 测试的耗时上界断言）：删除该数字。④ Standards 轴指出 release.yml 的 test job 与 ci.yml 逐字重复（Duplicated Code）：ci.yml 增加 `workflow_call` 入口，release.yml 以 `uses: ./.github/workflows/ci.yml` 复用同一份 test 定义。⑤ Standards 轴指出用户可见配置清单在 config.go/usage/.env.example/README 四处镜像（Shotgun Surgery/Divergent Change）：接受为判断裁决——文档面与代码面镜像系部署文档的常规代价，不在本票重构；config 包仍是唯一解析实现。