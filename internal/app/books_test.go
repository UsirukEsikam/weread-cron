// `weread-cron books` 的应用边界 seam 测试（ticket 09；ADR-0006）：真实 App +
// fake weread 服务端，断言线上行为——renewal 后抓取 Shelf、输出 bookId/title、
// 不产生 Task（无 /web/book/read 与 Reader 页请求）、不读写终态、不通知、
// 会话失效的报错与提示（凭明确证据重建一次 → 仍失败 → 更新初始 Cookie 提示）。
package app

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"weread-cron/internal/terminal"
	"weread-cron/internal/weread"
)

// assertNoBooksSideEffects 断言 books 查询无 Task/终态/通知副作用（issue 验收：
// 不产生 Task、不读取或修改终态、不发送通知）。
func (h *testHarness) assertNoBooksSideEffects(t *testing.T) {
	t.Helper()
	if n := h.weread.count("/web/book/read"); n != 0 {
		t.Errorf("books 不得产生 /web/book/read 请求，实际 %d 条", n)
	}
	if n := h.weread.count("/web/reader/"); n != 0 {
		t.Errorf("books 不得抓取 Reader 页（不建立 Reader Context），实际 %d 条", n)
	}
	if _, err := os.Stat(filepath.Join(h.cfg.DataDir, terminal.FileName)); !os.IsNotExist(err) {
		t.Errorf("books 不得写 Terminal State（存在: %v）", err)
	}
	if got := len(h.bark.snapshot()) + len(h.wecom.snapshot()); got != 0 {
		t.Errorf("books 不得发送任何通知，实际 %d 条", got)
	}
}

// TestListBooksHappyPath 是 books 主 happy path：renewal → /web/shelf/sync →
// 返回 bookId/title（按书架顺序）；renewal 新 Cookie 并入会话并落盘；无其它副作用。
func TestListBooksHappyPath(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.weread.shelfBooks = []fakeShelfBook{
			{BookID: "601111", Title: "未读完的书", FinishReading: false},
			{BookID: "602222", Title: "已读完的书", FinishReading: true},
		}
	})
	books, err := h.app.ListBooks(context.Background())
	if err != nil {
		t.Fatalf("ListBooks 失败: %v", err)
	}
	want := []weread.ShelfBook{
		{BookID: "601111", Title: "未读完的书", FinishReading: false},
		{BookID: "602222", Title: "已读完的书", FinishReading: true},
	}
	if len(books) != len(want) {
		t.Fatalf("books = %+v，期望 %+v", books, want)
	}
	for i := range want {
		if books[i] != want[i] {
			t.Errorf("books[%d] = %+v，期望 %+v", i, books[i], want[i])
		}
	}

	// 线上：renewal → shelf；无 Reader 页、无上报。
	reqs := h.weread.snapshot()
	if len(reqs) != 2 {
		t.Fatalf("请求数 = %d，期望 2（renewal + shelf）；%+v", len(reqs), reqs)
	}
	if reqs[0].Path != "/web/login/renewal" || reqs[1].Path != "/web/shelf/sync" {
		t.Errorf("请求序 = %v", []string{reqs[0].Path, reqs[1].Path})
	}
	// renewal 新 Cookie 已并入后续 shelf 请求（会话机制与 daemon 相同）。
	if c := reqs[1].Header.Get("Cookie"); !strings.Contains(c, "wr_gid=new123") {
		t.Errorf("shelf 请求 Cookie = %q，期望已并入 renewal 新 Cookie", c)
	}
	// 会话文件：renewal 的新 Cookie 已持久化（重启不回退旧会话）。
	sessData, err := os.ReadFile(filepath.Join(h.cfg.DataDir, "login_session.json"))
	if err != nil {
		t.Fatalf("Login Session 文件不存在: %v", err)
	}
	if !strings.Contains(string(sessData), `"value": "new123"`) || !strings.Contains(string(sessData), `"value": "newabc"`) {
		t.Errorf("Login Session 应并入 renewal 新 Cookie:\n%s", sessData)
	}

	h.assertNoBooksSideEffects(t)
}

// TestListBooksSessionInvalidRebuildOnceThenPrompts 断言会话失效路径：首选 renewal
// 凭明确证据（succ!=1）判定失效 → 从初始 Cookie 重建一次 → 重试仍失败 → 报错
// 包装 weread.ErrLoginInvalid（CLI 层提示更新初始 Cookie）；重建恰好一次（2 笔
// renewal）；无终态、无通知。重建后的重试携带初始 Cookie（会话机制与 Task 相同）。
func TestListBooksSessionInvalidRebuildOnceThenPrompts(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.weread.renewReject = 5 // 一律拒绝：证据 + 重建后重试均失败
	})
	_, err := h.app.ListBooks(context.Background())
	if err == nil {
		t.Fatal("会话失效时 books 应失败")
	}
	if !errors.Is(err, weread.ErrLoginInvalid) {
		t.Errorf("错误应包装 weread.ErrLoginInvalid，实际: %v", err)
	}

	// 有界：恰好 2 笔 renewal（证据 + 重建后重试）。
	var renewals []wereadRequest
	for _, r := range h.weread.snapshot() {
		if r.Path == "/web/login/renewal" {
			renewals = append(renewals, r)
		}
	}
	if len(renewals) != 2 {
		t.Fatalf("renewal 数 = %d，期望 2（1 次证据 + 1 次重建后重试）", len(renewals))
	}
	if c := renewals[1].Header.Get("Cookie"); !strings.Contains(c, "wr_gid=123") || !strings.Contains(c, "wr_skey=abc") {
		t.Errorf("重建后 renewal Cookie = %q，期望初始 Cookie 重建的会话", c)
	}
	if strings.HasPrefix(err.Error(), "登录 renewal 失败") {
		t.Errorf("登录失效不得套普通失败文案: %v", err)
	}
	h.assertNoBooksSideEffects(t)
}

// TestListBooksSessionInvalidWithoutInitialCookiePrompts 断言无初始 Cookie 可重建时
// 同样报错并提示（仅持久化会话也已失效，checklist 判别与 Task 一致）。
func TestListBooksSessionInvalidWithoutInitialCookiePrompts(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.cfg.Cookie = ""
		h.weread.renewReject = 5
		seedSession(t, h.cfg.DataDir, &http.Cookie{Name: "wr_gid", Value: "OLDSTALE"})
	})
	_, err := h.app.ListBooks(context.Background())
	if err == nil {
		t.Fatal("会话失效且无初始 Cookie 时 books 应失败")
	}
	if !errors.Is(err, weread.ErrLoginInvalid) {
		t.Errorf("错误应包装 weread.ErrLoginInvalid，实际: %v", err)
	}
	// 无重建种子：仅 1 笔 renewal（证据）即失败，不重试。
	if n := h.weread.count("/web/login/renewal"); n != 1 {
		t.Errorf("renewal 数 = %d，期望 1", n)
	}
	h.assertNoBooksSideEffects(t)
}

// TestListBooksTransientRenewalFailureIsNotLoginInvalid 断言暂时性 renewal 失败
// （HTTP 500）不套登录失效文案、不重建（checklist #2 判别边界与 Task 一致）。
func TestListBooksTransientRenewalFailureIsNotLoginInvalid(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.weread.renewStatus = 500
	})
	_, err := h.app.ListBooks(context.Background())
	if err == nil {
		t.Fatal("renewal HTTP 500 时 books 应失败")
	}
	if errors.Is(err, weread.ErrLoginInvalid) {
		t.Errorf("非明确证据不得归类为登录失效: %v", err)
	}
	if n := h.weread.count("/web/login/renewal"); n != 1 {
		t.Errorf("renewal 数 = %d，期望 1（无证据不重建）", n)
	}
	h.assertNoBooksSideEffects(t)
}

// TestListBooksShelfFailureIsPlainError 断言 Shelf 抓取失败（HTTP 500 / errCode!=0）
// 为普通查询错误：不套登录失效文案、无终态、无通知。
func TestListBooksShelfFailureIsPlainError(t *testing.T) {
	for name, mutate := range map[string]func(*testHarness){
		"HTTP 500":      func(h *testHarness) { h.weread.shelfStatus = 500 },
		"errCode -2010": func(h *testHarness) { h.weread.shelfErrCode = -2010 },
	} {
		t.Run(name, func(t *testing.T) {
			h := setup(t, mutate)
			_, err := h.app.ListBooks(context.Background())
			if err == nil {
				t.Fatal("Shelf 失败时 books 应失败")
			}
			if errors.Is(err, weread.ErrLoginInvalid) {
				t.Errorf("Shelf 错误不得归类为登录失效（checklist #2 判别边界）: %v", err)
			}
			if name == "errCode -2010" && !strings.Contains(err.Error(), "-2010") {
				t.Errorf("错误应含 errCode 信息: %v", err)
			}
			h.assertNoBooksSideEffects(t)
		})
	}
}

// TestListBooksShelfRejectedDoesNotMergeResponseCookies 断言 Shelf 业务失败响应
// （errCode!=0）携带的 Set-Cookie 不并入会话（issue 16 一致语义）：renewal 新
// Cookie 照常并入并持久化，Shelf 失败响应的不并入。
func TestListBooksShelfRejectedDoesNotMergeResponseCookies(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.weread.shelfErrCode = -2010
		h.weread.shelfErrCookies = true
	})
	_, err := h.app.ListBooks(context.Background())
	if err == nil {
		t.Fatal("Shelf errCode!=0 时 books 应失败")
	}
	// renewal 成功 → 新 Cookie 照常并入；Shelf 失败响应的 wr_gid=TRAPSHELF 不得并入。
	sessData, err := os.ReadFile(filepath.Join(h.cfg.DataDir, "login_session.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sessData), `"value": "new123"`) {
		t.Errorf("renewal 新 Cookie 应照常并入并持久化:\n%s", sessData)
	}
	if strings.Contains(string(sessData), "TRAPSHELF") {
		t.Errorf("Shelf 失败响应的 Set-Cookie 不得并入持久化 Login Session:\n%s", sessData)
	}
}

// TestListBooksEmptyShelf 断言空书架返回空列表（无错误；CLI 输出为空即退出码 0）。
func TestListBooksEmptyShelf(t *testing.T) {
	h := setup(t, nil) // shelfBooks 默认 nil = 空书架
	books, err := h.app.ListBooks(context.Background())
	if err != nil {
		t.Fatalf("空书架不应报错: %v", err)
	}
	if len(books) != 0 {
		t.Errorf("books = %+v，期望空列表", books)
	}
}

// TestListBooksRejectedWhileTaskRunning 断言并发守卫（与 Task 共用 runMu）：Task
// 运行中 books 被拒绝（ErrTaskRunning），且不打断 Task；Task 完成后可正常查询。
func TestListBooksRejectedWhileTaskRunning(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.weread.shelfBooks = []fakeShelfBook{{BookID: "601111", Title: "书"}}
	})
	h.weread.blockTimed = make(chan struct{})
	h.weread.blockTimedTriggered = make(chan struct{}, 1)

	ctx := context.Background()
	done := make(chan struct{})
	var taskErr error
	go func() {
		_, taskErr = h.app.RunTask(ctx)
		close(done)
	}()
	select {
	case <-h.weread.blockTimedTriggered:
	case <-time.After(10 * time.Second):
		t.Fatal("timed report 未在 10s 内到达")
	}

	_, err := h.app.ListBooks(ctx)
	if err == nil {
		t.Fatal("Task 运行中 books 应被拒绝")
	}
	if !errors.Is(err, ErrTaskRunning) {
		t.Errorf("错误应包装 ErrTaskRunning，实际: %v", err)
	}

	close(h.weread.blockTimed)
	select {
	case <-done:
		if taskErr != nil {
			t.Fatalf("books 拒绝不应影响 Task: %v", taskErr)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Task 未在 30s 内完成")
	}
}
