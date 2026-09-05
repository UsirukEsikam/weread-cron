// 应用边界 seam 测试（ADR-0006）：进程内使用生产代码，构造注入 clock、RNG、
// HTTP endpoints、session store；httptest.Server 扮演微信读书服务端与通知端点；
// 临时目录充当 /data。只断言线上行为：请求时序、payload 字段集与 s/sg 重算、
// /data 文件内容、通知端点收到的消息体。
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"weread-cron/internal/clock"
	"weread-cron/internal/config"
	"weread-cron/internal/session"
	"weread-cron/internal/task"
	"weread-cron/internal/terminal"
	"weread-cron/internal/weread"
)

const (
	testBookID    = "695233"
	testBookTitle = "三体全集"
	testChapter   = 112
	// reader token 来自 Reader 页 __INITIAL_STATE__（sg 用它重算）。
	testReaderToken = "fake-reader-token-42"
	// 页面携带的 psvts/pclts（e() 编码后的秒级时间戳；与真实页面形态一致）。
	testPsvts = "4ee326507a65a465g015fae"
	testPclts = "aab32e207a65a466g010615"
	testSummary = "太空是无尽黑暗的，深邃而寒冷"
)

var testTZ = time.FixedZone("Asia/Shanghai", 8*3600)

// wereadRequest 是 fake 服务端记录的一条请求。
type wereadRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// fakeWeread 扮演微信读书服务端：renewal、Reader 页、/web/book/read。
type fakeWeread struct {
	t *testing.T

	mu       sync.Mutex
	requests []wereadRequest

	// lastReaderEncoded 是最近一次 Reader 页请求的编码 bookId（referer 断言用）。
	lastReaderEncoded string

	// reportRejected 为 true 时 /web/book/read 返回 {"errCode":-2014}（拒绝）；
	// renewNoCookies 为 true 时 renewal 不返回 Set-Cookie。
	reportRejected bool
	renewNoCookies bool

	// blockTimed 非 nil 时，下一笔 timed report 请求到达后等待 channel 关闭才响应
	//（用于注入"响应期间时钟跳变"模拟 suspend）。
	blockTimed          chan struct{}
	blockTimedTriggered chan struct{}
}

func newFakeWeread(t *testing.T) *fakeWeread {
	return &fakeWeread{t: t}
}

func (f *fakeWeread) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/web/login/renewal", f.handleRenewal)
	mux.HandleFunc("/web/reader/", f.handleReaderPage)
	mux.HandleFunc("/web/book/read", f.handleReport)
	return mux
}

func (f *fakeWeread) record(r *http.Request) {
	body := readAll(r) // 读取并还原，后续 handler 仍可读
	f.mu.Lock()
	f.requests = append(f.requests, wereadRequest{
		Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body,
	})
	f.mu.Unlock()
}

// snapshot 返回请求记录副本（按到达顺序）。
func (f *fakeWeread) snapshot() []wereadRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]wereadRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

func (f *fakeWeread) count(prefix string) int {
	n := 0
	for _, r := range f.snapshot() {
		if strings.HasPrefix(r.Path, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeWeread) handleRenewal(w http.ResponseWriter, r *http.Request) {
	f.record(r)
	// 断言：renewal 请求体固定（spec 决策 #5）。
	body := `{"rq":"%2Fweb%2Fbook%2Fread","ql":false}`
	if got := strings.TrimSpace(string(readAll(r))); got != body {
		f.t.Errorf("renewal body = %s，期望 %s", got, body)
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/json;charset=UTF-8" {
		f.t.Errorf("renewal Content-Type = %q", ct)
	}
	if f.renewNoCookies {
		fmt.Fprint(w, `{"succ":1}`)
		return
	}
	// 新 Cookie（wr_*；属性待 ticket 04 核对持久化细节，这里断言并入会话）。
	http.SetCookie(w, &http.Cookie{Name: "wr_gid", Value: "new123", Path: "/"})
	http.SetCookie(w, &http.Cookie{Name: "wr_skey", Value: "newabc", Path: "/", HttpOnly: true})
	fmt.Fprint(w, `{"succ":1}`)
}

func (f *fakeWeread) handleReaderPage(w http.ResponseWriter, r *http.Request) {
	f.record(r)
	// 与 koplugin 一致：reader 页 referer 指向自身。
	if ref := r.Header.Get("Referer"); ref != "http://"+r.Host+r.URL.Path {
		f.t.Errorf("reader 页 Referer = %q，期望自身 %q", ref, "http://"+r.Host+r.URL.Path)
	}
	if r.Header.Get("User-Agent") != weread.DefaultUserAgent {
		f.t.Errorf("reader 页 UA = %q", r.Header.Get("User-Agent"))
	}
	f.mu.Lock()
	f.lastReaderEncoded = strings.TrimPrefix(r.URL.Path, "/web/reader/")
	f.mu.Unlock()
	io.WriteString(w, readerPageHTML())
}

func (f *fakeWeread) handleReport(w http.ResponseWriter, r *http.Request) {
	f.record(r)
	isTimed := strings.Contains(string(readAll(r)), `"rt":`)
	if isTimed && f.blockTimed != nil {
		f.mu.Lock()
		ch := f.blockTimed
		f.mu.Unlock()
		if f.blockTimedTriggered != nil {
			select {
			case f.blockTimedTriggered <- struct{}{}:
			default:
			}
		}
		<-ch
	}
	// 与 koplugin 一致：report referer = Reader 页地址（同书）。
	f.mu.Lock()
	lastReader := f.lastReaderEncoded
	f.mu.Unlock()
	if lastReader == "" {
		f.t.Errorf("report 先于 Reader 页到达（referer 无从断言）")
	} else if ref := r.Header.Get("Referer"); ref != "http://"+r.Host+"/web/reader/"+lastReader {
		f.t.Errorf("report Referer = %q，期望 reader 页 %q", ref, "http://"+r.Host+"/web/reader/"+lastReader)
	}
	if f.reportRejected {
		fmt.Fprint(w, `{"errCode":-2014,"errMsg":"err"}`)
		return
	}
	fmt.Fprint(w, `{"succ":1}`)
}

// readAll 读取（并还原）r.Body。
func readAll(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	b, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(strings.NewReader(string(b)))
	return b
}

// readerPageHTML 生成含 __INITIAL_STATE__ 的 Reader 页（与真实页面形态一致：
// JSON 后跟 "); (function"，chapterOffset 为字符串演示 flexInt 兼容）。
func readerPageHTML() string {
	state := map[string]any{
		"reader": map[string]any{
			"psvts": testPsvts,
			"pclts": testPclts,
			"token": testReaderToken,
			"bookInfo": map[string]any{
				"bookId": testBookID,
				"title":  testBookTitle,
			},
			"currentChapter": map[string]any{
				"chapterUid":    testChapter,
				"chapterIdx":    3,
				"chapterOffset": "1234", // 真实页面常见字符串形式
			},
			"progress": map[string]any{
				"book": map[string]any{
					"chapterUid":    testChapter,
					"chapterIdx":    3,
					"chapterOffset": 1234,
					"progress":      35,
					"summary":       testSummary,
				},
			},
		},
	}
	raw, err := json.Marshal(state)
	if err != nil {
		panic(err)
	}
	return `<html><head><title>` + testBookTitle + `</title></head><body>` +
		`<script>window.__INITIAL_STATE__ = ` + string(raw) + `; (function(){ /* ... */ })();</script>` +
		`</body></html>`
}

// notifyRecord 是 fake 通知端点记录的请求。
type notifyRecord struct {
	Path  string
	Body  map[string]any
	Raw   []byte
	Check func() error // 请求到达时执行（断言终态已先落盘等时序）
}

// fakeNotify 扮演 Bark / 企业微信端点。
type fakeNotify struct {
	t      *testing.T
	mu     sync.Mutex
	status int // 返回的 HTTP 状态（默认 200）
	Check  func() error
	records []notifyRecord
}

func newFakeNotify(t *testing.T) *fakeNotify {
	return &fakeNotify{t: t, status: 200}
}

func (f *fakeNotify) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(raw, &m)
		rec := notifyRecord{Path: r.URL.Path, Body: m, Raw: raw, Check: f.Check}
		f.mu.Lock()
		f.records = append(f.records, rec)
		f.mu.Unlock()
		if f.Check != nil {
			if err := f.Check(); err != nil {
				f.t.Errorf("通知端点时序断言失败: %v", err)
			}
		}
		w.WriteHeader(f.status)
		fmt.Fprint(w, `{"ok":true}`)
	})
}

func (f *fakeNotify) snapshot() []notifyRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]notifyRecord, len(f.records))
	copy(out, f.records)
	return out
}

// testHarness 是应用 seam 测试的公共装配。
type testHarness struct {
	t      *testing.T
	app    *App
	weread *fakeWeread
	bark   *fakeNotify
	wecom  *fakeNotify
	clk    *clock.Fake
	rng    *rand.Rand
	cfg    *config.Config
}

// setup 装配：fake weread/通知端点（mutate 后可改行为）、fake clock
// （2025-09-06 10:00 CST）、seeded RNG、临时 /data。
func setup(t *testing.T, mutate func(h *testHarness)) *testHarness {
	t.Helper()
	cfg := &config.Config{
		Cookie:         "wr_skey=abc; wr_gid=123",
		Books:          []string{testBookID},
		ReadMinutesMin: 1,
		ReadMinutesMax: 1,
		DataDir:        t.TempDir(),
		TZ:             testTZ,
	}
	h := &testHarness{
		t:      t,
		weread: newFakeWeread(t),
		bark:   &fakeNotify{t: t, status: 200},
		wecom:  &fakeNotify{t: t, status: 200},
		clk:    clock.NewFake(time.Date(2025, 9, 6, 10, 0, 0, 0, testTZ)),
		rng:    rand.New(rand.NewSource(42)),
		cfg:    cfg,
	}
	if mutate != nil {
		mutate(h)
	}
	wereadSrv := httptest.NewServer(h.weread.Handler())
	t.Cleanup(wereadSrv.Close)
	barkSrv := httptest.NewServer(h.bark.Handler())
	t.Cleanup(barkSrv.Close)
	wecomSrv := httptest.NewServer(h.wecom.Handler())
	t.Cleanup(wecomSrv.Close)
	cfg.BarkURL = barkSrv.URL
	cfg.WeComWebhookURL = wecomSrv.URL

	h.app = New(cfg, Deps{
		Clock:         h.clk,
		RNG:           h.rng,
		WereadBaseURL: wereadSrv.URL,
		Sessions:      session.NewFileStore(cfg.DataDir),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return h
}

// runTask 执行 Task 并等待结果（带超时护栏）。
func (h *testHarness) runTask(ctx context.Context) (task.Result, error) {
	h.t.Helper()
	type outcome struct {
		res task.Result
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		res, err := h.app.RunTask(ctx)
		ch <- outcome{res, err}
	}()
	select {
	case o := <-ch:
		return o.res, o.err
	case <-time.After(30 * time.Second):
		h.t.Fatal("RunTask 超时")
		return task.Result{}, nil
	}
}

// payloadFromWire 把线上 JSON（数字字段为数字字面量）解码为 map[string]string，
// 用于重算 s/sg。
func payloadFromWire(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("解析线上 payload 失败: %v; body=%s", err, raw)
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if len(v) > 0 && v[0] != '"' {
			out[k] = string(v) // 数字字面量
		} else {
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				t.Fatalf("字段 %s 不是字符串: %v (raw=%s)", k, err, v)
			}
			out[k] = s
		}
	}
	return out
}

// reportRecords 返回 /web/book/read 的请求记录。
func (h *testHarness) reportRecords() []wereadRequest {
	var out []wereadRequest
	for _, r := range h.weread.snapshot() {
		if r.Path == "/web/book/read" {
			out = append(out, r)
		}
	}
	return out
}

// verifySignature 重算 payload 的 s 并断言一致。
func verifySignature(t *testing.T, p map[string]string) {
	t.Helper()
	params := make(map[string]string, len(p))
	for k, v := range p {
		if k != "s" {
			params[k] = v
		}
	}
	if got := weread.SignPayload(params); got != p["s"] {
		t.Errorf("s 重算不一致：线上 %s，重算 %s", p["s"], got)
	}
}

// verifySG 重算 timed payload 的 sg（sha256(ts+rn+token)；token 来自 Reader 页）。
func verifySG(t *testing.T, p map[string]string, token string) {
	t.Helper()
	want := sha256.Sum256([]byte(p["ts"] + p["rn"] + token))
	if hex.EncodeToString(want[:]) != p["sg"] {
		t.Errorf("sg 重算不一致：线上 %s，重算 %s", p["sg"], hex.EncodeToString(want[:]))
	}
}

// assertNumeric 断言线上字段是数字字面量（protocol.go：ci/co/pr/ct/rt/ts/rn 为数字）。
func assertNumeric(t *testing.T, raw []byte, fields ...string) {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("解析线上 payload 失败: %v", err)
	}
	for _, f := range fields {
		v, ok := m[f]
		if !ok {
			t.Errorf("payload 缺字段 %s", f)
			continue
		}
		if len(v) > 0 && v[0] == '"' {
			t.Errorf("字段 %s 在线上必须是数字，实际为字符串 %s", f, v)
		}
	}
}

// --- 测试用例 ---

// TestRunHappyPath 是主 happy path：请求时序、payload 字段集与 s/sg 重算、
// rt 演进、终态先落盘再通知、通知内容、会话文件、Result 字段。
func TestRunHappyPath(t *testing.T) {
	h := setup(t, nil)
	ctx := context.Background()

	// 终态必须在通知请求到达前已落盘（spec 决策 #9/#10）。
	checkTerminal := func() error {
		data, err := os.ReadFile(filepath.Join(h.cfg.DataDir, terminal.FileName))
		if err != nil {
			return fmt.Errorf("通知时 Terminal State 尚未落盘: %w", err)
		}
		var st terminal.State
		if err := json.Unmarshal(data, &st); err != nil {
			return fmt.Errorf("Terminal State 解析失败: %w", err)
		}
		if st.LastTaskDate != "2025-09-06" || st.LastTaskResult != terminal.ResultSuccess {
			return fmt.Errorf("Terminal State 内容 = %+v", st)
		}
		return nil
	}
	h.bark.Check = checkTerminal
	h.wecom.Check = checkTerminal

	res, err := h.runTask(ctx)
	if err != nil {
		t.Fatalf("RunTask 失败: %v", err)
	}

	// Result 字段（计划/实际时长、report 次数、书名）。
	if res.Date != "2025-09-06" {
		t.Errorf("Date = %q", res.Date)
	}
	if res.BookTitle != testBookTitle || res.BookID != testBookID {
		t.Errorf("书名 = %q/%q", res.BookTitle, res.BookID)
	}
	if res.Planned != time.Minute {
		t.Errorf("Planned = %v，期望 1 分钟", res.Planned)
	}
	if res.Actual != time.Minute {
		t.Errorf("Actual = %v，期望 1 分钟（2 次 rt=30）", res.Actual)
	}
	if res.Reports != 2 {
		t.Errorf("Reports = %d，期望 2", res.Reports)
	}

	// ---- 线上请求时序：renewal → reader 页 → enter → timed → timed ----
	reqs := h.weread.snapshot()
	if len(reqs) < 5 {
		t.Fatalf("请求数 = %d，期望 ≥5；记录:%+v", len(reqs), reqs)
	}
	if reqs[0].Path != "/web/login/renewal" || reqs[0].Method != http.MethodPost {
		t.Errorf("首个请求应为 renewal POST，实际 %s %s", reqs[0].Method, reqs[0].Path)
	}
	if reqs[1].Path != "/web/reader/"+weread.EncodeID(testBookID) {
		t.Errorf("第二请求应为 Reader 页 %s，实际 %s", "/web/reader/"+weread.EncodeID(testBookID), reqs[1].Path)
	}

	// Renewal 请求头（UA / Origin）。
	if ua := reqs[0].Header.Get("User-Agent"); ua != weread.DefaultUserAgent {
		t.Errorf("renewal UA = %q", ua)
	}
	if org := reqs[0].Header.Get("Origin"); org != h.app.deps.WereadBaseURL {
		t.Errorf("renewal Origin = %q", org)
	}
	// 与 koplugin 一致：reader 页 referer 指向自身。
	if ref := reqs[1].Header.Get("Referer"); ref != h.app.deps.WereadBaseURL+"/web/reader/"+weread.EncodeID(testBookID) {
		t.Errorf("reader 页 Referer = %q", ref)
	}
	// renewal 的新 Cookie 应已并入会话（reader 页请求携带着）。
	if c := reqs[1].Header.Get("Cookie"); !strings.Contains(c, "wr_gid=new123") || !strings.Contains(c, "wr_skey=newabc") {
		t.Errorf("reader 页请求 Cookie = %q，期望已并入 renewal 新 Cookie", c)
	}

	// ---- enter report ----
	reports := h.reportRecords()
	if len(reports) != 3 {
		t.Fatalf("report 请求数 = %d，期望 3（1 enter + 2 timed）", len(reports))
	}
	enter := payloadFromWire(t, reports[0].Body)
	for _, k := range []string{"appId", "b", "c", "ci", "co", "sm", "pr", "ct", "ps", "pc", "s"} {
		if _, ok := enter[k]; !ok {
			t.Errorf("enter payload 缺字段 %s", k)
		}
	}
	for _, k := range []string{"rt", "ts", "rn", "sg"} {
		if _, ok := enter[k]; ok {
			t.Errorf("enter payload 不应含字段 %s", k)
		}
	}
	verifySignature(t, enter)
	assertNumeric(t, reports[0].Body, "ci", "co", "pr", "ct")
	if enter["b"] != weread.EncodeID(testBookID) {
		t.Errorf("b = %q，期望 %q", enter["b"], weread.EncodeID(testBookID))
	}
	if enter["c"] != weread.EncodeID(fmt.Sprint(testChapter)) {
		t.Errorf("c = %q", enter["c"])
	}
	if enter["ci"] != "3" || enter["co"] != "1234" || enter["pr"] != "35" {
		t.Errorf("位置字段 ci=%s co=%s pr=%s", enter["ci"], enter["co"], enter["pr"])
	}
	if enter["ps"] != testPsvts || enter["pc"] != testPclts {
		t.Errorf("ps=%s pc=%s", enter["ps"], enter["pc"])
	}
	if want := weread.AppID(weread.DefaultUserAgent); enter["appId"] != want {
		t.Errorf("appId = %q，期望 %q", enter["appId"], want)
	}

	// ---- timed reports：rt 每次为距上次成功接受的实际间隔（ADR-0004，非累计）----
	expectTS := time.Date(2025, 9, 6, 10, 0, 30, 0, testTZ).UnixMilli()
	for i, rec := range reports[1:] {
		p := payloadFromWire(t, rec.Body)
		for _, k := range []string{"appId", "b", "c", "ci", "co", "sm", "pr", "ct", "ps", "pc", "s", "rt", "ts", "rn", "sg"} {
			if _, ok := p[k]; !ok {
				t.Errorf("timed payload[%d] 缺字段 %s", i, k)
			}
		}
		if p["rt"] != "30" {
			t.Errorf("timed[%d].rt = %s，期望 30（节奏为实际间隔）", i, p["rt"])
		}
		if p["ts"] != fmt.Sprint(expectTS+int64(i)*30000) {
			t.Errorf("timed[%d].ts = %s，期望 %d", i, p["ts"], expectTS+int64(i)*30000)
		}
		verifySignature(t, p)
		verifySG(t, p, testReaderToken)
		assertNumeric(t, rec.Body, "rt", "ts", "rn", "ct")
	}
	// 本地累计达标即停止：恰好 2 次（1 分钟目标）；不继续上报。
	if h.weread.count("/web/book/read") != 3 {
		t.Errorf("report 总数 = %d，期望 3（达标停止）", h.weread.count("/web/book/read"))
	}

	// rn 来自 seeded RNG，且只发生在 target 生成之后（确定性）。
	witness := rand.New(rand.NewSource(42))
	witness.Intn(1) // pickBook 消耗
	witness.Intn(1) // target 消耗（min==max）
	for i, rec := range reports[1:] {
		p := payloadFromWire(t, rec.Body)
		want := fmt.Sprint(witness.Intn(1000))
		if p["rn"] != want {
			t.Errorf("timed[%d].rn = %s，seeded 重放期望 %s", i, p["rn"], want)
		}
	}

	// ---- /data：Terminal State 与 Login Session ----
	data, err := os.ReadFile(filepath.Join(h.cfg.DataDir, terminal.FileName))
	if err != nil {
		t.Fatalf("Terminal State 文件不存在: %v", err)
	}
	var st terminal.State
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	if st != (terminal.State{LastTaskDate: "2025-09-06", LastTaskResult: terminal.ResultSuccess}) {
		t.Errorf("Terminal State = %+v", st)
	}

	sessData, err := os.ReadFile(filepath.Join(h.cfg.DataDir, "login_session.json"))
	if err != nil {
		t.Fatalf("Login Session 文件不存在: %v", err)
	}
	for _, kv := range []string{`"name": "wr_gid"`, `"value": "new123"`, `"name": "wr_skey"`, `"value": "newabc"`} {
		if !strings.Contains(string(sessData), kv) {
			t.Errorf("Login Session 文件缺少 %s；内容:\n%s", kv, sessData)
		}
	}
	// 原子写不留半写临时文件。
	if leftovers, _ := filepath.Glob(filepath.Join(h.cfg.DataDir, ".tmp-*")); len(leftovers) > 0 {
		t.Errorf("DataDir 残留临时文件: %v", leftovers)
	}

	// ---- 通知内容（Bark + 企业微信，均到达且含关键信息）----
	for name, nf := range map[string]*fakeNotify{"bark": h.bark, "wecom": h.wecom} {
		recs := nf.snapshot()
		if len(recs) != 1 {
			t.Fatalf("%s 通知数 = %d，期望 1", name, len(recs))
		}
		msg, err := json.Marshal(recs[0].Body)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{testBookTitle, "计划时长：1 分钟", "实际累计：1 分钟", "上报次数：2", "2025-09-06"} {
			if !strings.Contains(string(msg), want) {
				t.Errorf("%s 通知缺少 %q；body=%s", name, want, msg)
			}
		}
	}
	// Bark 形态：title/body/group；企业微信形态：msgtype/text.content。
	if h.bark.snapshot()[0].Body["title"] != "微信读书阅读任务完成" {
		t.Errorf("Bark title = %v", h.bark.snapshot()[0].Body["title"])
	}
	if h.wecom.snapshot()[0].Body["msgtype"] != "text" {
		t.Errorf("WeCom msgtype = %v", h.wecom.snapshot()[0].Body["msgtype"])
	}
}

// TestRunTargetDurationDeterministic 断言 Target Duration 在配置范围内随机、一次 Task
// 只生成一次（seeded RNG 重放：首个 Intn 消耗于选书，第二个生成 target，之后每笔
// timed report 的 rn 依序对应）。
func TestRunTargetDurationDeterministic(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		// 多本候选：选书也走 RNG。
		h.cfg.Books = []string{testBookID, "112233"}
		h.cfg.ReadMinutesMin = 40
		h.cfg.ReadMinutesMax = 45
	})
	res, err := h.runTask(context.Background())
	if err != nil {
		t.Fatalf("RunTask 失败: %v", err)
	}

	witness := rand.New(rand.NewSource(42))
	wantBook := h.cfg.Books[witness.Intn(len(h.cfg.Books))]
	wantPlan := time.Duration(40+witness.Intn(6)) * time.Minute
	if res.BookID != wantBook {
		t.Errorf("BookID = %s，重放期望 %s", res.BookID, wantBook)
	}
	if res.Planned != wantPlan {
		t.Errorf("Planned = %v，重放期望 %v", res.Planned, wantPlan)
	}
	for _, rec := range h.reportRecords()[1:] {
		p := payloadFromWire(t, rec.Body)
		want := fmt.Sprint(witness.Intn(1000))
		if p["rn"] != want {
			t.Errorf("rn = %s，重放期望 %s", p["rn"], want)
		}
	}
	// 时长在配置范围内。
	if res.Planned < 40*time.Minute || res.Planned > 45*time.Minute {
		t.Errorf("Planned = %v 超出 [40,45] 分钟", res.Planned)
	}
}

// TestRunAbnormalIntervalRebuildsSession 断言异常间隔（>90s）不作为大 rt 上报而是
// 重建 Reading Session（重新 enter；Task 继续、累计保留）。
func TestRunAbnormalIntervalRebuildsSession(t *testing.T) {
	h := setup(t, nil)
	h.weread.blockTimed = make(chan struct{})
	h.weread.blockTimedTriggered = make(chan struct{}, 1)

	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		_, err := h.app.RunTask(ctx)
		done <- err
	}()

	// 等待第一笔 timed report 到达并被挂起。
	select {
	case <-h.weread.blockTimedTriggered:
	case <-time.After(10 * time.Second):
		t.Fatal("timed report 未在 10s 内到达")
	}
	// 模拟挂起噪声：响应期间时钟跳变 120s。
	h.clk.Advance(120 * time.Second)
	close(h.weread.blockTimed)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunTask 失败: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("RunTask 未在 30s 内完成")
	}

	// 线上：两次 enter（初始 + 重建），所有 rt 均为 30（120s 间隙从未上报）。
	enterCount, timedCount := 0, 0
	for _, rec := range h.reportRecords() {
		p := payloadFromWire(t, rec.Body)
		if _, ok := p["rt"]; !ok {
			enterCount++
			continue
		}
		timedCount++
		if p["rt"] != "30" {
			t.Errorf("rt = %s，期望 30（异常间隔不得作为大 rt 上报）", p["rt"])
		}
	}
	if enterCount != 2 {
		t.Errorf("enter 数 = %d，期望 2（初始 + 异常间隔重建）", enterCount)
	}
	if timedCount != 2 {
		t.Errorf("timed 数 = %d，期望 2（累计满 60s 停止）", timedCount)
	}

	// 累计保留且达标：Actual = 60s（不含 120s 间隙）。
	data, err := os.ReadFile(filepath.Join(h.cfg.DataDir, terminal.FileName))
	if err != nil {
		t.Fatal(err)
	}
	var st terminal.State
	json.Unmarshal(data, &st)
	if st.LastTaskResult != terminal.ResultSuccess {
		t.Errorf("Terminal State = %+v", st)
	}
}

// TestRunNotifyFailureDoesNotAffectTask 断言通知端点故障不影响 Task 结果与终态，
// 且渠道互不影响（Bark 500 时企业微信仍收到消息）。
func TestRunNotifyFailureDoesNotAffectTask(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.bark.status = 500
	})
	res, err := h.runTask(context.Background())
	if err != nil {
		t.Fatalf("通知端点故障不应使 Task 失败: %v", err)
	}
	if res.Reports != 2 {
		t.Errorf("Reports = %d", res.Reports)
	}
	data, err := os.ReadFile(filepath.Join(h.cfg.DataDir, terminal.FileName))
	if err != nil {
		t.Fatalf("Terminal State 应仍落盘: %v", err)
	}
	var st terminal.State
	json.Unmarshal(data, &st)
	if st.LastTaskResult != terminal.ResultSuccess {
		t.Errorf("Terminal State = %+v", st)
	}
	if got := len(h.wecom.snapshot()); got != 1 {
		t.Errorf("企业微信渠道应不受 Bark 故障影响，收到 %d 条", got)
	}
}

// TestRunSessionEstablishedFromInitialCookie 断言无 renewal Set-Cookie 时，初始
// Cookie 建立的 Login Session 仍已落盘（"初始 Cookie 建立 Login Session"）。
func TestRunSessionEstablishedFromInitialCookie(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.weread.renewNoCookies = true
	})
	if _, err := h.runTask(context.Background()); err != nil {
		t.Fatalf("RunTask 失败: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(h.cfg.DataDir, "login_session.json"))
	if err != nil {
		t.Fatalf("Login Session 文件不存在: %v", err)
	}
	for _, kv := range []string{`"name": "wr_skey"`, `"value": "abc"`, `"name": "wr_gid"`, `"value": "123"`} {
		if !strings.Contains(string(data), kv) {
			t.Errorf("Login Session 文件缺少 %q；内容:\n%s", kv, data)
		}
	}
}

// TestRunReportRejectedFailsTask 断言服务器拒绝上报时 Task 失败（恢复链由 ticket 05
// 交付；本票只保证明确失败、不产生成功终态）。
func TestRunReportRejectedFailsTask(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.weread.reportRejected = true
	})
	_, err := h.runTask(context.Background())
	if err == nil {
		t.Fatal("服务器拒绝上报时 Task 应失败")
	}
	if _, statErr := os.Stat(filepath.Join(h.cfg.DataDir, terminal.FileName)); !os.IsNotExist(statErr) {
		t.Errorf("失败时不得写入 success 终态")
	}
}
