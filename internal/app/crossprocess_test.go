// ticket 11 跨进程 Task 互斥的进程级测试：用"测试二进制自启动子进程"模式验证
// 真实 App 在独立 OS 进程间的互斥（ADR-0006 主 seam 之外、唯一能覆盖跨进程边界
// 的形态）。
//
// 每个子进程是独立 OS 进程 + 独立 App 实例 + 独立 runMu——与生产 daemon/run 的
// 进程形态一致：CLI 每次命令调用都新建 App，daemon 与 `weread-cron run` 是两个
// 操作系统进程，互斥只能由 /data 锁文件（flock）承担。
//
// 子进程不经过 CLI 层（URL 注入是仅测试用途的生产面，ADR-0006 明确不引入）；
// 被测面是 App 的跨进程互斥本身：同一 /data 共享 flock，不同 /data 互不干扰。
//
// 怎么读：子进程注入 fake clock（Sleep 自动推进），整段 Task 在毫秒级完成，
// 仅当父进程在 fake 服务端挂起首笔 timed report 时才停留在运行中（锁必然持有、
// "Task 运行中"状态确定）。
package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"weread-cron/internal/clock"
	"weread-cron/internal/config"
	"weread-cron/internal/filelock"
	"weread-cron/internal/terminal"
)

// testCookie 是子进程的初始 Cookie（与 setup 的默认 Cookie 一致）。
const testCookie = "wr_skey=abc; wr_gid=123"

// TestHelperProcess 是子进程入口（Go 标准"测试二进制自启动"模式）：环境变量
// WR_TEST_HELPER=1 时执行子进程动作并 os.Exit（真实退出码），否则为空测试
// （父进程侧正常收集）。子进程动作（WR_TEST_MODE）：
//
//   - run：装配真实 App 后 RunTask；Books：ListBooks。成功 → exit 0；
//     ErrTaskRunning（并发守卫拒绝）→ exit 3（映射 CLI 的 ExitRunRejected 口径）；
//     其他错误 → exit 1。
//
// 子进程配置：fake clock（2025-09-06 10:00 CST，Sleep 自动推进）、种子 RNG、
// Cookie = 初始 Cookie（testCookie）、候选书 = testBookID、目标 1 分钟、无通知
// 渠道（不发通知）；DataDir 与服务端基址经 WR_TEST_* 注入。
func TestHelperProcess(t *testing.T) {
	if os.Getenv("WR_TEST_HELPER") != "1" {
		return
	}
	os.Exit(helperProcessMain())
}

// helperProcessMain 是子进程主体（返回退出码）。
func helperProcessMain() int {
	cfg := &config.Config{
		Cookie:         testCookie,
		Books:          []string{testBookID},
		ReadMinutesMin: 1,
		ReadMinutesMax: 1,
		DataDir:        os.Getenv("WR_TEST_DATA_DIR"),
		TZ:             testTZ,
	}
	a := New(cfg, Deps{
		Clock:         clock.NewFake(time.Date(2025, 9, 6, 10, 0, 0, 0, testTZ)),
		RNG:           rand.New(rand.NewSource(42)),
		WereadBaseURL: os.Getenv("WR_TEST_BASE_URL"),
		Logger:        slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	ctx := context.Background()
	var err error
	switch os.Getenv("WR_TEST_MODE") {
	case "run":
		_, err = a.RunTask(ctx)
	case "books":
		_, err = a.ListBooks(ctx)
	default:
		fmt.Fprintln(os.Stderr, "WR_TEST_MODE 非法")
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrTaskRunning):
		// 并发守卫拒绝：对应 CLI 的 ExitRunRejected（ticket 08 约定口径）。
		fmt.Fprintln(os.Stderr, "拒绝：已有 Task 正在运行")
		return 3
	default:
		fmt.Fprintf(os.Stderr, "失败: %v\n", err)
		return 1
	}
}

// spawnChild 启动一个 helper 子进程（同一测试二进制；WR_TEST_* 环境注入）。
// 返回的 Cmd 已 Start；由调用方负责 Wait（或 Kill 后 Wait）。
func spawnChild(t *testing.T, mode, baseURL, dataDir string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(),
		"WR_TEST_HELPER=1",
		"WR_TEST_MODE="+mode,
		"WR_TEST_BASE_URL="+baseURL,
		"WR_TEST_DATA_DIR="+dataDir,
	)
	cmd.Stdout = &bytes.Buffer{}
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动子进程失败: %v", err)
	}
	return cmd
}

// waitChild 等待子进程退出并断言退出码；失败消息附带子进程 stderr。
// 注意：非零退出码下 cmd.Wait 返回非 nil 错误（exit status N）是正常形态，
// 退出码断言以 ProcessState.ExitCode 为准。
func waitChild(t *testing.T, cmd *exec.Cmd, wantCode int) {
	t.Helper()
	err := cmd.Wait()
	stderr := cmd.Stderr.(*bytes.Buffer).String()
	code := cmd.ProcessState.ExitCode()
	if wantCode == 0 && err != nil {
		t.Fatalf("子进程退出 = %v（期望 0）；stderr:\n%s", err, stderr)
	}
	if code != wantCode {
		t.Fatalf("子进程退出码 = %d，期望 %d；stderr:\n%s", code, wantCode, stderr)
	}
}

// countPathDelta 返回 since 之后新增的、以 prefix 开头的请求数。
func (h *testHarness) countPathDelta(since int, prefix string) int {
	n := 0
	for _, r := range h.weread.snapshot()[since:] {
		if len(r.Path) >= len(prefix) && r.Path[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

// waitTimedBlocked 等待子进程阻塞在首笔 timed report（锁必然已持有、Task 运行中）。
func waitTimedBlocked(t *testing.T, h *testHarness, triggers int) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for i := 0; i < triggers; i++ {
		select {
		case <-h.weread.blockTimedTriggered:
		case <-deadline:
			t.Fatalf("等待 %d 个 timed report 到达超时（已到 %d 个）", triggers, i)
		}
	}
}

// hanger 是测试注入的 timed report 挂起器：channel 关闭前，每笔 timed report 在
// fake 服务端等待；release 关闭 channel 释放全部等待（幂等；t.Cleanup 兜底——
// 失败路径也释放，避免 httptest Close 悬挂）。设好 h.weread.blockTimed 后，Task
// 的首笔 timed report 到达即被挂起（锁必然已持有、Task 运行中状态确定）。
type hanger struct {
	once      sync.Once
	releaseFn func()
}

func newHanger(h *testHarness, triggers int) *hanger {
	blockTimed := make(chan struct{})
	h.weread.blockTimed = blockTimed
	h.weread.blockTimedTriggered = make(chan struct{}, triggers)
	c := &hanger{}
	c.releaseFn = func() { close(blockTimed) }
	return c
}

// release 释放挂起的 timed report（幂等）。
func (c *hanger) release() {
	c.once.Do(c.releaseFn)
}

// TestCrossProcessRejectsSecondTaskAndBooks：验收 1/2/6 + 正常退出锁释放。
// daemon（或 run）进程的 Task 运行中：另一进程 `run`（父测试进程 = 独立 OS
// 进程、独立 App）被非阻塞拒绝（ErrTaskRunning、零网络请求、不中断运行中的
// Task）；books 子进程同样被拒（共用守卫）；子进程正常退出后锁自动释放——
// 父进程 RunTask 不再被并发守卫拒绝（落入终态规则 ErrTerminalSuccess，只有
// 拿到锁才会走到终态门控），books 子进程恢复可用且不受终态影响。
func TestCrossProcessRejectsSecondTaskAndBooks(t *testing.T) {
	h := setup(t, nil)
	hang := newHanger(h, 1)
	t.Cleanup(hang.release)

	// 进程 1：真实 App 执行 run，阻塞在首笔 timed report（锁持有、Task 运行中）。
	ch1 := spawnChild(t, "run", h.app.deps.WereadBaseURL, h.cfg.DataDir)
	defer func() {
		if ch1.ProcessState == nil {
			ch1.Process.Kill()
			ch1.Wait()
		}
	}()
	waitTimedBlocked(t, h, 1)

	// 进程 2（父测试进程）：同一 /data、独立 App → 非阻塞拒绝。
	before := len(h.weread.snapshot())
	_, err := h.app.RunTask(context.Background())
	if !errors.Is(err, ErrTaskRunning) {
		t.Errorf("跨进程并发 RunTask = %v，期望 ErrTaskRunning", err)
	}
	// 拒绝路径：零网络请求、不中断运行中的 Task（被拒的是父进程自己的调用；
	// 子进程仍存活且停在首笔 timed report）。
	if got := len(h.weread.snapshot()) - before; got != 0 {
		t.Errorf("拒绝路径产生 %d 个新请求，期望 0（非阻塞拒绝不发起任何请求）", got)
	}
	if ch1.ProcessState != nil {
		t.Fatal("被拒绝的是父进程调用；子进程的 Task 不应被中断")
	}

	// 进程 3：books 子进程同样被拒（Task/books 共用守卫，避免会话写竞争）。
	ch2 := spawnChild(t, "books", h.app.deps.WereadBaseURL, h.cfg.DataDir)
	waitChild(t, ch2, 3) // 拒绝退出码（ticket 08 口径）

	// 释放进程 1：正常退出（exit 0）→ 终态落盘 → 锁随进程退出自动释放。
	hang.release()
	waitChild(t, ch1, 0)
	st, has, err := terminal.NewFileStore(h.cfg.DataDir).Load()
	if err != nil || !has {
		t.Fatalf("子进程 Task 完成应有终态: has=%v err=%v", has, err)
	}
	if st.LastTaskResult != terminal.ResultSuccess {
		t.Errorf("终态 = %+v，期望 success", st)
	}

	// 锁已释放（正常退出路径）：父进程 RunTask 不再返回 ErrTaskRunning——走到
	// 终态门控被 ErrTerminalSuccess 拒绝，严格证明锁已随子进程退出自动释放。
	_, err = h.app.RunTask(context.Background())
	if !errors.Is(err, ErrTerminalSuccess) {
		t.Errorf("子进程退出后 RunTask = %v，期望 ErrTerminalSuccess（若锁未释放则为 ErrTaskRunning）", err)
	}

	// books 子进程在锁释放后恢复可用（终态规则不约束 books；纯查询走通）。
	ch3 := spawnChild(t, "books", h.app.deps.WereadBaseURL, h.cfg.DataDir)
	waitChild(t, ch3, 0)
}

// TestCrossProcessCrashReleasesLock：验收 3（崩溃释放）。Task 运行中子进程被
// SIGKILL（模拟崩溃，无任何清理机会）→ flock 由内核自动释放 → 同一 /data 的
// 父进程可立即完整执行 Task（无终态时不受终态门控影响）：success 终态落盘、
// 完整请求时序（renewal + reader + enter + 2×timed）。
func TestCrossProcessCrashReleasesLock(t *testing.T) {
	h := setup(t, nil)
	hang := newHanger(h, 1)
	t.Cleanup(hang.release)

	ch := spawnChild(t, "run", h.app.deps.WereadBaseURL, h.cfg.DataDir)
	waitTimedBlocked(t, h, 1)

	// 崩溃前锁确实被持有（父进程被非阻塞拒绝）。
	if _, err := h.app.RunTask(context.Background()); !errors.Is(err, ErrTaskRunning) {
		t.Errorf("崩溃前并发 RunTask = %v，期望 ErrTaskRunning", err)
	}

	// SIGKILL（内核直接释放 flock：无 defer、无清理）。
	if err := ch.Process.Kill(); err != nil {
		t.Fatalf("Kill 失败: %v", err)
	}
	if err := ch.Wait(); err == nil {
		t.Fatal("子进程应被 SIGKILL 终止")
	}
	// 释放 fake 服务端挂起的 handler；后续请求不再挂起。
	hang.release()
	h.weread.blockTimed = nil

	// 锁已自动释放：父进程完整执行 Task（无终态 → 不落入终态门控）。
	before := len(h.weread.snapshot())
	res, err := h.app.RunTask(context.Background())
	if err != nil {
		t.Fatalf("崩溃后 RunTask 失败: %v", err)
	}
	if res.Reports != 2 {
		t.Errorf("Reports = %d，期望 2", res.Reports)
	}
	if got := h.countPathDelta(before, "/web/book/read"); got != 3 {
		t.Errorf("崩溃后 Task 的 report 请求 = %d，期望 3（enter + 2×timed）", got)
	}
	if got := h.countPathDelta(before, "/web/login/renewal"); got != 1 {
		t.Errorf("崩溃后 Task 的 renewal = %d，期望 1", got)
	}
	st, has, err := terminal.NewFileStore(h.cfg.DataDir).Load()
	if err != nil || !has {
		t.Fatalf("崩溃后 Task 应有 success 终态: has=%v err=%v", has, err)
	}
	if st.LastTaskResult != terminal.ResultSuccess {
		t.Errorf("终态 = %+v，期望 success", st)
	}
}

// TestCrossProcessDifferentDataDirsIndependent：验收 5（范围边界）。两个子进程
// 在不同 /data（不同 deployment）下同时运行 Task——双双阻塞在首笔 timed report
// （同时运行中）→ 同时释放后双双成功退出、各自 /data 各落 success 终态：
// 互斥按 /data 隔离，不同 deployment 互不干扰。
func TestCrossProcessDifferentDataDirsIndependent(t *testing.T) {
	h := setup(t, nil)
	hang := newHanger(h, 2) // 两个子进程各触发一次
	t.Cleanup(hang.release)

	dirA := h.cfg.DataDir
	dirB := t.TempDir() // 另一 deployment（不同 /data）

	chA := spawnChild(t, "run", h.app.deps.WereadBaseURL, dirA)
	chB := spawnChild(t, "run", h.app.deps.WereadBaseURL, dirB)

	// 两个子进程都阻塞在首笔 timed report：同一时刻两个 Task 同时运行中——
	// 若互斥错误地按全局共享（而非 /data）实现，第二个子进程会在此被拒。
	waitTimedBlocked(t, h, 2)

	hang.release()
	waitChild(t, chA, 0)
	waitChild(t, chB, 0)

	// 各自 deployment 的终态互不干扰。
	for name, dir := range map[string]string{"A": dirA, "B": dirB} {
		st, has, err := terminal.NewFileStore(dir).Load()
		if err != nil || !has {
			t.Fatalf("deployment %s 应有 success 终态: has=%v err=%v", name, has, err)
		}
		if st.LastTaskResult != terminal.ResultSuccess {
			t.Errorf("deployment %s 终态 = %+v，期望 success", name, st)
		}
	}
	// 锁文件各自落位（范围边界在文件系统层面可观察）。
	for name, dir := range map[string]string{"A": dirA, "B": dirB} {
		if _, err := os.Stat(filepath.Join(dir, filelock.LockFileName)); err != nil {
			t.Errorf("deployment %s 应有锁文件: %v", name, err)
		}
	}
}
