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
// ticket 09 范围（本文件）：未配置候选书时自动从 Shelf 选书——Shelf 元数据过滤
// （优先未读完）→ 有界探测 Reader Context（验证可建）→ 全部不可用回退已读完但可用
// 的书 → 仍不可用 → failed 终态 + 失败通知；`weread-cron books` 由应用边界交付
// （internal/app 的 ListBooks），本包不涉及。显式指定候选书不因 progress=100% 被排除。
// Shelf 端点与响应结构按验证清单 #1 的设计假设实现（未实测，冲突时按处理原则讨论）。
// ticket 13 范围（本文件）：自动选书的候选随机化——探测前对未读完层与已读完回退层
// 各自洗牌（注入 RNG），稳定 Shelf 下多日执行不再固定选中同一本（用户故事 #10 的
// 随机化意图）；分层语义、每层有界探测上限、无可用书失败姿态均不变。
// ticket 24 范围（本文件）：Task 终结语义收敛（findings/04；spec 决策 #7/#9/#12）——
// 移除 FinalFailureAfter / 窗口截止收敛机制：一切暂时性失败（renewal / Reader
// Context / enter / timed 预算耗尽）无条件收敛为 failed 终态 + 失败通知（Task 一旦
// 启动即无 whole-Task 自动重排，窗口不再参与 Task 失败判定）；全部最终结果先持久化
// 对应终态、持久化成功后才发送对应通知（吸收 #15：终态成功落盘才发最终通知，
// 落盘失败 → 不发通知、Task 返回持久化错误）；终态与通知日期统一为 Task 开始日
// （跨午夜不转移日期归属）；手动 run 与自动 Task 使用同一 finalization。
// timed report 单次传输级失败的会话内容错保留（跳过本节奏点、会话继续，连续失败
// 超预算判失败）；Task 失败后由 `weread-cron run` 手动重试。
// ticket 18 范围（本文件）：Task 的取消生命周期——timed report 间隔等待分片 + 每片
// 检查 ctx（取消响应延迟 ≤2s，与 daemon 睡眠对齐；共用 clock.WaitUntil）；取消在
// 一切统一 finalization（finalizeTransient / failBookSelection）之前被识别：不写
// success/failed 终态、不发最终通知（取消不是业务最终失败），当天无终态，重启/
// 手动 run 按"无 Terminal State"规则重新执行。
// ticket 29 范围（本文件）：终态决策形成后的 finalization 持久化失败不丢失决策
// 身份（findings/07 H4）——恢复链耗尽（failRecoveryExhausted）与链内登录失效
// （failLoginInvalid）出口在写盘失败时，返回的错误仍联合包装 ErrRejected /
// ErrLoginInvalid（errors.Is 贯通）：Timed 循环据此把"已形成终态决策但存储失败"
// 判为终止而非可重试暂时性失败，绝不重入上报路径（否则存储恢复后后续上报可把
// 该最终失败翻转为 success 终态）。
//
// # rt 语义（ADR-0004）
//
// 每次 timed report 的 rt = 距上一次被成功接受的报告（enter 或 timed，以其发送时刻
// 为基准）的实际墙钟间隔，非累计；累计由本地汇总被接受的 rt 得出。
// 网络延迟与失败重试自然并入下一次 rt（响应晚于下一节奏点时，直接按实测间隔上报）。
// 间隔超过内部异常阈值（默认 90s）时，不把大间隔作为一次 rt 上报，而是重建
// Reading Session（重新 enter report；Task 继续、同一本书、累计保留）。阈值判定
// 沿用既有整秒边界（按上报的整秒 rt 值比较：> 阈值才重建；恰等于阈值不重建）。
// 连续性判定对每一笔上报尝试生效——含恢复链内重试（重试时刻重算 rt 后再判，ticket
// 28，findings/07 H2）与阻塞式工作（如慢速 Reader Context 抓取）推迟后的首次尝试
// （时间基准在阻塞工作之后取得）：超过阈值的间隔不得作为一笔 rt 上报。
//
// # 有界恢复链（spec 决策 #7）
//
// 只有服务器"明确拒绝"（report.ErrRejected：响应已完成但不被接受）才进入恢复链；
// 传输错误、HTTP 非 200 等暂时性失败不进入（沿用 ticket 04 的判别原则）。
// 恢复链按固定有界序列执行 5 步：refresh Reader Context → retry → renewal →
// refresh Reader Context → retry。任何一步成功即视为恢复成功、Task 继续；
// 最后一步重试仍被拒绝 → 恢复链耗尽 → failed 终态先落盘、再发失败通知。
// 链中步骤的非拒绝失败（传输/HTTP/解析）按暂时性失败处理（ticket 24：经
// finalizeTransient 无条件收敛为 failed 终态 + 失败通知，不再检查窗口截止时刻）——
// 与 ticket 04 对 renewal 暂时性失败收敛为失败终态的姿态一致；renewal 的登录失效
// 明确证据仍走 ticket 04 的判别与路径（重建一次 → 仍失败 → 登录失效终态 +
// 登录失效通知，不混入普通失败文案）。
// 链中重试按重试时刻重算 rt；若重算后已超过异常阈值（恢复期间耗时 / 时钟跳变），
// 不发送该笔超限 rt、终止链（以 chainStop 载体返回），由调用方按 ADR-0004 重建
// Reading Session（ticket 28）。
// 恢复链终止的连续性断链计入 timed 循环的连续失败预算——拒绝 + 慢恢复把每次重试
// 推过阈值的循环必须收敛（spec 决策 #7/#9/#12 有界收敛；与 ticket 27 同一预算
// 语义：重建 enter 不清零、只有被接受的 timed report 清零）。
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

// ErrTaskRunning 是并发守卫的拒绝错误（spec 决策 #11；ticket 11/ADR-0008）：同一
// deployment（同一 /data）内已有 Task 运行时，第二个 Task（run 或 daemon 触发）
// 或 books 查询被拒绝。非阻塞拒绝语义：不等待、不中断运行中的 Task。
//
// 定义在本包供调度层与应用边界共用（app 以别名导出，保持 ticket 08 以来的判别
// 身份）：若仅定义在 app，scheduler 判别本错误会引入 scheduler → app 依赖，而
// daemon 的集成测试（app 包内）又依赖 scheduler——形成测试编译环。
var ErrTaskRunning = errors.New("已有 Task 正在运行，拒绝并发启动第二个 Task")

// errIntervalOverThreshold 是一次上报尝试发现"距上次被接受上报的间隔已超过异常阈值"
// 的内部哨兵错误（ADR-0004 连续性失效；ticket 28，findings/07 H2）：该间隔不得作为
// 一笔 rt 上报（绝不落线），由调用方重建 Reading Session。首次尝试与恢复链内重试
// （重算 rt 后）共用同一判定；仅在 timed 循环内被识别，不外露。
var errIntervalOverThreshold = errors.New("间隔超过异常阈值，Reading Session 连续性失效")

// 内部默认值（spec 决策 #13：不暴露为配置）。
const (
	// DefaultRhythm 是 timed report 节奏（参考实现默认 ~30s）。
	DefaultRhythm = 30 * time.Second
	// DefaultAnomalyThreshold 是异常间隔阈值：超过则重建 Reading Session（ADR-0004）。
	DefaultAnomalyThreshold = 90 * time.Second
	// DefaultMaxSelectionProbes 是自动选书的有界探测上限（每个候选层：未读完 / 已读完）。
	DefaultMaxSelectionProbes = 20
	// DefaultMaxConsecutiveReportFailures 是 timed report 连续暂时性失败预算：单次
	// 传输级失败跳过本节奏点、会话继续；连续失败达到预算才判本 Task 最终失败
	// （ticket 12/24：不因一次抖动丢失整个 Task，也不无限跳过）。
	DefaultMaxConsecutiveReportFailures = 3
)

// 失败阶段（notify.Failure.Stage；spec 决策 #10：失败通知含失败阶段）。
const (
	// StageEnterReport 是 enter report 失败的阶段。
	StageEnterReport = "enter report"
	// StageTimedReport 是 timed report 失败的阶段。
	StageTimedReport = "timed report"
	// StageBookSelection 是自动选书失败的阶段（书架无可建立 Reader Context 的书）。
	StageBookSelection = "自动选书"
	// StageRenewal 是登录 renewal 失败的阶段（ticket 24 的暂时性失败统一收敛用）。
	StageRenewal = "renewal"
	// StageReaderContext 是读取 Reading Progress / 抓取 Reader Context 失败的阶段
	// （ticket 24 的暂时性失败统一收敛用）。
	StageReaderContext = "Reader Context"
)

// 已尝试恢复动作（notify.Failure.Actions；spec 决策 #10：失败通知含已尝试恢复动作）。
const (
	// ActionRefreshContext 是"刷新 Reader Context"恢复动作。
	ActionRefreshContext = "刷新 Reader Context"
	// ActionRetryReport 是"重试上报"恢复动作。
	ActionRetryReport = "重试上报"
	// ActionRenewal 是"renewal"恢复动作。
	ActionRenewal = "renewal"
	// ActionShelfFilter 是"Shelf 元数据过滤"选书动作。
	ActionShelfFilter = "Shelf 元数据过滤"
	// ActionProbeReaderContext 是"Reader Context 有界探测"选书动作。
	ActionProbeReaderContext = "Reader Context 有界探测"
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

	// Books 是候选 bookId（cfg.Books）；空 = 自动从 Shelf 选书（ticket 09）。
	Books []string
	// TargetMinMinutes/TargetMaxMinutes 是 Target Duration 随机范围（分钟）。
	TargetMinMinutes int
	TargetMaxMinutes int
	// TZ 决定 Task 日期等时间语义。
	TZ *time.Location

	// Manual 是手动 Task 的指定参数（ticket 26；Minutes > 0 时精确覆盖时长）。
	Manual ManualOptions

	Logger *slog.Logger
}

// ManualOptions 是手动 Task 的指定参数（ticket 26）。
type ManualOptions struct {
	// Minutes > 0 时精确覆盖 Target Duration（手动 run --minutes N）。
	Minutes int
	// BookID 非空时精确指定阅读书籍（手动 run --book <bookId>），
	// 绕过 Books 候选配置与自动选书。
	BookID string
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
//
// 失败出口（ticket 24）：一切未形成终态的失败都经 finalizeTransient 统一出口——
// 无条件收敛为 failed 终态 + 失败通知（不再检查窗口截止时刻：Task 一旦启动即无
// whole-Task 自动重排，任何失败都是最终失败）。已收敛的失败（report.ErrRejected
// 恢复链耗尽 / weread.ErrLoginInvalid 登录失效）不重复收敛。终态与通知日期统一为
// Task 开始日（taskDate）。
//
// 取消出口（ticket 18）：ctx 取消（SIGINT/SIGTERM 等）不是业务最终失败——等待
// 分片 + 每片检查 ctx，取消（含任何阶段请求因 ctx 产生的失败）在统一 finalization
// 之前被识别并直接返回：不写终态、不发通知、不累计失败预算；当天无终态，重启/
// 手动 run 按"无 Terminal State"规则重新执行。
func (r *Runner) Run(ctx context.Context) (Result, error) {
	o := r.opts

	// 1. 选书：显式指定书籍（ticket 26：Manual.BookID 非空）；未显式指定时，候选配置随机取一
	//    （用户故事 #18）；均未指定 = 自动从 Shelf 选书（用户故事 #19–#23，ticket 09）——
	//    自动选书需要登录后访问 Shelf，故放在 renewal 之后。
	var bookID string
	autoSelect := false
	if o.Manual.BookID != "" {
		bookID = o.Manual.BookID
	} else if len(o.Books) > 0 {
		bookID = o.Books[o.RNG.Intn(len(o.Books))]
	} else {
		autoSelect = true
	}

	// 2. Target Duration：手动指定（ticket 26：Manual.Minutes > 0）使用精确时长；
	//    否则每次 Task 在配置区间只生成一次（用户故事 #16；seeded RNG 下确定性）。
	var target time.Duration
	if o.Manual.Minutes > 0 {
		target = time.Duration(o.Manual.Minutes) * time.Minute
	} else {
		target = time.Duration(o.TargetMinMinutes+o.RNG.Intn(o.TargetMaxMinutes-o.TargetMinMinutes+1)) * time.Minute
	}

	// 2.5 Task 所属日期（ticket 24）：在 Task 启动时确定一次（开始日的 TZ 日期，窗口
	//     所属日）、对 success 与全部 failed 路径恒稳定。窗口末尾的 Task 可越过午夜
	//     （用户故事 #11：允许越过窗口结束点），完成/失败时刻的日期可能已是次日——
	//     终态与通知一律用开始日：窗口日不静默空过、次日的自动执行不被误抑制。
	taskDate := o.Clock.Now().In(o.TZ).Format(terminal.DayLayout)

	// 3. Task 开始时即 renewal（spec 决策 #6；新 Cookie 经客户端回调并入会话并持久化）。
	//    ticket 04：明确证据判定登录失效时从初始 Cookie 重建一次并重试。
	if err := r.renew(ctx, taskDate); err != nil {
		return Result{}, r.finalizeTransient(ctx, StageRenewal, err, taskDate)
	}

	// 4. 自动选书（Shelf 元数据过滤 → 有界探测 Reader Context → 回退已读完可用书）。
	if autoSelect {
		var err error
		bookID, err = r.selectFromShelf(ctx, taskDate)
		if err != nil {
			return Result{}, err
		}
	}

	// 5. 读真实 Reading Progress 并建立 Reader Context（同一 Reader 页；CONTEXT.md）。
	//    （自动选书时选定书已在探测中抓取，命中 TTL 缓存即零额外请求。）
	initial, err := r.fetchReaderState(ctx, bookID)
	if err != nil {
		return Result{}, fmt.Errorf("读取 Reading Progress 失败: %w", r.finalizeTransient(ctx, StageReaderContext, err, taskDate))
	}
	bookTitle := initial.Title
	o.Logger.Info("Task 开始",
		"book_id", bookID, "book_title", bookTitle,
		"chapter_uid", initial.Progress.ChapterUID, "target", target.String())

	// 6. enter report 建立 Reading Session（用户故事 #26）；被拒时按有界恢复链恢复。
	now := o.Clock.Now()
	enterResult, err := r.sendEnter(ctx, bookID, *initial, now, taskDate)
	if err != nil {
		return Result{}, fmt.Errorf("enter report 失败: %w", r.finalizeTransient(ctx, StageEnterReport, err, taskDate))
	}
	st, lastSent := enterResult.st, enterResult.at

	// 7. 周期 timed report；rt 按 ADR-0004；本地累计达标即停止（用户故事 #27/#28）。
	//    被拒时按有界恢复链恢复（spec 决策 #7），恢复成功后 Task 继续。
	//    ticket 12/24：单次传输级失败（非拒绝）不再终止 Task——跳过本节奏点、Reading
	//    Session 继续，被跳过的间隔自然并入下一次被接受上报的 rt；连续失败达到
	//    DefaultMaxConsecutiveReportFailures 预算才判定本 Task 最终失败（收敛为
	//    failed 终态 + 失败通知）。ticket 28：恢复链内重试重算 rt 超阈值（连续性
	//    断链、不发送）的终止同样计入该预算——拒绝 + 慢恢复的循环必有界收敛。
	next := lastSent.Add(DefaultRhythm)
	var accumulated time.Duration
	reports := 0
	consecutiveFailures := 0
	for accumulated < target {
		// ticket 18：取消识别先于本轮一切动作——上一轮处理期间发生的取消（如
		// Fetch/上报请求以 ctx 错误失败）在此直接退出：不写终态、不发通知
		// （取消不是业务最终失败；当天无终态，重启/手动 run 按"无 Terminal
		// State"规则重新执行）。
		if err := ctx.Err(); err != nil {
			return Result{}, cancelledExit(ctx)
		}
		if d := next.Sub(o.Clock.Now()); d > 0 {
			// ticket 18：间隔等待分片 + 每片检查 ctx（与 daemon 睡眠同一形态，共用
			// clock.WaitUntil）——取消响应延迟 ≤ clock.WaitChunk；正常 timed report
			// 节奏与 rt 计算不受影响。
			if err := clock.WaitUntil(ctx, o.Clock, next); err != nil {
				return Result{}, cancelledExit(ctx)
			}
		}

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
			// ticket 18：Fetch 因 ctx 取消失败不是暂时性故障——不沿用现有 Context
			// 继续上报（取消的上报只会以传输级错误失败并误计失败预算），直接退出。
			if ctx.Err() != nil {
				return Result{}, cancelledExit(ctx)
			}
			o.Logger.Warn("Reader Context 刷新失败（沿用现有 Context，按暂时性处理）", "err", err)
		} else {
			st = *fresh
		}
		// 时间基准（now）在阻塞式 Fetch 之后取得（ticket 28 附带边界）：慢速 Reader
		// Context 抓取把实际间隔推迟到阈值之外时，以真实发送前的时刻判定连续性——
		// 阻塞耗时与恢复期间耗时服从同一连续性边界，不产生跨断口的上报。
		now = o.Clock.Now()

		// 每笔 timed report 的 rt = 距上次被成功接受上报的实际间隔（ADR-0004）。
		// send 对"首次尝试"与"恢复链内每次重试"（重试时刻重算 rt）共用同一连续性
		// 判定：rt 超过异常阈值（DefaultAnomalyThreshold）时返回 errIntervalOverThreshold
		// 且不发送该笔间隔（绝不落线），由调用方重建 Reading Session（ticket 28，
		// findings/07 H2）——阈值内的重试延迟仍自然并入下一次被接受的 rt（用户故事
		// #28）；恰等于阈值沿用既有边界（> 阈值才重建）。
		send := func(s readercontext.State, t time.Time) (int, error) {
			rt := rtSeconds(t, lastSent)
			if time.Duration(rt)*time.Second > DefaultAnomalyThreshold {
				return 0, errIntervalOverThreshold
			}
			return rt, o.Sender.Timed(ctx, bookID, s.Progress, s.Context, t, rt, t.UnixMilli(), o.RNG.Intn(1000))
		}
		newSt, rt, at, err := r.recoverSend(ctx, bookID, StageTimedReport, st, now, send, taskDate)
		if errors.Is(err, errIntervalOverThreshold) {
			// 异常间隔（如主机 suspend / 恢复期间耗时 / 慢速 Fetch）：不作为一次大 rt
			// 上报，重建 Reading Session（重新 enter；Task 继续、累计保留，用户故事
			// #29；ADR-0004）。enter 使用 recoverSend 返回的 Context（恢复链刷新后的
			// 最新 Context；未进链时为上方 Fetch 给出的 Context，跳变越过 TTL 即为刚
			// 重抓的新 Context），同样走恢复链。
			//
			// 恢复链内触发的断链（重试重算 rt 超阈值；错误经 chainStop 载体返回）计入
			// 连续失败预算：该拒绝未被链解决而重建，连续出现（拒绝 + 慢恢复把每轮重试
			// 都推过阈值）时必须收敛为 failed 终态（spec 决策 #7/#9/#12 有界收敛）——
			// 与 ticket 27 同一预算语义：重建 enter 不清零，只有下方被接受的 timed
			// report 清零。首次尝试的连续性断链（非拒绝路径）不计入：其后必然接一笔
			// ≤ 阈值的上报或其它失败路径，天然有界。
			var stop *chainStop
			if errors.As(err, &stop) {
				consecutiveFailures++
				if consecutiveFailures >= DefaultMaxConsecutiveReportFailures {
					err = fmt.Errorf("timed report 连续 %d 次暂时性失败: %w", consecutiveFailures, err)
					return Result{}, r.finalizeTransient(ctx, StageTimedReport, err, taskDate)
				}
			}
			o.Logger.Warn("异常间隔，重建 Reading Session", "rt_sec", rtSeconds(o.Clock.Now(), lastSent))
			enterResult, err := r.sendEnter(ctx, bookID, newSt, o.Clock.Now(), taskDate)
			if err != nil {
				return Result{}, fmt.Errorf("重建 Reading Session 的 enter report 失败: %w", r.finalizeTransient(ctx, StageEnterReport, err, taskDate))
			}
			st, lastSent = enterResult.st, enterResult.at
			// ticket 27（findings/07 H1）：重建 enter 成功只证明 Reader Context /
			// Reading Session 重新建立，不证明 timed report 路径已恢复——连续失败
			// 预算只在"一笔被接受的 timed report"出现时清零（下方成功分支），重建
			// enter 不清零。否则持续慢故障（每笔 timed report 延迟后失败、每次失败
			// 把下一次尝试推过异常阈值）时，重建 enter 会反复清零预算，Task 永不
			// 收敛为 failed 终态（有界收敛保证，spec 决策 #7/#9/#12）。
			next = lastSent.Add(DefaultRhythm)
			continue
		}
		if err != nil {
			// 已收敛的终态失败（服务器拒绝且恢复链耗尽 / 登录失效）：Task 终止，
			// 不再重试（终态与通知已由恢复链/登录失效路径完成）。
			if errors.Is(err, report.ErrRejected) || errors.Is(err, weread.ErrLoginInvalid) {
				return Result{}, fmt.Errorf("timed report 失败: %w", err)
			}
			// ticket 18：取消期间的请求失败（传输级 context canceled）不是暂时性
			// 故障——不得计入连续失败预算、不触发 finalizeTransient；直接退出，
			// 当天无终态（与 #24 预算收敛路径明确区分）。
			if ctx.Err() != nil {
				return Result{}, cancelledExit(ctx)
			}
			// 暂时性失败（传输/HTTP/解析）：跳过本节奏点、Reading Session 继续，
			// 被跳过的间隔并入下一次被接受上报的 rt（ADR-0004）——单次抖动不丢失
			// 整个 Task；连续失败达预算 → 本 Task 最终失败（ticket 24：经
			// finalizeTransient 无条件收敛为 failed 终态 + 失败通知）。
			consecutiveFailures++
			if consecutiveFailures >= DefaultMaxConsecutiveReportFailures {
				err = fmt.Errorf("timed report 连续 %d 次暂时性失败: %w", consecutiveFailures, err)
				return Result{}, r.finalizeTransient(ctx, StageTimedReport, err, taskDate)
			}
			o.Logger.Warn("timed report 暂时性失败，跳过本节奏点（Reading Session 继续）", "consecutive_failures", consecutiveFailures, "err", err)
			next = now.Add(DefaultRhythm)
			continue
		}
		consecutiveFailures = 0
		st, lastSent = newSt, at
		accumulated += time.Duration(rt) * time.Second
		reports++
		o.Logger.Debug("timed report 接受", "rt_sec", rt, "accumulated", accumulated.String(), "reports", reports)
		next = lastSent.Add(DefaultRhythm)
	}

	// 8. success 终态先落盘，再发成功通知（spec 决策 #9/#10；用户故事 #39）；终态与
	//    通知日期用 Task 开始日（taskDate；ticket 24——跨午夜完成不转移日期归属）。
	if err := r.finalize(ctx, terminal.State{
		LastTaskDate:   taskDate,
		LastTaskResult: terminal.ResultSuccess,
	}, func(ctx context.Context) error {
		return o.Notify.NotifySuccess(ctx, notify.Success{
			Date:      taskDate,
			BookTitle: bookTitle,
			BookID:    bookID,
			Planned:   target,
			Actual:    accumulated,
			Reports:   reports,
		})
	}); err != nil {
		// 终态持久化失败：不发成功通知、Task 返回持久化错误（#15：不存在"已发通知
		// 但持久化无对应终态"的可达状态）。
		return Result{}, err
	}

	o.Logger.Info("Task 完成",
		"book_id", bookID, "target", target.String(), "actual", accumulated.String(), "reports", reports)
	return Result{
		Date:      taskDate,
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

// cancelledExit 是取消（ctx 取消）退出的统一错误（ticket 18）：取消不是 Task 的
// 业务最终失败——尽快返回、不写 success/failed 终态、不发最终通知（当天无终态，
// 重启/手动 run 按"无 Terminal State"规则重新执行）。包装 ctx.Err() 保持
// errors.Is(err, context.Canceled) 判别身份（daemon 凭自身 ctx.Err() 判定"取消 =
// 正常退出"；CLI 判别"已取消"提示与正常退出码）。
func cancelledExit(ctx context.Context) error {
	return fmt.Errorf("Task 已取消（未形成终态）: %w", ctx.Err())
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
// taskDate 是 Task 开始日（恢复链耗尽的终态/通知日期用；见 Run）。
func (r *Runner) sendEnter(ctx context.Context, bookID string, st readercontext.State, now time.Time, taskDate string) (sendResult, error) {
	send := func(s readercontext.State, t time.Time) (int, error) {
		return 0, r.opts.Sender.Enter(ctx, bookID, s.Progress, s.Context, t)
	}
	newSt, rt, at, err := r.recoverSend(ctx, bookID, StageEnterReport, st, now, send, taskDate)
	if err != nil {
		return sendResult{}, err
	}
	return sendResult{st: newSt, rt: rt, at: at}, nil
}

// reportSend 发送一笔上报并返回被接受的 rt（enter 恒为 0）；err 为 nil 代表被接受。
// 恢复链的每次重试都重新调用（fresh Reader Context、fresh rt/ts/rn）。
// 实现方（timed 循环的 send 闭包）对每次尝试（含重试）执行连续性判定：rt 超过异常
// 阈值时返回 errIntervalOverThreshold 而不发送（ticket 28，findings/07 H2）。
type reportSend func(st readercontext.State, now time.Time) (rtSec int, err error)

// recoverSend 是上报发送 + 有界恢复链核心：
//   - 首次尝试被接受 → 直接成功；
//   - 首次尝试非"拒绝"（传输/HTTP/解析失败）→ 原样返回（恢复链只针对明确拒绝）；
//   - 首次尝试被拒绝 → 按 spec 决策 #7 执行有界序列：refresh Reader Context → retry →
//     renewal → refresh Reader Context → retry。任何一步成功即恢复；
//     最后一步重试仍被拒绝 → failRecoveryExhausted（failed 终态 + 失败通知）；
//     链中步骤的非拒绝失败按暂时性失败处理（ticket 24：由调用方经 finalizeTransient
//     收敛为 failed 终态 + 失败通知）；
//     renewal 的登录失效明确证据经 r.renew 走 ticket 04 路径（登录失效终态 + 通知）；
//     重试重算 rt 后若已超异常阈值（恢复期间耗时 / 时钟跳变），终止链并以 chainStop
//     携带 errIntervalOverThreshold 返回（不发送该笔超限 rt），由调用方重建 Reading
//     Session 并计入连续失败预算（ticket 28）。
//
// taskDate 是 Task 开始日（终态/通知日期用；见 Run）。
func (r *Runner) recoverSend(ctx context.Context, bookID, stage string, st readercontext.State, now time.Time, send reportSend, taskDate string) (readercontext.State, int, time.Time, error) {
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
				return current, 0, now, &chainStop{
					actions: append([]string(nil), actions...),
					err:     fmt.Errorf("%s 恢复链终止（刷新 Reader Context 失败，按暂时性失败处理）: %w", stage, ferr),
				}
			}
			current = *newSt
		case 1, 4:
			// 重试上报：rt/ts/rn 重算（重试延迟自然并入下一次 rt，ADR-0004 / 用户
			// 故事 #28）。
			actions = append(actions, ActionRetryReport)
			t := o.Clock.Now()
			rt, err := send(current, t)
			if err == nil {
				return current, rt, t, nil
			}
			lastErr = err
			if errors.Is(err, errIntervalOverThreshold) {
				// ticket 28（findings/07 H2）：重试重算 rt 后发现间隔已超过异常阈值
				//（恢复期间耗时 / 时钟跳变 / 慢速刷新）——不发送这笔超限 rt、终止链，
				// 由调用方按 ADR-0004 重建 Reading Session（重新 enter；Task 继续、
				// 同一本书、累计保留）。以 chainStop 载体返回：调用方据此把该断链
				// 计入连续失败预算（拒绝 + 慢恢复循环必有界收敛）、并在预算耗尽时
				// 用已尝试动作构造失败通知。
				return current, 0, t, &chainStop{
					actions: append([]string(nil), actions...),
					err:     err,
				}
			}
			if !errors.Is(err, report.ErrRejected) {
				return current, 0, now, &chainStop{
					actions: append([]string(nil), actions...),
					err:     fmt.Errorf("%s 恢复链终止（重试失败，按暂时性失败处理）: %w", stage, err),
				}
			}
			if step == recoverySteps-1 {
				return current, 0, now, r.failRecoveryExhausted(ctx, stage, lastErr, actions, taskDate)
			}
		case 2:
			// renewal：ticket 04 语义内嵌（明确证据 → 从初始 Cookie 重建一次 → 重试；
			// 仍失败 → 登录失效终态 + 登录失效通知，错误在此返回不进入普通失败路径）。
			actions = append(actions, ActionRenewal)
			if rerr := r.renew(ctx, taskDate); rerr != nil {
				return current, 0, now, &chainStop{
					actions: append([]string(nil), actions...),
					err:     fmt.Errorf("%s 恢复链终止（renewal 失败）: %w", stage, rerr),
				}
			}
		}
	}
	// 不可达：recoverySteps 内的最后一次 retry 已返回。
	return current, 0, now, fmt.Errorf("%s 恢复链耗尽: %w", stage, lastErr)
}

// chainStop 是恢复链"未解决即终止"的错误的载体（ticket 12/24/28）：携带链中已尝试的
// 恢复动作（失败通知的 notify.Failure.Actions 数据源）。errors.As 提取；包装链
// 保留（errors.Is 贯通——恢复链从不携带 ErrRejected，携带的收敛错误
// ErrLoginInvalid 经 errors.Is 判别跳过重复收敛；携带 errIntervalOverThreshold
// 时表示"重试重算 rt 超阈值、不发送、链终止"，ticket 28）。
type chainStop struct {
	actions []string
	err     error
}

func (e *chainStop) Error() string { return e.err.Error() }
func (e *chainStop) Unwrap() error { return e.err }

// finalizeTransient 是暂时性失败的统一收敛出口（ticket 24）：一切未形成终态的失败
// （传输/HTTP/解析失败、timed report 预算耗尽、恢复链因暂时性失败终止）无条件收敛
// 为 failed 终态 + 失败通知（spec 决策 #7/#9/#12）。不再检查窗口截止时刻——Task 一旦
// 启动即无 whole-Task 自动重排，任何失败都是最终失败；手动 run 与自动 Task 同一
// finalization。
//
// 已收敛的错误（report.ErrRejected 恢复链耗尽 / weread.ErrLoginInvalid 登录失效）
// 原样返回：终态与通知已由各自路径完成，不重复收敛。
//
// taskDate 是 Task 开始日（ticket 24）：终态与通知的日期一律用开始日而非失败时刻
// 的日期（Task 可越过午夜，用户故事 #11）。cause 保持包装链不变（errors.Is 判别
// 身份保留），收敛与否只影响终态与通知；恢复链终止的错误经 errors.As 提取已尝试
// 动作（chainStop）。终态持久化失败 → 返回持久化错误、不发通知（#15）。
func (r *Runner) finalizeTransient(ctx context.Context, stage string, cause error, taskDate string) error {
	// ticket 18：取消识别先于统一 finalization——ctx 已取消（SIGINT/SIGTERM 等）
	// 时，一切"失败"都是取消的后果而非业务失败：不收敛为 failed 终态、不发通知，
	// 直接退出（当天无终态；重启/手动 run 按"无 Terminal State"规则重新执行）。
	// 该检查覆盖经本入口收敛的全部未形成终态失败（renewal / Shelf 抓取 /
	// Reader Context / enter / timed 预算 / 恢复链暂时性终止）；不经本入口的
	// 失败（选书"无可用书"）在 failBookSelection 入口做同样检查。
	if ctx.Err() != nil {
		return cancelledExit(ctx)
	}
	if errors.Is(cause, report.ErrRejected) || errors.Is(cause, weread.ErrLoginInvalid) {
		return cause
	}
	f := notify.Failure{Date: taskDate, Stage: stage, Error: cause.Error()}
	var stop *chainStop
	if errors.As(cause, &stop) {
		f.Actions = stop.actions // 恢复链中已尝试的动作（若有）
	}
	o := r.opts
	if err := r.finalize(ctx, terminal.State{LastTaskDate: taskDate, LastTaskResult: terminal.ResultFailed}, func(ctx context.Context) error {
		return o.Notify.NotifyFailure(ctx, f)
	}); err != nil {
		// 终态持久化失败：不发失败通知、返回持久化错误（#15）。
		return err
	}
	o.Logger.Info("暂时性失败收敛为 failed 终态（Task 不再自动重排）", "stage", stage, "err", cause)
	return cause
}

// finalize 是全部最终结果（success 与各 failed 出口）的统一收敛流程（spec 决策
// #9/#10；ticket 24）：先持久化对应的 Terminal State（success/failed），持久化成功
// 后才发送对应的最终通知；持久化失败 → 不发送通知、返回持久化错误（#15：不存在
// "已发通知但持久化无对应终态"的可达状态）。所有最终结果路径（成功、各失败出口、
// 统一收敛以及登录失效）都经由本方法。
//
// notify 为 nil（nil Notifier 或出口无需通知）时跳过发送；通知发送失败不影响 Task
// 结果与终态（用户故事 #38），仅记日志。
//
// 返回的持久化错误为裸错误（不携带终态决策身份）；经本方法返回错误后直接流向 Task
// 调用方的出口（finalizeTransient / success / 选书失败）不会重入上报路径，无需身份。
// 例外：经恢复链回流入 Timed 循环的终态决策出口（failRecoveryExhausted /
// failLoginInvalid）在写盘失败时把返回错误与决策错误联合包装（ticket 29），
// errors.Is 判别身份贯通——调用方据此区分"已形成终态决策但存储失败"与"可重试的
// 暂时性失败"，绝不重入上报业务路径。
func (r *Runner) finalize(ctx context.Context, st terminal.State, notify func(context.Context) error) error {
	o := r.opts
	shouldSave := true
	// 防降级规则（ticket 26）：若失败 Task 的所属日期已有 success 终态，保留已有的 success
	// 终态（不降级为 failed）。
	if st.LastTaskResult == terminal.ResultFailed {
		existing, has, err := o.Terminal.Load()
		if err != nil {
			return fmt.Errorf("读取 Terminal State 失败: %w", err)
		}
		if has && existing.LastTaskDate == st.LastTaskDate && existing.LastTaskResult == terminal.ResultSuccess {
			shouldSave = false
			o.Logger.Info("当天已有 success 终态，保留已持久化的 success 终态（防降级规则生效）", "date", st.LastTaskDate)
		}
	}
	if shouldSave {
		if err := o.Terminal.Save(st); err != nil {
			return fmt.Errorf("写入 Terminal State 失败: %w", err)
		}
	}
	if o.Notify == nil || notify == nil {
		return nil
	}
	if err := notify(ctx); err != nil {
		o.Logger.Warn("最终通知发送失败（不影响 Task 结果）", "err", err)
	}
	return nil
}

// failRecoveryExhausted 输出恢复链耗尽的失败结果：failed 终态先落盘、再发失败通知
// （spec 决策 #9/#10：失败阶段、主要错误、已尝试恢复动作；通知失败不影响 Task 结果；
// 终态持久化失败 → 返回持久化错误、不发通知，#15）。返回的错误供调用方报告，包装
// 最终拒绝错误（errors.Is(err, report.ErrRejected)）；写盘失败路径同样保留该判别
// 身份（ticket 29：终态决策已形成，调用方不得把"已形成终态决策但存储失败"判为
// 可重试暂时性失败而重入上报路径）。taskDate 是 Task 开始日。
func (r *Runner) failRecoveryExhausted(ctx context.Context, stage string, cause error, actions []string, taskDate string) error {
	o := r.opts
	if err := r.finalize(ctx, terminal.State{LastTaskDate: taskDate, LastTaskResult: terminal.ResultFailed}, func(ctx context.Context) error {
		return o.Notify.NotifyFailure(ctx, notify.Failure{
			Date:    taskDate,
			Stage:   stage,
			Error:   cause.Error(),
			Actions: actions,
		})
	}); err != nil {
		// ticket 29（findings/07 H4）：终态决策已形成——联合包装持久化错误与决策
		// 错误（cause），errors.Is(err, report.ErrRejected) 贯通：Timed 循环据此
		// 直接终止，绝不重入上报路径（否则存储恢复后后续成功上报可把该最终失败
		// 翻转为 success 终态）。
		return fmt.Errorf("终态持久化失败（恢复链已耗尽，Task 终止）: %w: %w", err, cause)
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
// 无明确证据的失败（传输错误、HTTP 非 200、响应解析失败）不重建——由调用方经
// finalizeTransient 收敛为 failed 终态 + 失败通知（ticket 24：与其它暂时性失败同一
// 最终收敛；不再于窗口内重排）。恢复链的 renewal 步骤复用本方法（有界恢复链 ≠
// 无限重试：重建机会仍只消耗一次）。taskDate 是 Task 开始日（终态/通知日期用）。
func (r *Runner) renew(ctx context.Context, taskDate string) error {
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
		return r.failLoginInvalid(ctx, err, errors.New("未配置从初始 Cookie 重建 Login Session 的入口"), taskDate)
	}
	if rerr := o.RebuildLoginSession(); rerr != nil {
		return r.failLoginInvalid(ctx, err, rerr, taskDate)
	}
	if err2 := o.Client.Renewal(ctx); err2 != nil {
		// 已消耗唯一一次重建机会；重试失败（无论是否再次证据）即重建失败。
		return r.failLoginInvalid(ctx, err, err2, taskDate)
	}
	return nil
}

// failLoginInvalid 输出登录失效的失败结果：failed 终态先落盘、再发送登录失效通知
// （spec 决策 #7/#9/#10；通知失败不影响结果——用户故事 #38；终态持久化失败 →
// 返回持久化错误、不发通知，#15）。返回的错误供调用方报告，包装 weread.ErrLoginInvalid
// 并含更新初始 Cookie 的明确提示；写盘失败路径同样保留该判别身份（ticket 29：终态
// 决策已形成，恢复链/外层循环不得判为可重试暂时性失败而重入上报路径）。taskDate 是
// Task 开始日。
func (r *Runner) failLoginInvalid(ctx context.Context, evidence, cause error, taskDate string) error {
	o := r.opts
	if err := r.finalize(ctx, terminal.State{LastTaskDate: taskDate, LastTaskResult: terminal.ResultFailed}, func(ctx context.Context) error {
		return o.Notify.NotifyLoginInvalid(ctx, notify.LoginInvalid{Date: taskDate, Cause: cause.Error()})
	}); err != nil {
		// ticket 29（findings/07 H4）：同 failRecoveryExhausted——联合包装持久化
		// 错误与完整登录失效错误（含证据/重建诊断与 ErrLoginInvalid 身份），
		// errors.Is(err, weread.ErrLoginInvalid) 贯通且诊断不丢。
		return fmt.Errorf("终态持久化失败（登录失效，Task 终止）: %w: %w", err,
			fmt.Errorf("%w: 从初始 Cookie 重建 Login Session 后 renewal 仍失败。证据: %v；重建: %v。%s",
				weread.ErrLoginInvalid, evidence, cause, notify.LoginInvalidPrompt))
	}
	return fmt.Errorf("%w: 从初始 Cookie 重建 Login Session 后 renewal 仍失败。证据: %v；重建: %v。%s",
		weread.ErrLoginInvalid, evidence, cause, notify.LoginInvalidPrompt)
}

// selectFromShelf 自动从 Shelf 选书（spec 决策 #6；用户故事 #19–#23）：
//  1. 抓取 Shelf（需登录后访问，故在 renewal 之后）；
//  2. 元数据过滤：未读完（finishReading != 1）优先、已读完作为回退层；
//     无 bookId/title 的条目不可建 Reader Context，直接过滤；
//  3. 候选层随机化（ticket 13）：未读完层与已读完回退层各自用注入 RNG 洗牌——
//     稳定 Shelf 响应下多日执行不再固定选中同一本（用户故事 #10 的随机化意图、
//     决策 #4：RNG 是领域真实依赖）；分层语义（未读完优先/已读完回退）不变；
//  4. 有界探测：对洗牌后候选依次抓取 Reader 页（Reader Context 验证可建）——每层
//     不超过 DefaultMaxSelectionProbes 次，失败换下一本；选定的书命中 Reader
//     缓存（随后 fetchReaderState 零额外请求，进入上报即用探测时的 Context）；
//  5. 全部不可用 → failBookSelection（failed 终态 + 失败通知）。
//
// 与任务级恢复链的姿态一致：Shelf 抓取失败（传输/HTTP/解析）按暂时性处理（由调用方
// 经 finalizeTransient 无条件收敛为 failed 终态 + 失败通知，ticket 24——不再于窗口内
// 重排）；只有"Shelf 有数据但无可用书"才是本 Task 的失败（终态 + 失败通知——书可用性
// 的数据问题，重跑大概率依旧失败）。取消（ticket 18）：ctx 已取消时探测请求以 ctx
// 错误失败不是书不可用证据——probe 停止探测、failBookSelection 入口直接取消退出（不写
// failed 终态、不发通知，当天无终态）。taskDate 是 Task 开始日（finalizeTransient 与
// failBookSelection 的终态/通知日期用；见 Run）。
func (r *Runner) selectFromShelf(ctx context.Context, taskDate string) (string, error) {
	o := r.opts
	shelf, err := o.Client.Shelf(ctx)
	if err != nil {
		return "", fmt.Errorf("抓取 Shelf 失败: %w", r.finalizeTransient(ctx, StageBookSelection, err, taskDate))
	}

	var unread, finished []weread.ShelfBook
	for _, b := range shelf {
		if b.BookID == "" || b.Title == "" {
			continue // 无效条目（无 bookId/title）不可建 Reader Context
		}
		if b.FinishReading {
			finished = append(finished, b)
		} else {
			unread = append(unread, b)
		}
	}

	// 候选层随机化（ticket 13）：探测前对未读完层与已读完回退层各自洗牌（Fisher-
	// Yates，就地）。层内全部候选参与随机，探测仍从洗牌后顺序取前 N 本——每层
	// 有界探测上限（DefaultMaxSelectionProbes）与"第一本可用即命中"语义不变，
	// 但稳定 Shelf 下多日执行的选中结果不再固定。
	shuffleTier(unread, o.RNG)
	shuffleTier(finished, o.RNG)

	probe := func(tier []weread.ShelfBook) (string, bool) {
		for i, b := range tier {
			if i >= DefaultMaxSelectionProbes {
				return "", false
			}
			if _, err := o.Reader.Fetch(ctx, b.BookID); err != nil {
				// ticket 18：探测请求因 ctx 取消失败不是"书不可用"证据——停止探测，
				// 由 failBookSelection 的取消检查统一退出（不写 failed 终态、
				// 不发通知，当天无终态）。
				if ctx.Err() != nil {
					return "", false
				}
				o.Logger.Info("候选书探测不可用，换下一本", "book_id", b.BookID, "err", err)
				continue
			}
			o.Logger.Info("自动选书命中", "book_id", b.BookID, "book_title", b.Title)
			return b.BookID, true
		}
		return "", false
	}

	if id, ok := probe(unread); ok {
		return id, nil
	}
	if id, ok := probe(finished); ok {
		return id, nil
	}
	return "", r.failBookSelection(ctx, taskDate, len(shelf), len(unread), len(finished))
}

// shuffleTier 对候选层就地 Fisher-Yates 随机化（ticket 13）：使用注入 RNG（决策
// #4：RNG 是领域真实依赖，随机化是产品特性）。少于 2 个元素时无操作。
func shuffleTier(tier []weread.ShelfBook, rng *rand.Rand) {
	rng.Shuffle(len(tier), func(i, j int) {
		tier[i], tier[j] = tier[j], tier[i]
	})
}

// failBookSelection 输出自动选书失败的失败结果：failed 终态先落盘、再发失败通知
// （spec 决策 #9/#10：失败阶段、主要错误、已尝试动作；通知失败不影响 Task 结果；
// 终态持久化失败 → 返回持久化错误、不发通知，#15）。返回的错误供调用方报告（最终
// 由 CLI 以非零退出码呈现）。taskDate 是 Task 开始日（终态/通知日期用）。
func (r *Runner) failBookSelection(ctx context.Context, taskDate string, shelfCount, unreadCount, finishedCount int) error {
	// ticket 18：取消识别先于"无可用书"终态——ctx 已取消时（探测间取消、探测请求
	// 以 ctx 错误失败后耗尽分层）不是书可用性数据问题：直接退出，不写 failed
	// 终态、不发通知（不经 finalizeTransient 的失败出口在这里检查）。
	if ctx.Err() != nil {
		return cancelledExit(ctx)
	}
	o := r.opts
	detail := fmt.Sprintf("书架 %d 本（未读完 %d 本、已读完 %d 本）均无法建立 Reader Context（探测上限 %d）",
		shelfCount, unreadCount, finishedCount, DefaultMaxSelectionProbes)
	if err := r.finalize(ctx, terminal.State{LastTaskDate: taskDate, LastTaskResult: terminal.ResultFailed}, func(ctx context.Context) error {
		return o.Notify.NotifyFailure(ctx, notify.Failure{
			Date:    taskDate,
			Stage:   StageBookSelection,
			Error:   detail,
			Actions: []string{ActionShelfFilter, ActionProbeReaderContext},
		})
	}); err != nil {
		return err
	}
	return fmt.Errorf("自动选书失败：%s", detail)
}
