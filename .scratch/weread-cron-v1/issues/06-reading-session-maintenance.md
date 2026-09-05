# 06: Reading Session 维护

**What to build:** 长阅读会话的两个可靠性行为：(1) Reader Context 到期（参考默认约 15 分钟 TTL）时主动重新抓取 Reader 页刷新 Context，Reading Session 继续（不中断、无多余 enter）；(2) 异常墙钟间隔超过内部阈值时不形成大 `rt` 上报，而是重新 enter、重建 Reading Session——同一本书继续，Task 已累计时长保留。TTL 与阈值为内部默认值，不做配置。

**Blocked by:** 03

**Status:** ready-for-agent

- [ ] 注入 clock 推进越过 Context TTL → 观察到主动重新抓取 Reader 页（新 token/psvts），后续 timed report 继续，无 enter 重复
- [ ] clock 跳变超过内部阈值 → 无大 `rt` 上报（最后一次 timed 的 `rt` 不包含缺口）
- [ ] 异常间隔后观察到新的 enter report（Reading Session 重建）
- [ ] 重建后同一本书、Task 已累计时长保留
- [ ] 行为与 ADR-0004 一致；TTL 表述为参考默认值而非服务器协议常量（验证清单 #7/#8）
- [ ] 若实测表明服务器要求刷新后重新 enter（参考实现如此），在 Comments 提出讨论