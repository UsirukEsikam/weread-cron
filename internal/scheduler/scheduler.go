// 本文件实现 daemon 主循环。
//
// 跨重启无状态（ADR-0002）：调度不持久化任何"已排定"状态，每次迭代都从 Terminal
// State 现况出发重新计算——重启后若当天无终态且窗口未过，自然在剩余窗口内重排；
// 已有终态则排次日。错过的整天不补跑。
package scheduler

import (
	"context"
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

// replanMinGap 是暂时性失败后的最短重排间隔（内部默认，不暴露为配置，ADR-0005）。
// 失败 Task 通常至少耗时数秒～数分钟，间隔避免窗口末尾 next==now 的紧循环
// （[now, 结束] 随机可能再次指回 now，sleepUntil 立即返回 → 背靠背重跑）。
const replanMinGap = time.Minute

// sleepChunk 是 sleepUntil 的分片上限。clock.Sleep 不感知 ctx（clock 抽象对 Task
// 的语义是"不可打断"，注释见 clock.Clock），分片保证 SIGINT/SIGTERM 的取消延迟
// 有上界（≤ sleepChunk；2s 在取消延迟与唤醒频率之间取平衡）。
const sleepChunk = 2 * time.Second

// TaskRunner 执行一次 Task（生产 = internal/app.App；测试注入 fake；ADR-0006）。
// daemon 只负责"到点执行"，Task 内部的终态落盘/通知由 Task 自身完成（spec 决策 #9）。
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
		if err := sleepUntil(ctx, d.clk, next); err != nil {
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

		d.log.Info("已到启动时刻，执行 Task",
			"at", d.clk.Now().In(d.cfg.TZ).Format(dayLayout+" 15:04:05"))
		if _, err := d.task.RunTask(ctx); err != nil {
			// Task 失败但被取消：正常退出。
			if ctx.Err() != nil {
				d.log.Info("daemon 退出")
				return nil
			}
			// 未形成终态（暂时性失败）→ 循环重排：窗口未过则在剩余窗口内再随机
			// 一次（用户故事 #17）；窗口已过或已形成 failed 终态 → 排次日。
			d.log.Warn("Task 执行失败（未形成终态则窗口内重排；已形成终态则排次日）", "err", err)
			// 最短重排间隔：防止窗口末尾 next 再次指回 now 的紧循环。
			if err := sleepUntil(ctx, d.clk, d.clk.Now().Add(replanMinGap)); err != nil {
				d.log.Info("daemon 退出")
				return nil
			}
			continue
		}
		d.log.Info("Task 完成，排定次日")
	}
}

// schedule 读取 Terminal State 并计算下次启动；同时输出决策依据日志
// （当天已有终态 → 跳过今日；无终态 → 今日剩余窗口/明天窗口）。
func (d *Daemon) schedule() (time.Time, error) {
	now := d.clk.Now()
	st, has, err := d.term.Load()
	if err != nil {
		return time.Time{}, fmt.Errorf("读取 Terminal State 失败: %w", err)
	}
	next, err := NextStart(now, Window{Start: d.cfg.WindowStart, End: d.cfg.WindowEnd}, st, d.cfg.TZ, d.rng)
	if err != nil {
		return time.Time{}, err
	}
	if has && d.todayTerminal(st, now) {
		d.log.Info("当天已有终态，跳过今日自动执行", "result", st.LastTaskResult)
	}
	d.log.Info("已排定下次 Task 启动", "at", next.In(d.cfg.TZ).Format(dayLayout+" 15:04:05"))
	return next, nil
}

// terminalDone 报告今天（cfg.TZ）是否已形成终态（启动前再校验用）。
func (d *Daemon) terminalDone() (bool, error) {
	st, has, err := d.term.Load()
	if err != nil {
		return false, fmt.Errorf("读取 Terminal State 失败: %w", err)
	}
	return has && d.todayTerminal(st, d.clk.Now()), nil
}

// todayTerminal 报告 st 是否为 now 当天（cfg.TZ）的终态（终态日期匹配的唯一定义）。
func (d *Daemon) todayTerminal(st terminal.State, now time.Time) bool {
	return st.LastTaskDate == now.In(d.cfg.TZ).Format(dayLayout)
}

// sleepUntil 阻塞到 until。以 sleepChunk 分片睡眠，每次醒来检查 ctx：
// ctx 取消（SIGINT/SIGTERM）时返回该错误；到点返回 nil（取消延迟 ≤ sleepChunk）。
func sleepUntil(ctx context.Context, c clock.Clock, until time.Time) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		d := until.Sub(c.Now())
		if d <= 0 {
			return nil
		}
		if d > sleepChunk {
			d = sleepChunk
		}
		c.Sleep(d)
	}
}
