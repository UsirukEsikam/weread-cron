// daemon 在应用边界 seam 的集成测试（ADR-0006）：真实 App（生产代码）+ fake weread
// 服务端 + 注入 clock/RNG，验证 daemon 闭环——启动时窗口未过且无终态 → 当天剩余
// 窗口内随机排一次 → 到点执行真实 Task（请求时序/终态/通知全走线上断言）→ 排定次日。
package app

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"weread-cron/internal/clock"
	"weread-cron/internal/scheduler"
	"weread-cron/internal/session"
	"weread-cron/internal/terminal"
)

// daemonLog 是并发安全的日志缓冲（daemon 在 goroutine 中写日志）。
type daemonLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (d *daemonLog) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.b.Write(p)
}

func (d *daemonLog) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.b.String()
}

// waitDaemonLog 轮询日志直到出现 want 或超时。
func waitDaemonLog(t *testing.T, log *daemonLog, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(log.String(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待 daemon 日志 %q 超时；实际:\n%s", want, log.String())
}

// TestDaemonExecutesTaskAndSchedulesNextDay：daemon 闭环。注入 Capped 时钟：
// 启动（23:35，窗口 [23:30,23:59] 未过、无终态）→ 当天剩余窗口内排一次 → 到点
// 执行真实 Task（renewal/Reader 页/enter+timed reports 全部走 fake 服务端）→
// success 终态落盘 + 成功通知 → 排定次日。调用后取消，daemon 干净退出。
func TestDaemonExecutesTaskAndSchedulesNextDay(t *testing.T) {
	h := setup(t, func(h *testHarness) {
		h.cfg.TZName = "Asia/Shanghai"
		h.cfg.WindowStart = 23*60 + 30
		h.cfg.WindowEnd = 23*60 + 59
		h.clk = clock.NewFake(time.Date(2025, 9, 6, 23, 35, 0, 0, testTZ))
	})
	// daemon 与 Task 各持一个起点一致的时钟：daemon 用 Capped（睡眠钉在次日窗口前，
	// 取消时序确定）；Task 用 Fake（report 节奏自动推进）。两者都从 23:35 开始。
	daemonClk := clock.NewCapped(
		time.Date(2025, 9, 6, 23, 35, 0, 0, testTZ),
		time.Date(2025, 9, 7, 0, 30, 0, 0, testTZ), // 次日窗口前钉住：不可能执行次日 Task
		nil)
	var log daemonLog
	d := scheduler.New(h.cfg, scheduler.Deps{
		Clock:  daemonClk,
		RNG:    h.rng,
		Task:   h.app,
		Logger: slog.New(slog.NewTextHandler(&log, nil)),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	// 首次排定在当天剩余窗口内（启动日志即含今天日期），随后 Task 执行完成并排次日。
	waitDaemonLog(t, &log, "已到启动时刻，执行 Task")
	waitDaemonLog(t, &log, "Task 完成，排定次日")
	waitDaemonLog(t, &log, "2025-09-07")
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("取消后应返回 nil，got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("取消后 daemon 未退出")
	}

	// 线上断言：Task 真的执行了（renewal/Reader 页/enter+timed reports）。
	if n := h.weread.count("/web/login/renewal"); n != 1 {
		t.Errorf("renewal 请求 = %d，期望 1（Task 应执行一次）", n)
	}
	if n := h.weread.count("/web/reader/"); n < 1 {
		t.Errorf("Reader 页请求 = %d，期望 ≥1", n)
	}
	if n := h.weread.count("/web/book/read"); n < 3 {
		t.Errorf("上报请求 = %d，期望 ≥3（enter + 2×timed）", n)
	}

	// /data 断言：今天 success 终态（Task 完成时落盘，spec 决策 #9）。
	st, has, err := terminal.NewFileStore(h.cfg.DataDir).Load()
	if err != nil || !has {
		t.Fatalf("终态应存在: has=%v err=%v", has, err)
	}
	if st.LastTaskDate != "2025-09-06" || st.LastTaskResult != terminal.ResultSuccess {
		t.Errorf("终态 = %+v，期望 2025-09-06/success", st)
	}

	// 通知断言：成功通知发送且含当天日期。
	if n := len(h.bark.snapshot()); n != 1 {
		t.Errorf("Bark 通知 = %d，期望 1", n)
	}
	body, _ := h.bark.snapshot()[0].Body["body"].(string)
	if !strings.Contains(body, "2025-09-06") {
		t.Errorf("Bark 通知文本应含当天日期；body=%q", body)
	}

	// daemon 退出日志。
	if got := log.String(); !strings.Contains(got, "daemon 退出") {
		t.Errorf("缺少退出日志:\n%s", got)
	}
	if got := log.String(); !strings.Contains(got, "已排定下次 Task 启动") {
		t.Errorf("缺少排定日志:\n%s", got)
	}
}

// TestDaemonCancelDuringTaskRunExitsAndRestartReexecutes：daemon 内真实 Task 于
// report 间隔等待中被取消（SIGINT/SIGTERM）→ 取消不是业务最终失败：不写终态、
// daemon 按现有语义正常退出（ticket 18 验收 6）；重启后当天无终态 → 按"无
// Terminal State"异常启动规则（ticket 24：窗口内立即执行）重新执行并形成 success
// 终态（验收 4 新增覆盖）。
func TestDaemonCancelDuringTaskRunExitsAndRestartReexecutes(t *testing.T) {
	start := time.Date(2025, 9, 6, 10, 0, 0, 0, testTZ)
	pin := newPinClock(start)
	h := setup(t, func(h *testHarness) {
		h.cfg.TZName = "Asia/Shanghai"
		h.cfg.WindowStart = 9*60 + 55
		h.cfg.WindowEnd = 10*60 + 5
		h.clk = pin // Task 时钟：节奏等待可钉住
	})

	// 第 1 次运行：daemon 用独立 Fake（无终态 + 窗口内 → 立即执行，无 daemon 睡眠）；
	// Task 的真实节奏等待被 pin 钉住。
	var log daemonLog
	d := scheduler.New(h.cfg, scheduler.Deps{
		Clock:  clock.NewFake(start),
		RNG:    h.rng,
		Task:   h.app,
		Logger: slog.New(slog.NewTextHandler(&log, nil)),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	waitDaemonLog(t, &log, "已到启动时刻，执行 Task")
	select {
	case <-pin.slep:
	case <-time.After(10 * time.Second):
		t.Fatal("Task 未进入 report 间隔等待")
	}
	cancel()
	close(pin.release)

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("取消后 daemon 应正常退出（nil），got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("取消后 daemon 未退出")
	}
	if got := log.String(); !strings.Contains(got, "daemon 退出") {
		t.Errorf("取消后应记录 daemon 退出日志:\n%s", got)
	}
	// 取消不写终态：重启前当天无 Terminal State（取消 = 正常退出，非业务失败）。
	if _, has, err := terminal.NewFileStore(h.cfg.DataDir).Load(); err != nil || has {
		t.Fatalf("取消后不应有 Terminal State: has=%v err=%v", has, err)
	}

	// 第 2 次运行（重启）：新的 App（全新 Fake 时钟）与 daemon（Capped 钉在次日
	// 窗口前）；无终态 + 窗口内 → 立即执行，成功形成终态并排定次日。
	restart := time.Date(2025, 9, 6, 10, 0, 30, 0, testTZ)
	app2 := New(h.cfg, Deps{
		Clock:         clock.NewFake(restart),
		RNG:           h.rng,
		WereadBaseURL: h.wereadURL,
		Sessions:      session.NewFileStore(h.cfg.DataDir),
		Terminal:      terminal.NewFileStore(h.cfg.DataDir),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	var log2 daemonLog
	d2 := scheduler.New(h.cfg, scheduler.Deps{
		Clock:  clock.NewCapped(restart, time.Date(2025, 9, 7, 0, 30, 0, 0, testTZ), nil),
		RNG:    h.rng,
		Task:   app2,
		Logger: slog.New(slog.NewTextHandler(&log2, nil)),
	})
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan error, 1)
	go func() { done2 <- d2.Run(ctx2) }()
	waitDaemonLog(t, &log2, "已到启动时刻，执行 Task")
	waitDaemonLog(t, &log2, "Task 完成，排定次日")
	waitDaemonLog(t, &log2, "2025-09-07")
	cancel2()
	select {
	case err := <-done2:
		if err != nil {
			t.Errorf("重启 daemon 取消后应返回 nil，got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("重启 daemon 未退出")
	}

	// 重启后重新执行并形成 success 终态（无终态异常启动规则）；通知恰 1 条。
	st, has, err := terminal.NewFileStore(h.cfg.DataDir).Load()
	if err != nil || !has {
		t.Fatalf("重启后应有 success 终态: has=%v err=%v", has, err)
	}
	if st.LastTaskResult != terminal.ResultSuccess || st.LastTaskDate != "2025-09-06" {
		t.Errorf("终态 = %+v，期望 2025-09-06/success", st)
	}
	if n := len(h.bark.snapshot()); n != 1 {
		t.Errorf("通知 = %d，期望 1（仅重启后的成功通知；取消不发通知）", n)
	}
	// 线上：第 1 次运行仅 enter（取消前），第 2 次完整 enter + 2×timed。
	if n := h.weread.count("/web/book/read"); n != 4 {
		t.Errorf("report 总数 = %d，期望 4（1 + enter + 2×timed）", n)
	}
}
