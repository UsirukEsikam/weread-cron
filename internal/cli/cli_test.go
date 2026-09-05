package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"weread-cron/internal/app"
	"weread-cron/internal/config"
	"weread-cron/internal/task"
	"weread-cron/internal/weread"
)

// syncBuffer 是并发安全的输出缓冲，供 daemon 阻塞路径测试读取启动标记。
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

// testEnv 返回一份配置合法的环境变量（DataDir = 测试临时目录，不含 Cookie）。
// overrides 形如 "KEY=value"，替换或追加。
func testEnv(t *testing.T, overrides ...string) []string {
	t.Helper()
	env := []string{
		config.EnvWindowStart + "=01:00",
		config.EnvWindowEnd + "=03:00",
		config.EnvReadMin + "=40",
		config.EnvReadMax + "=70",
		config.EnvDataDir + "=" + t.TempDir(),
		"TZ=Asia/Shanghai",
	}
	for _, o := range overrides {
		key := o[:strings.IndexByte(o, '=')]
		replaced := false
		for i, kv := range env {
			if kv[:strings.IndexByte(kv, '=')] == key {
				env[i] = o
				replaced = true
			}
		}
		if !replaced {
			env = append(env, o)
		}
	}
	return env
}

func withCookie(t *testing.T) string {
	t.Helper()
	return config.EnvCookie + "=wr_skey=abc; wr_gid=123"
}

// writeLoginSessionFile 写入一个可恢复的 Login Session 文件（含一个 Cookie）。
func writeLoginSessionFile(t *testing.T, dir string) {
	t.Helper()
	data := `{"cookies":[{"name":"wr_skey","value":"abc"}]}`
	if err := os.WriteFile(filepath.Join(dir, "login_session.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUsageErrors(t *testing.T) {
	env := testEnv(t)
	cases := []struct {
		name string
		args []string
		want string // stderr 必须包含
	}{
		{"--help", []string{"--help"}, "用法"},
		{"-h", []string{"-h"}, "用法"},
		{"run --help", []string{"run", "--help"}, "用法"},
		{"books --help", []string{"books", "-h"}, "用法"},
		{"未知子命令", []string{"frobnicate"}, "未知子命令"},
		{"未知 flag", []string{"--verbose"}, "未知 flag"},
		{"run 多余参数", []string{"run", "extra"}, "不接受额外参数"},
		{"books 多余参数", []string{"books", "x", "y"}, "不接受额外参数"},
		{"run --force（无 force 选项）", []string{"run", "--force"}, "不接受额外参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr syncBuffer
			code := Run(context.Background(), tc.args, env, &stdout, &stderr)
			if code != ExitUsage {
				t.Errorf("退出码 = %d，期望 %d", code, ExitUsage)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr 缺少 %q；实际:\n%s", tc.want, stderr.String())
			}
		})
	}
}

func TestConfigValidationFailsStartup(t *testing.T) {
	cases := []struct {
		name      string
		overrides []string
		wantErr   string
	}{
		{
			name:      "窗口 start 晚于 end",
			overrides: []string{config.EnvWindowStart + "=03:00", config.EnvWindowEnd + "=01:00"},
			wantErr:   "运行窗口",
		},
		{
			name:      "时间格式非法",
			overrides: []string{config.EnvWindowStart + "=25:00"},
			wantErr:   "HH:MM",
		},
		{
			name:      "时长 min 大于 max",
			overrides: []string{config.EnvReadMin + "=90", config.EnvReadMax + "=45"},
			wantErr:   "大于",
		},
		{
			name:      "时区非法",
			overrides: []string{config.EnvTZ + "=Foo/Bar"},
			wantErr:   "无法识别的时区",
		},
		{
			name:      "窗口变量缺失",
			overrides: []string{config.EnvWindowEnd + "="}, // 置空覆盖测试：改为删除
			wantErr:   "未设置",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t, tc.overrides...)
			if tc.name == "窗口变量缺失" {
				// 显式删除 RUN_WINDOW_END
				filtered := env[:0]
				for _, kv := range env {
					if kv[:strings.IndexByte(kv, '=')] != config.EnvWindowEnd {
						filtered = append(filtered, kv)
					}
				}
				env = filtered
			}
			var stdout, stderr syncBuffer
			code := Run(context.Background(), nil, env, &stdout, &stderr)
			if code != ExitConfig {
				t.Errorf("退出码 = %d，期望 %d", code, ExitConfig)
			}
			if !strings.Contains(stderr.String(), tc.wantErr) {
				t.Errorf("stderr 缺少 %q；实际:\n%s", tc.wantErr, stderr.String())
			}
		})
	}
}

func TestMissingCookieFastFail(t *testing.T) {
	var stdout, stderr syncBuffer
	code := Run(context.Background(), nil, testEnv(t), &stdout, &stderr)
	if code != ExitConfig {
		t.Errorf("退出码 = %d，期望 %d", code, ExitConfig)
	}
	if !strings.Contains(stderr.String(), config.EnvCookie) {
		t.Errorf("stderr 应指明 WEREAD_CRON_COOKIE；实际:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "Login Session") {
		t.Errorf("stderr 应说明 Login Session 恢复；实际:\n%s", stderr.String())
	}
}

// TestDaemonStartupFailsOnEmptySessionFile：持久化 Login Session 为空（损坏）→
// daemon 启动失败（ticket 07：恢复 Login Session 是调度循环第 1 步；exit code + stderr）。
func TestDaemonStartupFailsOnEmptySessionFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "login_session.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := testEnv(t, withCookie(t), config.EnvDataDir+"="+dir)
	var stdout, stderr syncBuffer
	code := Run(context.Background(), nil, env, &stdout, &stderr)
	if code != ExitConfig {
		t.Errorf("退出码 = %d，期望 %d", code, ExitConfig)
	}
	if !strings.Contains(stderr.String(), "恢复 Login Session") {
		t.Errorf("stderr 应指明 Login Session 恢复失败；实际:\n%s", stderr.String())
	}
}

// TestDaemonStartsWithPersistedLoginSessionAndNoCookie：有可恢复的持久化 Login
// Session（无需初始 Cookie）→ daemon 正常启动，取消后以退出码 0 结束。
func TestDaemonStartsWithPersistedLoginSessionAndNoCookie(t *testing.T) {
	dir := t.TempDir()
	writeLoginSessionFile(t, dir)
	env := testEnv(t, config.EnvDataDir+"="+dir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- Run(ctx, nil, env, &stdout, &stderr)
	}()

	// 等待 daemon 占位路径进入阻塞（出现启动日志），再取消。
	waitFor(t, &stderr, "daemon 启动")
	cancel()

	select {
	case code := <-done:
		if code != ExitOK {
			t.Errorf("退出码 = %d，期望 %d", code, ExitOK)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon 未在取消后退出")
	}
}

// TestRunWithoutBooksNoLongerFailsFast：ticket 09 起未配置候选书是合法输入
// （自动从 Shelf 选书），不再于 CLI 层快速失败；自动选书行为（含全部候选不可用 →
// Task 失败）由应用 seam 覆盖（internal/app/autoselect_test.go）。
func TestRunWithoutBooksNoLongerFailsFast(t *testing.T) {
	env := testEnv(t, withCookie(t))
	var stdout, stderr syncBuffer
	code := runWithApp(context.Background(), []string{"run"}, env, &stdout, &stderr,
		func(cfg *config.Config, logger *slog.Logger) (App, error) {
			return &fakeApp{res: task.Result{Date: "2025-09-06", BookID: "601111", BookTitle: "自动选书书", Planned: time.Minute, Actual: time.Minute, Reports: 2}}, nil
		})
	if code != ExitOK {
		t.Errorf("未配置候选书不再失败（自动选书），退出码 = %d；stderr=%s", code, stderr.String())
	}
}

// --- ticket 09：books 子命令（进程级呈现；应用层行为见 internal/app/books_test.go）---

func TestBooksPrintsBookIdAndTitle(t *testing.T) {
	env := testEnv(t, withCookie(t))
	var stdout, stderr syncBuffer
	code := runWithApp(context.Background(), []string{"books"}, env, &stdout, &stderr,
		func(cfg *config.Config, logger *slog.Logger) (App, error) {
			return &fakeApp{books: []weread.ShelfBook{
				{BookID: "695233", Title: "三体全集"},
				{BookID: "888888", Title: "活着"},
			}}, nil
		})
	if code != ExitOK {
		t.Errorf("退出码 = %d，期望 %d; stderr=%s", code, ExitOK, stderr.String())
	}
	want := "695233\t三体全集\n888888\t活着\n"
	if stdout.String() != want {
		t.Errorf("stdout = %q，期望每行 bookId\ttitle\n%s", stdout.String(), want)
	}
	if stderr.String() != "" {
		t.Errorf("成功时 stderr 应为空；实际:\n%s", stderr.String())
	}
}

// TestBooksSessionInvalidPromptsCookieUpdate：books 会话失效（renewal 明确拒绝）→
// 报错并提示更新初始 Cookie（issue 验收：不静默），退出码 ExitConfig。
func TestBooksSessionInvalidPromptsCookieUpdate(t *testing.T) {
	env := testEnv(t, withCookie(t))
	var stdout, stderr syncBuffer
	code := runWithApp(context.Background(), []string{"books"}, env, &stdout, &stderr,
		func(cfg *config.Config, logger *slog.Logger) (App, error) {
			return &fakeApp{booksErr: fmt.Errorf("%w: 从初始 Cookie 重建 Login Session 后 renewal 仍失败", weread.ErrLoginInvalid)}, nil
		})
	if code != ExitConfig {
		t.Errorf("退出码 = %d，期望 %d（会话失效应失败）", code, ExitConfig)
	}
	for _, want := range []string{"登录已失效", "请更新初始 Cookie", config.EnvCookie} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr 缺少 %q；实际:\n%s", want, stderr.String())
		}
	}
	if stdout.String() != "" {
		t.Errorf("失败时不应输出书架；实际:\n%s", stdout.String())
	}
}

// TestBooksQueryFailureExitsNonZero：其他查询失败（暂时性/服务端错误）→ 非零退出 + stderr 原因。
func TestBooksQueryFailureExitsNonZero(t *testing.T) {
	env := testEnv(t, withCookie(t))
	var stdout, stderr syncBuffer
	code := runWithApp(context.Background(), []string{"books"}, env, &stdout, &stderr,
		func(cfg *config.Config, logger *slog.Logger) (App, error) {
			return &fakeApp{booksErr: errors.New("抓取 Shelf 失败: HTTP 500")}, nil
		})
	if code != ExitConfig {
		t.Errorf("退出码 = %d，期望 %d", code, ExitConfig)
	}
	if !strings.Contains(stderr.String(), "books 查询失败") {
		t.Errorf("stderr 应说明查询失败；实际:\n%s", stderr.String())
	}
	if strings.Contains(stderr.String(), "登录已失效") {
		t.Errorf("普通失败不得套登录失效文案；实际:\n%s", stderr.String())
	}
}

// TestBooksRejectedWhenTaskRunning：已有 Task 运行中 → books 被拒绝（并发守卫与
// Task 共用：避免会话写竞争），退出码 ExitRunRejected + stderr 说明。
func TestBooksRejectedWhenTaskRunning(t *testing.T) {
	env := testEnv(t, withCookie(t))
	var stdout, stderr syncBuffer
	code := runWithApp(context.Background(), []string{"books"}, env, &stdout, &stderr,
		func(cfg *config.Config, logger *slog.Logger) (App, error) {
			return &fakeApp{booksErr: app.ErrTaskRunning}, nil
		})
	if code != ExitRunRejected {
		t.Errorf("退出码 = %d，期望 %d", code, ExitRunRejected)
	}
	if !strings.Contains(stderr.String(), "正在运行") {
		t.Errorf("stderr 应说明拒绝原因；实际:\n%s", stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("拒绝时不应输出书架；实际:\n%s", stdout.String())
	}
}

// fakeApp 是 run/books 子命令的可注入 App（进程内行为测试：退出码/stdout/stderr）。
// calls 记录 RunTask 被调用次数（run 不受窗口限制的断言用）。
type fakeApp struct {
	res   task.Result
	err   error
	calls int

	books    []weread.ShelfBook
	booksErr error
}

func (f *fakeApp) RunTask(ctx context.Context, finalFailureAfter time.Time) (task.Result, error) {
	f.calls++
	return f.res, f.err
}

func (f *fakeApp) ListBooks(ctx context.Context) ([]weread.ShelfBook, error) {
	return f.books, f.booksErr
}

func TestRunTaskSuccessPrintsSummary(t *testing.T) {
	env := testEnv(t, withCookie(t), config.EnvBooks+"=695233")
	res := task.Result{
		Date:      "2025-09-06",
		BookID:    "695233",
		BookTitle: "三体全集",
		Planned:   time.Minute,
		Actual:    90 * time.Second,
		Reports:   3,
	}
	var stdout, stderr syncBuffer
	code := runWithApp(context.Background(), []string{"run"}, env, &stdout, &stderr,
		func(cfg *config.Config, logger *slog.Logger) (App, error) { return &fakeApp{res: res}, nil })
	if code != ExitOK {
		t.Errorf("退出码 = %d，期望 %d; stderr=%s", code, ExitOK, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"Task 成功", "三体全集", "695233", "1 分钟", "1 分钟 30 秒", "上报次数: 3"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout 缺少 %q；实际:\n%s", want, out)
		}
	}
}

func TestRunTaskFailureExitsNonZero(t *testing.T) {
	env := testEnv(t, withCookie(t), config.EnvBooks+"=695233")
	var stdout, stderr syncBuffer
	code := runWithApp(context.Background(), []string{"run"}, env, &stdout, &stderr,
		func(cfg *config.Config, logger *slog.Logger) (App, error) {
			return &fakeApp{err: errors.New("timed report 失败")}, nil
		})
	if code != ExitConfig {
		t.Errorf("退出码 = %d，期望 %d", code, ExitConfig)
	}
	if !strings.Contains(stderr.String(), "Task 失败") {
		t.Errorf("stderr 缺少失败说明；实际:\n%s", stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("失败时不应输出摘要；实际:\n%s", stdout.String())
	}
}

// TestRunRejectedWhenTodaySuccessTerminal：当天 success 终态 → run 拒绝
// （issue 08 验收口径：stdout 说明原因 + 约定退出码 ExitRunRejected）。
// 终态规则本身由应用 seam 覆盖；本层断言进程级呈现。
func TestRunRejectedWhenTodaySuccessTerminal(t *testing.T) {
	env := testEnv(t, withCookie(t), config.EnvBooks+"=695233")
	var stdout, stderr syncBuffer
	code := runWithApp(context.Background(), []string{"run"}, env, &stdout, &stderr,
		func(cfg *config.Config, logger *slog.Logger) (App, error) {
			return &fakeApp{err: app.ErrTerminalSuccess}, nil
		})
	if code != ExitRunRejected {
		t.Errorf("退出码 = %d，期望 %d（约定拒绝退出码）", code, ExitRunRejected)
	}
	if !strings.Contains(stdout.String(), "success 终态") {
		t.Errorf("stdout 应说明拒绝原因（success 终态）；实际:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "无 force") {
		t.Errorf("stdout 应说明 V1 无 force；实际:\n%s", stdout.String())
	}
	if strings.Contains(stderr.String(), "Task 失败") {
		t.Errorf("拒绝不是 Task 失败，stderr 不应误报；实际:\n%s", stderr.String())
	}
	if strings.Contains(stdout.String(), "Task 成功") {
		t.Errorf("拒绝时不应输出成功摘要；实际:\n%s", stdout.String())
	}
}

// TestRunRejectedWhenTaskRunning：已有 Task 运行中 → 第二个 run 被拒绝
// （退出码 ExitRunRejected + stderr 说明原因）。
func TestRunRejectedWhenTaskRunning(t *testing.T) {
	env := testEnv(t, withCookie(t), config.EnvBooks+"=695233")
	var stdout, stderr syncBuffer
	code := runWithApp(context.Background(), []string{"run"}, env, &stdout, &stderr,
		func(cfg *config.Config, logger *slog.Logger) (App, error) {
			return &fakeApp{err: app.ErrTaskRunning}, nil
		})
	if code != ExitRunRejected {
		t.Errorf("退出码 = %d，期望 %d（约定拒绝退出码）", code, ExitRunRejected)
	}
	if !strings.Contains(stderr.String(), "正在运行") {
		t.Errorf("stderr 应说明拒绝原因（运行中）；实际:\n%s", stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("拒绝时不应输出摘要；实际:\n%s", stdout.String())
	}
}

// TestRunIgnoresRunWindowAtCLI：run 路径不检查 Run Window——窗口已过（00:00–01:00）
// 仍直接调用 RunTask 并成功退出（spec 决策 #12；窗口语义由应用 seam 全覆盖）。
func TestRunIgnoresRunWindowAtCLI(t *testing.T) {
	env := testEnv(t, withCookie(t), config.EnvBooks+"=695233",
		config.EnvWindowStart+"=00:00", config.EnvWindowEnd+"=01:00")
	fa := &fakeApp{res: task.Result{Date: "2025-09-06", BookID: "695233", BookTitle: "三体全集", Planned: time.Minute, Actual: time.Minute, Reports: 2}}
	var stdout, stderr syncBuffer
	code := runWithApp(context.Background(), []string{"run"}, env, &stdout, &stderr,
		func(cfg *config.Config, logger *slog.Logger) (App, error) { return fa, nil })
	if code != ExitOK {
		t.Errorf("窗口已过时 run 仍应执行，退出码 = %d；stderr=%s", code, stderr.String())
	}
	if fa.calls != 1 {
		t.Errorf("RunTask 调用次数 = %d，期望 1（run 立即执行、不受窗口约束）", fa.calls)
	}
	if !strings.Contains(stdout.String(), "Task 成功") {
		t.Errorf("stdout 应输出成功摘要；实际:\n%s", stdout.String())
	}
}

// waitFor 轮询 buf 直到出现 want 或超时。
func waitFor(t *testing.T, buf *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待 %q 超时；输出:\n%s", want, buf.String())
}
