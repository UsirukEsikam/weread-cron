// Package scheduler 是 daemon 的调度层：nextStart 决策、daemon 主循环、终态门控。
//
// ticket 01 只提供 daemon 占位路径：配置与前置校验通过后阻塞直至收到取消信号，
// 不崩溃。nextStart 纯决策与调度循环由 ticket 07 交付。
package scheduler

import (
	"context"
	"log/slog"

	"weread-cron/internal/config"
)

// Run 运行 daemon，直到 ctx 被取消（SIGINT/SIGTERM 由 main 注入）。
func Run(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	logger.Info("daemon 启动（占位：调度循环待 ticket 07 实现）",
		"tz", cfg.TZName,
		"window", cfg.WindowString(),
		"data_dir", cfg.DataDir,
	)
	// TODO(ticket 07): nextStart 决策 + 每日 Task 触发 + 终态门控（ADR-0001/0002）。
	<-ctx.Done()
	logger.Info("daemon 退出")
	return nil
}
