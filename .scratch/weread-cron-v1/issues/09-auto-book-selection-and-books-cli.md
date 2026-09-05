# 09: 自动选书与 books CLI

**What to build:** 纯 Cookie 书架访问（端点待实测确认，见 `docs/protocol-validation-checklist.md` #1）→ `weread-cron books` 列出当前书架 bookId 与 title（复用 daemon 的 Login Session 恢复/初始化机制，可正常 renewal 并持久化，但不产生 Task、不读取或修改终态、不发送通知）；未配置候选书时 Task 自动选书：Shelf 元数据过滤优先"未读完且可建 Reader Context"的书 → 有界探测验证 → 全部不可用时回退"已读完但可用"的书。

**Blocked by:** 04

**Status:** ready-for-agent

- [ ] books 在 fake shelf 上输出正确 bookId/title
- [ ] books 会话失效时报错并提示更新初始 Cookie（不静默）
- [ ] books 不写终态、不产生 Task、不发通知
- [ ] 无候选书时 Task 从书架自动选书（未读完优先）
- [ ] 候选探测有界（失败换下一个，次数上限为内部默认）
- [ ] 未读完候选全部不可用 → 回退已读完可用书（progress=100% 计费行为待实测，冲突则提出讨论，验证清单 #3）
- [ ] 探测全部失败 → Task 失败走失败通知
- [ ] 显式指定的候选书不因 progress=100% 被排除