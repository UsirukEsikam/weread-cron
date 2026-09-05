# 04: Login Session 持久化、恢复与失效重建

**What to build:** Login Session 的跨重启生命周期：原子持久化到 `/data`、启动时恢复优先于初始 Cookie、renewal 更新的 Cookie 落盘（重启不回退旧会话）；登录失效凭明确证据判别（判别清单见 `docs/protocol-validation-checklist.md` #2/#4）→ 从初始 Cookie 重建一次 → 仍失败 → failed 终态 + 明确提示"更新初始 Cookie"的登录失效通知。

**Blocked by:** 03

**Status:** ready-for-agent

- [ ] 运行中 renewal 后重启 → 恢复的是最新会话而非初始 Cookie
- [ ] 无持久化会话 → 从初始 Cookie 初始化
- [ ] 持久化为原子写（写入中途崩溃不留半写状态）
- [ ] 凭明确证据判定登录失效 → 自动从初始 Cookie 重建一次；重建成功则当天 Task 继续
- [ ] 重建仍失败 → failed 终态 + 登录失效通知（明确提示更新初始 Cookie）
- [ ] 登录失效判别特征与验证清单一致；发现假设冲突时在 Comments 提出讨论，不静默替换判别方案