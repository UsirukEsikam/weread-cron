# 协议实测验证清单（Manual Protocol Validation Checklist）

本清单记录需要在**真实微信读书账号 + 浏览器 Cookie** 环境下由人工配合验证的协议事实。对应实现 ticket 在实施时可先按当前设计假设（参考实现源码调查结论）推进；但以下事实未经实测确认前，**不得**把相关假设表述为"服务器协议事实"。

验证结果若与当前设计假设不一致：**影响到的 ticket 应提出问题讨论，不自行替换为另一套接口或方案**。

| # | 验证项 | 影响 ticket | 当前假设（待验证） |
| --- | --- | --- | --- |
| 1 | 纯 Cookie 可用的 Shelf（书架）端点：endpoint、鉴权方式、返回结构、条目类型字段（普通书籍 vs 其他） | T9 自动选书 | 存在 Cookie-only 书架接口，返回 bookId/title/finishReading 等字段；若不存在，T9 需讨论替代来源 |
| 2 | 登录失效的响应特征：哪些 HTTP 状态/响应体可明确判别"登录已失效"（vs 普通暂时性失败） | T4 失效重建、T5 恢复链 | renewal 失败是明确证据之一；其余判别特征待实测 |
| 3 | progress=100%（已读完）的书是否仍能被服务端接受并计入阅读时长 | T9 回退行为 | 参考实现无护栏（结构上可上报），服务端行为未知；若已读完不计时，T9 回退逻辑需讨论 |
| 4 | renewal 返回 Cookie 集合与持久化属性：`wr_skey`/`wr_gid` 凭证偏好、Set-Cookie 各属性（expires/domain/path/httpOnly 等）是否需要完整保存 | T4 会话持久化 | Go Cookie Jar 全套属性持久化可恢复 |
| 5 | `reader.token` 的来源、轮换与有效期；固定 fallback token 是否仍被接受 | T2 协议核心、T3 payload | reader.token 来自 `__INITIAL_STATE__`，优先使用；固定值仅兼容 fallback |
| 6 | `s` 签名的边界行为：payload 大小、特殊字符、URL encoding 细节与排序规则 | T2 协议核心 | 排序 key=urlencode 串 + 0x15051505 滚动哈希（与两参考实现一致） |
| 7 | Reader Context 刷新（重新抓取 Reader 页）后是否必须重新 enter report | T6 Reading Session 维护 | 本设计：TTL 到期主动刷新后 Reading Session 继续（不重新 enter）；参考实现刷新后 re-enter——两者冲突，需实测裁决 |
| 8 | Reader Context 的生命周期：服务端何时拒收旧 Context；约 15 分钟 TTL 是否成立 | T6 Reading Session 维护 | 15 分钟仅参考实现默认值，非协议常量 |
| 9 | 成功判定边界：响应含 `succ==1` 但无 `synckey`、或有 `synckey` 但无 `succ` 时分别意味着什么 | T2、T3 | `succ==1` 或 `synckey` 存在即接受 |
| 10 | enter report 的必要性：服务器是否接受直接开始 timed report（无 enter） | T3、T6 | enter report 先行（两参考实现均如此） |

## 验证方式

- 真实账号 + 浏览器 Cookie，对照参考项目（`weread.koplugin` / `wxread`）当前行为与抓包结果
- 每次验证记录：日期、账号环境、请求/响应摘要、结论
- 结论变化时更新本清单并通知对应 ticket

## 处理原则

验证结果与设计假设不一致 → 在受影响 ticket 的 Comments 中提出问题与证据，由用户/维护者决定调整方向；不静默改用另一接口、另一套字段语义或另一签名方案。