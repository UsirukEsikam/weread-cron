// Package task 是 Task 编排层（spec 决策 #3/#6/#7）：renew → 选书 → 读真实 Reading
// Progress 与 Reader Context → enter report → 周期 timed report（rt 按 ADR-0004）→
// 本地累计达标 → 终态落盘 → 通知。
//
// ticket 03 范围：指定候选书（WEREAD_CRON_BOOKS）下的最小 happy path。
// ticket 04 范围：renewal 凭明确证据（weread.ErrLoginInvalid，checklist #2）判定登录失效
// → 从初始 Cookie 重建 Login Session 一次 → 重试；仍失败 → failed 终态 + 登录失效通知
// （明确提示更新初始 Cookie，spec 决策 #7/#9/#10）。
// ticket 05 范围（本文件）：report 被拒时的有界恢复链（spec 决策 #7）——refresh Reader
// Context → retry → renewal → refresh Reader Context → retry → 仍失败 → failed 终态 +
// 失败通知（失败阶段、主要错误、已尝试恢复动作）。
// ticket 06 范围：Reader Context TTL 到期（参考默认 ≈15 分钟）时主动重新抓取 Reader
// 页刷新 Context（Reading Session 继续：无 enter、rt 基准不变）；异常墙钟间隔超过
// 内部阈值（DefaultAnomalyThreshold）时不形成大 rt 上报，而是重建 Reading Session
// （重新 enter；Task 继续、同一本书、累计保留）。TTL 与阈值均为内部默认（spec 决策 #13）。
// 自动选书由 ticket 09 交付。
//
// # rt 语义（ADR-0004）
//
// 每次 timed report 的 rt = 距上一次被成功接受的报告（enter 或 timed，以其发送时刻
// 为基准）的实际墙钟间隔，非累计；累计由本地汇总被接受的 rt 得出。
// 网络延迟与失败重试自然并入下一次 rt（响应晚于下一节奏点时，直接按实测间隔上报）。
// 间隔超过内部异常阈值（默认 90s）时，不把大间隔作为一次 rt 上报，而是重建
// Reading Session（重新 enter report；Task 继续、同一本书、累计保留）。
//
// # 有界恢复链（spec 决策 #7）
//
// 只有服务器"明确拒绝"（report.ErrRejected：响应已完成但不被接受）才进入恢复链；
// 传输错误、HTTP 非 200 等暂时性失败不进入（沿用 ticket 04 的判别原则）。
// 恢复链按固定有界序列执行 5 步：refresh Reader Context → retry → renewal →
// refresh Reader Context → retry。任何一步成功即视为恢复成功、Task 继续；
// 最后一步重试仍被拒绝 → 恢复链耗尽 → failed 终态先落盘、再发失败通知。
// 链中步骤的非拒绝失败（传输/HTTP/解析）按暂时性失败处理：不写终态、不发通知
// （当日可再次调度）——与 ticket 04 对 renewal 暂时性失败的姿态一致；
// renewal 的登录失效明确证据仍走 ticket 04 的判别与路径（重建一次 → 仍失败 →
// 登录失效终态 + 登录失效通知，不混入普通失败文案）。
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
	"weread-cron/internal/readercontext"
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

// 失败阶段（notify.Failure.Stage；spec 决策 #10：失败通知含失败阶段）。
const (
	// StageEnterReport 是 enter report 失败的阶段。
	StageEnterReport = "enter report"
	// StageTimedReport 是 timed report 失败的阶段。
	StageTimedReport = "timed report"
)

// 已尝试恢复动作（notify.Failure.Actions；spec 决策 #10：失败通知含已尝试恢复动作）。
const (
	// ActionRefreshContext 是"刷新 Reader Context"恢复动作。
	ActionRefreshContext = "刷新 Reader Context"
	// ActionRetryReport 是"重试上报"恢复动作。
	ActionRetryReport = "重试上报"
	// ActionRenewal 是"renewal"恢复动作。
	ActionRenewal = "renewal"
)

// recoverySteps 是有界恢复链的固定步数（spec 决策 #7：refresh → retry → renewal →
// refresh → retry；不无限重试）。
const recoverySteps = 5

// Options 是 Runner 的全部依赖（ADR-0006：clock/RNG/HTTP/notify/session store 注入；
// 零值使用内部默认）。
type Options struct {
	Clock clock.Clock
	RNG   *rand.Rand

	// Client 是微信读书 HTTP 客户端（Cookie 与会话持久化由 App 装配时经回调接入）。
	Client *weread.Client
	// Sender 是 enter/timed 上报发送器。
	Sender *report.Sender
	// Reader 是 Reader Context 提供者（抓取/刷新；nil = 由 Client 组装）。
	Reader *readercontext.Provider
	// Terminal 是 Terminal State 持久化（临时 /data）。
	Terminal terminal.Store
	// Notify 是通知；nil 表示不通知（合法渠道组合，用户故事 #37）。
	Notify notify.Notifier

	// RebuildLoginSession 是登录失效重建入口（spec 决策 #7）：凭明确证据判定失效后，
	// 从初始 Cookie 重建 Login Session（覆盖持久化会话）并让客户端使用新会话。
	// 由 App 装配层提供；nil 表示无重建能力（防御：证据出现时按重建失败处理）。
	RebuildLoginSession func() error

	// Books 是候选 bookId（cfg.Books）；空 = 自动选书（ticket 09，当前直接报错）。
	Books []string
	// TargetMinMinutes/TargetMaxMinutes 是 Target Duration 随机范围（分钟）。
	TargetMinMinutes int
	TargetMaxMinutes int
	// TZ 决定 Task 日期等时间语义。
	TZ *time.Location

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
	if opts.Reader == nil {
		opts.Reader = readercontext.NewProvider(opts.Client, readercontext.Options{
			Clock: opts.Clock,
		})
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
	//    ticket 04：明确证据判定登录失效时从初始 Cookie 重建一次并重试。
	if err := r.renew(ctx); err != nil {
		return Result{}, err
	}

	// 4. 读真实 Reading Progress 并建立 Reader Context（同一 Reader 页；CONTEXT.md）。
	initial, err := r.fetchReaderState(ctx, bookID)
	if err != nil {
		return Result{}, err
	}
	bookTitle := initial.Title
	o.Logger.Info("Task 开始",
		"book_id", bookID, "book_title", bookTitle,
		"chapter_uid", initial.Progress.ChapterUID, "target", target.String())

	// 5. enter report 建立 Reading Session（用户故事 #26）；被拒时按有界恢复链恢复。
	now := o.Clock.Now()
	enterResult, err := r.sendEnter(ctx, bookID, *initial, now)
	if err != nil {
		return Result{}, fmt.Errorf("enter report 失败: %w", err)
	}
	st, lastSent := enterResult.st, enterResult.at

	// 6. 周期 timed report；rt 按 ADR-0004；本地累计达标即停止（用户故事 #27/#28）。
	//    被拒时按有界恢复链恢复（spec 决策 #7），恢复成功后 Task 继续。
	next := lastSent.Add(DefaultRhythm)
	var accumulated time.Duration
	reports := 0
	for accumulated < target {
		if d := next.Sub(o.Clock.Now()); d > 0 {
			o.Clock.Sleep(d)
		}
		now = o.Clock.Now()

		// Reader Context TTL 主动刷新（spec 决策 #5/#6；用户故事 #30）：每笔 timed
		// report 前经 Fetch 取 Context——TTL（readercontext.DefaultContextTTL，参考
		// 默认 ≈15 分钟）内命中缓存零网络请求；到期时重新抓取 Reader 页刷新
		// （新 token/psvts）。刷新不重置 rt 基准：Reading Session 继续，无 enter、
		// 不中断（用户故事 #27/#30）。
		// 刷新失败按暂时性处理（ticket 05 对链中 refresh 失败的先例）：沿用现有
		// Context 继续，下次周期再试；若服务器因此拒绝上报，恢复链会先 refresh 再
		// retry（已覆盖该情形）。
		fresh, err := r.opts.Reader.Fetch(ctx, bookID)
		if err != nil {
			o.Logger.Warn("Reader Context 刷新失败（沿用现有 Context，按暂时性处理）", "err", err)
		} else {
			st = *fresh
		}

		rtSec := rtSeconds(now, lastSent)
		if time.Duration(rtSec)*time.Second > DefaultAnomalyThreshold {
			// 异常间隔（如主机 suspend）：不作为一次大 rt 上报，重建 Reading Session
			//（重新 enter；Task 继续、累计保留，用户故事 #29；ADR-0004）。enter 使用
			// 上方 Fetch 给出的 Context（跳变越过 TTL 时即为刚重抓的新 Context；
			// 未越 TTL 时命中缓存，无需额外请求），同样走恢复链。
			o.Logger.Warn("异常间隔，重建 Reading Session", "rt_sec", rtSec)
			enterResult, err := r.sendEnter(ctx, bookID, st, now)
			if err != nil {
				return Result{}, fmt.Errorf("重建 Reading Session 的 enter report 失败: %w", err)
			}
			st, lastSent = enterResult.st, enterResult.at
			next = lastSent.Add(DefaultRhythm)
			continue
		}
		// 每笔 timed report 的 rt = 距上次被成功接受上报的实际间隔（ADR-0004）；
		// 恢复链重试重新计算（重试延迟自然并入下一次 rt，用户故事 #28）。
		send := func(s readercontext.State, t time.Time) (int, error) {
			rt := rtSeconds(t, lastSent)
			return rt, o.Sender.Timed(ctx, bookID, s.Progress, s.Context, t, rt, t.UnixMilli(), o.RNG.Intn(1000))
		}
		newSt, rt, at, err := r.recoverSend(ctx, bookID, StageTimedReport, st, now, send)
		if err != nil {
			return Result{}, fmt.Errorf("timed report 失败: %w", err)
		}
		st, lastSent = newSt, at
		accumulated += time.Duration(rt) * time.Second
		reports++
		o.Logger.Debug("timed report 接受", "rt_sec", rt, "accumulated", accumulated.String(), "reports", reports)
		next = lastSent.Add(DefaultRhythm)
	}

	// 7. success 终态先落盘，再发成功通知（spec 决策 #9/#10；用户故事 #39）。
	date := o.Clock.Now().In(o.TZ).Format(terminal.DayLayout)
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

// rtSeconds 返回 t 距 lastSent 的整秒间隔（ADR-0004：rt = 距上次被成功接受上报的
// 实际墙钟间隔）；不足 1 秒按 1 秒（服务端不接受 0 时长上报）。
func rtSeconds(t, lastSent time.Time) int {
	s := int(t.Sub(lastSent) / time.Second)
	if s < 1 {
		s = 1
	}
	return s
}

// fetchReaderState 抓取并解析 Reader 页（Task 建立时使用）。
func (r *Runner) fetchReaderState(ctx context.Context, bookID string) (*readercontext.State, error) {
	return r.opts.Reader.Fetch(ctx, bookID)
}

// sendResult 是一次上报尝试的结果：被接受的 Reader 状态、被接受的 rt（enter 为 0）
// 与发送时刻（恢复链重试时为重试时刻）。
type sendResult struct {
	st readercontext.State
	rt int
	at time.Time
}

// sendEnter 发送 enter report（不含计时字段）；被拒时按有界恢复链恢复，返回刷新后的状态。
func (r *Runner) sendEnter(ctx context.Context, bookID string, st readercontext.State, now time.Time) (sendResult, error) {
	send := func(s readercontext.State, t time.Time) (int, error) {
		return 0, r.opts.Sender.Enter(ctx, bookID, s.Progress, s.Context, t)
	}
	newSt, rt, at, err := r.recoverSend(ctx, bookID, StageEnterReport, st, now, send)
	if err != nil {
		return sendResult{}, err
	}
	return sendResult{st: newSt, rt: rt, at: at}, nil
}

// reportSend 发送一笔上报并返回被接受的 rt（enter 恒为 0）；err 为 nil 代表被接受。
// 恢复链的每次重试都重新调用（fresh Reader Context、fresh rt/ts/rn）。
type reportSend func(st readercontext.State, now time.Time) (rtSec int, err error)

// recoverSend 是上报发送 + 有界恢复链核心：
//   - 首次尝试被接受 → 直接成功；
//   - 首次尝试非"拒绝"（传输/HTTP/解析失败）→ 原样返回（恢复链只针对明确拒绝）；
//   - 首次尝试被拒绝 → 按 spec 决策 #7 执行有界序列：refresh Reader Context → retry →
//     renewal → refresh Reader Context → retry。任何一步成功即恢复；
//     最后一步重试仍被拒绝 → failRecoveryExhausted（failed 终态 + 失败通知）；
//     链中步骤的非拒绝失败按暂时性失败处理（不写终态、不发通知）；
//     renewal 的登录失效明确证据经 r.renew 走 ticket 04 路径（登录失效终态 + 通知）。
func (r *Runner) recoverSend(ctx context.Context, bookID, stage string, st readercontext.State, now time.Time, send reportSend) (readercontext.State, int, time.Time, error) {
	o := r.opts

	rt, err := send(st, now)
	if err == nil {
		return st, rt, now, nil
	}
	if !errors.Is(err, report.ErrRejected) {
		return st, 0, now, err
	}
	o.Logger.Warn("上报被服务器拒绝，进入有界恢复链", "stage", stage, "err", err)

	actions := []string{}
	lastErr := err
	current := st
	for step := 0; step < recoverySteps; step++ {
		switch step {
		case 0, 3:
			// 刷新 Reader Context（强制重新抓取 Reader 页；ticket 06 前的 Refresh 原语）。
			actions = append(actions, ActionRefreshContext)
			newSt, ferr := r.opts.Reader.Refresh(ctx, bookID)
			if ferr != nil {
				return current, 0, now, fmt.Errorf("%s 恢复链终止（刷新 Reader Context 失败，按暂时性失败处理）: %w", stage, ferr)
			}
			current = *newSt
		case 1, 4:
			// 重试上报：rt/ts/rn 重算（重试延迟并入 rt，ADR-0004 / 用户故事 #28）。
			actions = append(actions, ActionRetryReport)
			t := o.Clock.Now()
			rt, err := send(current, t)
			if err == nil {
				return current, rt, t, nil
			}
			lastErr = err
			if !errors.Is(err, report.ErrRejected) {
				return current, 0, now, fmt.Errorf("%s 恢复链终止（重试失败，按暂时性失败处理）: %w", stage, err)
			}
			if step == recoverySteps-1 {
				return current, 0, now, r.failRecoveryExhausted(ctx, stage, lastErr, actions)
			}
		case 2:
			// renewal：ticket 04 语义内嵌（明确证据 → 从初始 Cookie 重建一次 → 重试；
			// 仍失败 → 登录失效终态 + 登录失效通知，错误在此返回不进入普通失败路径）。
			actions = append(actions, ActionRenewal)
			if rerr := r.renew(ctx); rerr != nil {
				return current, 0, now, fmt.Errorf("%s 恢复链终止（renewal 失败）: %w", stage, rerr)
			}
		}
	}
	// 不可达：recoverySteps 内的最后一次 retry 已返回。
	return current, 0, now, fmt.Errorf("%s 恢复链耗尽: %w", stage, lastErr)
}

// failRecoveryExhausted 输出恢复链耗尽的失败结果：failed 终态先落盘、再发失败通知
// （spec 决策 #9/#10：失败阶段、主要错误、已尝试恢复动作；通知失败不影响 Task 结果）。
// 返回的错误供调用方报告，包装最终拒绝错误（errors.Is(err, report.ErrRejected)）。
func (r *Runner) failRecoveryExhausted(ctx context.Context, stage string, cause error, actions []string) error {
	o := r.opts
	date := o.Clock.Now().In(o.TZ).Format(terminal.DayLayout)
	if err := o.Terminal.Save(terminal.State{LastTaskDate: date, LastTaskResult: terminal.ResultFailed}); err != nil {
		o.Logger.Warn("恢复链耗尽时写入 failed 终态失败", "err", err)
	}
	if o.Notify != nil {
		if err := o.Notify.NotifyFailure(ctx, notify.Failure{
			Date:    date,
			Stage:   stage,
			Error:   cause.Error(),
			Actions: actions,
		}); err != nil {
			o.Logger.Warn("失败通知发送失败（不影响 Task 结果）", "err", err)
		}
	}
	return fmt.Errorf("%s 被服务器拒绝且恢复链耗尽: %w", stage, cause)
}

// renew 执行 Task 开始时的 renewal；凭明确证据（weread.ErrLoginInvalid：renewal
// HTTP 200 + succ != 1，checklist #2 当前假设）判定登录失效时，从初始 Cookie 重建
// Login Session 一次并重试（spec 决策 #7；用户故事 #33）：
//   - 重建 + 重试成功 → 返回 nil，Task 继续（当天不放弃）；
//   - 重建失败或重试仍失败 → failed 终态先落盘、再发登录失效通知（用户故事 #34），
//     返回包装 ErrLoginInvalid 的错误（含更新初始 Cookie 的明确提示）。
//
// 无明确证据的失败（传输错误、HTTP 非 200、响应解析失败）不重建、不写终态——
// 暂时性失败，当日可再次调度（用户故事 #17；判别特征见 client.go 的 ErrLoginInvalid）。
// 恢复链的 renewal 步骤复用本方法（有界恢复链 ≠ 无限重试：重建机会仍只消耗一次）。
func (r *Runner) renew(ctx context.Context) error {
	o := r.opts
	err := o.Client.Renewal(ctx)
	if err == nil {
		return nil
	}
	if !errors.Is(err, weread.ErrLoginInvalid) {
		return fmt.Errorf("登录 renewal 失败: %w", err)
	}

	o.Logger.Warn("检测到登录失效（renewal 明确拒绝），从初始 Cookie 重建 Login Session", "err", err)
	if o.RebuildLoginSession == nil {
		return r.failLoginInvalid(ctx, err, errors.New("未配置从初始 Cookie 重建 Login Session 的入口"))
	}
	if rerr := o.RebuildLoginSession(); rerr != nil {
		return r.failLoginInvalid(ctx, err, rerr)
	}
	if err2 := o.Client.Renewal(ctx); err2 != nil {
		// 已消耗唯一一次重建机会；重试失败（无论是否再次证据）即重建失败。
		return r.failLoginInvalid(ctx, err, err2)
	}
	return nil
}

// failLoginInvalid 输出登录失效的失败结果：failed 终态先落盘、再发送登录失效通知
// （spec 决策 #7/#9/#10；通知失败不影响结果——用户故事 #38）。返回的错误供调用方
// 报告，包装 weread.ErrLoginInvalid 并含更新初始 Cookie 的明确提示。
func (r *Runner) failLoginInvalid(ctx context.Context, evidence, cause error) error {
	o := r.opts
	date := o.Clock.Now().In(o.TZ).Format(terminal.DayLayout)
	if err := o.Terminal.Save(terminal.State{LastTaskDate: date, LastTaskResult: terminal.ResultFailed}); err != nil {
		o.Logger.Warn("登录失效时写入 failed 终态失败", "err", err)
	}
	if o.Notify != nil {
		if err := o.Notify.NotifyLoginInvalid(ctx, notify.LoginInvalid{Date: date, Cause: cause.Error()}); err != nil {
			o.Logger.Warn("登录失效通知发送失败（不影响 Task 结果）", "err", err)
		}
	}
	return fmt.Errorf("%w: 从初始 Cookie 重建 Login Session 后 renewal 仍失败。证据: %v；重建: %v。%s",
		weread.ErrLoginInvalid, evidence, cause, notify.LoginInvalidPrompt)
}

// pickBook 从候选书随机取一本；空候选 = 自动选书（ticket 09 交付，当前明确报错）。
func pickBook(rng *rand.Rand, books []string) (string, error) {
	if len(books) == 0 {
		return "", errors.New("WEREAD_CRON_BOOKS 未配置：自动从 Shelf 选书由 ticket 09 交付")
	}
	return books[rng.Intn(len(books))], nil
}
