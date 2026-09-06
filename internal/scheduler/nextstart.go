// Package scheduler 是 daemon 的调度层：nextStart 纯决策函数（ADR-0001/0002 的
// 调度语义）与 daemon 主循环（恢复 Login Session → 计算下次启动 → 睡眠到点 →
// 启动前再校验当天终态与 Run Window → 执行 Task → 排定次日，spec 决策 #8/#9；
// ticket 17：睡眠/挂起越过窗口结束则不启动，改排次日）。
package scheduler

import (
	"fmt"
	"math/rand"
	"time"

	"weread-cron/internal/terminal"
)

// Window 是每日 Run Window（分钟数，自当天 00:00 起）。只约束 Task 的**开始**时刻
// （ADR-0001）：任务启动后读满目标时长，允许越过窗口结束点。Start==End = 固定启动
// 时刻（合法）。
type Window struct {
	Start int
	End   int
}

// Valid 报告窗口是否合法：0 <= Start <= End 且 End < 24:00。
// Start > End（跨午夜窗口）是配置错误（用户故事 #5）；Start==End 合法。
func (w Window) Valid() bool {
	return w.Start >= 0 && w.Start <= w.End && w.End < 24*60
}

func (w Window) String() string {
	return fmt.Sprintf("%02d:%02d-%02d:%02d", w.Start/60, w.Start%60, w.End/60, w.End%60)
}

// NextStart 是调度决策的纯函数（spec 决策 #8；ADR-0001/0002）：
//
//   - Run Window 只约束开始时刻；"窗口已过"按 now 与当天窗口起始点的关系判定；
//   - 当天已有终态（success 或 failed，按 last.LastTaskDate 与天名匹配）→
//     明天窗口内随机一次（终态门控，ADR-0002：当天不再自动执行）；
//   - 无终态且窗口尚未开始 → [开始, 结束] 内随机一次（正常每日调度与窗口前的
//     异常启动同规则）；
//   - 无终态且 now 已在窗口内 → 立即返回 now（异常启动/重启的恢复语义，ticket 24：
//     不再从 [now, 结束] 随机——Task 一旦启动不再 whole-Task 自动重排，窗口内重排
//     语义已不存在）；
//   - 窗口已过 → 明天窗口内随机（错过整天不补跑）；
//   - start==end → 固定启动时刻（常量窗口退化为一个时间点）。
//
// tz 决定"今天/明天"与窗口边界（ADR-0003：TZ 决定一切时间语义）；rng 注入随机源
// （ADR-0006：随机化是产品特性，同种子同结果保证调度确定性可测）。
// 非法窗口（跨午夜/越界）返回错误（配置错误；config.Load 已前置校验，此处为
// 防御性复检）。
func NextStart(now time.Time, win Window, last terminal.State, tz *time.Location, rng *rand.Rand) (time.Time, error) {
	if !win.Valid() {
		return time.Time{}, fmt.Errorf("非法 Run Window %s（跨午夜窗口是配置错误；start==end 为固定时刻）", win)
	}
	if tz == nil {
		tz = time.UTC
	}
	if rng == nil {
		rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	t := now.In(tz)
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, tz)
	tomorrow := time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, tz)

	// 终态门控：当天已有终态（无论 success/failed）→ 明天窗口内随机。
	if last.LastTaskDate == terminal.TodayKey(t, tz) {
		return randomInWindow(tomorrow, win, rng), nil
	}

	start := day.Add(minutes(win.Start))
	end := dayWindowEnd(day, win)
	switch {
	case now.Before(start):
		// 窗口尚未开始：今天窗口内随机。
		return randomInRange(start, end, rng), nil
	case now.After(end):
		// 窗口已过：明天窗口内随机（错过不补跑）。
		return randomInWindow(tomorrow, win, rng), nil
	default:
		// 无终态且 now 已在窗口内（含恰在开始/结束点）：立即执行（异常启动/重启的
		// 恢复语义，ticket 24；用户故事 #14/#17）——不改变常驻 daemon 的每日随机
		// （窗口前启动仍走完整窗口随机；Task 完成后终态门控排次日）。
		return now, nil
	}
}

// CanStartAt 报告 now 是否仍是可启动的 Task 启动时刻（daemon 睡眠返回后的 Run
// Window 复查，ticket 17；ADR-0001：窗口只约束 Task 开始时刻）。
//
// 判定以"计划启动日"（planned 所在日）的窗口为基准：
//   - now 未越过窗口结束（now ≤ 计划日 end）→ 可启动（含恰在结束点；与 NextStart
//     的"窗口内立即执行"边界一致）；
//   - now 已越过窗口结束，但相对 planned 的迟到 ≤ startGrace（睡眠准时返回、仅真实
//     时钟过冲）→ 可启动（start==end 固定时刻的常规唤醒即此情形，不得误判为挂起）；
//   - 其余 → 不可启动：真实时间跳变（主机挂起/恢复）已使启动时刻越出窗口，越出即
//     "错过当天"，daemon 改排次日（错过不补跑，ADR-0002）。
//
// 前置条件：now ≥ planned（sleepUntil 只在其目标时刻到达或越过之后返回；调用点即
// daemon 的睡眠返回处）。窗口配置已由 config 校验（Window.Valid），本函数不复检。
func CanStartAt(now, planned time.Time, win Window) bool {
	plannedDay := time.Date(planned.Year(), planned.Month(), planned.Day(), 0, 0, 0, 0, planned.Location())
	end := dayWindowEnd(plannedDay, win)
	if !now.After(end) {
		return true
	}
	return now.Sub(planned) <= startGrace
}

// dayWindowEnd 返回 day（当天 00:00）的窗口结束时刻（与 minutes 同级窗口算术
// 助手：NextStart 与 CanStartAt 共用同一"窗口结束"定义）。
func dayWindowEnd(day time.Time, win Window) time.Time {
	return day.Add(minutes(win.End))
}

// startGrace 是窗口复查对"准时唤醒的调度迟到"的容差（ticket 17）：真实时钟下
// time.Sleep 过冲毫秒级，且复查前还有终态载入等 I/O，启动时刻可能比计划启动时刻
// 晚一点；start==end 固定时刻窗口的计划启动时刻==窗口结束点，任何过冲都会被
// "已越过窗口结束"判为不可启动而整天错过（真实时钟破坏 ADR-0001 语义）。容差把
// 调度自身的迟到与"挂起/恢复造成的时间跳变"区分开：跳变以秒、分钟计，远大于
// 该值。
const startGrace = 5 * time.Second

// randomInWindow 在 day（当天 00:00）的窗口 [Start, End] 内随机取一时刻（秒级）。
func randomInWindow(day time.Time, win Window, rng *rand.Rand) time.Time {
	return randomInRange(day.Add(minutes(win.Start)), day.Add(minutes(win.End)), rng)
}

// randomInRange 在闭区间 [from, to] 内随机取一时刻（秒级；含两端）。
// 前置条件：from <= to（NextStart 的一切调用路径已保证）。
func randomInRange(from, to time.Time, rng *rand.Rand) time.Time {
	n := int(to.Sub(from)/time.Second) + 1
	return from.Add(time.Duration(rng.Intn(n)) * time.Second)
}

func minutes(m int) time.Duration { return time.Duration(m) * time.Minute }
