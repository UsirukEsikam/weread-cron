// daemon 在应用边界 seam 的集成测试（ADR-0006）：真实 App（生产代码）+ fake weread
// 服务端 + 注入 clock/RNG，验证 daemon 闭环——启动时窗口未过且无终态 → 当天剩余
// 窗口内随机排一次 → 到点执行真实 Task（请求时序/终态/通知全走线上断言）→ 排定次日。
package app

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"weread-cron/internal/clock"
	"weread-cron/internal/scheduler"
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
