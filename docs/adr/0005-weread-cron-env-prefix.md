# 环境变量统一使用 WEREAD_CRON_ 前缀

用户可见环境变量统一使用 `WEREAD_CRON_` 前缀（如 `WEREAD_CRON_COOKIE`、`WEREAD_CRON_BOOKS`），与项目/二进制名 `weread-cron` 一致，避免与参考项目 findmover/wxread 的配置（`WXREAD_CURL_BASH`）混淆，防止用户误以为配置语义兼容。曾考虑沿用调研文档中的 `WXREAD_` 前缀，因与参考项目生态雷同而放弃。