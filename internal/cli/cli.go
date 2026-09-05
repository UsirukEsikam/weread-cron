// Package cli 是 weread-cron 的薄 CLI 层：参数解析、子命令路由、退出码。
//
// 路由规则（spec Implementation Decisions #1）：
//   - 无子命令 = daemon（常驻）
//   - run / books 两个子命令
//   - --help 与未知子命令/flag 输出清晰用法并以非零退出码结束
//
// 本包只做 executable 边界可观察的行为（用法/退出码/启动失败）；
// run/books 在本票是路由占位，真实行为由后续票交付。
package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"weread-cron/internal/config"
	"weread-cron/internal/scheduler"
	"weread-cron/internal/session"
)

// 退出码（全部非零情形须在 stderr 指明原因）。
const (
	// ExitOK 表示成功。
	ExitOK = 0
	// ExitConfig 表示配置校验失败或启动前置条件不满足（缺 Cookie、缺 Login Session）。
	ExitConfig = 1
	// ExitUsage 表示用法错误（--help、未知子命令、未知 flag、多余参数）。
	// 按 spec 决策，--help 也以非零退出码结束。
	ExitUsage = 2
)

// Run 执行整个 CLI 并返回退出码。args 不含程序名，environ 为完整环境变量列表。
func Run(ctx context.Context, args []string, environ []string, stdout, stderr io.Writer) int {
	cmd, code := parseArgs(args, stderr)
	if code != ExitOK {
		return code
	}

	cfg, err := config.Load(envLookup(environ))
	if err != nil {
		fmt.Fprintf(stderr, "weread-cron: %v\n", err)
		return ExitConfig
	}

	store := session.NewFileStore(cfg.DataDir)
	if err := requireAuth(cfg, store); err != nil {
		fmt.Fprintf(stderr, "weread-cron: 启动失败: %v\n", err)
		return ExitConfig
	}

	logger := slog.New(slog.NewTextHandler(stderr, nil))

	switch cmd {
	case cmdDaemon:
		if err := scheduler.Run(ctx, cfg, logger); err != nil {
			fmt.Fprintf(stderr, "weread-cron: daemon 异常退出: %v\n", err)
			return ExitConfig
		}
		return ExitOK
	case cmdRun:
		// 占位路由（ticket 08 交付真实 Task 执行）。
		fmt.Fprintln(stderr, "weread-cron: run 尚未实现（ticket 08 交付 Task 执行）")
		return ExitConfig
	case cmdBooks:
		// 占位路由（ticket 09 交付书架查询）。
		fmt.Fprintln(stderr, "weread-cron: books 尚未实现（ticket 09 交付书架查询）")
		return ExitConfig
	}
	return ExitOK
}

// requireAuth 实现"首次启动快速失败"：无持久化 Login Session 且未配置初始 Cookie 即失败。
func requireAuth(cfg *config.Config, store session.Store) error {
	has, err := store.HasLoginSession()
	if err != nil {
		return fmt.Errorf("读取 %s 下的 Login Session 失败: %w", cfg.DataDir, err)
	}
	if cfg.Cookie == "" && !has {
		return fmt.Errorf(
			"%s 未配置，且 %s 下无可恢复的 Login Session；首次启动必须设置初始 Cookie（微信读书 Cookie header 字符串）",
			config.EnvCookie, cfg.DataDir)
	}
	return nil
}

type command int

const (
	cmdDaemon command = iota
	cmdRun
	cmdBooks
)

const usageText = `weread-cron — 微信读书自动阅读服务

用法:
  weread-cron             运行 daemon（每日在 Run Window 内随机安排一次 Task）
  weread-cron run         手动执行当天 Task
  weread-cron books       列出书架 bookId 与 title

选项:
  --help, -h              显示本帮助

配置全部通过环境变量提供（前缀 WEREAD_CRON_，见 ADR-0005）：
  WEREAD_CRON_COOKIE        初始 Cookie header 字符串（无持久化 Login Session 时必需）
  WEREAD_CRON_BOOKS         候选 bookId，逗号分隔（可选，默认自动从 Shelf 选书）
  WEREAD_CRON_RUN_WINDOW_START/END   Run Window，HH:MM（start==end = 固定时刻）
  WEREAD_CRON_READ_MINUTES_MIN/MAX   Target Duration 范围（分钟）
  WEREAD_CRON_BARK_URL / WEREAD_CRON_WECOM_WEBHOOK_URL   通知渠道（可选）
  WEREAD_CRON_DATA_DIR      持久化目录（默认 /data）
  TZ                        时区（默认 Asia/Shanghai，二进制内嵌 tzdata）
`

// parseArgs 解析 args；解析失败时向 stderr 输出错误与用法并返回 ExitUsage。
// 成功时返回 (command, ExitOK)；--help 按 spec 决策同样返回 ExitUsage。
func parseArgs(args []string, stderr io.Writer) (command, int) {
	printUsage := func() {
		fmt.Fprint(stderr, usageText)
	}
	if len(args) == 0 {
		return cmdDaemon, ExitOK
	}
	switch args[0] {
	case "--help", "-h":
		printUsage()
		return cmdDaemon, ExitUsage
	case cmdRunName, cmdBooksName:
		if len(args) > 1 {
			if args[1] == "--help" || args[1] == "-h" {
				printUsage()
				return cmdDaemon, ExitUsage
			}
			fmt.Fprintf(stderr, "weread-cron: %s 不接受额外参数: %q\n\n", args[0], strings.Join(args[1:], " "))
			printUsage()
			return cmdDaemon, ExitUsage
		}
		if args[0] == cmdRunName {
			return cmdRun, ExitOK
		}
		return cmdBooks, ExitOK
	default:
		if strings.HasPrefix(args[0], "-") {
			fmt.Fprintf(stderr, "weread-cron: 未知 flag: %q\n\n", args[0])
		} else {
			fmt.Fprintf(stderr, "weread-cron: 未知子命令: %q\n\n", args[0])
		}
		printUsage()
		return cmdDaemon, ExitUsage
	}
}

const (
	cmdRunName   = "run"
	cmdBooksName = "books"
)

// envLookup 把 environ 切片转换成形如 os.LookupEnv 的读取函数。
func envLookup(environ []string) func(string) (string, bool) {
	m := make(map[string]string, len(environ))
	for _, kv := range environ {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}
