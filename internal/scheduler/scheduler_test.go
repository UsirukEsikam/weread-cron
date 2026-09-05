package scheduler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"weread-cron/internal/clock"
	"weread-cron/internal/config"
	"weread-cron/internal/task"
	"weread-cron/internal/terminal"
)

// syncBuffer 是并发安全的输出缓冲（Run 在 goroutine 中写日志）。
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitFor 轮询 buf 直到出现 want 或超时。
func waitFor(t *testing.T, buf *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待日志 %q 超时；输出:\n%s", want, buf.String())
}

// recordingRunner 记录被调用次数（测试注入 fake TaskRunner）。语义与真实 Task 一致：
// 成功（err == nil）时写入当天 success 终态（spec 决策 #9：终态落盘是 Task 的职责）；
// err != nil 且 failTerminal 时写入当天 failed 终态（恢复链耗尽的语义）；
// err != nil 且 !failTerminal 时是暂时性失败（不写终态）。
type recordingRunner struct {
	mu           sync.Mutex
	calls        int
	err          error
	failTerminal bool
	term         terminal.Store
	tz           *time.Location
	clk          clock.Clock
}

func (r *recordingRunner) RunTask(ctx context.Context) (task.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.err != nil {
		if r.failTerminal && r.term != nil {
			_ = r.term.Save(terminal.State{
				LastTaskDate:   r.clk.Now().In(r.tz).Format(terminal.DayLayout),
				LastTaskResult: terminal.ResultFailed,
			})
		}
		return task.Result{}, r.err
	}
	if r.term != nil {
		if err := r.term.Save(terminal.State{
			LastTaskDate:   r.clk.Now().In(r.tz).Format(terminal.DayLayout),
			LastTaskResult: terminal.ResultSuccess,
		}); err != nil {
			return task.Result{}, err
		}
	}
	return task.Result{}, nil
}

func (r *recordingRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// daemonConfig 构造 daemon 测试配置（TZ=testLoc；窗口等经 mutate 覆盖）。
func daemonConfig(t *testing.T, mutate func(cfg *config.Config)) *config.Config {
	t.Helper()
	cfg := &config.Config{
		Cookie:         "wr_skey=abc; wr_gid=123",
		Books:          []string{"695233"},
		WindowStart:    23*60 + 30,
		WindowEnd:      23*60 + 59,
		ReadMinutesMin: 1,
		ReadMinutesMax: 1,
		DataDir:        t.TempDir(),
		TZ:             testLoc,
		TZName:         "Asia/Shanghai",
	}
	if mutate != nil {
		mutate(cfg)
	}
	return cfg
}

// newDaemon 装配 daemon：Capped 时钟钉在 cap（测试可随时确定地取消）、
// 种子 RNG、记录 runner 与日志缓冲。
func newDaemon(cfg *config.Config, start, cap time.Time, hook func(time.Time), err error, failTerminal bool) (*Daemon, *syncBuffer, *recordingRunner) {
	var out syncBuffer
	clk := clock.NewCapped(start, cap, hook)
	runner := &recordingRunner{err: err, failTerminal: failTerminal, term: terminal.NewFileStore(cfg.DataDir), tz: cfg.TZ, clk: clk}
	d := New(cfg, Deps{
		Clock:  clk,
		RNG:    rand.New(rand.NewSource(7)),
		Task:   runner,
		Logger: slog.New(slog.NewTextHandler(&out, nil)),
	})
	return d, &out, runner
}

// startDaemon 启动 daemon；返回等待其退出的函数。
func startDaemon(t *testing.T, d *Daemon, ctx context.Context) func() error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	return func() error {
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("daemon 未退出")
			return nil
		}
	}
}

// TestDaemonDoesNotExecuteWhenTodayTerminalExists：当天已有终态（seed 文件）→
// daemon 不自动执行，直接排定次日（用户故事 #15；ADR-0002）。
func TestDaemonDoesNotExecuteWhenTodayTerminalExists(t *testing.T) {
	cfg := daemonConfig(t, nil)
	term := terminal.NewFileStore(cfg.DataDir)
	if err := term.Save(terminal.State{
		LastTaskDate:   "2025-09-06",
		LastTaskResult: terminal.ResultSuccess,
	}); err != nil {
		t.Fatal(err)
	}

	// 时钟钉在次日窗口开始前：daemon 不可能执行次日 Task，取消时序确定。
	d, out, runner := newDaemon(cfg, at(2025, 9, 6, 23, 35), at(2025, 9, 7, 23, 30), nil, nil, false)
	ctx, cancel := context.WithCancel(context.Background())
	waitDone := startDaemon(t, d, ctx)

	// 决策日志：当天已有终态 → 跳过今日，下次启动为次日。
	waitFor(t, out, "当天已有终态，跳过今日自动执行")
	waitFor(t, out, "2025-09-07")
	cancel()
	if err := waitDone(); err != nil {
		t.Errorf("取消后应返回 nil，got %v", err)
	}
	if n := runner.count(); n != 0 {
		t.Errorf("已有当天终态时不应执行 Task，实际调用 %d 次", n)
	}
	if got := out.String(); !strings.Contains(got, "daemon 退出") {
		t.Errorf("缺少退出日志:\n%s", got)
	}
	// 终态不被改写（daemon 只读门控；写由 Task 完成）。
	st, has, err := term.Load()
	if err != nil || !has {
		t.Fatalf("终态应存在: has=%v err=%v", has, err)
	}
	if st.LastTaskResult != terminal.ResultSuccess {
		t.Errorf("终态被改写: %+v", st)
	}
}

// TestDaemonDoesNotBackfillMissedWindow：窗口已过且无终态 → 不补跑当天，排次日
// （用户故事 #13；ADR-0002）。
func TestDaemonDoesNotBackfillMissedWindow(t *testing.T) {
	cfg := daemonConfig(t, func(c *config.Config) {
		c.WindowStart = 60 // 01:00
		c.WindowEnd = 180  // 03:00
	})
	d, out, runner := newDaemon(cfg, at(2025, 9, 6, 4, 0), at(2025, 9, 7, 0, 30), nil, nil, false)
	ctx, cancel := context.WithCancel(context.Background())
	waitDone := startDaemon(t, d, ctx)

	waitFor(t, out, "已排定下次 Task 启动")
	waitFor(t, out, "2025-09-07")
	cancel()
	if err := waitDone(); err != nil {
		t.Errorf("取消后应返回 nil，got %v", err)
	}
	if n := runner.count(); n != 0 {
		t.Errorf("错过整天不应补跑，实际执行 %d 次", n)
	}
	if got := out.String(); strings.Contains(got, "2025-09-06") {
		t.Errorf("不应排定今天: %s", got)
	}
}

// TestDaemonRechecksTerminalBeforeStart：daemon 睡眠期间被手动 run 形成终态 →
// 启动前再校验 → 跳过自动执行（spec 决策 #8；ADR-0002）。
func TestDaemonRechecksTerminalBeforeStart(t *testing.T) {
	cfg := daemonConfig(t, func(c *config.Config) {
		c.WindowStart = 23*60 + 31 // 窗口未开始：下次启动 ∈ [23:31, 23:59] 今天
	})
	// 首次越过 23:31（即可能的最早醒来点）时写入当天 success 终态：模拟 daemon
	// 睡眠期间 `weread-cron run` 手动执行形成终态。时钟同时钉在 23:40——
	// daemon 不可能推进到次日，写入与醒来的顺序完全确定。
	hook := func(now time.Time) {
		if !now.In(testLoc).Before(at(2025, 9, 6, 23, 31)) {
			if err := terminal.NewFileStore(cfg.DataDir).Save(terminal.State{
				LastTaskDate:   now.In(testLoc).Format(terminal.DayLayout),
				LastTaskResult: terminal.ResultSuccess,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	d, out, runner := newDaemon(cfg, at(2025, 9, 6, 23, 30), at(2025, 9, 6, 23, 40), hook, nil, false)
	ctx, cancel := context.WithCancel(context.Background())
	waitDone := startDaemon(t, d, ctx)

	waitFor(t, out, "跳过自动执行")
	cancel()
	if err := waitDone(); err != nil {
		t.Errorf("取消后应返回 nil，got %v", err)
	}
	if n := runner.count(); n != 0 {
		t.Errorf("睡眠期间形成终态后不应执行 Task，实际调用 %d 次", n)
	}
	if got := out.String(); !strings.Contains(got, "当天已形成终态") {
		t.Errorf("应输出启动前再校验的跳过日志:\n%s", got)
	}
}

// TestDaemonSkipsWhenAnotherProcessRunsTask：并发守卫拒绝（ticket 11）——到点执行
// 时另一进程持有 Task 锁（手动 run 运行中）：非失败语义——日志为 Info 级拒绝原因、
// 窗口内继续排定重试（不进入 Warn 的"执行失败"文案、不写终态、不取消）。
func TestDaemonSkipsWhenAnotherProcessRunsTask(t *testing.T) {
	cfg := daemonConfig(t, nil) // 窗口 [23:30, 23:59]
	d, out, runner := newDaemon(cfg, at(2025, 9, 6, 23, 35), at(2025, 9, 6, 23, 59), nil, task.ErrTaskRunning, false)
	ctx, cancel := context.WithCancel(context.Background())
	waitDone := startDaemon(t, d, ctx)

	waitFor(t, out, "另一进程正在运行 Task，本次自动执行被拒绝（窗口内继续排定）")
	waitFor(t, out, "已排定下次 Task 启动")
	cancel()
	if err := waitDone(); err != nil {
		t.Errorf("取消后应返回 nil，got %v", err)
	}
	if got := out.String(); strings.Contains(got, "Task 执行失败") {
		t.Errorf("并发拒绝不是 Task 失败，不得进入 Warn 文案:\n%s", got)
	}
	// 不写终态（同暂时性失败语义：窗口内可再次排定）。
	if _, has, err := terminal.NewFileStore(cfg.DataDir).Load(); err != nil || has {
		t.Fatalf("并发拒绝不应产生终态: has=%v err=%v", has, err)
	}
	if n := runner.count(); n < 1 {
		t.Errorf("应至少尝试执行一次，实际 %d 次", n)
	}
}

// TestDaemonReplansWithinWindowAfterTransientFailure：Task 暂时性失败（未写终态）→
// 当天剩余窗口内重排、再次执行（用户故事 #17）；失败不形成终态。
func TestDaemonReplansWithinWindowAfterTransientFailure(t *testing.T) {
	cfg := daemonConfig(t, nil) // 窗口 [23:30, 23:59]
	// 时钟钉在窗口结束：重排后至多推进到 end，之后 daemon 卡住等待取消。
	d, out, runner := newDaemon(cfg, at(2025, 9, 6, 23, 30), at(2025, 9, 6, 23, 59), nil, errors.New("网络暂时不可用"), false)
	ctx, cancel := context.WithCancel(context.Background())
	waitDone := startDaemon(t, d, ctx)

	waitFor(t, out, "Task 执行失败")
	waitFor(t, out, "已排定下次 Task 启动")
	cancel()
	if err := waitDone(); err != nil {
		t.Errorf("取消后应返回 nil，got %v", err)
	}
	if n := runner.count(); n < 1 {
		t.Errorf("至少应尝试执行一次，实际 %d 次", n)
	}
	// 暂时性失败不产生终态（daemon 不写终态；Task 内部规则）。
	if _, has, err := terminal.NewFileStore(cfg.DataDir).Load(); err != nil || has {
		t.Fatalf("暂时性失败不应产生终态: has=%v err=%v", has, err)
	}
}

// TestDaemonSchedulesNextDayAfterFailedTerminal：Task 形成 failed 终态（恢复链耗尽）→
// 当天不再自动重跑，排定次日（ADR-0002：success 与 failed 都阻止当天再次自动执行）。
func TestDaemonSchedulesNextDayAfterFailedTerminal(t *testing.T) {
	cfg := daemonConfig(t, nil) // 窗口 [23:30, 23:59]
	d, out, runner := newDaemon(cfg, at(2025, 9, 6, 23, 35), at(2025, 9, 7, 0, 30), nil, errors.New("恢复链耗尽"), true)
	ctx, cancel := context.WithCancel(context.Background())
	waitDone := startDaemon(t, d, ctx)

	waitFor(t, out, "Task 执行失败")
	waitFor(t, out, "当天已有终态，跳过今日自动执行")
	waitFor(t, out, "2025-09-07")
	cancel()
	if err := waitDone(); err != nil {
		t.Errorf("取消后应返回 nil，got %v", err)
	}
	if n := runner.count(); n != 1 {
		t.Errorf("failed 终态后当天不应重跑，实际调用 %d 次", n)
	}
	// 终态为 failed（Task 写入），daemon 不改写。
	st, has, err := terminal.NewFileStore(cfg.DataDir).Load()
	if err != nil || !has {
		t.Fatalf("终态应存在: has=%v err=%v", has, err)
	}
	if st.LastTaskResult != terminal.ResultFailed || st.LastTaskDate != "2025-09-06" {
		t.Errorf("终态 = %+v，期望 2025-09-06/failed", st)
	}
}

// TestDaemonStopsOnCancel：取消（SIGINT/SIGTERM）时 daemon 在睡眠中干净退出，
// 且不执行 Task（取消延迟 ≤ sleepChunk + 轻微等待）。
func TestDaemonStopsOnCancel(t *testing.T) {
	cfg := daemonConfig(t, func(c *config.Config) {
		c.WindowStart = 60
		c.WindowEnd = 180
	})
	d, out, runner := newDaemon(cfg, at(2025, 9, 6, 4, 0), at(2025, 9, 7, 0, 30), nil, nil, false)
	ctx, cancel := context.WithCancel(context.Background())
	waitDone := startDaemon(t, d, ctx)

	waitFor(t, out, "daemon 启动")
	waitFor(t, out, "已排定下次 Task 启动")
	cancel()
	if err := waitDone(); err != nil {
		t.Errorf("取消后应返回 nil，got %v", err)
	}
	if got := out.String(); !strings.Contains(got, "daemon 退出") {
		t.Errorf("缺少退出日志:\n%s", got)
	}
	if n := runner.count(); n != 0 {
		t.Errorf("睡眠中取消不应执行 Task，实际 %d 次", n)
	}
	if got := out.String(); !strings.Contains(got, "01:00-03:00") {
		t.Errorf("启动日志应含窗口描述:\n%s", got)
	}
}

// TestDaemonStartupFailsOnEmptySessionFile：持久化 Login Session 为空（损坏）→
// daemon 启动失败（恢复 Login Session 是循环第 1 步）。
func TestDaemonStartupFailsOnEmptySessionFile(t *testing.T) {
	cfg := daemonConfig(t, func(c *config.Config) {
		c.Cookie = "" // 也无初始 Cookie：唯一凭据是损坏的空会话文件
	})
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "login_session.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out syncBuffer
	d := New(cfg, Deps{
		Clock:  clock.NewFake(at(2025, 9, 6, 4, 0)),
		RNG:    rand.New(rand.NewSource(7)),
		Task:   &recordingRunner{},
		Logger: slog.New(slog.NewTextHandler(&out, nil)),
	})
	err := d.Run(context.Background())
	if err == nil {
		t.Fatal("空 Login Session 下 daemon 应启动失败")
	}
	if !strings.Contains(err.Error(), "Login Session") {
		t.Errorf("错误应指明 Login Session: %v", err)
	}
}

// TestDaemonStartupFailsWithoutTask：Task 执行器未装配（Deps 缺失）→ 启动即报错。
func TestDaemonStartupFailsWithoutTask(t *testing.T) {
	cfg := daemonConfig(t, nil)
	var out syncBuffer
	d := New(cfg, Deps{
		Clock:  clock.NewFake(at(2025, 9, 6, 4, 0)),
		RNG:    rand.New(rand.NewSource(7)),
		Logger: slog.New(slog.NewTextHandler(&out, nil)),
	})
	if err := d.Run(context.Background()); err == nil {
		t.Fatal("Task 未装配时 daemon 应启动失败")
	}
}
