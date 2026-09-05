# 02: 协议核心：编码与签名

**What to build:** 微信读书协议纯逻辑层——`_e`（bookId/chapterUid 编码）、appId（UA 派生）、`sg`、`s`（排序查询串 + 滚动哈希）、enter/timed payload 构造（字段集差异）、`succ`/`synckey` 成功判定——以参考源码调查确认的 golden vectors 和纯函数测试验证。这是后续一切上报功能的地基；本票不要求建立完整应用 HTTP seam（fake WeRead server 从 T3 起使用）。

**Blocked by:** 01

**Status:** ready-for-agent

- [ ] `_e(bookId)` / `_e(chapterUid)` 对参考源码已知样例输出一致（含类型 flag、切块、填充规则）
- [ ] `sg = sha256(ts + rn + token)` 与 golden 向量一致；token 优先 `reader.token`，固定 fallback 值按"兼容默认"实现并标注（非核心协议假设）
- [ ] `s` 对排序 `key=urlencode(value)` 串的哈希与参考实现等价（含边界输入：空值、中文、特殊字符、大 payload）
- [ ] appId 由 UA 派生与已知示例一致
- [ ] enter report 与 timed report payload 字段集合规：timed 含 `rt/ts/rn/sg`，enter 不含
- [ ] 成功判定（`succ==1` 或 `synckey` 存在）有配套纯函数测试
- [ ] 未实测项（见 `docs/protocol-validation-checklist.md` #5/#6/#9）在代码/文档中标注，不写死服务器语义断言