package weread

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// goldenEncode 是 _e 编码的 golden 向量。
// 前 4 条来自 wxread config.py 中真实抓包样本（b/c/ps/pc），已与线上 weread 页面交叉验证：
// e("695233") 对应 三体全集 readerURL（weread.qq.com/web/reader/ce032b305a9bc1ce0b0dd2a），
// e("112") 对应该书 reader 页中的 chapterUid 112。
// 其余向量由官方 JS 语义（charCodeAt/parseInt/toString(16) 的独立实现）生成。
func TestEncodeID(t *testing.T) {
	vectors := []struct {
		in   string
		want string
	}{
		{"695233", "ce032b305a9bc1ce0b0dd2a"},
		{"112", "7f632b502707f6ffaa6bf2e"},
		{"1744333815", "4ee326507a65a465g015fae"},
		{"1744333820", "aab32e207a65a466g010615"},
		{"0", "cfc32da010cfcd208495488"},
		{"7", "8f132430178f14e45fce0f7"},
		{"57", "72b327f023972b32a1f7e2d"},
		{"22691208", "d2c32380715a3d88d2ccb4b"},
		{"3300060341", "81032840813ab7e12g0111d4"},
		{"123456789", "25f320b0775bcd1525f9647"},
		{"1234567890", "e80329f0775bcd15g0109e5"},
		{"987654321987654321", "0a132f8083ade68b1g083ade68b199d"},
		{"18446744073709551616", "6a0323a07afebff0g082bef2f5cg0210871"},
		{"MP_WXS_AbCdEf", "9f742781a4d505f5758535f416243644566765"},
		{"abcXYZ", "bf8422e0c61626358595a970"},
		{"测试book", "cd64223106d4b8bd5626f6f6b004"},
		{"0x15051505g", "31a42401630783135303531353035676e5"},
	}
	for _, v := range vectors {
		if got := EncodeID(v.in); got != v.want {
			t.Errorf("EncodeID(%q) = %q, want %q", v.in, got, v.want)
		}
	}
}

// TestEncodeIDShortPadding 覆盖长度不足 20 时的 md5 前缀补齐路径（3 个字符以内的输入）。
func TestEncodeIDShortPadding(t *testing.T) {
	for _, in := range []string{"0", "1", "57", "112"} {
		got := EncodeID(in)
		if len(got) < 20 {
			t.Errorf("EncodeID(%q) len = %d < 20", in, len(got))
		}
	}
}

// TestSG 是 sg = sha256(ts + rn + token) 的 golden 向量。
// 第 1 条来自 wxread 真实抓包样本（ts/rn 与 sg 自洽）；其余由 sha256 独立计算。
func TestSG(t *testing.T) {
	vectors := []struct {
		ts, rn, token, want string
	}{
		{
			ts: "1744264311434", rn: "466", token: "3c5c8717f3daf09iop3423zafeqoi",
			want: "2b2ec618394b99deea35104168b86381da9f8946d4bc234e062fa320155409fb",
		},
		{ts: "0", rn: "0", token: "t", want: "f3c168b1bb542077f5158b46ede4a163d42dc7508eb4f1c4388492288a8826fc"},
		{ts: "1717000000000", rn: "999", token: "t", want: "58b49dc7b37e1d2e29c7cd0f652650a5e9bc81a9d336306396a15b01f8936632"},
	}
	for _, v := range vectors {
		if got := SG(v.ts, v.rn, v.token); got != v.want {
			t.Errorf("SG(%s, %s, %q) = %s, want %s", v.ts, v.rn, v.token, got, v.want)
		}
	}
}

// TestSGDefaultTokenFallback 验证空 token 回退 DefaultReaderToken（兼容默认，checklist #5 标注项）。
func TestSGDefaultTokenFallback(t *testing.T) {
	got := SG("100", "7", "")
	want := SG("100", "7", DefaultReaderToken)
	if got != want {
		t.Errorf("SG with empty token = %s, want fallback %s", got, want)
	}
}

// TestQueryString 验证排序 + encodeURIComponent 语义（官方 JS）：排序键名、未保留字符不转义、
// %XX 大写、空格/制表/换行按字节转义。
func TestQueryString(t *testing.T) {
	vectors := []struct {
		name   string
		params map[string]string
		want   string
	}{
		{
			name:   "sort by byte order",
			params: map[string]string{"b": "1", "a": "2", "C": "3", "10": "4"},
			want:   "10=4&C=3&a=2&b=1",
		},
		{
			name:   "encodeURIComponent unescaped set",
			params: map[string]string{"v": "!*'()~-_.a b"},
			want:   "v=!*'()~-_.a%20b",
		},
		{
			name:   "specials percent-encoded uppercase",
			params: map[string]string{"v": `&%+/,;=?<>#$[]{}|^` + "`" + `\` + " @:/"},
			want:   "v=%26%25%2B%2F%2C%3B%3D%3F%3C%3E%23%24%5B%5D%7B%7D%7C%5E%60%5C%20%40%3A%2F",
		},
		{
			name:   "chinese utf-8 bytes",
			params: map[string]string{"sm": "聚会"},
			want:   "sm=%E8%81%9A%E4%BC%9A",
		},
		{
			name:   "includes s when present (official builder semantics)",
			params: map[string]string{"a": "1", "s": "deadbeef"},
			want:   "a=1&s=deadbeef",
		},
		{
			name:   "empty value and empty payload",
			params: map[string]string{"a": ""},
			want:   "a=",
		},
	}
	for _, v := range vectors {
		if got := QueryString(v.params); got != v.want {
			t.Errorf("%s: QueryString() = %q, want %q", v.name, got, v.want)
		}
	}
}

// TestSignPayload 是 s 签名的 golden 向量（官方 JS 解混淆实现生成，含边界输入：
// 空值、中文、特殊字符、大 payload、排序）。
func TestSignPayload(t *testing.T) {
	vectors := []struct {
		name   string
		params map[string]string
		want   string
	}{
		{name: "empty value", params: map[string]string{"a": ""}, want: "2a0a2b46"},
		{name: "single ascii", params: map[string]string{"k": "v"}, want: "2a0a2bda"},
		{
			name:   "chinese value",
			params: map[string]string{"sm": "19聚会《三体》网友的聚会地点是一处僻静"},
			want:   "3b1d9741",
		},
		{
			name:   "encodeURIComponent specials",
			params: map[string]string{"v": "!*'()~-_.&%+/,;=?<>#$[]{}|^`\\ @:/"},
			want:   "974ab4d2",
		},
		{name: "bare specials only", params: map[string]string{"v": "!'()*~"}, want: "2a0a13b2"},
		{name: "spaces and tabs", params: map[string]string{"v": "a b\tc\nd"}, want: "2a203ece"},
		{name: "mixed order sort", params: map[string]string{"b": "1", "a": "2", "C": "3", "10": "4"}, want: "2a38c986"},
		{name: "empty payload", params: map[string]string{}, want: "2a0a2a0a"},
		{name: "long value", params: map[string]string{"k": repeat("x", 5000)}, want: "2aaa2a48"},
		{
			name: "full timed payload",
			params: map[string]string{
				"appId": "wb182564874603h266381671",
				"b":     "ce032b305a9bc1ce0b0dd2a",
				"c":     "7f632b502707f6ffaa6bf2e",
				"ci":    "27",
				"co":    "389",
				"sm":    "19聚会《三体》网友的聚会地点是一处僻静",
				"pr":    "74",
				"rt":    "15",
				"ts":    "1744264311434",
				"rn":    "466",
				"sg":    "2b2ec618394b99deea35104168b86381da9f8946d4bc234e062fa320155409fb",
				"ct":    "1744264311",
				"ps":    "4ee326507a65a465g015fae",
				"pc":    "aab32e207a65a466g010615",
			},
			want: "b352ad8c",
		},
	}
	for _, v := range vectors {
		if got := SignPayload(v.params); got != v.want {
			t.Errorf("%s: SignPayload() = %s, want %s", v.name, got, v.want)
		}
	}
}

// TestAppID 是 appId = web_app_id(UA) 的向量（官方 JS 语义；wxread 抓包样本的 appId
// 来自其历史 UA，无法复现，故不作为本测试向量，见 protocol.go 包注释）。
func TestAppID(t *testing.T) {
	vectors := []struct {
		ua   string
		want string
	}{
		{
			ua:   "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Edg/131.0.0.0",
			want: "wb182564874663h1753858726",
		},
		{
			ua:   "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36 Edg/135.0.0.0",
			want: "wb115321887466h529830856",
		},
		{ua: "Mozilla/5.0", want: "wb1h925749708"},
		{ua: "", want: "wb0h0"},
	}
	for _, v := range vectors {
		if got := AppID(v.ua); got != v.want {
			t.Errorf("AppID(%q) = %s, want %s", v.ua, got, v.want)
		}
	}
}

// TestEnterReportPayloadFieldSet 验证 enter report 字段集：含 appId/b/c/ci/co/sm/pr/ct/ps/pc/s，
// 不含 rt/ts/rn/sg（CONTEXT.md Enter report 定义）；s 与字段自我一致（重算相等）。
func TestEnterReportPayloadFieldSet(t *testing.T) {
	now := time.Unix(1744264311, 0)
	pos := ReadingProgress{
		BookID: "695233", ChapterUID: 112, ChapterIdx: 27, ChapterOffset: 389,
		Progress: 74, Summary: "19聚会《三体》网友的聚会地点是一处僻静",
	}
	rc := ReaderContext{Psvts: "4ee326507a65a465g015fae", Pclts: "aab32e207a65a466g010615", Token: "tok"}
	got := EnterReportPayload(pos, rc, ResolvePC(rc, now), now, testUA)

	for _, k := range []string{"appId", "b", "c", "ci", "co", "sm", "pr", "ct", "ps", "pc", "s"} {
		if _, ok := got[k]; !ok {
			t.Errorf("enter payload 缺少字段 %q", k)
		}
	}
	for _, k := range []string{"rt", "ts", "rn", "sg"} {
		if _, ok := got[k]; ok {
			t.Errorf("enter payload 不应含字段 %q", k)
		}
	}
	if got["s"] != SignPayload(withoutS(got)) {
		t.Errorf("enter payload s 与字段不一致: %s vs %s", got["s"], SignPayload(withoutS(got)))
	}
	if got["b"] != EncodeID("695233") || got["c"] != EncodeID("112") {
		t.Errorf("b/c 编码错误: b=%s c=%s", got["b"], got["c"])
	}
	if got["ct"] != "1744264311" {
		t.Errorf("ct = %s, want 1744264311", got["ct"])
	}
}

// TestTimedReportPayloadFieldSet 验证 timed report 字段集：enter 全字段 + rt/ts/rn/sg + s；
// sg = sha256(ts+rn+token) 且 s 与字段自洽。
func TestTimedReportPayloadFieldSet(t *testing.T) {
	now := time.Unix(1744264311, 0)
	pos := ReadingProgress{
		BookID: "695233", ChapterUID: 112, ChapterIdx: 27, ChapterOffset: 389,
		Progress: 74, Summary: "19聚会《三体》网友的聚会地点是一处僻静",
	}
	rc := ReaderContext{Psvts: "4ee326507a65a465g015fae", Pclts: "aab32e207a65a466g010615", Token: "tok"}
	got := TimedReportPayload(pos, rc, ResolvePC(rc, now), now, testUA, 15, 1744264311434, 466)

	for _, k := range []string{"appId", "b", "c", "ci", "co", "sm", "pr", "ct", "ps", "pc", "rt", "ts", "rn", "sg", "s"} {
		if _, ok := got[k]; !ok {
			t.Errorf("timed payload 缺少字段 %q", k)
		}
	}
	if got["rt"] != "15" || got["ts"] != "1744264311434" || got["rn"] != "466" {
		t.Errorf("rt/ts/rn 错误: %s/%s/%s", got["rt"], got["ts"], got["rn"])
	}
	if got["sg"] != SG("1744264311434", "466", "tok") {
		t.Errorf("sg = %s, want %s", got["sg"], SG("1744264311434", "466", "tok"))
	}
	if got["s"] != SignPayload(withoutS(got)) {
		t.Errorf("timed payload s 与字段不一致: %s vs %s", got["s"], SignPayload(withoutS(got)))
	}
}

// TestPayloadPositionRules 验证位置字段规整规则：progress 夹取、偏移非负、
// sm 截取 20 代码点、pc 缺失时经 ResolvePC 回退 e(会话建立时刻)、chapterUid=0 时
// c=e("0")（官方 JS e(chapterUid||0)）。
func TestPayloadPositionRules(t *testing.T) {
	now := time.Unix(1744264311, 0)
	pos := ReadingProgress{
		BookID: "695233", ChapterUID: 0, ChapterIdx: -3, ChapterOffset: -1,
		Progress: 150, Summary: "这是一段超过二十个字符的摘要文本用于验证截取行为是否正确生效",
	}
	rc := ReaderContext{Psvts: "", Pclts: ""} // ps 为空透传，pc 应回退
	pc := ResolvePC(rc, now)
	got := EnterReportPayload(pos, rc, pc, now, testUA)

	if got["pr"] != "100" || got["ci"] != "0" || got["co"] != "0" {
		t.Errorf("规整错误: pr=%s ci=%s co=%s", got["pr"], got["ci"], got["co"])
	}
	if runes := len([]rune(got["sm"])); runes != SummaryMaxChars {
		t.Errorf("sm 截取后长度 = %d, want %d", runes, SummaryMaxChars)
	}
	if got["pc"] != EncodeID("1744264311") {
		t.Errorf("pc 回退错误: %s, want %s", got["pc"], EncodeID("1744264311"))
	}
	if pc != EncodeID("1744264311") {
		t.Errorf("ResolvePC 回退错误: %s, want %s", pc, EncodeID("1744264311"))
	}
	if got["ps"] != "" {
		t.Errorf("ps 应为空字符串透传, got %q", got["ps"])
	}
	if got["c"] != EncodeID("0") {
		t.Errorf("chapterUid=0 时 c 应等于 e(0), got %s", got["c"])
	}
	if got["s"] != SignPayload(withoutS(got)) {
		t.Errorf("s 不一致")
	}
}

// TestPcltsZeroFallback 验证 pc == "0" 也触发回退（参考实现 tonumber(pc)==0 语义），
// 经 ResolvePC 在会话建立时解析为 e(会话建立时刻)。
func TestPcltsZeroFallback(t *testing.T) {
	now := time.Unix(1744264311, 0)
	pos := ReadingProgress{BookID: "695233", ChapterUID: 1}
	pc := ResolvePC(ReaderContext{Psvts: "ps", Pclts: "0"}, now)
	got := EnterReportPayload(pos, ReaderContext{Psvts: "ps", Pclts: "0"}, pc, now, testUA)
	if got["pc"] != EncodeID("1744264311") {
		t.Errorf("pc 应为 e(会话建立时刻), got %s", got["pc"])
	}
}

// TestResolvePC 断言会话级 pc 的解析（issue 30）：Pclts 可用（非空、非 "0"，ticket
// 25 语义）时返回 Reader Context 原值（与 issue 30 之前的行为一致）；为空或 "0" 时
// 返回 e(会话建立时刻的秒级时间戳)——fallback pc 只在明确建立新的 Reading Session
// 时生成，会话内（enter 与全部 timed reports）复用，与官方客户端"初始 pclts 为 0 的
// 页面发送非零 pc、会话内复用"的口径一致。
func TestResolvePC(t *testing.T) {
	sessionAt := time.Unix(1744333820, 0) // e() golden 向量使用的秒级时间戳

	// 可用 pclts（已编码串 / 非零数字时间戳串）：返回原值，与会话建立时刻无关。
	for _, pclts := range []string{"aab32e207a65a466g010615", "1744333820"} {
		if got := ResolvePC(ReaderContext{Pclts: pclts}, sessionAt); got != pclts {
			t.Errorf("ResolvePC(%q) = %q, want 原值 %q", pclts, got, pclts)
		}
	}
	if got := ResolvePC(ReaderContext{Pclts: "aab32e207a65a466g010615"}, sessionAt.Add(5*time.Minute)); got != "aab32e207a65a466g010615" {
		t.Errorf("可用 pclts 不得受会话建立时刻影响, got %q", got)
	}

	// 空 / "0"：回退 e(会话建立时刻的秒级时间戳)。
	for _, pclts := range []string{"", "0"} {
		want := EncodeID(strconv.FormatInt(sessionAt.Unix(), 10))
		if got := ResolvePC(ReaderContext{Pclts: pclts}, sessionAt); got != want {
			t.Errorf("ResolvePC(%q) = %q, want fallback %q", pclts, got, want)
		}
	}

	// 不同会话建立时刻产生不同的 fallback（重建不复用前一会话的值）。
	a := ResolvePC(ReaderContext{}, sessionAt)
	b := ResolvePC(ReaderContext{}, sessionAt.Add(time.Second))
	if a == b {
		t.Errorf("不同建立时刻的 fallback pc 应不同: %s", a)
	}
}

// TestIsAccepted 验证成功判定：succ==1（bool/数值/字符串）或 synckey 存在即接受；
// 其余情况拒绝（与 weread.koplugin 一致；wxread 更严格需两者兼备，两者非共识；
// checklist #9 边界未经实测，本测试断言本包与 koplugin 一致的 OR 判定）。
func TestIsAccepted(t *testing.T) {
	truthy := []map[string]any{
		{"succ": true},
		{"succ": 1},
		{"succ": float64(1)},
		{"succ": "1"},
		{"succ": 0, "synckey": "xyz"},
		{"succ": false, "synckey": map[string]any{}},
		{"synckey": "abc"},
	}
	for _, body := range truthy {
		if !IsAccepted(body) {
			t.Errorf("IsAccepted(%v) = false, want true", body)
		}
	}
	falsy := []map[string]any{
		nil,
		{},
		{"succ": false},
		{"succ": 0},
		{"succ": "0"},
		{"succ": nil, "synckey": nil},
		{"errCode": -2012, "errmsg": "bad"},
	}
	for _, body := range falsy {
		if IsAccepted(body) {
			t.Errorf("IsAccepted(%v) = true, want false", body)
		}
	}
}

// TestTruncateSummaryUTF16Length 验证 sm 截取以 UTF-16 码元计（官方 substr(0,20)），
// 且不切开代理对（emoji 占 2 个码元）。
func TestTruncateSummaryUTF16Length(t *testing.T) {
	// 20 个中文字符 = 20 码元，完整保留。
	chinese := "一二三四五六七八九十一二三四五六七八九十"
	if got := truncateSummary(chinese); got != chinese {
		t.Errorf("20 个 BMP 字符应保留, got %q", got)
	}
	// 21 个中文字符 → 20 码元。
	long := chinese + "甲"
	if got := truncateSummary(long); len(utf16Units(got)) != SummaryMaxChars {
		t.Errorf("21 字符截取后 UTF-16 长度 = %d, want 20", len(utf16Units(got)))
	}
	// 11 个 emoji = 22 码元 → 截到 10 个（20 码元），不切开代理对。
	surf := strings.Repeat("😀", 11)
	got := truncateSummary(surf)
	if len(utf16Units(got)) != 20 || got != strings.Repeat("😀", 10) {
		t.Errorf("emoji 截取: got %q (units=%d)", got, len(utf16Units(got)))
	}
}

func TestRepeatHelper(t *testing.T) {
	if got := repeat("x", 3); got != "xxx" {
		t.Errorf("repeat = %q", got)
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

// withoutS 返回移除 s 键的副本（构造器在写入 s 之前签名，重算时需用不含 s 的参数集）。
func withoutS(params map[string]string) map[string]string {
	cp := make(map[string]string, len(params))
	for k, v := range params {
		if k != "s" {
			cp[k] = v
		}
	}
	return cp
}

const testUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Edg/131.0.0.0"
