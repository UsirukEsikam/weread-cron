// Package app 是应用装配与执行入口（ADR-0006 的应用边界）：生产 main/cli 与测试
// 都经 New + RunTask 执行一次 Task；clock、RNG、HTTP endpoints、session store、
// notifier 全部经 Deps 注入（零值 = 生产默认）。
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"weread-cron/internal/clock"
	"weread-cron/internal/config"
	"weread-cron/internal/notify"
	"weread-cron/internal/readercontext"
	"weread-cron/internal/report"
	"weread-cron/internal/session"
	"weread-cron/internal/task"
	"weread-cron/internal/terminal"
	"weread-cron/internal/weread"
)

// DefaultHTTPTimeout 是内部默认 HTTP 超时（spec 决策 #13）。
const DefaultHTTPTimeout = 30 * time.Second

// 终态规则与并发守卫的拒绝错误（ticket 08；调用方用 errors.Is 判别）。
var (
	// ErrTaskRunning 是并发守卫（spec 决策 #11）：已有 Task 运行时第二个 Task
	// （run 或 daemon 触发）被拒绝。
	ErrTaskRunning = errors.New("已有 Task 正在运行，拒绝并发启动第二个 Task")
	// ErrTerminalSuccess 是终态规则（spec 决策 #12）：当天已形成 success 终态时
	// run 被拒绝（V1 无 force），避免同一任务被重复执行。
	ErrTerminalSuccess = errors.New("当天已形成 success 终态，拒绝重复执行（V1 无 force）")
)

// Deps 是注入点（ADR-0006）；零值字段使用生产默认。
type Deps struct {
	// Clock 是时间源（生产 Real，测试 Fake）。
	Clock clock.Clock
	// RNG 是随机源（生产按时间播种，测试种子确定）。
	RNG *rand.Rand
	// HTTPClient 是与微信读书/通知端点通信的 HTTP 客户端。
	HTTPClient *http.Client
	// WereadBaseURL 是微信读书基址（生产 https://weread.qq.com，测试 httptest server）。
	WereadBaseURL string
	// UserAgent 覆盖内部默认 UA（一般不需要）。
	UserAgent string
	// Sessions 是 Login Session 存储（临时 /data 注入）。
	Sessions session.Store
	// Terminal 是 Terminal State 存储（终态规则门控/ticket 08；临时 /data 注入）。
	Terminal terminal.Store
	// Notify 覆盖默认通知装配（按 cfg 渠道构建）。
	Notify notify.Notifier
	// Logger 是日志输出。
	Logger *slog.Logger
}

// App 是装配好的应用。
type App struct {
	cfg  *config.Config
	deps Deps
	// runMu 是进程内单 Task 互斥（spec 决策 #11）：同一进程内第二个 Task
	// （run 或 daemon 触发）被拒绝。多实例防重不在 V1 范围，跨进程不互斥。
	runMu sync.Mutex
}

// New 装配应用。nil 或零值 Deps 字段回退生产默认。
func New(cfg *config.Config, deps Deps) *App {
	if deps.Clock == nil {
		deps.Clock = clock.Real{}
	}
	if deps.RNG == nil {
		deps.RNG = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	if deps.HTTPClient == nil {
		deps.HTTPClient = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	if deps.WereadBaseURL == "" {
		deps.WereadBaseURL = weread.DefaultBaseURL
	}
	if deps.UserAgent == "" {
		deps.UserAgent = weread.DefaultUserAgent
	}
	if deps.Sessions == nil {
		deps.Sessions = session.NewFileStore(cfg.DataDir)
	}
	if deps.Terminal == nil {
		deps.Terminal = terminal.NewFileStore(cfg.DataDir)
	}
	if deps.Notify == nil {
		deps.Notify = notify.NewMulti(
			notifyChannels(deps.HTTPClient, cfg)...)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &App{cfg: cfg, deps: deps}
}

// notifyChannels 按配置构建通知渠道（Bark / 企业微信，各自独立启用；都无 = 空列表）。
func notifyChannels(hc *http.Client, cfg *config.Config) []notify.Notifier {
	var channels []notify.Notifier
	if cfg.BarkURL != "" {
		channels = append(channels, &notify.Bark{URL: cfg.BarkURL, Client: hc})
	}
	if cfg.WeComWebhookURL != "" {
		channels = append(channels, &notify.WeCom{URL: cfg.WeComWebhookURL, Client: hc})
	}
	return channels
}

// RunTask 完整执行一次 Task（终态规则门控 + 并发守卫 → Login Session 建立/恢复 →
// weread 客户端 → 编排）。
// 登录失效重建（ticket 04）：客户端回调读取可变会话句柄 current，重建时由
// session.Rebuild 替换为新会话（初始 Cookie 重建并覆盖持久化会话）。
//
// ticket 08 终态规则（spec 决策 #12）：当天无终态 → 执行并形成终态；当天 failed
// → 允许重试，成功后结果更新为 success；当天 success → 拒绝（ErrTerminalSuccess）
// 且不发起任何网络请求。并发守卫（spec 决策 #11）：多协程同时调用时，锁未获得方
// 返回 ErrTaskRunning（运行中的 Task 未被中断）。
func (a *App) RunTask(ctx context.Context) (task.Result, error) {
	// 并发守卫先于一切：运行中的 Task 不被第二个 Task 打断（TryLock 非阻塞拒绝）。
	if !a.runMu.TryLock() {
		return task.Result{}, ErrTaskRunning
	}
	defer a.runMu.Unlock()

	// 终态规则门控：success 终态是 run 的非法输入状态（V1 无 force）。
	// 必须在锁内：避免两个并发 RunTask 同时通过门控后双双执行。
	if err := a.checkTerminalGate(); err != nil {
		return task.Result{}, err
	}

	sess, err := session.New(a.deps.Sessions, a.cfg.Cookie)
	if err != nil {
		return task.Result{}, err
	}
	current := sess
	client := weread.NewClient(a.deps.WereadBaseURL, a.deps.HTTPClient, a.deps.UserAgent,
		func() string { return current.CookieHeader() },
		func(cookies []*http.Cookie) error { return current.MergeAndSaveCookies(cookies) })

	runner := task.New(task.Options{
		Clock:            a.deps.Clock,
		RNG:              a.deps.RNG,
		Client:           client,
		Sender:           report.NewSender(client),
		Reader:           readercontext.NewProvider(client, readercontext.Options{Clock: a.deps.Clock}),
		Terminal:         a.deps.Terminal,
		Notify:           a.deps.Notify,
		Books:            a.cfg.Books,
		TargetMinMinutes: a.cfg.ReadMinutesMin,
		TargetMaxMinutes: a.cfg.ReadMinutesMax,
		TZ:               a.cfg.TZ,
		RebuildLoginSession: func() error {
			ns, err := session.Rebuild(a.deps.Sessions, a.cfg.Cookie)
			if err != nil {
				return err
			}
			current = ns
			return nil
		},
		Logger: a.deps.Logger,
	})
	return runner.Run(ctx)
}

// checkTerminalGate 实现终态规则：当天（cfg.TZ）已形成 success 终态 → 拒绝
// （ErrTerminalSuccess）；failed 终态是 run 的合法输入（重试，成功后更新为
// success）；昨日及更早的终态不约束今天（日期匹配的唯一定义在 terminal.IsToday）。
// 终态不可读时返回错误（保守失败：无法判定今天是否已完成）。
func (a *App) checkTerminalGate() error {
	st, has, err := a.deps.Terminal.Load()
	if err != nil {
		return fmt.Errorf("读取 Terminal State 失败: %w", err)
	}
	if has && terminal.IsToday(st, a.deps.Clock.Now(), a.cfg.TZ) &&
		st.LastTaskResult == terminal.ResultSuccess {
		return ErrTerminalSuccess
	}
	return nil
}
