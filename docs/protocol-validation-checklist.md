# 协议实测验证清单（Manual Protocol Validation Checklist）

本清单记录需要在**真实微信读书账号 + 浏览器 Cookie** 环境下由人工配合验证的协议事实。对应实现 ticket 在实施时可先按当前设计假设（参考实现源码调查结论）推进；但以下事实未经实测确认前，**不得**把相关假设表述为"服务器协议事实"。

已 resolved 的实现 ticket 只表示基于当前协议假设的实现已完成，**不代表**对应协议事实已获真实账号验证;协议事实以本清单各行「实机验证结果」为准。执行一轮受控验证的工作项见 `.scratch/weread-cron-v1/issues/23-controlled-real-account-protocol-validation.md`（ready-for-human）。

验证结果若与当前设计假设不一致：**影响到的 ticket 应提出问题讨论，不自行替换为另一套接口或方案**。

| # | 验证项 | 影响 ticket | 当前假设（待验证） | 实机验证结果 |
| --- | --- | --- | --- | --- |
| 1 | 纯 Cookie 可用的 Shelf（书架）端点：endpoint、鉴权方式、返回结构、条目类型字段（普通书籍 vs 其他） | T9 自动选书 | 存在 Cookie-only 书架接口，返回 bookId/title/finishReading 等字段；若不存在，T9 需讨论替代来源 | 待验证 |
| 2 | 登录失效的响应特征：哪些 HTTP 状态/响应体可明确判别"登录已失效"（vs 普通暂时性失败） | T4 失效重建、T5 恢复链 | renewal 失败是明确证据之一；其余判别特征待实测 | 待验证 |
| 3 | progress=100%（已读完）的书是否仍能被服务端接受并计入阅读时长 | T9 回退行为 | 参考实现无护栏（结构上可上报），服务端行为未知；若已读完不计时，T9 回退逻辑需讨论 | 待验证 |
| 4 | renewal 返回的 Cookie 集合与 Set-Cookie 属性：`wr_skey`/`wr_gid` 凭证偏好、expires/domain/path/httpOnly；哪些 Cookie 被返回/删除/过期；失效 renewal 是否返回有意义的 Set-Cookie；发送侧简化匹配（不分域/路径）是否充分 | T4 会话持久化 | Go Cookie Jar 全套属性持久化可恢复；发送侧简化匹配足够（V1 假设） | 待验证 |
| 5 | `reader.token` 的来源、轮换与有效期；固定 fallback token 是否仍被接受 | T2 协议核心、T3 payload | reader.token 来自 `__INITIAL_STATE__`，优先使用；固定值仅兼容 fallback | 待验证 |
| 6 | `s` 签名的边界行为：payload 大小、特殊字符、URL encoding 细节与排序规则 | T2 协议核心 | 排序 key=urlencode 串 + 0x15051505 滚动哈希（与两参考实现一致） | 待验证 |
| 7 | Reader Context 刷新（重新抓取 Reader 页）后是否必须重新 enter report | T6 Reading Session 维护 | 本设计：TTL 到期主动刷新后 Reading Session 继续（不重新 enter）；参考实现刷新后 re-enter——两者冲突，需实测裁决 | 待验证 |
| 8 | Reader Context 的生命周期：服务端何时拒收旧 Context；约 15 分钟 TTL 是否成立 | T6 Reading Session 维护 | 15 分钟仅参考实现默认值，非协议常量 | 待验证 |
| 9 | 成功判定边界：响应含 `succ==1` 但无 `synckey`、或有 `synckey` 但无 `succ` 时分别意味着什么 | T2、T3 | `succ==1` 或 `synckey` 存在即接受（与 weread.koplugin 一致；wxread 更严格需两者兼备；两者非共识） | 待验证 |
| 10 | enter report 的必要性：服务器是否接受直接开始 timed report（无 enter） | T3、T6 | enter report 先行（两参考实现均如此） | 待验证 |
| 11 | `rt` interval 语义的真实计时效果：Timed report 的 `rt` 是否按真实墙钟间隔计时；大间隔被折叠/合并还是拒收；间隔异常后重建 Reading Session（重新 enter）是否被服务端接受 | T6 Reading Session 维护 · ADR-0004 | rt = 距上次被接受上报的实际墙钟间隔；大缺口不合并为一次 rt，重建会话（ADR-0004 设计选择，非已确认协议事实） | 待验证 |
| 12 | 完全未阅读过的 Shelf 书的 Reader Context：`__INITIAL_STATE__` 是否含当前章节与位置；最小可建 Context 的状态 | T9 自动选书 | 未打开过的书可取得当前章节与位置；参考实现含章节信息 fallback，是否必需未实测 | 待验证 |

注：异常间隔阈值（内部经验默认 ~90s）不是服务端协议阈值，不列入验证项（findings/03 V11）；真实使用中若观察到问题，再据证据调整。

## 验证记录

每次受控验证会话在此追加记录（日期、账号环境、请求/响应摘要、结论），并在上表对应行更新「实机验证结果」列。

模板:

```markdown
### YYYY-MM-DD: <会话简述>
- 账号环境: <设备/平台/浏览器版本>
- 覆盖项: #N, #M
- 结论摘要: <逐项结论>
- 证据: <请求/响应摘要、对照参考项目行为或抓包结果>
- 需要讨论的项: <受影响 ticket 与议题>
```

## 验证方式

- 真实账号 + 浏览器 Cookie，对照参考项目（`weread.koplugin` / `wxread`）当前行为与抓包结果
- 每次验证记录：日期、账号环境、请求/响应摘要、结论
- 结论变化时更新本清单并通知对应 ticket

## 处理原则

验证结果与设计假设不一致 → 在受影响 ticket 的 Comments 中提出问题与证据，由用户/维护者决定调整方向；不静默改用另一接口、另一套字段语义或另一签名方案。

## 以下为一些手工记测试后的记录

Verified:
- Cookie-only Shelf endpoint works.
- Shelf parsing works.
- Real Reader state can return reader.pclts as JSON number.
- Observed value: pclts = 0.
- reader.psvts was a JSON string.
- 5-minute Task on a previously read book completed successfully and real account reading time increased.
- Longer manual Tasks reproducibly received an empty-object server rejection after approximately 5–5.5 minutes.
- The same failure occurred with a newly added unread book, a completed book, and an ordinary unfinished book.
- Recreating the container/volume and supplying a fresh initial Cookie did not change the behavior.
- Therefore book completion/unread status is not yet independently validated; those cases are blocked by the broader long-session report rejection.