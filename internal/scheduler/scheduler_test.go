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

// recordingRunner 记录被调用次数（测试注入 fake TaskRunner）。语义与真实 Task 一致
// （ticket 24）：成功（err == nil）时写入当天 success 终态（spec 决策 #9：终态落盘
// 是 Task 的职责）；err != nil 且非 ErrTaskRunning 时写入当天 failed 终态（一切最终
// 失败都无条件收敛为 failed 终态 + 相应通知，调度层不再整 Task 重排）。
// ErrTaskRunning（并发拒绝）不是失败：Task 未运行，当天结果由运行中的 Task 负责。
type recordingRunner struct {
	mu    sync.Mutex
	calls int
	err   error
	term  terminal.Store
	tz    *time.Location
	clk   clock.Clock
}

func (r *recordingRunner) RunTask(ctx context.Context) (task.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.err != nil {
		if !errors.Is(r.err, task.ErrTaskRunning) {
			r.saveFailed()
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

// saveFailed 写入当天 failed 终态（tick：与真实 Task 的失败收敛出口同一落盘形态）。
func (r *recordingRunner) saveFailed() {
	if r.term == nil {
		return
	}
	_ = r.term.Save(terminal.State{
		LastTaskDate:   r.clk.Now().In(r.tz).Format(terminal.DayLayout),
		LastTaskResult: terminal.ResultFailed,
	})
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
func newDaemon(cfg *config.Config, start, cap time.Time, hook func(time.Time), err error) (*Daemon, *syncBuffer, *recordingRunner) {
	var out syncBuffer
	clk := clock.NewCapped(start, cap, hook)
	runner := &recordingRunner{err: err, term: terminal.NewFileStore(cfg.DataDir), tz: cfg.TZ, clk: clk}
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
	d, out, runner := newDaemon(cfg, at(2025, 9, 6, 23, 35), at(2025, 9, 7, 23, 30), nil, nil)
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
	d, out, runner := newDaemon(cfg, at(2025, 9, 6, 4, 0), at(2025, 9, 7, 0, 30), nil, nil)
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
	d, out, runner := newDaemon(cfg, at(2025, 9, 6, 23, 30), at(2025, 9, 6, 23, 40), hook, nil)
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
	d, out, runner := newDaemon(cfg, at(2025, 9, 6, 23, 35), at(2025, 9, 6, 23, 59), nil, task.ErrTaskRunning)
	ctx, cancel := context.WithCancel(context.Background())
	waitDone := startDaemon(t, d, ctx)

	waitFor(t, out, "另一进程正在运行 Task，本次自动执行被拒绝（最短间隔后重试）")
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

// TestDaemonSchedulesNextDayAfterTransientFailure：Task 暂时性失败（ticket 24：任何
// 失败都无条件收敛为 failed 终态）→ 当天不再自动重跑，排定次日（无 whole-Task 自动
// 重排：窗口内不再启动第二个完整 Task）。启动（23:30）已在窗口内且无终态 → 立即
// 执行；失败形成 failed 终态 → 排定次日。
func TestDaemonSchedulesNextDayAfterTransientFailure(t *testing.T) {
	cfg := daemonConfig(t, nil) // 窗口 [23:30, 23:59]
	d, out, runner := newDaemon(cfg, at(2025, 9, 6, 23, 30), at(2025, 9, 7, 0, 30), nil, errors.New("网络暂时不可用"))
	ctx, cancel := context.WithCancel(context.Background())
	waitDone := startDaemon(t, d, ctx)

	waitFor(t, out, "Task 执行失败")
	// 失败已形成 failed 终态 → 不再自动重跑，排定次日（含次日日期）。
	waitFor(t, out, "Task 失败后下次启动排定于次日")
	waitFor(t, out, "2025-09-07")
	cancel()
	if err := waitDone(); err != nil {
		t.Errorf("取消后应返回 nil，got %v", err)
	}
	// 无 whole-Task 重排：窗口内只执行一次（失败即为最终结果），不重跑第二个 Task。
	if n := runner.count(); n != 1 {
		t.Errorf("窗口内应只执行一次（失败后排次日），实际调用 %d 次", n)
	}
	// 暂时性失败同样形成 failed 终态（Task 收敛；daemon 不改写）。
	st, has, err := terminal.NewFileStore(cfg.DataDir).Load()
	if err != nil || !has {
		t.Fatalf("终态应存在: has=%v err=%v", has, err)
	}
	if st.LastTaskResult != terminal.ResultFailed || st.LastTaskDate != "2025-09-06" {
		t.Errorf("终态 = %+v，期望 2025-09-06/failed", st)
	}
}

// noTerminalRunner 模拟终态持久化失败等存储级错误的 Task：失败且不写终态
// （ticket 24：存储级错误返回无终态）。用于断言 daemon 不借"窗口内立即执行"再次
// 启动当天第二个完整 Task。
type noTerminalRunner struct {
	calls int
	err   error
}

func (r *noTerminalRunner) RunTask(ctx context.Context) (task.Result, error) {
	r.calls++
	return task.Result{}, r.err
}

// TestDaemonNoSecondTaskWhenFailureLeavesNoTerminal 断言无终态的 Task 失败（终态持久
// 化失败等存储级错误）同样不得在窗口内启动第二个完整 Task（ticket 24：无 whole-Task
// 重排；"窗口内立即执行"只适用于进程启动/重启的异常恢复，失败后直接排定次日）。
// 假时钟钉在次日窗口前（00:30）：若 daemon 误把失败当重启而立即重试，窗口 [23:30,
// 23:59] 期间会再次调用 RunTask——调用次数必须仍为 1。
func TestDaemonNoSecondTaskWhenFailureLeavesNoTerminal(t *testing.T) {
	cfg := daemonConfig(t, nil) // 窗口 [23:30, 23:59]
	clk := clock.NewCapped(at(2025, 9, 6, 23, 30), at(2025, 9, 7, 0, 30), nil)
	runner := &noTerminalRunner{err: errors.New("写入 Terminal State 失败: 磁盘故障")}
	var out syncBuffer
	d := New(cfg, Deps{
		Clock:  clk,
		RNG:    rand.New(rand.NewSource(7)),
		Task:   runner,
		Logger: slog.New(slog.NewTextHandler(&out, nil)),
	})
	ctx, cancel := context.WithCancel(context.Background())
	waitDone := startDaemon(t, d, ctx)

	waitFor(t, &out, "Task 执行失败")
	// 失败后直接排定次日（不得于当天窗口内再次启动第二个 Task）。
	waitFor(t, &out, "Task 失败后下次启动排定于次日")
	waitFor(t, &out, "2025-09-07")
	cancel()
	if err := waitDone(); err != nil {
		t.Errorf("取消后应返回 nil，got %v", err)
	}
	if n := runner.calls; n != 1 {
		t.Errorf("无终态失败后当天不得启动第二个完整 Task，实际执行 %d 次", n)
	}
	// 存储级失败不留终态（daemon 不伪造）。
	if _, has, err := terminal.NewFileStore(cfg.DataDir).Load(); err != nil || has {
		t.Fatalf("存储级失败不应有终态: has=%v err=%v", has, err)
	}
}

// TestDaemonStartsImmediatelyInsideWindowAtStartup：异常启动/重启（ticket 24；
// 用户故事 #14/#17）——假时钟 23:40 已在窗口 [23:30, 23:59] 内且当天无终态 → daemon
// 立即执行 Task（不再从 [now, 结束] 随机），成功形成 success 终态后排定次日。
// "已排定下次 Task 启动"的 at 恰为启动时刻 23:40（而非窗口内随机点）。
func TestDaemonStartsImmediatelyInsideWindowAtStartup(t *testing.T) {
	cfg := daemonConfig(t, nil) // 窗口 [23:30, 23:59]
	d, out, runner := newDaemon(cfg, at(2025, 9, 6, 23, 40), at(2025, 9, 7, 0, 30), nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	waitDone := startDaemon(t, d, ctx)

	waitFor(t, out, "已到启动时刻，执行 Task")
	waitFor(t, out, "Task 完成，排定次日")
	waitFor(t, out, "2025-09-07")
	// 首次排定的启动时刻恰为启动时刻（23:40:00）：立即执行，而非窗口内随机点。
	waitFor(t, out, "23:40:00")
	cancel()
	if err := waitDone(); err != nil {
		t.Errorf("取消后应返回 nil，got %v", err)
	}
	if n := runner.count(); n != 1 {
		t.Errorf("异常启动窗口内应立即执行一次，实际调用 %d 次", n)
	}
	// success 终态已落盘（Task 执行成功；daemon 不改写）。
	st, has, err := terminal.NewFileStore(cfg.DataDir).Load()
	if err != nil || !has {
		t.Fatalf("终态应存在: has=%v err=%v", has, err)
	}
	if st.LastTaskResult != terminal.ResultSuccess || st.LastTaskDate != "2025-09-06" {
		t.Errorf("终态 = %+v，期望 2025-09-06/success", st)
	}
}

// TestDaemonSchedulesNextDayAfterFailedTerminal：Task 形成 failed 终态（恢复链耗尽）→
// 当天不再自动重跑，排定次日（ADR-0002：success 与 failed 都阻止当天再次自动执行）。
func TestDaemonSchedulesNextDayAfterFailedTerminal(t *testing.T) {
	cfg := daemonConfig(t, nil) // 窗口 [23:30, 23:59]
	d, out, runner := newDaemon(cfg, at(2025, 9, 6, 23, 35), at(2025, 9, 7, 0, 30), nil, errors.New("恢复链耗尽"))
	ctx, cancel := context.WithCancel(context.Background())
	waitDone := startDaemon(t, d, ctx)

	waitFor(t, out, "Task 执行失败")
	// 已写 failed 终态 → 不再自动重跑，排定次日（含次日日期）。
	waitFor(t, out, "Task 失败后下次启动排定于次日")
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
	d, out, runner := newDaemon(cfg, at(2025, 9, 6, 4, 0), at(2025, 9, 7, 0, 30), nil, nil)
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
