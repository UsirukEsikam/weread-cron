// Package weread 是微信读书协议纯逻辑层（ticket 02）：bookId/chapterUid 编码（_e）、
// appId 派生、sg、s（排序查询串 + 滚动哈希）、enter/timed payload 构造与成功判定。
// 本层不发起 HTTP 请求、不持有状态；所有时间经注入（ADR-0006：clock 是领域真实依赖）。
//
// # 协议事实来源与验证状态
//
// 算法以官方微信读书 Web Reader 脚本（https://cdn.weread.qq.com/web/wrwebnjlogic/js/9.898844ac.js）
// 中解混淆后的实现为准（sign / 排序查询 / appId / psvts=e(serverTs)、pclts=e(clientTs)），
// 并与两个参考项目交叉核对：weread.koplugin（weread/lib/protocol.lua）与 wxread（config.py 中的真实抓包样本）。
//
// 已用真实线上数据验证的 golden 向量（见 protocol_test.go）：
//   - e("695233")        = ce032b305a9bc1ce0b0dd2a （wxread 抓包样本 b → 三体全集 readerURL）
//   - e("112")           = 7f632b502707f6ffaa6bf2e （wxread 抓包样本 c）
//   - e("1744333815")    = 4ee326507a65a465g015fae （wxread 样本 ps = e(服务器秒级时间戳)）
//   - e("1744333820")    = aab32e207a65a466g010615 （wxread 样本 pc = e(客户端秒级时间戳)）
//   - sg = sha256(ts+rn+token)，wxread 样本套件自洽（ts/rn/token → sg 完全一致）
//
// # 与参考实现的已知差异（按源码调查结论实现，非服务器语义断言）
//
//   - 排序查询串的 URL 编码与官方 JS encodeURIComponent 一致：除 `A-Za-z0-9-_.!~*'()` 外全部
//     按 UTF-8 字节转 %XX 大写。weread.koplugin 的 urlencode 还会编码 `!*'()`，与此不同
//     （koplugin 是其自实现的近似，不是官方行为）。
//   - s 滚动哈希以官方 JS（与 wxread Python 相同）为准：b 分量的位移是 d%30；
//     weread.koplugin 现版本 protocol.lua 中 b 分量误用了 (length-i+1)%30，与官方不等价。
//   - _e 的类型 4（非数字输入，如 MP_WXS_ 书 ID）按官方 JS charCodeAt 的 UTF-16 码元编码；
//     koplugin 按 UTF-8 字节编码。真实书 ID 均为 ASCII（数字串或 MP_WXS_ 前缀），两写法等价。
//     checklist #6 的边界行为（特殊字符、大小、URL 编码细节）已按官方实现验证于本包测试。
//
// # 未实测项（不得当作服务器协议事实，见 docs/protocol-validation-checklist.md）
//
//   - #5 reader.token 的来源、轮换与有效期；固定 fallback token 是否仍被服务器接受。
//     本包按"token 优先 reader.token，空值回退 DefaultReaderToken（兼容默认，非核心协议假设）"实现。
//   - #6 s 签名的服务器端校验行为（算法与官方 JS 一致，但服务器接受边界未经实测）。
//   - #9 成功判定边界（succ==1 但无 synckey / 有 synckey 但无 succ 的服务器语义）：
//     IsAccepted 与 weread.koplugin 行为一致（succ==1 或 synckey 存在即视为接受，OR 逻辑）；
//     wxread 更严格（succ 与 synckey 需同时存在才推进进度，AND 逻辑），两者不构成共识。
//     真实服务端 success boundary 仍属 protocol validation gap，本包不写死服务器语义断言。
//
// # 与 ADR-0004 的相关发现
//
// 官方 JS 中 timed report 的 rt = 距阅读开始（startReadingTime）的累计秒数
// （rt = max(0, (ts-startReadingTime)/1000)），与 ADR-0004 选择的"距上次被成功接受的
// 上报的实际间隔"不同。本层只负责构造字段（rt 值由调用方按 ADR-0004 语义传入），
// 该差异留给 ticket 06（Reading Session 维护）复核 ADR-0004 时讨论。
package weread

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// DefaultReaderToken 是兼容默认的 reader.token（weread.koplugin 与 wxread 的固定 KEY，即 wxread 的 KEY）。
// 它不是核心协议假设：正常路径使用 Reader Context 提供的 reader.token（checklist #5）。
const DefaultReaderToken = "3c5c8717f3daf09iop3423zafeqoi"

// SummaryMaxChars 是 payload 中 sm（位置摘要）的最大 UTF-16 码元数（官方 JS：content.substr(0, 0x14)
// 为 20 个码元；本实现同步截取 20 码元且不切开代理对，BMP 文本下与 koplugin utf8_substr(20) 等价）。
const SummaryMaxChars = 20

// ReadingProgress 是单本书的阅读位置状态（CONTEXT.md：chapterUid、chapterIdx、chapterOffset、progress、summary）。
type ReadingProgress struct {
	BookID        string // 书 ID（数字串或 MP_WXS_ 前缀）
	ChapterUID    uint64 // 当前章节 UID；0 表示无章节（对应官方 e(chapterUid||0)）
	ChapterIdx    int    // 章节序号
	ChapterOffset int    // 章节内偏移
	Progress      int    // 服务端进度 0–100（构造时截断并夹取）
	Summary       string // 位置附近摘要，payload 中截取至 SummaryMaxChars 个代码点
}

// ReaderContext 是 Web Reader 页面提取的上下文（CONTEXT.md），用于构造上报 payload。
type ReaderContext struct {
	// Psvts 是 e(服务器秒级时间戳)（官方 JS：psvts = e(serverTimestamp)）。
	Psvts string
	// Pclts 是 e(客户端秒级时间戳)；为空或 "0"（真实 Reader 页可返回数字 0，ticket 25
	// 已修解析）时表示"无可用的 pclts"，pc 由 ResolvePC 在 Reading Session 建立时
	// 生成会话级 fallback（issue 30：fallback pc 会话内稳定，与官方客户端一致）。
	Pclts string
	// Token 是 reader.token；为空时 SG 回退 DefaultReaderToken（兼容默认）。
	Token string
}

// ResolvePC 在 Reading Session 建立时解析该会话的 pc（issue 30）：
//   - Reader Context 提供可用 pclts（非空、非 "0"，ticket 25 语义）时，返回 Pclts
//     原值——pc 来自 Reader Context（可用 pclts 路径的取值方式与 issue 30 之前
//     一致，但解析时刻移到会话建立点）；
//   - pclts 为空或 "0" 时，返回会话级 fallback pc = e(会话建立时刻的秒级时间戳)（与
//     官方 Web Reader 对初始 pclts 为 0 的页面发送非零 pc、并在 enter/连续 timed
//     reports/真实翻页间复用同一 pc 的口径一致）。
//
// 解析结果由调用方在该 Reading Session 内（enter 与全部 timed reports，含 TTL 主动
// 刷新与有界恢复链内 refresh Reader Context）保持复用——可用 pclts 场景同样按会话
// 建立时的值携带：刷新换页带来的新 pclts 不改变会话 pc（与官方客户端页面会话内
// pc 稳定一致，ticket 30 语义；ps/psvts 仍逐笔跟随当前 Context）。仅明确建立新的
// Reading Session（重新 enter）时重新解析，不复用前一会话的值。
func ResolvePC(rc ReaderContext, sessionAt time.Time) string {
	if rc.Pclts == "" || rc.Pclts == "0" {
		return EncodeID(strconv.FormatInt(sessionAt.Unix(), 10))
	}
	return rc.Pclts
}

// EncodeID 实现微信读书的 _e 编码：
//   - 前 3 字符 = md5(输入) 前缀；
//   - 类型 flag："3"（纯数字串，按 9 位一组转十六进制）或 "4"（其他，整个输入按 UTF-16 码元转十六进制）；
//   - 固定 "2" + md5(输入) 后 2 字符；
//   - 每个 chunk 前加 2 位十六进制长度前缀（小写，超过 255 不加零填充，与官方一致），chunk 间以 "g" 分隔；
//   - 总长不足 20 用 md5(输入) 前缀补齐；
//   - 末尾追加 md5(当前结果) 前 3 字符。
func EncodeID(value string) string {
	h := md5.Sum([]byte(value))
	hhex := hex.EncodeToString(h[:])
	result := hhex[:3]

	chunks := []string{}
	if isDigits(value) {
		// 类型 3：9 位一组，parseInt(...).toString(16)（前导零随解析丢失，与官方 JS 一致）。
		for i := 0; i < len(value); i += 9 {
			end := i + 9
			if end > len(value) {
				end = len(value)
			}
			n, err := strconv.ParseUint(value[i:end], 10, 64)
			if err != nil {
				// 纯数字串且每组不超过 9 位，ParseUint 不会出错；防御性回退为原始组（不应发生）。
				chunks = append(chunks, value[i:end])
				continue
			}
			chunks = append(chunks, strconv.FormatUint(n, 16))
		}
		result += "3" + "2" + hhex[len(hhex)-2:]
	} else {
		// 类型 4：charCodeAt 的 UTF-16 码元十六进制（官方 JS 语义）。
		var b strings.Builder
		for _, cu := range utf16Units(value) {
			fmt.Fprintf(&b, "%x", cu)
		}
		chunks = []string{b.String()}
		result += "4" + "2" + hhex[len(hhex)-2:]
	}

	for i, chunk := range chunks {
		result += hexLenPrefix(chunk) + chunk
		if i < len(chunks)-1 {
			result += "g"
		}
	}
	if len(result) < 20 {
		result += hhex[:20-len(result)]
	}
	sum := md5.Sum([]byte(result))
	result += hex.EncodeToString(sum[:])[:3]
	return result
}

// hexLenPrefix 输出 chunk 长度的十六进制表示；一位数前面补 "0"（官方 JS 行为：
// 超过两位（长度 ≥ 256）不额外补零）。
func hexLenPrefix(chunk string) string {
	l := strconv.FormatInt(int64(len(chunk)), 16)
	if len(l) == 1 {
		return "0" + l
	}
	return l
}

// isDigits 判断是否全为 ASCII 数字（对应官方 JS /^\d+$/）。
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Sign 计算 0x15051505 滚动哈希（官方 JS 解混淆版）：
//
//	for (d = len-1; d > 0; d -= 2)
//	    a = 0x7fffffff & (a ^ charCodeAt(d) << (len-d) % 30)
//	    b = 0x7fffffff & (b ^ charCodeAt(d-1) << d % 30)
//	return (a + b).toString(16).toLowerCase()
//
// 与 JS 一致按 UTF-16 码元遍历（charCodeAt 是对码元而非字节）；ASCII 查询串下与按字节等价。
// 返回不含 "0x" 前缀的小写十六进制串。
func Sign(query string) string {
	units := utf16Units(query)
	a := int64(0x15051505)
	b := a
	n := int64(len(units))
	for i := n - 1; i > 0; i -= 2 {
		a = (a ^ (int64(units[i]) << uint((n-i)%30))) & 0x7fffffff
		b = (b ^ (int64(units[i-1]) << uint(i%30))) & 0x7fffffff
	}
	return strconv.FormatInt(a+b, 16)
}

// QueryString 构造排序查询串（官方 JS _0x261f9f 语义）：按键名排序（Go 字符串排序即字节序，
// 与 JS sort() 一致），每个值经 urlencodeURIComponent 编码后以 key=value 连接，分隔符 "&"。
// 与官方一致对传入的全部键编码（官方在写入 s 之前调用本函数；调用方应在不含 s 的
// 参数集上签名——本包的两个 payload 构造器即如此。koplugin 的 sorted_query 无条件丢弃 s，
// 与本实现在此细节上不同：对不含 s 的输入两者等价）。
func QueryString(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(urlencodeURIComponent(params[k]))
	}
	return b.String()
}

// SignPayload 对参数构造排序查询串并计算签名。
// 官方 JS 在给 s 赋值之前调用签名（查询从不含 s）；因此常规用法是传入不含 s 的参数集——
// 两个 payload 构造器均如此。若传入含 s 的参数集，本函数会连同 s 一起签名（与官方
// _0x261f9f 的全量编码行为一致），请自行保证调用时机与官方一致。
func SignPayload(params map[string]string) string {
	return Sign(QueryString(params))
}

// urlencodeURIComponent 与 JS encodeURIComponent 等价：除未保留字符外按 UTF-8 字节转
// %XX（大写十六进制）。未保留字符集：A-Z a-z 0-9 - _ . ! ~ * ' ( )。
func urlencodeURIComponent(s string) string {
	const safe = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.!~*'()"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(safe, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// AppID 由 User-Agent 派生 appId（官方 JS _0x368d06）：
// "wb" + 前 12 个空白分隔 token 的长度 % 10 + "h" + 滚动哈希（0x83 乘数，&0x7fffffff）。
// 与官方 JS 一致使用单空格 split；哈希按 UTF-16 码元（UA 实际均为 ASCII，等价于按字节）。
func AppID(userAgent string) string {
	parts := strings.Split(userAgent, " ")
	var prefix strings.Builder
	for i, p := range parts {
		if i >= 12 {
			break
		}
		prefix.WriteByte(byte(len(utf16Units(p))%10) + '0')
	}
	units := utf16Units(userAgent)
	var hash int64
	for _, cu := range units {
		// 官方 JS：h = 0x83*h + charCodeAt(i) & 0x7fffffff（& 为 32 位运算）。
		// 低 31 位不受 2^32 截断影响，int64 直接取 &0x7fffffff 等价。
		hash = (0x83*hash + int64(cu)) & 0x7fffffff
	}
	return "wb" + prefix.String() + "h" + strconv.FormatInt(hash, 10)
}

// SG 计算请求签名 sg = sha256(ts + rn + token)（十六进制小写）。
// token 为空时回退 DefaultReaderToken（兼容默认；fallback 是否仍被服务器接受未经实测，checklist #5）。
func SG(ts, rn, token string) string {
	if token == "" {
		token = DefaultReaderToken
	}
	sum := sha256.Sum256([]byte(ts + rn + token))
	return hex.EncodeToString(sum[:])
}

// IsAccepted 判定 /web/book/read 响应被接受：succ==1（bool true / 数值 1 / 字符串 "1"）
// 或存在非空 synckey 字段。
// 参考项目行为分歧：本实现与 weread.koplugin 一致（succ 成功或 synckey 存在即接受，OR 逻辑）；
// wxread（findmover/wxread）更严格（succ 与 synckey 均需存在才推进进度，AND 逻辑），
// 两者并不构成共识。真实服务端边界未经实测（protocol validation gap，checklist #9），
// 本函数不写死服务器语义断言，保持当前 OR 行为。
func IsAccepted(body map[string]any) bool {
	if body == nil {
		return false
	}
	if v, ok := body["succ"]; ok && succIsTrue(v) {
		return true
	}
	v, ok := body["synckey"]
	return ok && v != nil
}

// IsSucc 判定响应体的 succ 为 true/1/"1"——spec 决策 #5 中 renewal 的成功形态
//（{"succ":1}）。与 IsAccepted 不同：不把 synckey 视为接受（那是 report 的判定）。
func IsSucc(body map[string]any) bool {
	if body == nil {
		return false
	}
	v, ok := body["succ"]
	return ok && succIsTrue(v)
}

func succIsTrue(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t == 1
	case int:
		return t == 1
	case int64:
		return t == 1
	case string:
		return t == "1"
	}
	return false
}

// EnterReportPayload 构造 enter report 参数（CONTEXT.md：阅读会话开始的上报）。
// 字段集：appId, b, c, ci, co, sm, pr, ct, ps, pc, s（不含 rt/ts/rn/sg）。
// ct = now 的秒数；pc 是会话级 pc——由调用方在 Reading Session 建立时经 ResolvePC
// 解析一次并传入（issue 30：fallback 场景下会话内稳定，不再随每次构造时刻回退）；
// s 由签名函数计算。
func EnterReportPayload(p ReadingProgress, rc ReaderContext, pc string, now time.Time, userAgent string) map[string]string {
	params := positionParams(p, rc, pc, now, userAgent)
	params["s"] = SignPayload(params)
	return params
}

// TimedReportPayload 构造 timed report 参数（CONTEXT.md：按节奏发送、累计阅读时长的上报）。
// 字段集：enter 全字段 + rt, ts, rn, sg, s。
// rt 为本次上报的阅读时长秒数（语义由 ADR-0004 与 ticket 06 决定；官方 JS 为累计秒数，见包注释）；
// ts 为毫秒时间戳；rn 为请求随机数；sg = sha256(ts+rn+token)，token 为空回退 DefaultReaderToken。
// pc 是会话级 pc（同 EnterReportPayload：调用方在 Reading Session 建立时解析一次）。
func TimedReportPayload(p ReadingProgress, rc ReaderContext, pc string, now time.Time, userAgent string, rtSec int, tsMs int64, rn int) map[string]string {
	params := positionParams(p, rc, pc, now, userAgent)
	params["rt"] = strconv.Itoa(rtSec)
	tsStr := strconv.FormatInt(tsMs, 10)
	rnStr := strconv.Itoa(rn)
	params["ts"] = tsStr
	params["rn"] = rnStr
	params["sg"] = SG(tsStr, rnStr, rc.Token)
	params["s"] = SignPayload(params)
	return params
}

// positionParams 构造 enter/timed 共用的位置字段，并按 koplugin 的规整处理边界
// （官方 JS 直接透传 store 值，对合法输入两者等价）：ci/co 取非负整数、pr 截断为 0–100 整数、
// sm 截取至 SummaryMaxChars 个代码点、c 对应 e(chapterUid||0)。
// pc 不在此处决策：由调用方在 Reading Session 建立时经 ResolvePC 解析一次并传入
// （issue 30：fallback pc 会话内稳定，pc 决策点是会话建立、不是每笔构造）。
//
// 注意：本层返回 map[string]string 供签名使用；线上 JSON 中 ci/co/pr/rt/ct/ts/rn 为数字
// （官方 JS 与 wxread 抓包均如此），传输层序列化时需按数字输出。
func positionParams(p ReadingProgress, rc ReaderContext, pc string, now time.Time, userAgent string) map[string]string {
	ci := p.ChapterIdx
	if ci < 0 {
		ci = 0
	}
	co := p.ChapterOffset
	if co < 0 {
		co = 0
	}
	pr := p.Progress
	if pr < 0 {
		pr = 0
	}
	if pr > 100 {
		pr = 100
	}
	return map[string]string{
		"appId": AppID(userAgent),
		"b":     EncodeID(p.BookID),
		"c":     EncodeID(strconv.FormatUint(p.ChapterUID, 10)),
		"ci":    strconv.Itoa(ci),
		"co":    strconv.Itoa(co),
		"sm":    truncateSummary(p.Summary),
		"pr":    strconv.Itoa(pr),
		"ct":    strconv.FormatInt(now.Unix(), 10),
		"ps":    rc.Psvts,
		"pc":    pc,
	}
}

// utf16Units 返回字符串的 UTF-16 码元序列（JS charCodeAt / length 语义）。
func utf16Units(s string) []uint16 {
	return utf16.Encode([]rune(s))
}

// truncateSummary 截取摘要至 SummaryMaxChars 个 UTF-16 码元（官方 JS substr(0, 0x14) 语义）。
// 与官方差异：不拆开代理对（JS substr 会切开，随后的 encodeURIComponent 对孤立代理会抛 URIError）；
// BMP 文本（中文/ASCII）下与 20 个代码点等价（koplugin utf8_substr 语义亦等价）。
func truncateSummary(s string) string {
	units := utf16Units(s)
	if len(units) <= SummaryMaxChars {
		return s
	}
	units = units[:SummaryMaxChars]
	// 若末尾是孤立高代理（代理对被切断），丢弃之，避免解码出替换字符。
	if last := units[len(units)-1]; last >= 0xD800 && last <= 0xDBFF {
		units = units[:len(units)-1]
	}
	if len(units) == 0 {
		return ""
	}
	return string(utf16.Decode(units))
}
