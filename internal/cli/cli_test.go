package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"weread-cron/internal/config"
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

func writeLoginSessionFile(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "login_session.json"), []byte("{}"), 0o600); err != nil {
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

func TestRunAndBooksPlaceholders(t *testing.T) {
	env := testEnv(t, withCookie(t))
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"run 占位", []string{"run"}, "run 尚未实现"},
		{"books 占位", []string{"books"}, "books 尚未实现"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr syncBuffer
			code := Run(context.Background(), tc.args, env, &stdout, &stderr)
			if code != ExitConfig {
				t.Errorf("退出码 = %d，期望 %d（占位应明确失败而非假装成功）", code, ExitConfig)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr 缺少 %q；实际:\n%s", tc.want, stderr.String())
			}
		})
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
