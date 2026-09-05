# 08: run 终态规则与并发守卫

**What to build:** `weread-cron run` 与 Terminal State 的交互规则：当天无终态 → 执行并形成终态；已有 success 终态 → 拒绝重复执行（V1 无 force）；已有 failed 终态 → 允许手动重试，成功后当天结果更新为 success；Task 运行中 → 拒绝并发启动第二个 Task。failed 终态是 run 的合法输入状态，测试可直接 seed 持久化终态文件，无需经由 T5 的真实失败路径。

**Blocked by:** 03

**Status:** ready-for-agent

- [ ] 无终态 → run 完整执行并写 success 终态
- [ ] success 终态 → run 拒绝，stdout 说明原因并返回约定退出码
- [ ] failed 终态（seed）→ run 重试成功 → 终态更新为 success
- [ ] run 执行中第二个 Task（run 或 daemon 触发）被拒绝
- [ ] 无 force 选项
- [ ] run 不受 Run Window 限制（任意时刻可执行）