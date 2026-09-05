# 05: 有界恢复链与失败通知

**What to build:** timed report 被拒时的有界恢复链——refresh Reader Context → retry → renewal → refresh Reader Context → retry → 仍失败则 failed 终态；失败通知内容（失败阶段、主要错误、已尝试恢复动作）；通知渠道独立性（Bark 与企业微信互不影响）与通知失败不影响 Task 结果。

**Blocked by:** 04

**Status:** ready-for-agent

- [ ] fake server 拒绝场景下按序观察到 refresh/retry/renewal/refresh/retry 请求，顺序与次数有界
- [ ] 恢复链中途成功 → Task 继续并最终成功
- [ ] 恢复链耗尽 → failed 终态（阻止当天再次自动执行）
- [ ] 失败通知含失败阶段、主要错误、已尝试恢复动作
- [ ] Bark 与企业微信独立发送；单个渠道故障不影响另一渠道与 Task 结果
- [ ] 登录失效导致的失败走 T4 的判别与明确提示（不混入普通失败文案）