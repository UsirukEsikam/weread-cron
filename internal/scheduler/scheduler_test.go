package scheduler

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"weread-cron/internal/config"
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

// TestRunBlocksUntilCancelled 验证 daemon 占位路径：进入阻塞、不崩溃、取消后正常返回。
func TestRunBlocksUntilCancelled(t *testing.T) {
	cfg := &config.Config{
		TZName:      "Asia/Shanghai",
		WindowStart: 60,
		WindowEnd:   180,
		DataDir:     "/tmp/x",
	}
	var out syncBuffer
	logger := slog.New(slog.NewTextHandler(&out, nil))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cfg, logger)
	}()

	// 未取消前不应返回
	select {
	case err := <-done:
		t.Fatalf("Run 应在 ctx 取消前阻塞，却提前返回: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	// 启动日志应包含关键字段
	if !strings.Contains(out.String(), "daemon 启动") || !strings.Contains(out.String(), "01:00-03:00") {
		t.Errorf("启动日志缺少关键信息: %s", out.String())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("取消后应返回 nil，got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消后 Run 未返回")
	}

	if !strings.Contains(out.String(), "daemon 退出") {
		t.Errorf("缺少退出日志: %s", out.String())
	}
}
