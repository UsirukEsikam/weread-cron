# 02: 协议核心：编码与签名

**What to build:** 微信读书协议纯逻辑层——`_e`（bookId/chapterUid 编码）、appId（UA 派生）、`sg`、`s`（排序查询串 + 滚动哈希）、enter/timed payload 构造（字段集差异）、`succ`/`synckey` 成功判定——以参考源码调查确认的 golden vectors 和纯函数测试验证。这是后续一切上报功能的地基；本票不要求建立完整应用 HTTP seam（fake WeRead server 从 T3 起使用）。

**Blocked by:** 01

**Status:** resolved

- [x] `_e(bookId)` / `_e(chapterUid)` 对参考源码已知样例输出一致（含类型 flag、切块、填充规则）
- [x] `sg = sha256(ts + rn + token)` 与 golden 向量一致；token 优先 `reader.token`，固定 fallback 值按"兼容默认"实现并标注（非核心协议假设）
- [x] `s` 对排序 `key=urlencode(value)` 串的哈希与参考实现等价（含边界输入：空值、中文、特殊字符、大 payload）
- [x] appId 由 UA 派生与已知示例一致
- [x] enter report 与 timed report payload 字段集合规：timed 含 `rt/ts/rn/sg`，enter 不含
- [x] 成功判定（`succ==1` 或 `synckey` 存在）有配套纯函数测试
- [x] 未实测项（见 `docs/protocol-validation-checklist.md` #5/#6/#9）在代码/文档中标注，不写死服务器语义断言
## Comments

- 2025-09-05：源码调查发现官方 reader JS 的 timed report `rt` 为距阅读开始（startReadingTime）的累计秒数（`rt = max(0, (ts-startReadingTime)/1000)`），与 ADR-0004 的"距上次被成功接受上报的实际间隔"不同。本票未改变 ADR-0004（rt 语义属 ticket 06），仅在包注释与本题 Answer 中标注；建议 ticket 06 复核 ADR-0004 时参考此证据。
- 2025-09-05：wxread 抓包样本数据为拼接（sm 24 字符超上限、ps/pc 与 ct 不同会话），其 `s` golden（36cc0815）不可复现，未作为测试向量；按官方 JS 解混淆参考生成 `s`/appId 向量。

## Answer

实现（纯逻辑层，不发起 HTTP）：`internal/weread/protocol.go`（`EncodeID`/`QueryString`/`Sign`/`SignPayload`/`AppID`/`SG`/`IsAccepted`/`EnterReportPayload`/`TimedReportPayload` + `ReadingProgress`/`ReaderContext`），测试 `protocol_test.go`。

### 源码调查结论（以官方 Web Reader 脚本为准）

从线上 reader 页（weread.qq.com/web/reader/…）抓取官方 bundle（`wrwebnjlogic/js/9.898844ac.js`），解混淆出：
- `sign`（0x15051505 滚动哈希，b 分量位移为 `d%30`）与查询构建（排序键 + `encodeURIComponent`）；
- `appId`（wb + 前 12 个空格 segment 长度%10 + 0x83 滚动哈希）；
- `psvts = e(serverTs)`、`pclts = e(clientTs)`（秒级）；
- enter/timed payload 字段与官方一致（timed = enter + `rt/ts/rn/sg`；enter 为 `appId,b,c,ci,co,sm,pr,ct,ps,pc`）；
- `sm` 截取 20 字符；`c = e(chapterUid||0)`；`sg = sha256(ts+rn+token)`，token 来自 reader store。

### golden 向量（真实线上数据交叉验证）

- `e("695233")` = `ce032b305a9bc1ce0b0dd2a`（wxread 抓包 `b` ↔ 线上三体全集 readerURL 页面）
- `e("112")` = `7f632b502707f6ffaa6bf2e`（wxread 抓包 `c` ↔ 页面 chapterUid）
- `e("1744333815")` = `4ee326507a65a465g015fae`（wxread 抓包 `ps` = e(秒级服务端时间戳)）
- `e("1744333820")` = `aab32e207a65a466g010615`（wxread 抓包 `pc` = e(秒级客户端时间戳)）
- `sg`：wxread 样本 `ts/rn/token→sg` 完全自洽（sha256 字符串拼接）

### 与参考实现的差异（已按官方实现）

1. **weread.koplugin 的 `sign` 有位移笔误**：b 分量用 `(length-i+1)%30`，官方为 `d%30`（wxread Python 与官方一致）。本实现按官方。
2. **urlencode 字符集**：koplugin 额外编码 `!*'()`，官方 `encodeURIComponent` 不编码（未保留字符集更大）。本实现按官方。
3. **`_e` 类型 4 的编码**：官方 JS `charCodeAt` 按 UTF-16 码元；koplugin 按 UTF-8 字节。真实书 ID 均为 ASCII，等价（差异已标注）。
4. **wxread 抓包样本的 `s`（36cc0815）不可复现**：该样本是拼接数据——`sm` 为 24 字符（超过官方 20 字符上限）、`ps/pc` 来自另一会话时间戳（1744333815/1744333820 vs ct 1744264311）。因此没有把该 `s` 作为 golden 向量；`s` 的 golden 由官方 JS 解混淆参考生成（含空值/中文/特殊字符/5000 字符/空 payload 边界）。
5. **wxread 抓包 `appId` 不可复现**：`wb182564874603h266381671` 来自其历史 UA 捕获（当前 headers UA 会产生不同的 appId），不作为测试向量；按官方算法与 koplugin 一致实现（koplugin 当前 UA 向量已验证）。

### 发现的协议事实（影响后续票，已标注待讨论）

- **官方 timed report 的 `rt` = 距阅读开始（`startReadingTime`）的累计秒数**（`rt = max(0, (ts - startReadingTime)/1000)`），与 ADR-0004 选择的"距上次被成功接受上报的实际间隔"不同。本层只构造字段（rt 由调用方传入），差异留给 ticket 06 复核 ADR-0004 时讨论（见包注释与 Comments）。

### 未实测项标注（checklist #5/#6/#9）

已写入 `internal/weread/protocol.go` 包注释：token 来源/轮换/fallback 接受度（#5）、s 的服务器校验边界（#6）、succ/synckey 边界语义（#9）。`IsAccepted` 仅实现参考项目共识（succ==1 或 synckey 存在即接受），不写死服务器语义断言。测试：golden 向量（`_e`/`sg`/`s`/appId）、边界输入（空值、中文、特殊字符、大 payload、空 payload）、字段集与自洽签名、成功判定；另含开发期差分测试（读取 /tmp 参考用例，缺失时跳过）。全量 `go test -race ./...` 与 `go vet ./...` 通过。
