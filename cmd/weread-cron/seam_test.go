// 薄 CLI seam 测试：子进程运行真实二进制（ADR-0006），验证仅 executable
// 边界可观察的行为：用法/退出码、配置校验失败、缺 Cookie 快速失败、
// daemon 启动/优雅退出、子命令路由占位。
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var binPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "weread-cron-seam")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkdir temp: %v\n", err)
		os.Exit(1)
	}
	binPath = filepath.Join(dir, "weread-cron")
	if out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build weread-cron: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// runBin 运行二进制并阻塞等待退出，返回退出码与 stdout/stderr。
func runBin(t *testing.T, args []string, env []string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Env = env
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("启动二进制失败: %v", err)
		}
		code = ee.ExitCode()
	}
	return code, stdout.String(), stderr.String()
}

// baseEnv 返回 daemon 走查所需的合法配置环境（含 Cookie；DataDir 为临时目录）。
func baseEnv(t *testing.T) []string {
	t.Helper()
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"WEREAD_CRON_COOKIE=wr_skey=abc; wr_gid=123",
		"WEREAD_CRON_RUN_WINDOW_START=01:00",
		"WEREAD_CRON_RUN_WINDOW_END=03:00",
		"WEREAD_CRON_READ_MINUTES_MIN=40",
		"WEREAD_CRON_READ_MINUTES_MAX=70",
		"WEREAD_CRON_DATA_DIR=" + t.TempDir(),
		"TZ=Asia/Shanghai",
	}
}

func TestSeamHelpAndUnknownCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"--help 退出码 2 + 用法", []string{"--help"}, "用法"},
		{"-h 退出码 2 + 用法", []string{"-h"}, "用法"},
		{"未知子命令退出码 2", []string{"frobnicate"}, "未知子命令"},
		{"未知 flag 退出码 2", []string{"--bogus"}, "未知 flag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := runBin(t, tc.args, baseEnv(t))
			if code != 2 {
				t.Errorf("退出码 = %d，期望 2", code)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr 缺少 %q；实际:\n%s", tc.want, stderr)
			}
		})
	}
}

func TestSeamConfigValidationFails(t *testing.T) {
	cases := []struct {
		name string
		env  func(t *testing.T) []string
		want string
	}{
		{
			name: "窗口 start 晚于 end",
			env: func(t *testing.T) []string {
				e := baseEnv(t)
				return overrideEnv(e, "WEREAD_CRON_RUN_WINDOW_START=03:00", "WEREAD_CRON_RUN_WINDOW_END=01:00")
			},
			want: "运行窗口",
		},
		{
			name: "时间格式非法",
			env: func(t *testing.T) []string {
				return overrideEnv(baseEnv(t), "WEREAD_CRON_RUN_WINDOW_START=25:00")
			},
			want: "HH:MM",
		},
		{
			name: "时长 min 大于 max",
			env: func(t *testing.T) []string {
				return overrideEnv(baseEnv(t), "WEREAD_CRON_READ_MINUTES_MIN=90", "WEREAD_CRON_READ_MINUTES_MAX=45")
			},
			want: "大于",
		},
		{
			name: "时区非法",
			env: func(t *testing.T) []string {
				return overrideEnv(baseEnv(t), "TZ=Foo/Bar")
			},
			want: "无法识别的时区",
		},
		{
			name: "缺失必需变量",
			env: func(t *testing.T) []string {
				e := baseEnv(t)
				var out []string
				for _, kv := range e {
					if !strings.HasPrefix(kv, "WEREAD_CRON_RUN_WINDOW_END=") {
						out = append(out, kv)
					}
				}
				return out
			},
			want: "WEREAD_CRON_RUN_WINDOW_END 未设置",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			code, _, stderr := runBin(t, nil, tc.env(t))
			if code != 1 {
				t.Errorf("退出码 = %d，期望 1", code)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr 缺少 %q；实际:\n%s", tc.want, stderr)
			}
			if elapsed := time.Since(start); elapsed > 10*time.Second {
				t.Errorf("非法配置应快速失败，耗时 %v", elapsed)
			}
		})
	}
}

func TestSeamMissingCookieFastFail(t *testing.T) {
	start := time.Now()
	// 未配置初始 Cookie（环境变量完全缺失）且 DataDir 无持久化 Login Session。
	var env []string
	for _, kv := range baseEnv(t) {
		if !strings.HasPrefix(kv, "WEREAD_CRON_COOKIE=") {
			env = append(env, kv)
		}
	}
	code, _, stderr := runBin(t, nil, env)
	if code != 1 {
		t.Errorf("退出码 = %d，期望 1", code)
	}
	if !strings.Contains(stderr, "WEREAD_CRON_COOKIE") {
		t.Errorf("stderr 应指明 WEREAD_CRON_COOKIE；实际:\n%s", stderr)
	}
	if !strings.Contains(stderr, "Login Session") {
		t.Errorf("stderr 应说明 Login Session；实际:\n%s", stderr)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("缺 Cookie 应快速失败，耗时 %v", elapsed)
	}
}

func TestSeamDaemonStartsAndStopsCleanly(t *testing.T) {
	cmd := exec.Command(binPath)
	cmd.Env = baseEnv(t)
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 daemon: %v", err)
	}

	// 等待启动日志出现（daemon 已进入阻塞的占位路径）。
	marker := make(chan struct{})
	go func() {
		defer close(marker)
		sc := bufio.NewScanner(stderrPipe)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "daemon 启动") {
				return
			}
		}
	}()

	select {
	case <-marker:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatal("daemon 未在 10s 内输出启动日志")
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("发送 SIGTERM: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("SIGTERM 后应以退出码 0 结束，got %v", err)
		}
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatal("SIGTERM 后 daemon 未在 10s 内退出")
	}
}

// TestSeamBooksWithoutAuthFailsFast：`weread-cron books` 无初始 Cookie 且无持久化
// Login Session → 启动前置条件不满足，快速失败（exit 1 + 指明 WEREAD_CRON_COOKIE）。
// books 的成功路径（fake Shelf 服务端 + 输出 bookId/title）在应用 seam 覆盖：
// 子进程 seam 无法注入 test server（真实二进制固定访问 weread.qq.com）。
func TestSeamBooksWithoutAuthFailsFast(t *testing.T) {
	start := time.Now()
	var env []string
	for _, kv := range baseEnv(t) {
		if !strings.HasPrefix(kv, "WEREAD_CRON_COOKIE=") {
			env = append(env, kv)
		}
	}
	code, _, stderr := runBin(t, []string{"books"}, env)
	if code != 1 {
		t.Errorf("退出码 = %d，期望 1", code)
	}
	if !strings.Contains(stderr, "WEREAD_CRON_COOKIE") {
		t.Errorf("stderr 应指明 WEREAD_CRON_COOKIE；实际:\n%s", stderr)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("缺 Cookie 应快速失败，耗时 %v", elapsed)
	}
}

// overrideEnv 把 env 中的 KEY=value 项替换为新值（不存在则追加）。
func overrideEnv(env []string, pairs ...string) []string {
	out := append([]string(nil), env...)
	for _, p := range pairs {
		key := p[:strings.IndexByte(p, '=')]
		replaced := false
		for i, kv := range out {
			if kv[:strings.IndexByte(kv, '=')] == key {
				out[i] = p
				replaced = true
			}
		}
		if !replaced {
			out = append(out, p)
		}
	}
	return out
}
