# 内嵌 tzdata 以支持极简镜像下的 TZ

Go 二进制 `import _ "time/tzdata"` 内嵌时区数据库，配合 scratch/distroless 极简镜像，运行时通过 `TZ` 环境变量（默认 `Asia/Shanghai`）决定本地时区。曾考虑使用带 tzdata 的基础镜像（增大镜像）或代码硬编码时区（牺牲 TZ 可覆盖）。
