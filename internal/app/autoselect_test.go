// 自动选书（ticket 09）的应用边界 seam 测试：未配置候选书时 Task 从 Shelf 自动选书
// ——元数据过滤优先未读完 → 有界探测（失败换下一本）→ 未读完全部不可用回退已读完
// 可用书 → 全部不可用 → failed 终态 + 失败通知；Shelf 抓取为暂时性失败（不写终态、
// 不通知）；显式指定候选不因 progress=100% 被排除。
//
// ticket 13 范围：探测前对候选层随机化（注入 RNG 洗牌）——稳定 Shelf 下多日执行
// 选中结果随机分布，而非固定第一本；分层语义与每层有界探测上限保持不变。
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"weread-cron/internal/terminal"
	"weread-cron/internal/weread"
)

// shelfID 是 fake 服务端按编码 bookId 索引的辅助（Reader 页路径携带的是 EncodeID）。
func shelfID(bookID string) string { return weread.EncodeID(bookID) }

// TestRunAutoSelectsUnreadBookFromShelf 是自动选书主 happy path：bookIds 顺序为
// [已读完, 未读完] → 元数据过滤选择未读完层 → 探测命中的书（探测即抓取，进入上报
// 命中缓存零额外请求）→ Task 完整成功；Shelf 抓取恰好 1 次。
func TestRunAutoSelectsUnreadBookFromShelf(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.cfg.Books = nil // 未配置候选书 = 自动选书
		h.weread.shelfBooks = []fakeShelfBook{
			{BookID: "602222", Title: "已读完的书", FinishReading: true},
			{BookID: "601111", Title: "未读完的书", FinishReading: false},
		}
		h.weread.readerTitleByBook = map[string]string{shelfID("601111"): "未读完的书"}
	})
	res, err := h.runTask(context.Background())
	if err != nil {
		t.Fatalf("自动选书 Task 失败: %v", err)
	}
	if res.BookID != "601111" || res.BookTitle != "未读完的书" {
		t.Errorf("选中 = %s/%s，期望未读完的 601111/未读完的书", res.BookID, res.BookTitle)
	}
	if res.Reports != 2 {
		t.Errorf("Reports = %d，期望 2（任务完整执行）", res.Reports)
	}

	// 线上：renewal → shelf → reader(探测命中缓存) → enter → timed → timed。
	if n := h.weread.count("/web/shelf/sync"); n != 1 {
		t.Errorf("Shelf 请求数 = %d，期望 1", n)
	}
	h.assertRequestSequence("renewal", "refresh", "enter", "timed", "timed")
	if n := h.weread.count("/web/reader/"); n != 1 {
		t.Errorf("Reader 页抓取数 = %d，期望 1（探测 + 缓存命中，不重复抓取）", n)
	}
	// 未读完的书（601111）的 Reader 页被探测；已读完的 602222 未探测。
	found := false
	for _, r := range h.weread.snapshot() {
		if r.Path == "/web/reader/"+shelfID("601111") {
			found = true
		}
		if r.Path == "/web/reader/"+shelfID("602222") {
			t.Errorf("已读完的书不应被优先探测（未读完层有可用书）")
		}
	}
	if !found {
		t.Errorf("未发现未读完书的 Reader 页探测请求")
	}

	// success 终态 + 成功通知；无失败通知。
	st, has, err := terminal.NewFileStore(h.cfg.DataDir).Load()
	if err != nil || !has || st.LastTaskResult != terminal.ResultSuccess {
		t.Fatalf("终态 = %+v has=%v err=%v，期望 success", st, has, err)
	}
	if got := len(h.bark.snapshot()); got != 1 || h.bark.snapshot()[0].Body["title"] != "微信读书阅读任务完成" {
		t.Errorf("应只有 1 条成功通知，实际 %d 条: %+v", got, h.bark.snapshot())
	}
}

// TestRunAutoSelectFallsBackToFinishedWhenUnreadUnusable 断言未读完候选全部不可用
// （HTTP 500 / 无 __INITIAL_STATE__）→ 回退已读完但可用的书；探测逐本进行（失败换
// 下一本）、选定书命中缓存。
func TestRunAutoSelectFallsBackToFinishedWhenUnreadUnusable(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.cfg.Books = nil
		h.weread.shelfBooks = []fakeShelfBook{
			{BookID: "701111", Title: "未读完A", FinishReading: false},
			{BookID: "702222", Title: "未读完B", FinishReading: false},
			{BookID: "703333", Title: "已读完C", FinishReading: true},
		}
		h.weread.readerFailByBook = map[string]bool{shelfID("701111"): true}
		h.weread.readerNoStateByBook = map[string]bool{shelfID("702222"): true}
		h.weread.readerTitleByBook = map[string]string{shelfID("703333"): "已读完C"}
	})
	res, err := h.runTask(context.Background())
	if err != nil {
		t.Fatalf("回退选书 Task 失败: %v", err)
	}
	if res.BookID != "703333" || res.BookTitle != "已读完C" {
		t.Errorf("选中 = %s/%s，期望回退已读完 703333/已读完C（验证清单 #3 假设：progress=100%% 回退）", res.BookID, res.BookTitle)
	}

	// 3 次 Reader 页抓取（2 本不可用 + 1 本回退命中缓存）。
	if n := h.weread.count("/web/reader/"); n != 3 {
		t.Errorf("Reader 页抓取数 = %d，期望 3", n)
	}
	h.assertRequestSequence("renewal", "refresh", "refresh", "refresh", "enter", "timed", "timed")

	// success 终态：回退选书成功仍算完成任务。
	st, has, err := terminal.NewFileStore(h.cfg.DataDir).Load()
	if err != nil || !has || st.LastTaskResult != terminal.ResultSuccess {
		t.Fatalf("终态 = %+v has=%v err=%v，期望 success", st, has, err)
	}
}

// TestRunAutoSelectProbeBoundPerTier 断言探测有界（ticket 13 随机化后语义保持）：
// 未读完层 30 本全部不可用 → 只探测 20 本（洗牌后前 20，上限默认）即回退已读完层；
// 已读完层 4 本中只有 720004 可用，seed 42 洗牌后它落在第 2 位 → 恰好 22 次 Reader
// 页抓取（20 + 2）。探测顺序随种子变化（非固定列表序），但每层探测次数不超过上限、
// 命中书仍是唯一可用的 720004。
func TestRunAutoSelectProbeBoundPerTier(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.cfg.Books = nil
		// 显式固定种子：本测试的精确探测数依赖洗牌结果（seed 42 下可用书
		// 720004 在已读完层洗牌后落于第 2 位 → 已读完层 2 次探测）。不继承
		// setup 的默认种子，避免上游 RNG 消耗变化时静默改变计数。
		h.rng = rand.New(rand.NewSource(42))
		var shelf []fakeShelfBook
		fail := map[string]bool{}
		// 30 本未读完，全部不可用（Reader 页 500）。
		for i := 1; i <= 30; i++ {
			id := fmt.Sprintf("7100%02d", i)
			shelf = append(shelf, fakeShelfBook{BookID: id, Title: "未读完" + id, FinishReading: false})
			fail[shelfID(id)] = true
		}
		// 已读完层 4 本：前 3 本不可用，第 4 本可用（回退目标）。
		for i := 1; i <= 4; i++ {
			id := fmt.Sprintf("7200%02d", i)
			shelf = append(shelf, fakeShelfBook{BookID: id, Title: "已读完" + id, FinishReading: true})
			if i < 4 {
				fail[shelfID(id)] = true
			}
		}
		h.weread.shelfBooks = shelf
		h.weread.readerFailByBook = fail
	})
	res, err := h.runTask(context.Background())
	if err != nil {
		t.Fatalf("有界探测 Task 失败: %v", err)
	}
	if res.BookID != "720004" {
		t.Errorf("选中 = %s，期望唯一可用的已读完 720004", res.BookID)
	}
	// 未读完层恰好 20 次探测（有界上限：洗牌后前 20 本全部不可用），已读完层
	// 2 次（seed 42 洗牌后 720004 在第 2 位；1 失败 + 1 命中）→ 共 22 次。
	if n := h.weread.count("/web/reader/"); n != 22 {
		t.Errorf("Reader 页抓取数 = %d，期望 22（20 + 2，探测上限生效）", n)
	}
	unreadProbes := 0
	unreadEnc := map[string]bool{}
	for i := 1; i <= 30; i++ {
		unreadEnc[shelfID(fmt.Sprintf("7100%02d", i))] = true
	}
	for _, r := range h.weread.snapshot() {
		if unreadEnc[strings.TrimPrefix(r.Path, "/web/reader/")] {
			unreadProbes++
		}
	}
	if unreadProbes != 20 {
		t.Errorf("未读完层探测数 = %d，期望 20（每层上限；其余 10 本不被探测）", unreadProbes)
	}
}

// TestRunAutoSelectExhaustedFailsWithNotification 断言探测全部失败 → Task 失败：
// failed 终态先落盘、再发失败通知（失败阶段 = 自动选书、已尝试动作 = Shelf 过滤与
// 有界探测）；无成功通知；未进入 enter（无 /web/book/read 请求）。failed 终态下
// run 允许重试（用户故事 #41）。
func TestRunAutoSelectExhaustedFailsWithNotification(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.cfg.Books = nil
		h.weread.shelfBooks = []fakeShelfBook{
			{BookID: "701111", Title: "未读完A", FinishReading: false},
			{BookID: "702222", Title: "已读完B", FinishReading: true},
		}
		h.weread.readerFailByBook = map[string]bool{shelfID("701111"): true, shelfID("702222"): true}
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
	})
	_, err := h.runTask(context.Background())
	if err == nil {
		t.Fatal("全部候选不可用时 Task 应失败")
	}
	if !strings.Contains(err.Error(), "自动选书失败") {
		t.Errorf("错误应说明自动选书失败: %v", err)
	}
	// 失败发生在选书阶段：没有 enter/timed 上报。
	if n := h.weread.count("/web/book/read"); n != 0 {
		t.Errorf("不应进入上报阶段，实际 %d 条 report", n)
	}

	// failed 终态（先落盘再通知，spec 决策 #9/#10）。
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

	// 失败通知（Bark + 企业微信各 1 条）：失败阶段/主要错误/已尝试动作；无成功通知。
	for name, nf := range map[string]*fakeNotify{"bark": h.bark, "wecom": h.wecom} {
		recs := nf.snapshot()
		if len(recs) != 1 {
			t.Fatalf("%s 通知数 = %d，期望 1 条失败通知", name, len(recs))
		}
		msg, _ := json.Marshal(recs[0].Body)
		for _, want := range []string{
			"微信读书阅读任务失败",
			"失败阶段：自动选书",
			"主要错误",
			"已尝试恢复：Shelf 元数据过滤 → Reader Context 有界探测",
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

// TestRunAutoSelectEmptyShelfFailsWithNotification 断言空书架（Shelf 无数据）同样
// 走"全部不可用"失败路径：failed 终态 + 失败通知（书架 0 本）。
func TestRunAutoSelectEmptyShelfFailsWithNotification(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.cfg.Books = nil // shelfBooks 默认 nil = 空书架
	})
	_, err := h.runTask(context.Background())
	if err == nil {
		t.Fatal("空书架时 Task 应失败")
	}
	st, has, err := terminal.NewFileStore(h.cfg.DataDir).Load()
	if err != nil || !has || st.LastTaskResult != terminal.ResultFailed {
		t.Fatalf("终态 = %+v has=%v err=%v，期望 failed", st, has, err)
	}
	if got := len(h.bark.snapshot()); got != 1 {
		t.Errorf("应发送 1 条失败通知，实际 %d", got)
	}
	msg, _ := json.Marshal(h.bark.snapshot()[0].Body)
	if !strings.Contains(string(msg), "书架 0 本") {
		t.Errorf("失败通知应说明书架为空；body=%s", msg)
	}
}

// TestRunAutoSelectShelfTransientFailureNoTerminal 断言 Shelf 抓取失败（HTTP 500）
// 按暂时性处理：Task 失败但不写终态、不通知（当日可再次调度——与 Reader 页抓取
// 失败同一姿态）；错误为普通错误（非登录失效）。
// TestRunAutoSelectShelfTransientFailureConvergesToFailedTerminal 断言 Shelf 抓取失败
// （HTTP 500）按暂时性处理并经统一收敛（ticket 24）：Task 失败、failed 终态 + 失败
// 通知（阶段 = 自动选书）；不是登录失效；不再于窗口内重排。
func TestRunAutoSelectShelfTransientFailureConvergesToFailedTerminal(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.cfg.Books = nil
		h.weread.shelfStatus = 500
	})
	_, err := h.runTask(context.Background())
	if err == nil {
		t.Fatal("Shelf 抓取失败时 Task 应失败")
	}
	if !strings.Contains(err.Error(), "抓取 Shelf 失败") {
		t.Errorf("错误应说明 Shelf 抓取失败: %v", err)
	}
	// 无条件收敛：failed 终态 + 失败通知（阶段 = 自动选书）。
	st, has, err := terminal.NewFileStore(h.cfg.DataDir).Load()
	if err != nil || !has || st.LastTaskResult != terminal.ResultFailed {
		t.Fatalf("终态 = %+v has=%v err=%v，期望 failed（暂时性失败统一收敛）", st, has, err)
	}
	msg, _ := json.Marshal(h.bark.snapshot()[0].Body)
	for _, want := range []string{"微信读书阅读任务失败", "失败阶段：自动选书"} {
		if !strings.Contains(string(msg), want) {
			t.Errorf("失败通知缺少 %q；body=%s", want, msg)
		}
	}
}

// TestRunExplicitBookNotExcludedWhenProgress100 断言显式指定候选书不因
// progress=100% 被排除（issue 验收）：Reader 页 progress=100 → Task 完整成功，
// enter payload 的 pr=100 如实上报（验证清单 #3 计费行为未实测：结构上按"可上报"
// 实现，冲突时提出讨论）。
func TestRunExplicitBookNotExcludedWhenProgress100(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.weread.readerProgressByBook = map[string]int{shelfID(testBookID): 100}
		// Books 保持默认 [testBookID]（显式指定）。
	})
	res, err := h.runTask(context.Background())
	if err != nil {
		t.Fatalf("progress=100%% 的显式书不应被排除: %v", err)
	}
	if res.BookID != testBookID {
		t.Errorf("BookID = %s，期望 %s", res.BookID, testBookID)
	}
	enter := payloadFromWire(t, h.reportRecords()[0].Body)
	if enter["pr"] != "100" {
		t.Errorf("enter.pr = %s，期望 100（如实上报服务端进度）", enter["pr"])
	}
	st, has, err := terminal.NewFileStore(h.cfg.DataDir).Load()
	if err != nil || !has || st.LastTaskResult != terminal.ResultSuccess {
		t.Fatalf("终态 = %+v has=%v err=%v，期望 success", st, has, err)
	}
}

// TestRunAutoSelectRandomizesCandidates 断言候选层的随机化（ticket 13）：稳定
// Shelf 下，用不同 RNG 种子多次执行 Task（每轮独立种子 = 多日执行），选中结果在
// 候选间分布，而非固定第一本（用户故事 #10 的随机化意图）。两个用例覆盖两层：
// 未读完层（候选均可读）与已读完回退层（未读完全部不可用、已读完均可读）——
// 分层语义保持（选中限定在对应层、优先层有可用书时回退层不被探测、每轮抓取数
// 固定 = 有界探测 + 洗牌后第一本可用即命中）。
func TestRunAutoSelectRandomizesCandidates(t *testing.T) {
	cases := []struct {
		name       string
		shelf      []fakeShelfBook
		readerFail map[string]bool
		// wantPrefix 限定选中的 bookId 前缀（分层语义断言）。
		wantPrefix string
		// probesPerRun 是每轮 Reader 页抓取数（有界探测；优先层可用 ≥1 时回退层不探测）。
		probesPerRun int
		// neverProbed 是任何一轮都不应被探测的书（分层优先断言）。
		neverProbed []string
	}{
		{
			name: "未读完层随机化",
			shelf: []fakeShelfBook{
				{BookID: "601111", Title: "未读完A", FinishReading: false},
				{BookID: "601222", Title: "未读完B", FinishReading: false},
				{BookID: "601333", Title: "未读完C", FinishReading: false},
				{BookID: "602999", Title: "已读完D", FinishReading: true},
			},
			wantPrefix:   "601",
			probesPerRun: 1,
			neverProbed:  []string{"602999"},
		},
		{
			name: "已读完回退层随机化",
			shelf: []fakeShelfBook{
				{BookID: "701111", Title: "未读完A", FinishReading: false},
				{BookID: "701222", Title: "未读完B", FinishReading: false},
				{BookID: "702111", Title: "已读完A", FinishReading: true},
				{BookID: "702222", Title: "已读完B", FinishReading: true},
				{BookID: "702333", Title: "已读完C", FinishReading: true},
			},
			readerFail: map[string]bool{
				shelfID("701111"): true,
				shelfID("701222"): true,
			},
			wantPrefix:   "702",
			probesPerRun: 3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			selected := map[string]int{}
			for _, seed := range []int64{11, 22, 33, 44, 55, 66, 77, 88} {
				h := setup(t, func(h *testHarness) {
					h.cfg.Books = nil // 未配置候选书 = 自动选书
					h.rng = rand.New(rand.NewSource(seed))
					h.weread.shelfBooks = tc.shelf
					h.weread.readerFailByBook = tc.readerFail
				})
				res, err := h.runTask(context.Background())
				if err != nil {
					t.Fatalf("seed %d 自动选书 Task 失败: %v", seed, err)
				}
				selected[res.BookID]++
				if !strings.HasPrefix(res.BookID, tc.wantPrefix) {
					t.Errorf("seed %d 选中 = %s，期望 %s 前缀候选", seed, res.BookID, tc.wantPrefix)
				}
				// 有界探测 + 洗牌后第一本可用即命中：每轮抓取数固定。
				if n := h.weread.count("/web/reader/"); n != tc.probesPerRun {
					t.Errorf("seed %d Reader 页抓取数 = %d，期望 %d", seed, n, tc.probesPerRun)
				}
				for _, id := range tc.neverProbed {
					for _, r := range h.weread.snapshot() {
						if r.Path == "/web/reader/"+shelfID(id) {
							t.Errorf("seed %d 书 %s 被探测（分层优先语义）", seed, id)
						}
					}
				}
			}
			if len(selected) < 2 {
				t.Errorf("[%s] 多日执行选中结果固定为 %v（期望随机分布，非固定第一本）", tc.name, selected)
			}
			for id := range selected {
				if !strings.HasPrefix(id, tc.wantPrefix) {
					t.Errorf("[%s] 选中 %s 超出 %s 前缀候选范围: %v", tc.name, id, tc.wantPrefix, selected)
				}
			}
		})
	}
}

// TestRunAutoSelectUsesProbedContextForEnter 断言自动选书选定书的 enter 使用探测时
// 的 Reader Context（同一 Reader 页、缓存命中；enter.ps 与探测一致的初始 Context）。
func TestRunAutoSelectUsesProbedContextForEnter(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.cfg.Books = nil
		h.weread.shelfBooks = []fakeShelfBook{{BookID: "601111", Title: "未读完的书", FinishReading: false}}
		h.weread.rotateReaderState = true // 第 N 次抓取 = psvts-N；探测应为第 1 次
	})
	if _, err := h.runTask(context.Background()); err != nil {
		t.Fatalf("Task 失败: %v", err)
	}
	enter := payloadFromWire(t, h.reportRecords()[0].Body)
	if enter["ps"] != "fake-psvts-1" {
		t.Errorf("enter.ps = %s，期望探测时的 Context（psvts-1）", enter["ps"])
	}
	if n := h.weread.count("/web/reader/"); n != 1 {
		t.Errorf("Reader 页抓取数 = %d，期望 1（探测即建立、上报命中缓存）", n)
	}
}
