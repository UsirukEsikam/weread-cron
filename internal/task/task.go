// Package task 是 Task 编排层（spec 决策 #3/#6）：renew → 选书 → 读真实 Reading Progress
// 与 Reader Context → enter report → 周期 timed report（rt 按 ADR-0004）→ 本地累计达标
// → success 终态落盘 → 成功通知。
//
// ticket 03 范围：指定候选书（WEREAD_CRON_BOOKS）下的最小 happy path。
// 恢复链（refresh Context → renewal → 重试）与失败终态、失败通知由 ticket 05 交付；
// Reader Context TTL 缓存与主动刷新由 ticket 06 交付；自动选书由 ticket 09 交付。
//
// # rt 语义（ADR-0004）
//
// 每次 timed report 的 rt = 距上一次被成功接受的报告（enter 或 timed，以其发送时刻
// 为基准）的实际墙钟间隔，非累计；累计由本地汇总被接受的 rt 得出。
// 网络延迟与失败重试自然并入下一次 rt（响应晚于下一节奏点时，直接按实测间隔上报）。
// 间隔超过内部异常阈值（默认 90s）时，不把大间隔作为一次 rt 上报，而是重建
// Reading Session（重新 enter report；Task 继续、同一本书、累计保留）。
package task

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"weread-cron/internal/clock"
	"weread-cron/internal/notify"
	"weread-cron/internal/report"
	"weread-cron/internal/terminal"
	"weread-cron/internal/weread"
)

// 内部默认值（spec 决策 #13：不暴露为配置）。
const (
	// DefaultRhythm 是 timed report 节奏（参考实现默认 ~30s）。
	DefaultRhythm = 30 * time.Second
	// DefaultAnomalyThreshold 是异常间隔阈值：超过则重建 Reading Session（ADR-0004）。
	DefaultAnomalyThreshold = 90 * time.Second
)

// Options 是 Runner 的全部依赖（ADR-0006：clock/RNG/HTTP/notify/session store 注入；
// 零值使用内部默认）。
type Options struct {
	Clock clock.Clock
	RNG   *rand.Rand

	// Client 是微信读书 HTTP 客户端（Cookie 与会话持久化由 App 装配时经回调接入）。
	Client *weread.Client
	// Sender 是 enter/timed 上报发送器。
	Sender *report.Sender
	// Terminal 是 Terminal State 持久化（临时 /data）。
	Terminal terminal.Store
	// Notify 是成功通知；nil 表示不通知（合法渠道组合，用户故事 #37）。
	Notify notify.Notifier

	// Books 是候选 bookId（cfg.Books）；空 = 自动选书（ticket 09，当前直接报错）。
	Books []string
	// TargetMinMinutes/TargetMaxMinutes 是 Target Duration 随机范围（分钟）。
	TargetMinMinutes int
	TargetMaxMinutes int
	// TZ 决定 Task 日期等时间语义。
	TZ *time.Location

	// Rhythm / AnomalyThreshold 是内部节奏与异常阈值（0 = 内部默认）。
	Rhythm           time.Duration
	AnomalyThreshold time.Duration

	Logger *slog.Logger
}

// Result 是任务结果（stdout 摘要与通知内容的数据源）。
type Result struct {
	// Date 是 Task 所属日期（cfg.TZ 下 YYYY-MM-DD）。
	Date string
	// BookID / BookTitle 是本 Task 使用的书。
	BookID    string
	BookTitle string
	// Planned 是当天随机生成的 Target Duration（计划时长）。
	Planned time.Duration
	// Actual 是本地累计的实际阅读时长（被接受 rt 之和）。
	Actual time.Duration
	// Reports 是成功接受的 timed report 次数（enter report 不计）。
	Reports int
}

// Runner 执行一次 Task。
type Runner struct {
	opts Options
}

// New 构造 Runner，应用内部默认值。
func New(opts Options) *Runner {
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.RNG == nil {
		opts.RNG = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	if opts.Rhythm == 0 {
		opts.Rhythm = DefaultRhythm
	}
	if opts.AnomalyThreshold == 0 {
		opts.AnomalyThreshold = DefaultAnomalyThreshold
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Runner{opts: opts}
}

// Run 完整执行一次 Task（spec 决策 #6 的流程）。
func (r *Runner) Run(ctx context.Context) (Result, error) {
	o := r.opts

	// 1. 选书：指定候选随机取一（用户故事 #18；自动选书 = ticket 09）。
	//    本地决策先行：候选未配置时立即失败，不发起任何网络请求。
	bookID, err := pickBook(o.RNG, o.Books)
	if err != nil {
		return Result{}, err
	}

	// 2. Target Duration：每次 Task 只生成一次（用户故事 #16；seeded RNG 下确定性）。
	target := time.Duration(o.TargetMinMinutes+o.RNG.Intn(o.TargetMaxMinutes-o.TargetMinMinutes+1)) * time.Minute

	// 3. Task 开始时即 renewal（spec 决策 #6；新 Cookie 经客户端回调并入会话并持久化）。
	if err := o.Client.Renewal(ctx); err != nil {
		return Result{}, fmt.Errorf("登录 renewal 失败: %w", err)
	}

	// 4. 读真实 Reading Progress 并建立 Reader Context（同一 Reader 页；CONTEXT.md）。
	html, err := o.Client.ReaderPage(ctx, bookID)
	if err != nil {
		return Result{}, err
	}
	state, err := weread.ParseInitialState(html)
	if err != nil {
		return Result{}, err
	}
	progress, err := state.ReadingProgress(bookID)
	if err != nil {
		return Result{}, err
	}
	rc := state.ReaderContext()
	bookTitle := state.BookTitle()
	o.Logger.Info("Task 开始",
		"book_id", bookID, "book_title", bookTitle,
		"chapter_uid", progress.ChapterUID, "target", target.String())

	// 5. enter report 建立 Reading Session（用户故事 #26）。
	now := o.Clock.Now()
	if err := o.Sender.Enter(ctx, bookID, progress, rc, now); err != nil {
		return Result{}, fmt.Errorf("enter report 失败: %w", err)
	}

	// 6. 周期 timed report；rt 按 ADR-0004；本地累计达标即停止（用户故事 #27/#28）。
	lastSent := now
	next := now.Add(o.Rhythm)
	var accumulated time.Duration
	reports := 0
	for accumulated < target {
		if d := next.Sub(o.Clock.Now()); d > 0 {
			o.Clock.Sleep(d)
		}
		now = o.Clock.Now()
		rtSec := int(now.Sub(lastSent) / time.Second)
		if rtSec < 1 {
			rtSec = 1
		}
		if time.Duration(rtSec)*time.Second > o.AnomalyThreshold {
			// 异常间隔（如主机 suspend）：不作为一次大 rt 上报，重建 Reading Session
			//（重新 enter；Task 继续、累计保留，用户故事 #29）。
			o.Logger.Warn("异常间隔，重建 Reading Session", "rt_sec", rtSec)
			if err := o.Sender.Enter(ctx, bookID, progress, rc, now); err != nil {
				return Result{}, fmt.Errorf("重建 Reading Session 的 enter report 失败: %w", err)
			}
			lastSent = now
			next = now.Add(o.Rhythm)
			continue
		}
		tsMs := now.UnixMilli()
		rn := o.RNG.Intn(1000)
		if err := o.Sender.Timed(ctx, bookID, progress, rc, now, rtSec, tsMs, rn); err != nil {
			return Result{}, fmt.Errorf("timed report 失败: %w", err)
		}
		accumulated += time.Duration(rtSec) * time.Second
		reports++
		o.Logger.Debug("timed report 接受", "rt_sec", rtSec, "accumulated", accumulated.String(), "reports", reports)
		lastSent = now
		next = now.Add(o.Rhythm)
	}

	// 7. success 终态先落盘，再发成功通知（spec 决策 #9/#10；用户故事 #39）。
	date := o.Clock.Now().In(o.TZ).Format("2006-01-02")
	if err := o.Terminal.Save(terminal.State{
		LastTaskDate:   date,
		LastTaskResult: terminal.ResultSuccess,
	}); err != nil {
		return Result{}, fmt.Errorf("写入 Terminal State 失败: %w", err)
	}

	// 通知失败不影响 Task 结果与终态（用户故事 #38）。
	if o.Notify != nil {
		if err := o.Notify.NotifySuccess(ctx, notify.Success{
			Date:      date,
			BookTitle: bookTitle,
			BookID:    bookID,
			Planned:   target,
			Actual:    accumulated,
			Reports:   reports,
		}); err != nil {
			o.Logger.Warn("成功通知发送失败（不影响 Task 结果）", "err", err)
		}
	}

	o.Logger.Info("Task 完成",
		"book_id", bookID, "target", target.String(), "actual", accumulated.String(), "reports", reports)
	return Result{
		Date:      date,
		BookID:    bookID,
		BookTitle: bookTitle,
		Planned:   target,
		Actual:    accumulated,
		Reports:   reports,
	}, nil
}

// pickBook 从候选书随机取一本；空候选 = 自动选书（ticket 09 交付，当前明确报错）。
func pickBook(rng *rand.Rand, books []string) (string, error) {
	if len(books) == 0 {
		return "", errors.New("WEREAD_CRON_BOOKS 未配置：自动从 Shelf 选书由 ticket 09 交付")
	}
	return books[rng.Intn(len(books))], nil
}
