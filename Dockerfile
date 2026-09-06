# syntax=docker/dockerfile:1

FROM golang:1.27 AS build

WORKDIR /src

COPY . .

RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/weread-cron \
    ./cmd/weread-cron

FROM scratch

# scratch 镜像不含系统文件，需复制根证书以支持 HTTPS 请求
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/weread-cron /weread-cron

ENV TZ=Asia/Shanghai \
    WEREAD_CRON_DATA_DIR=/data

VOLUME /data

ENTRYPOINT ["/weread-cron"]
