# syntax=docker/dockerfile:1

# weread-cron 镜像：scratch 极简基础（内嵌 tzdata + 系统 CA 证书）。
#
# 构建产物为单个静态二进制（CGO_ENABLED=0），运行时无 shell、无包管理器：
#   - tzdata：二进制 import _ "time/tzdata" 内嵌（ADR-0003），TZ 环境变量可
#     覆盖为任意 IANA 时区名，默认 Asia/Shanghai；
#   - CA 证书：从构建镜像复制 /etc/ssl/certs/ca-certificates.crt 到运行时根
#     文件系统（TLS 握手需要：weread.qq.com / Bark / 企业微信）。

# ---- 构建阶段 ----
FROM golang:1.27 AS build

WORKDIR /src

# 依赖清单：当前无第三方依赖（仅 go.mod；引入 go.sum 后一并复制）。
COPY go.mod ./
COPY . .

RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/weread-cron \
    ./cmd/weread-cron

# ---- 运行时阶段 ----
FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/weread-cron /weread-cron

# TZ 默认 Asia/Shanghai（与二进制默认一致，镜像内显式声明）；
# WEREAD_CRON_DATA_DIR 默认 /data（与 config 包默认一致）。
ENV TZ=Asia/Shanghai \
    WEREAD_CRON_DATA_DIR=/data

# /data 持久卷：Login Session（login_session.json）与 Terminal State
# （terminal_state.json）。卷丢失后容器重启将要求重新提供 WEREAD_CRON_COOKIE。
VOLUME /data

ENTRYPOINT ["/weread-cron"]
