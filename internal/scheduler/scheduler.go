// 本文件实现 daemon 主循环。
//
// 跨重启无状态（ADR-0002）：调度不持久化任何"已排定"状态，每次迭代都从 Terminal
// State 现况出发重新计算——重启后若当天无终态且窗口未过，按异常启动规则立即执行
// （ticket 24：不再从剩余窗口随机）；已有终态则排次日。错过的整天不补跑。
// ticket 24：无 whole-Task 自动重排——实际开始执行的 Task 以对应终态结束，
// daemon 在 Task 失败后不再于窗口内启动另一个完整 Task。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"weread-cron/internal/clock"
	"weread-cron/internal/config"
	"weread-cron/internal/session"
	"weread-cron/internal/task"
	"weread-cron/internal/terminal"
)

// replanMinGap 是 ErrTaskRunning 并发拒绝后重试守卫的最短间隔（内部默认，不暴露为
// 配置，ADR-0005）。ticket 24 起 daemon 不再 whole-Task 重排（Task 失败直接排次日），
// 本间隔只用于并发拒绝后的再次尝试：NextStart 的"窗口内立即执行"会马上指回 now，
// 间隔避免 clock.WaitUntil 立即返回 → 背靠背重试的紧循环。
const replanMinGap = time.Minute

// TaskRunner 执行一次 Task（生产 = internal/app.App；测试注入 fake；ADR-0006）。
// daemon 只负责"到点执行"，Task 内部的终态落盘/通知由 Task 自身完成（spec 决策 #9）。
// ticket 24：实际开始执行的 Task 必然形成对应终态（success → success 终态；最终失败
// → failed 终态）；调度层不再进行 whole-Task 自动重排。
type TaskRunner interface {
	RunTask(ctx context.Context) (task.Result, error)
}

// Deps 是 daemon 的注入点（ADR-0006）；零值字段使用生产默认。
type Deps struct {
	// Clock 是时间源（生产 Real；测试 Fake，Sleep 自动推进）。
	Clock clock.Clock
	// RNG 是随机源（下次启动时刻的随机化；同种子同决策序列）。
	RNG *rand.Rand
	// Task 是到点执行的一次 Task；nil 时 Run 在启动后报错（装配错误）。
	Task TaskRunner
	// Sessions 是 Login Session 存储（启动恢复与失败快速感知）。
	Sessions session.Store
	// Terminal 是 Terminal State 存储（读门控；写由 Task 完成）。
	Terminal terminal.Store
	// Logger 是日志输出。
	Logger *slog.Logger
}

// Daemon 是每日调度循环。
type Daemon struct {
	cfg  *config.Config
	clk  clock.Clock
	rng  *rand.Rand
	task TaskRunner
	sess session.Store
	term terminal.Store
	log  *slog.Logger
}

// New 构造 daemon；零值 Deps 字段回退生产默认。
func New(cfg *config.Config, deps Deps) *Daemon {
	if deps.Clock == nil {
		deps.Clock = clock.Real{}
	}
	if deps.RNG == nil {
		deps.RNG = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	if deps.Sessions == nil {
		deps.Sessions = session.NewFileStore(cfg.DataDir)
	}
	if deps.Terminal == nil {
		deps.Terminal = terminal.NewFileStore(cfg.DataDir)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Daemon{
		cfg:  cfg,
		clk:  deps.Clock,
		rng:  deps.RNG,
		task: deps.Task,
		sess: deps.Sessions,
		term: deps.Terminal,
		log:  deps.Logger,
	}
}

// Run 运行 daemon 直到 ctx 被取消（SIGINT/SIGTERM 由 main 注入；取消视为正常退出，
// 返回 nil）。循环内部错误（配置非法、Terminal State 不可读、Task 未装配）直接返回，
// 由 CLI 层以非零退出码报告。
func (d *Daemon) Run(ctx context.Context) error {
	d.log.Info("daemon 启动",
		"tz", d.cfg.TZName,
		"window", d.cfg.WindowString(),
		"data_dir", d.cfg.DataDir)

	// 循环第 1 步：恢复 Login Session（spec 决策 #2、用户故事 #7/#8）。失败即启动
	// 失败（缺 Cookie、会话文件损坏/为空），不在 02:00 才发现。
	if _, err := session.New(d.sess, d.cfg.Cookie); err != nil {
		return fmt.Errorf("恢复 Login Session 失败: %w", err)
	}
	if d.task == nil {
		return fmt.Errorf("daemon 未装配 Task 执行器")
	}

	for {
		next, err := d.schedule()
		if err != nil {
			return err
		}
		if err := clock.WaitUntil(ctx, d.clk, next); err != nil {
			d.log.Info("daemon 退出")
			return nil
		}

		// 启动前再校验当天终态（ADR-0002/用户故事 #15）：睡眠期间可能被
		// `weread-cron run` 手动执行形成终态，此时跳过自动执行。
		done, err := d.terminalDone()
		if err != nil {
			return err
		}
		if done {
			d.log.Info("当天已形成终态（睡眠期间由 run 等执行），跳过自动执行")
			continue
		}

		// 启动前复查 Run Window（ticket 17）：睡眠期间主机挂起/恢复造成时间跳变时，
		// 睡眠返回后的当前时刻可能已越过当天窗口结束——Run Window 只约束 Task 开始
		// 时刻（ADR-0001），越出窗口的启动时刻不合规：跳过本次自动执行，由下一轮
		// schedule 排定次日（错过不补跑，ADR-0002）。start==end 固定时刻的准时唤醒
		// 由 CanStartAt 的调度容差覆盖，不受影响。
		if !CanStartAt(d.clk.Now(), next, d.window()) {
			d.log.Info("已越过 Run Window 结束（睡眠期间时间跳变），跳过自动执行，排定次日")
			continue
		}

		d.log.Info("已到启动时刻，执行 Task",
			"at", d.clk.Now().In(d.cfg.TZ).Format(terminal.DayLayout+" 15:04:05"))
		if _, err := d.task.RunTask(ctx); err != nil {
			// Task 失败但被取消：正常退出。
			if ctx.Err() != nil {
				d.log.Info("daemon 退出")
				return nil
			}
			// 并发守卫拒绝（ticket 11/ADR-0008）：另一进程（手动 run/books 或其他
			// daemon）持有同一 /data 的 Task 锁。这不是失败——本次自动执行被跳过，
			// 最短重排间隔后重试；运行中的 Task 不受影响。
			if errors.Is(err, task.ErrTaskRunning) {
				d.log.Info("另一进程正在运行 Task，本次自动执行被拒绝（最短间隔后重试）")
				if err := clock.WaitUntil(ctx, d.clk, d.clk.Now().Add(replanMinGap)); err != nil {
					d.log.Info("daemon 退出")
					return nil
				}
				continue
			}
			// 无 whole-Task 自动重排（ticket 24）：实际开始执行的 Task 以对应终态
			// 结束——正常失败已写 failed 终态，当天不再自动执行，排定次日；
			// 也不存在"未形成终态则窗口内重排"的分支。
			d.log.Warn("Task 执行失败（当天不再自动重排，排定次日）", "err", err)
			// 防御（终态持久化失败等存储级错误不会留下终态）：直接排定次日窗口内的
			// 随机启动点再进入循环——NextStart 的"窗口内立即执行"（异常启动/重启恢复
			// 语义，ticket 24）只在进程启动/重启时适用，不得把本次失败误判为重启而
			// 再次启动当天第二个完整 Task。
			next, err := d.nextDayStart()
			if err != nil {
				return err
			}
			d.log.Info("Task 失败后下次启动排定于次日",
				"at", next.In(d.cfg.TZ).Format(terminal.DayLayout+" 15:04:05"))
			if err := clock.WaitUntil(ctx, d.clk, next); err != nil {
				d.log.Info("daemon 退出")
				return nil
			}
			continue
		}
		d.log.Info("Task 完成，排定次日")
	}
}

// nextDayStart 计算次日窗口内随机启动点（Task 失败后"排定次日"用，ticket 24）：
// 以次日 00:00 为基准走 NextStart 的"窗口前"分支 → 完整窗口内随机（与正常每日随机
// 同一规则）。不经过 schedule() 的当日分支——终态持久化失败等存储级错误不会留下
// 终态，此时 NextStart 的"窗口内立即执行"（异常启动/重启恢复语义）只适用于进程
// 启动/重启，不得把失败误判为重启而再次启动当天第二个完整 Task。
func (d *Daemon) nextDayStart() (time.Time, error) {
	t := d.clk.Now().In(d.cfg.TZ)
	tomorrow := time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, d.cfg.TZ)
	next, err := NextStart(tomorrow, d.window(), terminal.State{}, d.cfg.TZ, d.rng)
	if err != nil {
		return time.Time{}, fmt.Errorf("计算次日启动时刻失败: %w", err)
	}
	return next, nil
}

// schedule 读取 Terminal State 并计算下次启动；同时输出决策依据日志
// （当天已有终态 → 跳过今日；无终态 → 今日立即/今日窗口/明天窗口）。
func (d *Daemon) schedule() (time.Time, error) {
	now := d.clk.Now()
	st, has, err := d.term.Load()
	if err != nil {
		return time.Time{}, fmt.Errorf("读取 Terminal State 失败: %w", err)
	}
	next, err := NextStart(now, d.window(), st, d.cfg.TZ, d.rng)
	if err != nil {
		return time.Time{}, err
	}
	if has && d.todayTerminal(st, now) {
		d.log.Info("当天已有终态，跳过今日自动执行", "result", st.LastTaskResult)
	}
	d.log.Info("已排定下次 Task 启动", "at", next.In(d.cfg.TZ).Format(terminal.DayLayout+" 15:04:05"))
	return next, nil
}

// window 返回配置的 Run Window（Start==End = 固定时刻；合法性由 config 校验）。
func (d *Daemon) window() Window {
	return Window{Start: d.cfg.WindowStart, End: d.cfg.WindowEnd}
}

// terminalDone 报告今天（cfg.TZ）是否已形成终态（启动前再校验用）。
func (d *Daemon) terminalDone() (bool, error) {
	st, has, err := d.term.Load()
	if err != nil {
		return false, fmt.Errorf("读取 Terminal State 失败: %w", err)
	}
	return has && d.todayTerminal(st, d.clk.Now()), nil
}

// todayTerminal 报告 st 是否为 now 当天（cfg.TZ）的终态（终态日期匹配的唯一定义
// 在 terminal.IsToday；此处是调度侧的本地别名）。
func (d *Daemon) todayTerminal(st terminal.State, now time.Time) bool {
	return terminal.IsToday(st, now, d.cfg.TZ)
}
