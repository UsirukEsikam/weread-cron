# 测试主 seam：应用边界 + 薄 CLI

测试主 seam 是"执行一次 Task"的**应用边界**：进程内使用生产代码，通过构造器注入 clock、RNG、HTTP endpoints、session store、notifier，用 `httptest.Server` 在线上 HTTP 边界断言协议行为（payload 字段、签名、enter/timed 时序、恢复链、通知内容、持久化状态）。clock 与 RNG 是领域本身的真实依赖（随机窗口/随机时长/上报抖动是产品特性），不是 test-only 钩子。

另保留一个**薄的真实二进制 CLI seam**（子进程），只验证 executable 边界才能观察的行为：配置解析与校验、子命令路由、stdout/stderr、exit code、daemon 启动失败；不要求覆盖长 Task 与全部协议恢复路径。调度纯逻辑（`nextStart` 等）在应用 seam 内直接单元测试，不形成额外 seam。

不拆 `internal/weread` HTTP client、通知渠道等更低 seam，除非实施中出现应用 seam 无法清晰覆盖的逻辑。曾评估进程级 CLI seam 承担全部测试（需隐藏 env 或 test-only clock 把真实二进制指向测试端点），因引入仅测试用途的生产面而放弃。