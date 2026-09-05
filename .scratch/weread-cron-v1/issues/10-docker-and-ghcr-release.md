# 10: Docker 镜像与 GHCR 发布

**What to build:** V1 的最终部署闭环：极简镜像（内嵌 tzdata，ADR-0003）+ docker compose 示例（`/data` 持久卷、TZ 环境变量）+ GitHub Actions（测试 → 构建 `linux/amd64` 与 `linux/arm64` → 推送 GHCR）。本票等 V1 功能票（T5–T9）全部完成后交付，**Done 表示发布镜像已包含完整 V1**（daemon + run + books 全子命令），而不是 daemon 子集。

**Blocked by:** 05, 06, 07, 08, 09

**Status:** ready-for-agent

- [ ] 镜像基于 scratch/distroless 极简基础，二进制内嵌 tzdata，`TZ` 可覆盖（默认 Asia/Shanghai）
- [ ] compose 示例可拉取对应架构镜像并以 `/data` 持久卷常驻运行
- [ ] Actions：test 全绿 → 构建 amd64 + arm64 → 推送 GHCR
- [ ] 镜像内二进制支持 daemon/run/books 全部子命令（完整 V1）
- [ ] 文档说明部署方式（含首次 Cookie 配置与 `/data` 卷要求）