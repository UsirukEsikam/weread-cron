// Package cli 是 weread-cron 的薄 CLI 层：参数解析、子命令路由、退出码。
//
// 路由规则（spec Implementation Decisions #1）：
//   - 无子命令 = daemon（常驻）
//   - run / books 两个子命令
//   - --help 与未知子命令/flag 输出清晰用法并以非零退出码结束
//
// `weread-cron run`（ticket 03）经内聚的应用边界（internal/app）执行一次完整 Task；
// 进程级行为（退出码、stdout 摘要、stderr）由本层负责。ticket 08 交付 run 的
// 终态规则（success 拒绝、failed 重试）与并发守卫的进程级呈现：success 拒绝原因
// 按 issue 验收口径输出到 stdout（约定退出码 ExitRunRejected）；其余非零情形
// （含运行中拒绝）按"全部非零情形须在 stderr 指明原因"的约定输出到 stderr。
// 无 force 选项：`run --force` 落入"不接受额外参数"的用法错误。
// `weread-cron books`（ticket 09）经同一应用边界列出 Shelf 的 bookId 与 title
// （每行一条，制表符分隔）；会话失效时在 stderr 报错并提示更新初始 Cookie
// （不静默；ExitConfig）；Task 运行中被并发守卫拒绝（ExitRunRejected）。
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"

	"weread-cron/internal/app"
	"weread-cron/internal/config"
	"weread-cron/internal/notify"
	"weread-cron/internal/scheduler"
	"weread-cron/internal/session"
	"weread-cron/internal/task"
	"weread-cron/internal/weread"
)

// 退出码（全部非零情形须指示原因）。
const (
	// ExitOK 表示成功。
	ExitOK = 0
	// ExitConfig 表示配置校验失败、启动前置条件不满足（缺 Cookie、缺 Login Session）
	// 或 Task 失败。
	ExitConfig = 1
	// ExitUsage 表示用法错误（--help、未知子命令、未知 flag、多余参数）。
	// 按 spec 决策，--help 也以非零退出码结束。
	ExitUsage = 2
	// ExitRunRejected 表示 run 被拒绝执行（ticket 08 约定退出码）：
	// 当天已有 success 终态（V1 无 force）或已有 Task 正在运行（并发守卫）。
	ExitRunRejected = 3
)

// App 是 run/books 子命令依赖的应用边界（生产实现 = internal/app；测试注入 fake）。
type App interface {
	// RunTask 完整执行一次 Task（ticket 24：与自动 Task 同一 finalization——一切最终
	// 结果先持久化对应终态、持久化成功后发送对应通知；暂时性失败同样收敛为 failed
	// 终态 + 失败通知）。
	RunTask(ctx context.Context) (task.Result, error)
	// RunManualTask 显式参数化手动执行一次 Task（ticket 26：覆盖 Target Duration，
	// 绕过当天终态门控，互斥守卫与终态防降级规则生效）。
	RunManualTask(ctx context.Context, opts app.ManualRunOptions) (task.Result, error)
	// ListBooks 列出当前 Shelf 的 bookId 与 title（纯查询：不产生 Task、
	// 不读写终态、不通知；内部执行 renewal 并可持久化 Login Session）。
	ListBooks(ctx context.Context) ([]weread.ShelfBook, error)
}

// appFactory 按配置装配 App。
type appFactory func(cfg *config.Config, logger *slog.Logger) (App, error)

// prodApp 是生产装配（main 经 Run 使用）。
func prodApp(cfg *config.Config, logger *slog.Logger) (App, error) {
	return app.New(cfg, app.Deps{Logger: logger}), nil
}

// Run 执行整个 CLI 并返回退出码。args 不含程序名，environ 为完整环境变量列表。
func Run(ctx context.Context, args []string, environ []string, stdout, stderr io.Writer) int {
	return runWithApp(ctx, args, environ, stdout, stderr, prodApp)
}

// runWithApp 是 Run 的可注入版本（应用边界 seam：进程级行为在此层测试）。
func runWithApp(ctx context.Context, args []string, environ []string, stdout, stderr io.Writer, makeApp appFactory) int {
	parsed, code := parseArgs(args, stderr)
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

	switch parsed.cmd {
	case cmdDaemon:
		a, err := makeApp(cfg, logger)
		if err != nil {
			fmt.Fprintf(stderr, "weread-cron: 装配失败: %v\n", err)
			return ExitConfig
		}
		if err := scheduler.New(cfg, scheduler.Deps{Task: a, Logger: logger}).Run(ctx); err != nil {
			fmt.Fprintf(stderr, "weread-cron: daemon 异常退出: %v\n", err)
			return ExitConfig
		}
		return ExitOK
	case cmdRun:
		a, err := makeApp(cfg, logger)
		if err != nil {
			fmt.Fprintf(stderr, "weread-cron: 装配失败: %v\n", err)
			return ExitConfig
		}
		// ticket 26：手动 run 显式指定参数（时长/可选书籍），立即执行且不受 Run Window
		// 约束；不受当天 Terminal State 门控阻拦；已有 Task 运行中时受并发守卫拒绝。
		res, err := a.RunManualTask(ctx, parsed.runOpts)
		switch {
		case errors.Is(err, app.ErrTaskRunning):
			fmt.Fprintln(stderr, "weread-cron: 已有 Task 正在运行，拒绝并发启动")
			return ExitRunRejected
		case errors.Is(err, context.Canceled):
			// ticket 18：取消不是业务最终失败——Task 已退出且未形成终态（可重新
			// 运行）；以正常退出码结束（与 daemon"取消 = 正常退出"一致）。
			fmt.Fprintln(stderr, "weread-cron: Task 已取消，未形成终态（可重新运行）")
			return ExitOK
		case err != nil:
			fmt.Fprintf(stderr, "weread-cron: Task 失败: %v\n", err)
			return ExitConfig
		}
		printTaskSummary(stdout, res)
		return ExitOK
	case cmdBooks:
		// ticket 09：纯查询——列出 Shelf 的 bookId 与 title（会话恢复/初始化与
		// renewal 在应用边界内完成；不产生 Task、不碰终态、不发通知）。
		a, err := makeApp(cfg, logger)
		if err != nil {
			fmt.Fprintf(stderr, "weread-cron: 装配失败: %v\n", err)
			return ExitConfig
		}
		books, err := a.ListBooks(ctx)
		switch {
		case errors.Is(err, weread.ErrLoginInvalid):
			// issue 验收：会话失效时报错并提示更新初始 Cookie（不静默）。
			fmt.Fprintf(stderr, "weread-cron: 登录已失效，books 查询失败: %v\n%s\n", err, notify.LoginInvalidPrompt)
			return ExitConfig
		case errors.Is(err, app.ErrTaskRunning):
			fmt.Fprintln(stderr, "weread-cron: 已有 Task 正在运行，拒绝并发执行 books 查询")
			return ExitRunRejected
		case err != nil:
			fmt.Fprintf(stderr, "weread-cron: books 查询失败: %v\n", err)
			return ExitConfig
		}
		for _, b := range books {
			// 每行一条：bookId 与 title 以制表符分隔（脚本可直接 cut 取用）。
			fmt.Fprintf(stdout, "%s\t%s\n", b.BookID, b.Title)
		}
		return ExitOK
	}
	return ExitOK
}

// printTaskSummary 输出 run 的 stdout 摘要（关键信息：书名、计划/实际时长、上报次数）。
func printTaskSummary(w io.Writer, res task.Result) {
	fmt.Fprintln(w, "weread-cron: Task 成功")
	fmt.Fprintf(w, "日期: %s\n", res.Date)
	fmt.Fprintf(w, "书名: %s（bookId %s）\n", res.BookTitle, res.BookID)
	fmt.Fprintf(w, "目标时长: %s\n", notify.FormatDuration(res.Planned))
	fmt.Fprintf(w, "实际累计: %s\n", notify.FormatDuration(res.Actual))
	fmt.Fprintf(w, "上报次数: %d\n", res.Reports)
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

type parsedArgs struct {
	cmd     command
	runOpts app.ManualRunOptions
}

const usageText = `weread-cron — 微信读书自动阅读服务

用法:
  weread-cron                                        运行 daemon（每日在 Run Window 内随机安排一次 Task）
  weread-cron run --minutes <N> [--book <bookId>]    手动执行一次 Task（N > 0）
  weread-cron books                                  列出书架 bookId 与 title

run 命令选项:
  --minutes <N>           阅读目标时长（分钟，必需正整数 N > 0）
  --book <bookId>         指定阅读的书籍 ID（可选，默认自动选书）

全局选项:
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
// 成功时返回 (parsedArgs, ExitOK)；--help 按 spec 决策同样返回 ExitUsage。
func parseArgs(args []string, stderr io.Writer) (parsedArgs, int) {
	printUsage := func() {
		fmt.Fprint(stderr, usageText)
	}
	if len(args) == 0 {
		return parsedArgs{cmd: cmdDaemon}, ExitOK
	}
	switch args[0] {
	case "--help", "-h":
		printUsage()
		return parsedArgs{cmd: cmdDaemon}, ExitUsage
	case cmdRunName:
		opts, code := parseRunArgs(args[1:], stderr, printUsage)
		if code != ExitOK {
			return parsedArgs{}, code
		}
		return parsedArgs{cmd: cmdRun, runOpts: opts}, ExitOK
	case cmdBooksName:
		if len(args) > 1 {
			if args[1] == "--help" || args[1] == "-h" {
				printUsage()
				return parsedArgs{}, ExitUsage
			}
			fmt.Fprintf(stderr, "weread-cron: %s 不接受额外参数: %q\n\n", args[0], strings.Join(args[1:], " "))
			printUsage()
			return parsedArgs{}, ExitUsage
		}
		return parsedArgs{cmd: cmdBooks}, ExitOK
	default:
		if strings.HasPrefix(args[0], "-") {
			fmt.Fprintf(stderr, "weread-cron: 未知 flag: %q\n\n", args[0])
		} else {
			fmt.Fprintf(stderr, "weread-cron: 未知子命令: %q\n\n", args[0])
		}
		printUsage()
		return parsedArgs{}, ExitUsage
	}
}

// parseRunArgs 解析 `weread-cron run` 的参数（ticket 26）：
//   - 必选 --minutes <N> (N > 0)
//   - 可选 --book <bookId>
//   - --help / -h 输出用法
//   - 其余 flag / 多余参数返回 ExitUsage
func parseRunArgs(args []string, stderr io.Writer, printUsage func()) (app.ManualRunOptions, int) {
	var opts app.ManualRunOptions
	hasMinutes := false

	// 先归一化 `--flag=val` 格式为独立 token。
	var tokens []string
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--minutes="):
			tokens = append(tokens, "--minutes", strings.TrimPrefix(a, "--minutes="))
		case strings.HasPrefix(a, "--book="):
			tokens = append(tokens, "--book", strings.TrimPrefix(a, "--book="))
		default:
			tokens = append(tokens, a)
		}
	}

	for i := 0; i < len(tokens); i++ {
		arg := tokens[i]
		switch {
		case arg == "--help" || arg == "-h":
			printUsage()
			return opts, ExitUsage
		case arg == "--minutes":
			if i+1 >= len(tokens) {
				fmt.Fprintf(stderr, "weread-cron: run 缺少 --minutes 参数值\n\n")
				printUsage()
				return opts, ExitUsage
			}
			i++
			val := tokens[i]
			m, err := strconv.Atoi(val)
			if err != nil || m <= 0 {
				fmt.Fprintf(stderr, "weread-cron: --minutes 必须为正整数 (N > 0): %q\n\n", val)
				printUsage()
				return opts, ExitUsage
			}
			opts.Minutes = m
			hasMinutes = true
		case arg == "--book":
			if i+1 >= len(tokens) {
				fmt.Fprintf(stderr, "weread-cron: run 缺少 --book 参数值\n\n")
				printUsage()
				return opts, ExitUsage
			}
			i++
			val := strings.TrimSpace(tokens[i])
			if val == "" {
				fmt.Fprintf(stderr, "weread-cron: --book 不能为空\n\n")
				printUsage()
				return opts, ExitUsage
			}
			opts.BookID = val
		case strings.HasPrefix(arg, "-"):
			fmt.Fprintf(stderr, "weread-cron: run 未知 flag: %q\n\n", arg)
			printUsage()
			return opts, ExitUsage
		default:
			fmt.Fprintf(stderr, "weread-cron: run 不接受额外参数: %q\n\n", arg)
			printUsage()
			return opts, ExitUsage
		}
	}

	if !hasMinutes {
		fmt.Fprintf(stderr, "weread-cron: run 缺少必需参数 --minutes <N>\n\n")
		printUsage()
		return opts, ExitUsage
	}

	return opts, ExitOK
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
