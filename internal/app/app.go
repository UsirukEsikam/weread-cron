// Package app 是应用装配与执行入口（ADR-0006 的应用边界）：生产 main/cli 与测试
// 都经 New + RunTask 执行一次 Task；clock、RNG、HTTP endpoints、session store、
// notifier 全部经 Deps 注入（零值 = 生产默认）。
package app

import (
	"context"
	"log/slog"
	"math/rand"
	"net/http"
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
	// Notify 覆盖默认通知装配（按 cfg 渠道构建）。
	Notify notify.Notifier
	// Logger 是日志输出。
	Logger *slog.Logger
}

// App 是装配好的应用。
type App struct {
	cfg  *config.Config
	deps Deps
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

// RunTask 完整执行一次 Task（Login Session 建立/恢复 → weread 客户端 → 编排）。
// 登录失效重建（ticket 04）：客户端回调读取可变会话句柄 current，重建时由
// session.Rebuild 替换为新会话（初始 Cookie 重建并覆盖持久化会话）。
func (a *App) RunTask(ctx context.Context) (task.Result, error) {
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
		Terminal:         terminal.NewFileStore(a.cfg.DataDir),
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
