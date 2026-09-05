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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"weread-cron/internal/clock"
	"weread-cron/internal/config"
	"weread-cron/internal/report"
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
	testPsvts   = "4ee326507a65a465g015fae"
	testPclts   = "aab32e207a65a466g010615"
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

	// reportRejected 为 true 时 /web/book/read 一律返回 {"errCode":-2014}（拒绝）；
	// timedRejectAll 为 true 时仅 timed report 一律拒绝（enter 接受）；
	// timedRejects > 0 时拒绝接下来 N 笔 timed report（恢复链中途成功场景）；
	// enterRejects > 0 时拒绝接下来 N 笔 enter report。
	// renewNoCookies 为 true 时 renewal 不返回 Set-Cookie。
	reportRejected bool
	timedRejectAll bool
	timedRejects   int
	enterRejects   int
	renewNoCookies bool

	// rotateReaderState 为 true 时每次 Reader 页抓取返回不同的 token/psvts
	//（第 N 次抓取 = fake-reader-token-N / fake-psvts-N），用于断言恢复链刷新后
	// 上报 payload 使用的是新 Reader Context。
	rotateReaderState bool
	readerFetches     int

	// readerFailAt > 0 时第 N 次 Reader 页抓取返回 HTTP 500（TTL 主动刷新失败的暂时性
	// 场景；后续抓取恢复正常）。
	readerFailAt int

	// renewReject > 0 时，前 N 次 renewal 返回 {"succ":0}（登录失效的明确证据形态）；
	// renewRejectFrom > 0 时从第 N 次起全部拒绝；renewRejectOnceAt > 0 时仅拒绝第 N 次
	//（恢复链场景：任务开始 renewal 必须成功、仅链中/重建后重试的选择性拒绝）；
	// renewStatus 非零时 renewal 返回该 HTTP 状态（传输/服务端故障，非明确证据）；
	// renewNoSucc 为 true 时 renewal 返回 200 但不含 succ 字段（如 errCode 错误体）。
	renewReject       int
	renewRejectFrom   int
	renewRejectOnceAt int
	renewAttempts     int
	renewStatus       int
	renewNoSucc       bool

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
	f.mu.Lock()
	f.renewAttempts++
	n := f.renewAttempts
	reject := f.renewReject > 0
	if reject {
		f.renewReject--
	}
	if f.renewRejectFrom > 0 && n >= f.renewRejectFrom {
		reject = true
	}
	if f.renewRejectOnceAt > 0 && n == f.renewRejectOnceAt {
		reject = true
	}
	status := f.renewStatus
	f.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		fmt.Fprint(w, `{"errCode":500,"errMsg":"boom"}`)
		return
	}
	if reject {
		fmt.Fprint(w, `{"succ":0,"errMsg":"login expired"}`)
		return
	}
	if f.renewNoSucc {
		fmt.Fprint(w, `{"errCode":-2012,"errMsg":"error"}`)
		return
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
	f.readerFetches++
	n := f.readerFetches
	rotate := f.rotateReaderState
	fail := f.readerFailAt > 0 && n == f.readerFailAt
	f.mu.Unlock()
	if fail {
		http.Error(w, "reader page unavailable", http.StatusInternalServerError)
		return
	}
	if rotate {
		io.WriteString(w, readerPageHTMLWith(fmt.Sprintf("fake-reader-token-%d", n), fmt.Sprintf("fake-psvts-%d", n)))
		return
	}
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
	f.mu.Lock()
	reject := f.reportRejected
	if isTimed {
		if f.timedRejectAll {
			reject = true
		} else if f.timedRejects > 0 {
			reject = true
			f.timedRejects--
		}
	} else if f.enterRejects > 0 {
		reject = true
		f.enterRejects--
	}
	f.mu.Unlock()
	if reject {
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

// readerPageHTMLWith 用指定 token/psvts 生成 Reader 页（恢复链刷新断言用）。
func readerPageHTMLWith(readerToken, psvts string) string {
	state := map[string]any{
		"reader": map[string]any{
			"psvts": psvts,
			"pclts": testPclts,
			"token": readerToken,
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

// readerPageHTML 生成含 __INITIAL_STATE__ 的 Reader 页（与真实页面形态一致：
// JSON 后跟 "); (function"，chapterOffset 为字符串演示 flexInt 兼容）。
func readerPageHTML() string {
	return readerPageHTMLWith(testReaderToken, testPsvts)
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
	t       *testing.T
	mu      sync.Mutex
	status  int // 返回的 HTTP 状态（默认 200）
	Check   func() error
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
// 重建 Reading Session（重新 enter；Task 继续、累计保留、同一本书）。
func TestRunAbnormalIntervalRebuildsSession(t *testing.T) {
	h := setup(t, nil)
	h.weread.blockTimed = make(chan struct{})
	h.weread.blockTimedTriggered = make(chan struct{}, 1)

	ctx := context.Background()
	done := make(chan struct{})
	var res task.Result
	var runErr error
	go func() {
		res, runErr = h.app.RunTask(ctx)
		close(done)
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
	case <-done:
		if runErr != nil {
			t.Fatalf("RunTask 失败: %v", runErr)
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

	// 重建后同一本书、Task 已累计时长保留：Result 只统计被接受的 rt（不含 120s 缺口）。
	if res.BookID != testBookID {
		t.Errorf("BookID = %s，期望重建后仍是同一本书 %s", res.BookID, testBookID)
	}
	if res.Actual != time.Minute {
		t.Errorf("Actual = %v，期望 1 分钟（缺口不计入）", res.Actual)
	}
	if res.Reports != 2 {
		t.Errorf("Reports = %d，期望 2", res.Reports)
	}

	// 累计保留且达标：success 终态。
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

// runTTLScenario 运行 20 分钟目标 Task（40 笔 timed report，跨越 DefaultContextTTL
// 边界）并断言公共骨架：Reports=40、41 笔 report（1 enter + 40 timed）、enter 恰 1 笔
// （无 enter 重复）、所有 rt=30（TTL 刷新不重置 rt 基准）。返回 report 记录供各测试
// 断言各自的 Context 切换边界。
func runTTLScenario(t *testing.T, h *testHarness) []wereadRequest {
	t.Helper()
	res, err := h.runTask(context.Background())
	if err != nil {
		t.Fatalf("RunTask 失败: %v", err)
	}
	if res.Reports != 40 {
		t.Fatalf("Reports = %d，期望 40（20 分钟 × 30s 节奏）", res.Reports)
	}
	reports := h.reportRecords()
	if len(reports) != 41 {
		t.Fatalf("report 数 = %d，期望 41（1 enter + 40 timed）", len(reports))
	}
	enterCount := 0
	for _, rec := range reports {
		if _, ok := payloadFromWire(t, rec.Body)["rt"]; !ok {
			enterCount++
		}
	}
	if enterCount != 1 {
		t.Errorf("enter 数 = %d，期望 1（TTL 刷新不重建 Reading Session）", enterCount)
	}
	for i, rec := range reports {
		p := payloadFromWire(t, rec.Body)
		if _, isTimed := p["rt"]; !isTimed {
			continue
		}
		if p["rt"] != "30" {
			t.Errorf("timed[%d].rt = %s，期望 30", i, p["rt"])
		}
	}
	return reports
}

// TestRunContextTTLExpiryRefreshesWithoutEnter 断言 Reader Context TTL（参考默认
// ≈15 分钟 = 900s；30s 节奏下第 30 笔 timed report 前到期）到期时主动重新抓取 Reader
// 页（新 token/psvts）刷新 Context；后续 timed report 继续——无 enter 重复、rt 不受
// 影响（用户故事 #30；spec 决策 #5/#6；验证清单 #7/#8 口径）。
func TestRunContextTTLExpiryRefreshesWithoutEnter(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.weread.rotateReaderState = true // 第 N 次抓取 = token-N/psvts-N，断言刷新生效
		h.cfg.ReadMinutesMin = 20
		h.cfg.ReadMinutesMax = 20
	})

	// ---- Reader 页恰好被抓取 2 次：Task 建立 + TTL 到期主动刷新；
	//      TTL 内的周期上报命中缓存，不产生额外请求。 ----
	reports := runTTLScenario(t, h)
	if n := h.weread.count("/web/reader/"); n != 2 {
		t.Errorf("Reader 页抓取数 = %d，期望 2（初始 + TTL 到期主动刷新）", n)
	}

	// ---- Context 切换边界：TTL = 900s、节奏 30s → 第 30 笔 timed（索引 30）起使用
	//      TTL 刷新后的 Context（psvts-2/token-2）；此前（索引 1..29）为初始 Context
	//      （psvts-1/token-1）。 ----
	enter := payloadFromWire(t, reports[0].Body)
	if enter["ps"] != "fake-psvts-1" {
		t.Errorf("enter.ps = %s，期望初始 Context（psvts-1）", enter["ps"])
	}
	for i, rec := range reports {
		p := payloadFromWire(t, rec.Body)
		if _, isTimed := p["rt"]; !isTimed {
			continue
		}
		switch {
		case i <= 29:
			verifySG(t, p, "fake-reader-token-1")
			if p["ps"] != "fake-psvts-1" {
				t.Errorf("timed[%d].ps = %s，期望初始 Context（psvts-1）", i, p["ps"])
			}
		default:
			verifySG(t, p, "fake-reader-token-2")
			if p["ps"] != "fake-psvts-2" {
				t.Errorf("timed[%d].ps = %s，期望 TTL 刷新后的 Context（psvts-2）", i, p["ps"])
			}
		}
	}
}

// TestRunContextTTLRefreshFailureContinuesWithExistingContext 断言 TTL 主动刷新失败
// （暂时性：HTTP 500）时 Task 不中断：沿用现有 Context 继续上报，下次周期重试刷新并
// 成功；Reading Session 无 enter 重建（ticket 05 对链中 refresh 暂时性失败的同一姿态）。
func TestRunContextTTLRefreshFailureContinuesWithExistingContext(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.weread.rotateReaderState = true
		h.weread.readerFailAt = 2 // 第 2 次抓取（TTL 到期那次）失败；第 3 次恢复
		h.cfg.ReadMinutesMin = 20
		h.cfg.ReadMinutesMax = 20
	})
	reports := runTTLScenario(t, h)

	// ---- 抓取 3 次：初始成功、TTL 到期失败、下次周期重试成功。 ----
	if n := h.weread.count("/web/reader/"); n != 3 {
		t.Errorf("Reader 页抓取数 = %d，期望 3（初始 + TTL 失败 + 重试成功）", n)
	}

	// ---- 失败周期（索引 30，t=900s 的 TTL 到期那次）沿用旧 Context（psvts-1）；
	//      之后（索引 31..40）使用重试成功的 Context（psvts-3/token-3）。 ----
	for i, rec := range reports {
		p := payloadFromWire(t, rec.Body)
		if _, isTimed := p["rt"]; !isTimed {
			continue
		}
		switch {
		case i <= 30:
			verifySG(t, p, "fake-reader-token-1")
			if p["ps"] != "fake-psvts-1" {
				t.Errorf("timed[%d].ps = %s，期望沿用现有 Context（psvts-1）", i, p["ps"])
			}
		default:
			verifySG(t, p, "fake-reader-token-3")
			if p["ps"] != "fake-psvts-3" {
				t.Errorf("timed[%d].ps = %s，期望重试刷新后的 Context（psvts-3）", i, p["ps"])
			}
		}
	}
}

// TestRunAbnormalIntervalBeyondTTLReentersWithFreshContext 断言时钟跳变同时越过异常
// 阈值（90s）与 Context TTL（900s）时：先主动重抓 Reader 页（新 token/psvts），再
// 重建 Reading Session——重建的 enter 使用刚重抓的新 Context（task.go 中"跳变越过
// TTL 时即为刚重抓的新 Context"路径的验证）。
func TestRunAbnormalIntervalBeyondTTLReentersWithFreshContext(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.weread.rotateReaderState = true
	})
	h.weread.blockTimed = make(chan struct{})
	h.weread.blockTimedTriggered = make(chan struct{}, 1)

	ctx := context.Background()
	done := make(chan struct{})
	var res task.Result
	var runErr error
	go func() {
		res, runErr = h.app.RunTask(ctx)
		close(done)
	}()

	// 等待第一笔 timed report 到达并被挂起。
	select {
	case <-h.weread.blockTimedTriggered:
	case <-time.After(10 * time.Second):
		t.Fatal("timed report 未在 10s 内到达")
	}
	// 模拟挂起噪声：响应期间时钟跳变 1200s（越过异常阈值与 Context TTL）。
	h.clk.Advance(1200 * time.Second)
	close(h.weread.blockTimed)

	select {
	case <-done:
		if runErr != nil {
			t.Fatalf("RunTask 失败: %v", runErr)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("RunTask 未在 30s 内完成")
	}

	// ---- 线上顺序：enter → timed(挂起前) → 跳变越过 TTL 时主动重抓 Reader 页 →
	//      enter(重建 Reading Session，使用重抓后的新 Context) → timed。 ----
	reports := h.reportRecords()
	if len(reports) != 4 {
		t.Fatalf("report 数 = %d，期望 4（2 enter + 2 timed）", len(reports))
	}
	if n := h.weread.count("/web/reader/"); n != 2 {
		t.Errorf("Reader 页抓取数 = %d，期望 2（初始 + 跳变越过 TTL 的主动重抓）", n)
	}
	for i, rec := range reports {
		p := payloadFromWire(t, rec.Body)
		if _, isTimed := p["rt"]; !isTimed {
			continue
		}
		if p["rt"] != "30" {
			t.Errorf("timed[%d].rt = %s，期望 30（缺口不上报）", i, p["rt"])
		}
		if i == 1 {
			verifySG(t, p, "fake-reader-token-1")
		} else {
			verifySG(t, p, "fake-reader-token-2")
			if p["ps"] != "fake-psvts-2" {
				t.Errorf("timed[%d].ps = %s，期望重建后的新 Context", i, p["ps"])
			}
		}
	}
	// enter：初始（索引 0）用初始 Context；重建（索引 2）用重抓后的新 Context。
	if ps := payloadFromWire(t, reports[2].Body)["ps"]; ps != "fake-psvts-2" {
		t.Errorf("重建 enter.ps = %s，期望重抓后的新 Context（psvts-2）", ps)
	}

	// ---- 重建后同一本书、Task 已累计时长保留（缺口不计入）、达标 success。 ----
	if res.BookID != testBookID {
		t.Errorf("BookID = %s，期望重建后仍是同一本书 %s", res.BookID, testBookID)
	}
	if res.Actual != time.Minute || res.Reports != 2 {
		t.Errorf("Result = actual:%v reports:%d，期望 1 分钟/2 次（缺口不计入）", res.Actual, res.Reports)
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

// requestKinds 把 fake 服务端请求记录压缩为可断言的序列（renewal/refresh/enter/timed）。
func (h *testHarness) requestKinds() []string {
	var kinds []string
	for _, r := range h.weread.snapshot() {
		switch {
		case r.Path == "/web/login/renewal":
			kinds = append(kinds, "renewal")
		case strings.HasPrefix(r.Path, "/web/reader/"):
			kinds = append(kinds, "refresh")
		case r.Path == "/web/book/read":
			if strings.Contains(string(r.Body), `"rt":`) {
				kinds = append(kinds, "timed")
			} else {
				kinds = append(kinds, "enter")
			}
		}
	}
	return kinds
}

// assertRequestSequence 断言请求按序等于 want（ticket 05：恢复链顺序与次数有界）。
func (h *testHarness) assertRequestSequence(want ...string) {
	h.t.Helper()
	got := h.requestKinds()
	if !reflect.DeepEqual(got, want) {
		h.t.Errorf("请求序列 = %v\n期望 = %v", got, want)
	}
}

// --- ticket 05：有界恢复链与失败通知 ---

// TestRunReportRejectedRecoveryChainExhausted 断言 timed report 被拒时按有序恢复链
// refresh → retry → renewal → refresh → retry 恢复（顺序与次数有界），仍失败 →
// failed 终态（先落盘再通知）+ 失败通知（失败阶段/主要错误/已尝试恢复动作）。
func TestRunReportRejectedRecoveryChainExhausted(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.weread.timedRejectAll = true // enter 接受；timed 一律拒绝
	})
	// 通知端点收到失败通知时 failed 终态必须已落盘（spec 决策 #9/#10）。
	h.bark.Check = func() error {
		data, err := os.ReadFile(filepath.Join(h.cfg.DataDir, terminal.FileName))
		if err != nil {
			return fmt.Errorf("通知时 failed 终态尚未落盘: %w", err)
		}
		var st terminal.State
		if err := json.Unmarshal(data, &st); err != nil {
			return err
		}
		if st.LastTaskResult != terminal.ResultFailed {
			return fmt.Errorf("终态 = %+v，期望 failed", st)
		}
		return nil
	}

	_, err := h.runTask(context.Background())
	if err == nil {
		t.Fatal("恢复链耗尽时 Task 应失败")
	}
	if !errors.Is(err, report.ErrRejected) {
		t.Errorf("错误应包装 report.ErrRejected，实际: %v", err)
	}

	// ---- 线上按序：renewal → refresh → enter → timed(拒) → refresh →
	//      retry(拒) → renewal → refresh → retry(拒)：恢复链 5 步、次数有界 ----
	h.assertRequestSequence(
		"renewal", "refresh", "enter",
		"timed", "refresh", "timed", "renewal", "refresh", "timed",
	)
	if n := h.weread.count("/web/reader/"); n != 3 {
		t.Errorf("Reader 页抓取数 = %d，期望 3（初始 + 恢复链 2 次刷新）", n)
	}
	if n := h.weread.count("/web/login/renewal"); n != 2 {
		t.Errorf("renewal 数 = %d，期望 2（任务开始 + 恢复链 1 次）", n)
	}
	if n := h.weread.count("/web/book/read"); n != 4 {
		t.Errorf("report 数 = %d，期望 4（enter + 3 次 timed（含 2 次重试））", n)
	}

	// ---- failed 终态（阻止当天再次自动执行）----
	data, err := os.ReadFile(filepath.Join(h.cfg.DataDir, terminal.FileName))
	if err != nil {
		t.Fatalf("failed 终态不存在: %v", err)
	}
	var st terminal.State
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	if st != (terminal.State{LastTaskDate: "2025-09-06", LastTaskResult: terminal.ResultFailed}) {
		t.Errorf("Terminal State = %+v", st)
	}

	// ---- 失败通知：Bark + 企业微信各 1 条，含失败阶段/主要错误/已尝试恢复动作；
	//      无成功通知、无登录失效通知 ----
	for name, nf := range map[string]*fakeNotify{"bark": h.bark, "wecom": h.wecom} {
		recs := nf.snapshot()
		if len(recs) != 1 {
			t.Fatalf("%s 通知数 = %d，期望 1 条失败通知", name, len(recs))
		}
		msg, err := json.Marshal(recs[0].Body)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"微信读书阅读任务失败",
			"失败阶段：timed report",
			"主要错误",
			"已尝试恢复：刷新 Reader Context → 重试上报 → renewal → 刷新 Reader Context → 重试上报",
			"2025-09-06",
		} {
			if !strings.Contains(string(msg), want) {
				t.Errorf("%s 失败通知缺少 %q；body=%s", name, want, msg)
			}
		}
		if strings.Contains(string(msg), "完成") {
			t.Errorf("%s 不得混入成功文案；body=%s", name, msg)
		}
	}
}

// TestRunRecoveryChainFirstRetrySucceeds 断言恢复链中途成功（refresh 后重试被接受）→
// Task 继续并最终成功；刷新后的 payload 使用新 Reader Context（新 token/psvts）。
func TestRunRecoveryChainFirstRetrySucceeds(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.weread.timedRejects = 1 // 仅第 1 笔 timed 拒绝；重试即成功
		h.weread.rotateReaderState = true
	})
	res, err := h.runTask(context.Background())
	if err != nil {
		t.Fatalf("恢复链中途成功时 Task 应继续: %v", err)
	}
	if res.Reports != 2 || res.Actual != time.Minute {
		t.Errorf("Result = reports:%d actual:%v，期望 2 次/1 分钟", res.Reports, res.Actual)
	}

	// ---- 线上按序：renewal → refresh(1) → enter → timed(拒) → refresh(2) →
	//      timed(接受) → timed(接受)；无 renewal（重试即恢复）----
	h.assertRequestSequence("renewal", "refresh", "enter", "timed", "refresh", "timed", "timed")
	if n := h.weread.count("/web/login/renewal"); n != 1 {
		t.Errorf("renewal 数 = %d，期望 1（仅任务开始）", n)
	}

	// ---- 刷新后的 payload 使用新 Reader Context：token=2 / psvts=2 ----
	reports := h.reportRecords()
	for i, rec := range reports {
		p := payloadFromWire(t, rec.Body)
		if _, isTimed := p["rt"]; !isTimed {
			continue
		}
		switch {
		case i == 1: // 被拒的首笔（refresh 之前）
			verifySG(t, p, "fake-reader-token-1")
			if p["ps"] != "fake-psvts-1" {
				t.Errorf("刷新前 payload[%d].ps = %s", i, p["ps"])
			}
		case i >= 2: // refresh 之后的接受/继续上报
			verifySG(t, p, "fake-reader-token-2")
			if p["ps"] != "fake-psvts-2" {
				t.Errorf("刷新后 payload[%d].ps = %s，期望新 Context", i, p["ps"])
			}
		}
	}

	// ---- success 终态 + 成功通知；无失败通知 ----
	data, err := os.ReadFile(filepath.Join(h.cfg.DataDir, terminal.FileName))
	if err != nil {
		t.Fatalf("success 终态不存在: %v", err)
	}
	var st terminal.State
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	if st.LastTaskResult != terminal.ResultSuccess {
		t.Errorf("Terminal State = %+v", st)
	}
	if got := len(h.bark.snapshot()); got != 1 {
		t.Errorf("应只有 1 条成功通知，实际 %d 条: %+v", got, h.bark.snapshot())
	}
	if h.bark.snapshot()[0].Body["title"] != "微信读书阅读任务完成" {
		t.Errorf("通知 title = %v", h.bark.snapshot()[0].Body["title"])
	}
}

// TestRunRecoveryChainViaRenewalSucceeds 断言恢复链走到 renewal 环节成功
// （renewal → refresh → retry 被接受）→ Task 继续并最终成功；次数有界。
func TestRunRecoveryChainViaRenewalSucceeds(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.weread.timedRejects = 2 // timed#1 与 retry#1 拒绝；renewal 后 retry#2 接受
	})
	res, err := h.runTask(context.Background())
	if err != nil {
		t.Fatalf("恢复链经 renewal 成功时 Task 应继续: %v", err)
	}
	if res.Reports != 2 || res.Actual != time.Minute {
		t.Errorf("Result = reports:%d actual:%v", res.Reports, res.Actual)
	}

	// ---- 线上按序：renewal → refresh → enter → timed(拒) → refresh →
	//      retry(拒) → renewal → refresh → retry(接受) → timed(接受) ----
	h.assertRequestSequence(
		"renewal", "refresh", "enter",
		"timed", "refresh", "timed", "renewal", "refresh", "timed", "timed",
	)
	if n := h.weread.count("/web/reader/"); n != 3 {
		t.Errorf("Reader 页抓取数 = %d，期望 3（初始 + 恢复链 2 次刷新）", n)
	}
	if n := h.weread.count("/web/login/renewal"); n != 2 {
		t.Errorf("renewal 数 = %d，期望 2（任务开始 + 恢复链 1 次）", n)
	}

	// success 终态 + 成功通知；无失败通知。
	data, err := os.ReadFile(filepath.Join(h.cfg.DataDir, terminal.FileName))
	if err != nil {
		t.Fatal(err)
	}
	var st terminal.State
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	if st.LastTaskResult != terminal.ResultSuccess {
		t.Errorf("Terminal State = %+v", st)
	}
	if got := len(h.bark.snapshot()); got != 1 {
		t.Errorf("应只有 1 条成功通知，实际 %d 条", got)
	}
}

// TestRunRecoveryChainNotifyChannelIndependence 断言失败通知的渠道独立性：Bark 端点
// 故障（500）不影响企业微信渠道收到失败通知，也不影响 Task 结果（failed 终态落盘）。
func TestRunRecoveryChainNotifyChannelIndependence(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.weread.timedRejectAll = true
		h.bark.status = 500
	})
	_, err := h.runTask(context.Background())
	if err == nil {
		t.Fatal("恢复链耗尽时 Task 应失败")
	}

	// 企业微信收到完整失败通知；Bark（故障渠道）的失败不阻断。
	wecom := h.wecom.snapshot()
	if len(wecom) != 1 {
		t.Fatalf("企业微信应收到 1 条失败通知，实际 %d", len(wecom))
	}
	msg, _ := json.Marshal(wecom[0].Body)
	for _, want := range []string{"失败阶段：timed report", "已尝试恢复"} {
		if !strings.Contains(string(msg), want) {
			t.Errorf("企业微信失败通知缺少 %q；body=%s", want, msg)
		}
	}
	// Task 结果不受通知渠道故障影响：failed 终态已落盘。
	data, err := os.ReadFile(filepath.Join(h.cfg.DataDir, terminal.FileName))
	if err != nil {
		t.Fatalf("failed 终态不存在: %v", err)
	}
	var st terminal.State
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	if st.LastTaskResult != terminal.ResultFailed {
		t.Errorf("Terminal State = %+v", st)
	}
}

// TestRunRecoveryChainLoginInvalidGoesThroughT4 断言恢复链中 renewal 出现登录失效
// 明确证据时走 ticket 04 判别与路径：从初始 Cookie 重建一次 → 仍失败 → failed 终态
// + 登录失效通知（固定提示）；不混入普通失败文案（无失败通知）。
func TestRunRecoveryChainLoginInvalidGoesThroughT4(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.weread.timedRejectAll = true
		h.weread.renewRejectFrom = 2 // 任务开始 renewal 成功；恢复链中的 renewal 起出现 succ:0 证据
		seedSession(t, h.cfg.DataDir, &http.Cookie{Name: "wr_gid", Value: "OLDSTALE"})
	})
	_, err := h.runTask(context.Background())
	if err == nil {
		t.Fatal("登录失效重建失败时 Task 应失败")
	}
	if !errors.Is(err, weread.ErrLoginInvalid) {
		t.Errorf("错误应包装 weread.ErrLoginInvalid，实际: %v", err)
	}

	// ---- 线上按序：renewal(任务开始) → refresh → enter → timed(拒) → refresh →
	//      retry(拒) → renewal(succ:0 证据) → renewal(重建后重试) ----
	//      有界：1 次证据 + 1 次重建重试，不无限重试。
	h.assertRequestSequence(
		"renewal", "refresh", "enter",
		"timed", "refresh", "timed", "renewal", "renewal",
	)
	if n := h.weread.count("/web/login/renewal"); n != 3 {
		t.Errorf("renewal 数 = %d，期望 3（任务开始 + 证据 + 重建后重试）", n)
	}
	// 证据 renewal 携带当前会话（任务开始 renewal 已并入新 Cookie）；
	// 重建后的重试携带初始 Cookie 重建的会话（ticket 04 语义在恢复链内同样成立）。
	var renewals []wereadRequest
	for _, r := range h.weread.snapshot() {
		if r.Path == "/web/login/renewal" {
			renewals = append(renewals, r)
		}
	}
	if len(renewals) < 3 {
		t.Fatalf("renewal 记录数 = %d，期望 ≥3", len(renewals))
	}
	if c := renewals[1].Header.Get("Cookie"); !strings.Contains(c, "new123") {
		t.Errorf("证据 renewal Cookie = %q，期望当读会话（含任务开始 renewal 并入的新 Cookie）", c)
	}
	if c := renewals[2].Header.Get("Cookie"); !strings.Contains(c, "wr_gid=123") || !strings.Contains(c, "wr_skey=abc") || strings.Contains(c, "new123") {
		t.Errorf("重建后 renewal Cookie = %q，期望初始 Cookie 重建的会话", c)
	}

	// ---- failed 终态 + 登录失效通知（固定提示）；无普通失败通知、无成功通知 ----
	data, err := os.ReadFile(filepath.Join(h.cfg.DataDir, terminal.FileName))
	if err != nil {
		t.Fatalf("failed 终态不存在: %v", err)
	}
	var st terminal.State
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	if st.LastTaskResult != terminal.ResultFailed {
		t.Errorf("Terminal State = %+v", st)
	}
	for name, nf := range map[string]*fakeNotify{"bark": h.bark, "wecom": h.wecom} {
		recs := nf.snapshot()
		if len(recs) != 1 {
			t.Fatalf("%s 通知数 = %d，期望 1 条登录失效通知", name, len(recs))
		}
		msg, _ := json.Marshal(recs[0].Body)
		for _, want := range []string{"微信读书登录已失效", "请更新初始 Cookie（WEREAD_CRON_COOKIE）"} {
			if !strings.Contains(string(msg), want) {
				t.Errorf("%s 登录失效通知缺少 %q；body=%s", name, want, msg)
			}
		}
		// 不混入普通失败文案（spec 决策 #10；issue 验收 ✓）。
		for _, forbid := range []string{"失败阶段", "已尝试恢复", "阅读任务失败"} {
			if strings.Contains(string(msg), forbid) {
				t.Errorf("%s 登录失效通知混入普通失败文案 %q；body=%s", name, forbid, msg)
			}
		}
	}
}

// TestRunRecoveryChainLoginInvalidRebuildSucceedsContinues 断言恢复链中登录失效证据
// 出现后从初始 Cookie 重建一次成功 → 恢复链继续（refresh → retry）→ Task 最终成功。
func TestRunRecoveryChainLoginInvalidRebuildSucceedsContinues(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.weread.timedRejects = 2      // timed#1 与 retry#1 拒绝
		h.weread.renewRejectOnceAt = 2 // 仅恢复链中的那次 renewal 出现一次 succ:0 证据
		seedSession(t, h.cfg.DataDir, &http.Cookie{Name: "wr_gid", Value: "OLDSTALE"})
	})
	res, err := h.runTask(context.Background())
	if err != nil {
		t.Fatalf("重建成功后 Task 应继续完成: %v", err)
	}
	if res.Reports != 2 {
		t.Errorf("Reports = %d，期望 2（任务完整执行）", res.Reports)
	}

	// ---- 线上按序：renewal(任务开始) → refresh → enter → timed(拒) → refresh →
	//      retry(拒) → renewal(证据) → renewal(重建后重试) → refresh → retry(接受)
	//      → timed(接受) ----
	h.assertRequestSequence(
		"renewal", "refresh", "enter",
		"timed", "refresh", "timed", "renewal", "renewal", "refresh", "timed", "timed",
	)

	// success 终态 + 成功通知；无登录失效通知、无失败通知。
	data, err := os.ReadFile(filepath.Join(h.cfg.DataDir, terminal.FileName))
	if err != nil {
		t.Fatal(err)
	}
	var st terminal.State
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	if st.LastTaskResult != terminal.ResultSuccess {
		t.Errorf("Terminal State = %+v", st)
	}
	if got := len(h.bark.snapshot()); got != 1 || h.bark.snapshot()[0].Body["title"] != "微信读书阅读任务完成" {
		t.Errorf("应只有 1 条成功通知，实际 %d 条: %+v", got, h.bark.snapshot())
	}
}

// TestRunEnterRejectedRecoveryChainSucceeds 断言 enter report 被拒同样走恢复链
// （refresh → retry 接受）→ Task 继续并最终成功（用户故事 #26/#31）。
func TestRunEnterRejectedRecoveryChainSucceeds(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.weread.enterRejects = 1 // 仅首笔 enter 拒绝；refresh 后重试接受
	})
	if _, err := h.runTask(context.Background()); err != nil {
		t.Fatalf("enter 恢复链成功后 Task 应继续: %v", err)
	}

	// ---- 线上按序：renewal → refresh → enter(拒) → refresh → enter(接受) →
	//      timed → timed；无 renewal（重试即恢复）。----
	h.assertRequestSequence("renewal", "refresh", "enter", "refresh", "enter", "timed", "timed")
	if n := h.weread.count("/web/reader/"); n != 2 {
		t.Errorf("Reader 页抓取数 = %d，期望 2（初始 + 恢复链 1 次刷新）", n)
	}
	if n := h.weread.count("/web/login/renewal"); n != 1 {
		t.Errorf("renewal 数 = %d，期望 1（仅任务开始）", n)
	}

	data, err := os.ReadFile(filepath.Join(h.cfg.DataDir, terminal.FileName))
	if err != nil {
		t.Fatal(err)
	}
	var st terminal.State
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	if st.LastTaskResult != terminal.ResultSuccess {
		t.Errorf("Terminal State = %+v", st)
	}
}

// TestRunEnterRejectedRecoveryChainExhausted 断言 enter report 被拒且恢复链耗尽 →
// failed 终态 + 失败通知（失败阶段 = enter report）：恢复链对 enter 同样适用、有界。
func TestRunEnterRejectedRecoveryChainExhausted(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.weread.reportRejected = true // 所有 /web/book/read 一律拒绝（含 enter）
	})
	_, err := h.runTask(context.Background())
	if err == nil {
		t.Fatal("恢复链耗尽时 Task 应失败")
	}
	if !errors.Is(err, report.ErrRejected) {
		t.Errorf("错误应包装 report.ErrRejected，实际: %v", err)
	}

	// ---- 线上按序：renewal → refresh → enter(拒) → refresh → retry(拒) →
	//      renewal → refresh → retry(拒)：恢复链 5 步、次数有界 ----
	h.assertRequestSequence(
		"renewal", "refresh", "enter",
		"refresh", "enter", "renewal", "refresh", "enter",
	)
	if n := h.weread.count("/web/reader/"); n != 3 {
		t.Errorf("Reader 页抓取数 = %d，期望 3（初始 + 恢复链 2 次刷新）", n)
	}
	if n := h.weread.count("/web/login/renewal"); n != 2 {
		t.Errorf("renewal 数 = %d，期望 2（任务开始 + 恢复链 1 次）", n)
	}

	// failed 终态 + 失败通知（失败阶段 = enter report）。
	data, err := os.ReadFile(filepath.Join(h.cfg.DataDir, terminal.FileName))
	if err != nil {
		t.Fatalf("failed 终态不存在: %v", err)
	}
	var st terminal.State
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	if st.LastTaskResult != terminal.ResultFailed {
		t.Errorf("Terminal State = %+v", st)
	}
	for name, nf := range map[string]*fakeNotify{"bark": h.bark, "wecom": h.wecom} {
		recs := nf.snapshot()
		if len(recs) != 1 {
			t.Fatalf("%s 通知数 = %d，期望 1 条失败通知", name, len(recs))
		}
		msg, _ := json.Marshal(recs[0].Body)
		if !strings.Contains(string(msg), "失败阶段：enter report") {
			t.Errorf("%s 失败通知缺少失败阶段；body=%s", name, msg)
		}
	}
}

// seedSession 预置持久化 Login Session（模拟上次运行留下、已失效的会话）。
func seedSession(t *testing.T, dir string, cookies ...*http.Cookie) {
	t.Helper()
	j := session.NewJar()
	j.MergeSetCookies(cookies)
	if err := session.NewFileStore(dir).SaveJar(j); err != nil {
		t.Fatalf("预置 Login Session 失败: %v", err)
	}
}

// TestRunLoginInvalidRebuildsFromInitialCookieAndContinues 断言：凭明确证据（renewal
// succ!=1）判定登录失效后，从初始 Cookie 重建 Login Session 一次并重试；重建成功
// 则当天 Task 继续（用户故事 #33）。
func TestRunLoginInvalidRebuildsFromInitialCookieAndContinues(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		// 预置失效的持久化会话（恢复后仍是旧 Cookie），首笔 renewal 拒绝。
		seedSession(t, h.cfg.DataDir, &http.Cookie{Name: "wr_gid", Value: "OLDSTALE"})
		h.weread.renewReject = 1
	})
	res, err := h.runTask(context.Background())
	if err != nil {
		t.Fatalf("重建成功时 Task 应继续完成: %v", err)
	}
	if res.Reports != 2 {
		t.Errorf("Reports = %d，期望 2（任务完整执行）", res.Reports)
	}

	// 两笔 renewal：首笔（恢复的旧会话）被拒；重建后重试携带初始 Cookie。
	var renewals []wereadRequest
	for _, r := range h.weread.snapshot() {
		if r.Path == "/web/login/renewal" {
			renewals = append(renewals, r)
		}
	}
	if len(renewals) != 2 {
		t.Fatalf("renewal 数 = %d，期望 2（1 次证据 + 重建后 1 次重试）", len(renewals))
	}
	if c := renewals[0].Header.Get("Cookie"); !strings.Contains(c, "wr_gid=OLDSTALE") {
		t.Errorf("首笔 renewal Cookie = %q，期望恢复的旧会话", c)
	}
	if c := renewals[1].Header.Get("Cookie"); !strings.Contains(c, "wr_gid=123") || !strings.Contains(c, "wr_skey=abc") || strings.Contains(c, "OLDSTALE") {
		t.Errorf("重建后 renewal Cookie = %q，期望初始 Cookie 重建的会话", c)
	}

	// 当天 Task 继续：success 终态 + 成功通知，无登录失效通知。
	data, err := os.ReadFile(filepath.Join(h.cfg.DataDir, terminal.FileName))
	if err != nil {
		t.Fatalf("Terminal State 不存在: %v", err)
	}
	var st terminal.State
	json.Unmarshal(data, &st)
	if st != (terminal.State{LastTaskDate: "2025-09-06", LastTaskResult: terminal.ResultSuccess}) {
		t.Errorf("Terminal State = %+v", st)
	}
	if got := len(h.bark.snapshot()); got != 1 || h.bark.snapshot()[0].Body["title"] != "微信读书阅读任务完成" {
		t.Errorf("应只有 1 条成功通知，实际 %d 条: %+v", got, h.bark.snapshot())
	}
	// 重建后的 renewal 新 Cookie 仍并入会话并落盘。
	sessData, err := os.ReadFile(filepath.Join(h.cfg.DataDir, "login_session.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sessData), `"value": "new123"`) || !strings.Contains(string(sessData), `"value": "newabc"`) {
		t.Errorf("重建后 Login Session 未并入 renewal 新 Cookie:\n%s", sessData)
	}
}

// TestRunLoginInvalidRebuildFailsWritesFailedTerminalAndNotifies 断言：重建仍失败 →
// failed 终态（先落盘再通知）+ 登录失效通知（固定文案提示更新初始 Cookie）。
func TestRunLoginInvalidRebuildFailsWritesFailedTerminalAndNotifies(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.weread.renewReject = 5
	})
	// 通知端点收到登录失效通知时，failed 终态必须已落盘（spec 决策 #9/#10）。
	h.bark.Check = func() error {
		data, err := os.ReadFile(filepath.Join(h.cfg.DataDir, terminal.FileName))
		if err != nil {
			return fmt.Errorf("通知时 failed 终态尚未落盘: %w", err)
		}
		var st terminal.State
		if err := json.Unmarshal(data, &st); err != nil {
			return err
		}
		if st.LastTaskResult != terminal.ResultFailed {
			return fmt.Errorf("终态 = %+v，期望 failed", st)
		}
		return nil
	}

	_, err := h.runTask(context.Background())
	if err == nil {
		t.Fatal("重建仍失败时 Task 应失败")
	}
	if !errors.Is(err, weread.ErrLoginInvalid) {
		t.Errorf("错误应包装 weread.ErrLoginInvalid，实际: %v", err)
	}

	// 有界：重建一次 + 重试一次，不无限重试。
	if n := h.weread.count("/web/login/renewal"); n != 2 {
		t.Errorf("renewal 数 = %d，期望 2（证据 + 重建后重试）", n)
	}

	// failed 终态（阻止当天再次自动执行，用户故事 #41 可手动 `run` 重试）。
	data, err := os.ReadFile(filepath.Join(h.cfg.DataDir, terminal.FileName))
	if err != nil {
		t.Fatalf("failed 终态不存在: %v", err)
	}
	var st terminal.State
	json.Unmarshal(data, &st)
	if st != (terminal.State{LastTaskDate: "2025-09-06", LastTaskResult: terminal.ResultFailed}) {
		t.Errorf("Terminal State = %+v", st)
	}

	// 登录失效通知（Bark + 企业微信各 1 条，固定文案提示更新初始 Cookie），无成功通知。
	for name, nf := range map[string]*fakeNotify{"bark": h.bark, "wecom": h.wecom} {
		recs := nf.snapshot()
		if len(recs) != 1 {
			t.Fatalf("%s 通知数 = %d，期望 1 条登录失效通知", name, len(recs))
		}
		if recs[0].Body["title"] != nil && recs[0].Body["title"] != "微信读书登录已失效" {
			t.Errorf("%s title = %v", name, recs[0].Body["title"])
		}
		msg, _ := json.Marshal(recs[0].Body)
		for _, want := range []string{
			"微信读书登录已失效",
			"请更新初始 Cookie（WEREAD_CRON_COOKIE）",
			"2025-09-06",
		} {
			if !strings.Contains(string(msg), want) {
				t.Errorf("%s 登录失效通知缺少 %q；body=%s", name, want, msg)
			}
		}
	}
	if strings.Contains(string(h.bark.snapshot()[0].Body["title"].(string)), "完成") {
		t.Errorf("不得发送成功通知")
	}
}

// TestRunTransientRenewalFailureDoesNotTriggerRebuild 断言无明确证据的失败
// （HTTP 非 200：传输/服务端故障）不触发重建、不写 failed 终态、不发登录失效通知。
func TestRunTransientRenewalFailureDoesNotTriggerRebuild(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.weread.renewStatus = 500
		seedSession(t, h.cfg.DataDir, &http.Cookie{Name: "wr_gid", Value: "OLDSTALE"})
	})
	_, err := h.runTask(context.Background())
	if err == nil {
		t.Fatal("renewal HTTP 500 时 Task 应失败")
	}
	if errors.Is(err, weread.ErrLoginInvalid) {
		t.Errorf("非明确证据不得归类为登录失效: %v", err)
	}
	if n := h.weread.count("/web/login/renewal"); n != 1 {
		t.Errorf("renewal 数 = %d，期望 1（无证据不重建、不重试）", n)
	}
	if _, statErr := os.Stat(filepath.Join(h.cfg.DataDir, terminal.FileName)); !os.IsNotExist(statErr) {
		t.Errorf("暂时性失败不得写终态（当日可再次调度）")
	}
	if got := len(h.bark.snapshot()) + len(h.wecom.snapshot()); got != 0 {
		t.Errorf("不得发送任何通知，实际 %d 条", got)
	}
}

// TestRunLoginInvalidRebuildWithoutInitialCookieFails 断言：无初始 Cookie 可重建时
// （部署只配置了持久化会话且已失效），同样走 failed 终态 + 登录失效通知。
func TestRunLoginInvalidRebuildWithoutInitialCookieFails(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.cfg.Cookie = ""
		h.weread.renewReject = 5
		seedSession(t, h.cfg.DataDir, &http.Cookie{Name: "wr_gid", Value: "OLDSTALE"})
	})
	_, err := h.runTask(context.Background())
	if err == nil {
		t.Fatal("重建失败时 Task 应失败")
	}
	if !errors.Is(err, weread.ErrLoginInvalid) {
		t.Errorf("错误应包装 weread.ErrLoginInvalid，实际: %v", err)
	}
	if n := h.weread.count("/web/login/renewal"); n != 1 {
		t.Errorf("renewal 数 = %d，期望 1（无初始 Cookie 直接失败，不重试）", n)
	}
	data, err := os.ReadFile(filepath.Join(h.cfg.DataDir, terminal.FileName))
	if err != nil {
		t.Fatalf("failed 终态不存在: %v", err)
	}
	var st terminal.State
	json.Unmarshal(data, &st)
	if st.LastTaskResult != terminal.ResultFailed {
		t.Errorf("Terminal State = %+v", st)
	}
	if got := len(h.bark.snapshot()); got != 1 {
		t.Errorf("应发送 1 条登录失效通知，实际 %d", got)
	}
}

// TestRunRenewalNoSuccIsNotLoginInvalid 断言 200 响应不含 succ 字段（如 errCode
// 错误体）不是"明确拒绝"的证据形态：不重建、不写终态、不发登录失效通知。
func TestRunRenewalNoSuccIsNotLoginInvalid(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.weread.renewNoSucc = true
		seedSession(t, h.cfg.DataDir, &http.Cookie{Name: "wr_gid", Value: "OLDSTALE"})
	})
	_, err := h.runTask(context.Background())
	if err == nil {
		t.Fatal("renewal 响应不含 succ 时 Task 应失败")
	}
	if errors.Is(err, weread.ErrLoginInvalid) {
		t.Errorf("不含 succ 的响应不得归类为登录失效证据: %v", err)
	}
	if n := h.weread.count("/web/login/renewal"); n != 1 {
		t.Errorf("renewal 数 = %d，期望 1（无证据不重建、不重试）", n)
	}
	if _, statErr := os.Stat(filepath.Join(h.cfg.DataDir, terminal.FileName)); !os.IsNotExist(statErr) {
		t.Errorf("暂时性失败不得写终态（当日可再次调度）")
	}
	if got := len(h.bark.snapshot()) + len(h.wecom.snapshot()); got != 0 {
		t.Errorf("不得发送任何通知，实际 %d 条", got)
	}
}

// TestRunRestartRestoresRenewedSession 断言跨进程重启（新 App + 同一 /data）：
// 恢复的是上次运行 renewal 后落盘的最新会话，而非初始 Cookie（用户故事 #7/#9）。
// 重启场景取次日（ticket 08：当天 success 终态下 run 被拒绝执行——同一天重启后
// 再 run 正是终态规则的拒绝场景，会话恢复语义由次日场景覆盖）。
func TestRunRestartRestoresRenewedSession(t *testing.T) {
	h := setup(t, nil)
	if _, err := h.runTask(context.Background()); err != nil {
		t.Fatalf("首次运行失败: %v", err)
	}
	firstRequests := len(h.weread.snapshot())

	// 重启：同一 /data、初始 Cookie 已更换（模拟运维更新 env）；时钟推进到次日
	//（昨日 success 终态不约束今天——ticket 08 的"只约束当天"语义同时被断言）；
	// 复用同一 fake 服务端以便观察重启后首笔请求携带的 Cookie。
	h2 := setup(t, func(h2 *testHarness) {
		h2.cfg.DataDir = h.cfg.DataDir
		h2.cfg.Cookie = "wr_gid=replaceme"
		h2.weread = h.weread
		h2.clk = clock.NewFake(time.Date(2025, 9, 7, 10, 0, 0, 0, testTZ))
	})
	if _, err := h2.runTask(context.Background()); err != nil {
		t.Fatalf("重启后运行失败: %v", err)
	}

	// 重启后首笔 renewal 携带的是落盘的会话（new123），而非新初始 Cookie。
	reqs := h.weread.snapshot()
	if firstRequests >= len(reqs) {
		t.Fatalf("重启后无新请求")
	}
	if c := reqs[firstRequests].Header.Get("Cookie"); !strings.Contains(c, "wr_gid=new123") || strings.Contains(c, "replaceme") {
		t.Errorf("重启后 renewal Cookie = %q，期望恢复 new123 而非初始 Cookie", c)
	}
	// 落盘会话未被替换：login_session.json 仍含 renewal 后的新 Cookie。
	sessData, err := os.ReadFile(filepath.Join(h.cfg.DataDir, "login_session.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sessData), "replaceme") || !strings.Contains(string(sessData), `"value": "new123"`) {
		t.Errorf("Login Session 文件应含 new123 且不含 replaceme:\n%s", sessData)
	}
}

// --- ticket 08：run 终态规则（success 拒绝 / failed 重试）与并发守卫 ---

// seedTerminal 直接预置持久化 Terminal State（issue 08：failed 终态是 run 的合法
// 输入状态，测试可直接 seed 持久化文件，无需经由 ticket 05 的真实失败路径）。
func seedTerminal(t *testing.T, dir string, st terminal.State) {
	t.Helper()
	if err := terminal.NewFileStore(dir).Save(st); err != nil {
		t.Fatalf("预置 Terminal State 失败: %v", err)
	}
}

// TestRunNoGateWhenNoTerminal 断言无终态为 run 的默认输入：完整执行并写 success
// 终态（用户故事 #42 的基线——由 ticket 03 happy path 与本文档共同覆盖）。
func TestRunNoGateWhenNoTerminal(t *testing.T) {
	h := setup(t, nil)
	if _, err := h.runTask(context.Background()); err != nil {
		t.Fatalf("无终态时 run 应完整执行: %v", err)
	}
	st, has, err := terminal.NewFileStore(h.cfg.DataDir).Load()
	if err != nil || !has {
		t.Fatalf("执行后应有终态: has=%v err=%v", has, err)
	}
	if st != (terminal.State{LastTaskDate: "2025-09-06", LastTaskResult: terminal.ResultSuccess}) {
		t.Errorf("Terminal State = %+v，期望 success", st)
	}
}

// TestRunRejectedWhenTodaySuccessTerminal 断言当天已有 success 终态 → run 拒绝：
// 返回 ErrTerminalSuccess、不发起任何网络请求（快速失败）、不发送通知、终态不被改写。
func TestRunRejectedWhenTodaySuccessTerminal(t *testing.T) {
	h := setup(t, nil)
	seedTerminal(t, h.cfg.DataDir, terminal.State{
		LastTaskDate:   "2025-09-06",
		LastTaskResult: terminal.ResultSuccess,
	})

	_, err := h.runTask(context.Background())
	if err == nil {
		t.Fatal("当天 success 终态下运行应被拒绝")
	}
	if !errors.Is(err, ErrTerminalSuccess) {
		t.Errorf("错误应包装 ErrTerminalSuccess，实际: %v", err)
	}
	if n := len(h.weread.snapshot()); n != 0 {
		t.Errorf("拒绝时不应发起任何网络请求，实际 %d 条: %+v", n, h.weread.snapshot())
	}
	if got := len(h.bark.snapshot()) + len(h.wecom.snapshot()); got != 0 {
		t.Errorf("拒绝时不应发送任何通知，实际 %d 条", got)
	}
	// 终态不被改写（仍是原 seed）。
	st, has, err := terminal.NewFileStore(h.cfg.DataDir).Load()
	if err != nil || !has {
		t.Fatalf("终态应存在: has=%v err=%v", has, err)
	}
	if st.LastTaskResult != terminal.ResultSuccess {
		t.Errorf("拒绝时终态被改写: %+v", st)
	}
}

// TestRunRetriesAfterTodayFailedTerminal 断言当天 failed 终态（seed）是 run 的合法
// 输入：重试成功 → 当天结果更新为 success（用户故事 #41；spec 决策 #12）。
func TestRunRetriesAfterTodayFailedTerminal(t *testing.T) {
	h := setup(t, nil)
	seedTerminal(t, h.cfg.DataDir, terminal.State{
		LastTaskDate:   "2025-09-06",
		LastTaskResult: terminal.ResultFailed,
	})
	res, err := h.runTask(context.Background())
	if err != nil {
		t.Fatalf("failed 终态下 run 应重试成功: %v", err)
	}
	if res.Reports != 2 {
		t.Errorf("Reports = %d，期望 2（任务完整执行）", res.Reports)
	}
	st, has, err := terminal.NewFileStore(h.cfg.DataDir).Load()
	if err != nil || !has {
		t.Fatalf("重试后应有终态: has=%v err=%v", has, err)
	}
	if st != (terminal.State{LastTaskDate: "2025-09-06", LastTaskResult: terminal.ResultSuccess}) {
		t.Errorf("Terminal State = %+v，期望更新为 success", st)
	}
	// 成功通知（无失败通知残留语义：通知渠道每次执行独立计数，仅成功 1 条）。
	if got := len(h.bark.snapshot()); got != 1 || h.bark.snapshot()[0].Body["title"] != "微信读书阅读任务完成" {
		t.Errorf("重试成功应有 1 条成功通知，实际 %d 条: %+v", got, h.bark.snapshot())
	}
}

// TestRunIgnoresStaleTerminalFromPreviousDay 断言昨日（及更早）终态不约束今天：
// success 终态只拒绝当天（日期匹配的唯一定义，同 scheduler 的 todayTerminal 谓词）。
func TestRunIgnoresStaleTerminalFromPreviousDay(t *testing.T) {
	h := setup(t, nil)
	seedTerminal(t, h.cfg.DataDir, terminal.State{
		LastTaskDate:   "2025-09-05",
		LastTaskResult: terminal.ResultSuccess,
	})
	if _, err := h.runTask(context.Background()); err != nil {
		t.Fatalf("昨日终态不应拒绝今天: %v", err)
	}
	if n := h.weread.count("/web/book/read"); n != 3 {
		t.Errorf("report 数 = %d，期望 3（任务完整执行）", n)
	}
}

// TestRunGateFailsOnCorruptTerminal 断言终态文件不可读时 run 保守失败（不执行）：
// 无法判定"今天是否已完成"时不得冒险重复执行（写终态前的失败也不发送通知）。
func TestRunGateFailsOnCorruptTerminal(t *testing.T) {
	h := setup(t, nil)
	if err := os.WriteFile(filepath.Join(h.cfg.DataDir, terminal.FileName), []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := h.runTask(context.Background())
	if err == nil {
		t.Fatal("终态文件损坏时 run 应保守失败")
	}
	if n := len(h.weread.snapshot()); n != 0 {
		t.Errorf("损坏终态下不应发起任何网络请求，实际 %d 条", n)
	}
	if got := len(h.bark.snapshot()) + len(h.wecom.snapshot()); got != 0 {
		t.Errorf("损坏终态下不应发送任何通知，实际 %d 条", got)
	}
}

// TestRunConcurrentSecondTaskRejected 断言并发守卫（spec 决策 #11）：第一个 Task
// 运行中，第二个 Task（run 或 daemon 触发的 RunTask——调用方身份无关）被拒绝，
// 返回 ErrTaskRunning 且第一个 Task 不被中断；第一个 Task 完成后形成 success 终态，
// 第三个调用落入终态规则（ErrTerminalSuccess）。
func TestRunConcurrentSecondTaskRejected(t *testing.T) {
	h := setup(t, nil)
	h.weread.blockTimed = make(chan struct{})
	h.weread.blockTimedTriggered = make(chan struct{}, 1)

	ctx := context.Background()
	done := make(chan struct{})
	var firstErr error
	go func() {
		_, firstErr = h.app.RunTask(ctx)
		close(done)
	}()

	// 等第一个 Task 阻塞在首笔 timed report（锁必然已持有）。
	select {
	case <-h.weread.blockTimedTriggered:
	case <-time.After(10 * time.Second):
		t.Fatal("timed report 未在 10s 内到达")
	}

	// 第二个 RunTask：锁被持有 → 非阻塞拒绝（不等待第一个完成）。
	_, err := h.app.RunTask(ctx)
	if err == nil {
		t.Fatal("运行中应拒绝第二个 Task")
	}
	if !errors.Is(err, ErrTaskRunning) {
		t.Errorf("错误应包装 ErrTaskRunning，实际: %v", err)
	}
	// 拒绝不是失败：不写终态、不通知、不打断第一个（请求数不因第二次调用增加）。
	if n := len(h.weread.snapshot()); n < 3 {
		t.Errorf("首个 Task 请求数 = %d，期望 ≥3（renewal + reader + enter）", n)
	}
	if got := len(h.bark.snapshot()) + len(h.wecom.snapshot()); got != 0 {
		t.Errorf("拒绝时不应发送任何通知，实际 %d 条", got)
	}

	// 释放第一个 Task；它应正常完成（未被中断）并形成 success 终态。
	close(h.weread.blockTimed)
	select {
	case <-done:
		if firstErr != nil {
			t.Fatalf("第一个 Task 不应被并发拒绝影响: %v", firstErr)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("第一个 Task 未在 30s 内完成")
	}
	st, has, err := terminal.NewFileStore(h.cfg.DataDir).Load()
	if err != nil || !has {
		t.Fatalf("首个 Task 完成应有终态: has=%v err=%v", has, err)
	}
	if st.LastTaskResult != terminal.ResultSuccess {
		t.Errorf("Terminal State = %+v，期望 success", st)
	}

	// 第三个调用：锁已释放但终态已形成 → 终态规则拒绝。
	_, err = h.app.RunTask(ctx)
	if !errors.Is(err, ErrTerminalSuccess) {
		t.Errorf("首个 Task 完成后的调用应落入终态规则（ErrTerminalSuccess），实际: %v", err)
	}
}

// TestRunIgnoresRunWindow 断言 run 不受 Run Window 限制（spec 决策 #12；用户故事
// #42）：配置窗口已过（01:00–03:00，假时钟 10:00），无终态 → run 立即完整执行。
func TestRunIgnoresRunWindow(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.cfg.WindowStart = 60 // 01:00
		h.cfg.WindowEnd = 180  // 03:00
	})
	if _, err := h.runTask(context.Background()); err != nil {
		t.Fatalf("窗口已过时 run 仍应执行（Run Window 只约束自动调度）: %v", err)
	}
	if h.weread.count("/web/book/read") != 3 {
		t.Errorf("report 数 = %d，期望 3（任务完整执行）", h.weread.count("/web/book/read"))
	}
}
