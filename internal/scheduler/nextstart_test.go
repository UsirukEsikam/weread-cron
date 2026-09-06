// Package scheduler 测试：nextStart/CanStartAt 纯决策函数（ADR-0001/0002；ticket
// 17）——窗口只约束开始时间、终态门控、窗口内立即执行、错过不补跑、start==end
// 固定时刻、跨午夜窗口为配置错误、TZ 决定日期边界、启动前 Run Window 复查。
// 直接在调度层单元测试（spec Testing Decisions：纯逻辑不形成额外 seam）。
package scheduler

import (
	"math/rand"
	"testing"
	"time"

	"weread-cron/internal/terminal"
)

// testLoc 是测试固定时区（UTC+8；与 app 包 testTZ 同值的独立定义）。
var testLoc = time.FixedZone("CST", 8*3600)

// win 构造 Window（分钟）。
func win(start, end int) Window { return Window{Start: start, End: end} }

// at 构造 testLoc 下的测试时刻。
func at(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, testLoc)
}

// assertInRange 断言 got ∈ [from, to]。
func assertInRange(t *testing.T, got, from, to time.Time) {
	t.Helper()
	if got.Before(from) || got.After(to) {
		t.Errorf("next = %v 不在 [%v, %v]", got, from, to)
	}
}

// TestNextStartWindowConstrainsStartOnly 验证窗口只约束开始时间的四个分支：
// 未到窗口、窗口内、恰在窗口结束、窗口已过。
func TestNextStartWindowConstrainsStartOnly(t *testing.T) {
	cases := []struct {
		name     string
		now      time.Time
		window   Window
		wantFrom time.Time
		wantTo   time.Time
		// wantDayTZ 为期望落在的日期（testLoc 下 YYYY-MM-DD）。
		wantDayTZ string
	}{
		{
			name:      "窗口未开始：随机在 [开始, 结束] 的当天",
			now:       at(2025, 9, 6, 0, 30),
			window:    win(60, 180), // 01:00-03:00
			wantFrom:  at(2025, 9, 6, 1, 0),
			wantTo:    at(2025, 9, 6, 3, 0),
			wantDayTZ: "2025-09-06",
		},
		{
			name:      "窗口内：立即执行（异常启动/重启恢复语义，ticket 24）",
			now:       at(2025, 9, 6, 1, 30),
			window:    win(60, 180),
			wantFrom:  at(2025, 9, 6, 1, 30),
			wantTo:    at(2025, 9, 6, 1, 30),
			wantDayTZ: "2025-09-06",
		},
		{
			name:      "恰在窗口结束：立即执行（不再含边界随机）",
			now:       at(2025, 9, 6, 3, 0),
			window:    win(60, 180),
			wantFrom:  at(2025, 9, 6, 3, 0),
			wantTo:    at(2025, 9, 6, 3, 0),
			wantDayTZ: "2025-09-06",
		},
		{
			name:      "窗口已过：明天窗口内随机（错过不补跑）",
			now:       at(2025, 9, 6, 4, 0),
			window:    win(60, 180),
			wantFrom:  at(2025, 9, 7, 1, 0),
			wantTo:    at(2025, 9, 7, 3, 0),
			wantDayTZ: "2025-09-07",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for seed := int64(1); seed <= 5; seed++ {
				next, err := NextStart(tc.now, tc.window, terminal.State{}, testLoc, rand.New(rand.NewSource(seed)))
				if err != nil {
					t.Fatalf("NextStart 返回错误: %v", err)
				}
				assertInRange(t, next, tc.wantFrom, tc.wantTo)
				if got := next.In(testLoc).Format(terminal.DayLayout); got != tc.wantDayTZ {
					t.Errorf("日期 = %s，期望 %s（next=%v）", got, tc.wantDayTZ, next)
				}
			}
		})
	}
}

// TestNextStartStartEqualsEndIsFixedTime 验证 start==end = 固定启动时刻。
func TestNextStartStartEqualsEndIsFixedTime(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			name: "窗口前：当天固定时刻",
			now:  at(2025, 9, 6, 0, 30),
			want: at(2025, 9, 6, 2, 0),
		},
		{
			name: "恰为固定时刻：即可开始",
			now:  at(2025, 9, 6, 2, 0),
			want: at(2025, 9, 6, 2, 0),
		},
		{
			name: "窗口已过：次日同一固定时刻",
			now:  at(2025, 9, 6, 3, 0),
			want: at(2025, 9, 7, 2, 0),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next, err := NextStart(tc.now, win(120, 120), terminal.State{}, testLoc, rand.New(rand.NewSource(1)))
			if err != nil {
				t.Fatalf("NextStart 返回错误: %v", err)
			}
			if !next.Equal(tc.want) {
				t.Errorf("next = %v，期望固定时刻 %v", next, tc.want)
			}
		})
	}
}

// TestNextStartTerminalBlocksToday 验证终态门控（ADR-0002）：
// success 与 failed 都阻止当天再自动执行；昨天（或更早）的终态不阻塞今天。
func TestNextStartTerminalBlocksToday(t *testing.T) {
	now := at(2025, 9, 6, 1, 30)
	cases := []struct {
		name     string
		last     terminal.State
		wantDay  string
		notToday bool // 期望结果不落在 2025-09-06
	}{
		{
			name:     "今日 success 终态 → 明天",
			last:     terminal.State{LastTaskDate: "2025-09-06", LastTaskResult: terminal.ResultSuccess},
			wantDay:  "2025-09-07",
			notToday: true,
		},
		{
			name:     "今日 failed 终态 → 明天",
			last:     terminal.State{LastTaskDate: "2025-09-06", LastTaskResult: terminal.ResultFailed},
			wantDay:  "2025-09-07",
			notToday: true,
		},
		{
			name:    "昨日 success 终态 → 今天正常调度",
			last:    terminal.State{LastTaskDate: "2025-09-05", LastTaskResult: terminal.ResultSuccess},
			wantDay: "2025-09-06",
		},
		{
			name:     "今日日期但结果未知 → 按已有终态处理（防御语义）",
			last:     terminal.State{LastTaskDate: "2025-09-06", LastTaskResult: "weird"},
			wantDay:  "2025-09-07",
			notToday: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for seed := int64(1); seed <= 3; seed++ {
				next, err := NextStart(now, win(60, 180), tc.last, testLoc, rand.New(rand.NewSource(seed)))
				if err != nil {
					t.Fatalf("NextStart 返回错误: %v", err)
				}
				if got := next.In(testLoc).Format(terminal.DayLayout); got != tc.wantDay {
					t.Errorf("日期 = %s，期望 %s（next=%v）", got, tc.wantDay, next)
				}
				if tc.notToday && next.In(testLoc).Format(terminal.DayLayout) == "2025-09-06" {
					t.Errorf("有终态时不应调度在今天：%v", next)
				}
			}
		})
	}
}

// TestNextStartRejectsInvalidWindow 验证跨午夜/越界窗口是配置错误；
// start==end（固定时刻）合法、start==end 且 now 在窗口前也可正常返回。
func TestNextStartRejectsInvalidWindow(t *testing.T) {
	now := at(2025, 9, 6, 0, 30)
	bad := []Window{
		win(23*60, 60), // 跨午夜：23:00-01:00
		win(180, 60),   // start > end 且不等
		win(-1, 60),    // 越界
		win(60, 24*60), // 越界
	}
	for _, w := range bad {
		if _, err := NextStart(now, w, terminal.State{}, testLoc, rand.New(rand.NewSource(1))); err == nil {
			t.Errorf("窗口 %+v 应返回错误（配置错误），却成功了", w)
		}
	}
	if _, err := NextStart(now, win(120, 120), terminal.State{}, testLoc, rand.New(rand.NewSource(1))); err != nil {
		t.Errorf("start==end 固定时刻应合法，却返回错误: %v", err)
	}
}

// TestNextStartDeterministicWithSeed 验证同种子 → 同结果（随机源注入决定确定性）。
func TestNextStartDeterministicWithSeed(t *testing.T) {
	now := at(2025, 9, 6, 1, 30)
	a, err := NextStart(now, win(60, 180), terminal.State{}, testLoc, rand.New(rand.NewSource(42)))
	if err != nil {
		t.Fatal(err)
	}
	b, err := NextStart(now, win(60, 180), terminal.State{}, testLoc, rand.New(rand.NewSource(42)))
	if err != nil {
		t.Fatal(err)
	}
	if !a.Equal(b) {
		t.Errorf("同种子结果不一致: %v vs %v", a, b)
	}
}

// TestCanStartAt 验证启动前 Run Window 复查的纯决策（ticket 17；ADR-0001：窗口
// 只约束 Task 开始时刻）：睡眠返回后当前时刻仍未越过"计划启动日"的窗口结束 →
// 可启动（含恰在结束点）；已越过窗口结束但相对计划启动时刻的迟到在调度容差内
// （真实时钟睡眠过冲，毫秒级）→ 可启动（start==end 固定时刻的常规唤醒即此情形，
// 不得误判为挂起）；真实时间跳变（主机挂起/恢复）越过窗口结束 → 不可启动，
// daemon 改排次日。
func TestCanStartAt(t *testing.T) {
	win1 := win(23*60+30, 23*60+59) // [23:30, 23:59]
	fixed := win(120, 120)          // 固定 02:00
	cases := []struct {
		name    string
		now     time.Time
		planned time.Time
		window  Window
		want    bool
	}{
		{
			name:    "窗口内正常唤醒：可启动",
			now:     at(2025, 9, 6, 23, 45),
			planned: at(2025, 9, 6, 23, 45),
			window:  win1,
			want:    true,
		},
		{
			name:    "恰在窗口结束点：可启动",
			now:     at(2025, 9, 6, 23, 59),
			planned: at(2025, 9, 6, 23, 50),
			window:  win1,
			want:    true,
		},
		{
			name:    "挂起跳变越过窗口结束：不可启动（改排次日）",
			now:     at(2025, 9, 7, 0, 5),
			planned: at(2025, 9, 6, 23, 45),
			window:  win1,
			want:    false,
		},
		{
			name:    "计划=窗口结束点、真实时钟过冲迟到 2s（容差内）：可启动",
			now:     at(2025, 9, 6, 23, 59).Add(2 * time.Second),
			planned: at(2025, 9, 6, 23, 59),
			window:  win1,
			want:    true,
		},
		{
			name:    "计划=窗口结束点、迟到超过容差：不可启动",
			now:     at(2025, 9, 6, 23, 59).Add(startGrace + time.Second),
			planned: at(2025, 9, 6, 23, 59),
			window:  win1,
			want:    false,
		},
		{
			name:    "固定时刻：准时唤醒",
			now:     at(2025, 9, 6, 2, 0),
			planned: at(2025, 9, 6, 2, 0),
			window:  fixed,
			want:    true,
		},
		{
			name:    "固定时刻：真实时钟过冲迟到 3s（容差内）",
			now:     at(2025, 9, 6, 2, 0).Add(3 * time.Second),
			planned: at(2025, 9, 6, 2, 0),
			window:  fixed,
			want:    true,
		},
		{
			name:    "固定时刻：迟到超过容差",
			now:     at(2025, 9, 6, 2, 0).Add(startGrace + time.Second),
			planned: at(2025, 9, 6, 2, 0),
			window:  fixed,
			want:    false,
		},
		{
			name:    "固定时刻：挂起跳变越过",
			now:     at(2025, 9, 6, 3, 0),
			planned: at(2025, 9, 6, 2, 0),
			window:  fixed,
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanStartAt(tc.now, tc.planned, tc.window); got != tc.want {
				t.Errorf("CanStartAt(now=%v, planned=%v, %s) = %v，期望 %v",
					tc.now, tc.planned, tc.window, got, tc.want)
			}
		})
	}
}

// TestNextStartHonorsTZ 验证 TZ 决定日期边界（同一 UTC 时刻、同一窗口，
// 不同时区可能落不同日期）。
func TestNextStartHonorsTZ(t *testing.T) {
	now := time.Date(2025, 9, 6, 4, 0, 0, 0, time.UTC) // UTC 04:00
	fixed := win(9*60, 9*60)                           // 固定 09:00

	// UTC 视角：04:00 < 09:00，今天 09:00 UTC。
	nextUTC, err := NextStart(now, fixed, terminal.State{}, time.UTC, rand.New(rand.NewSource(1)))
	if err != nil {
		t.Fatal(err)
	}
	wantUTC := time.Date(2025, 9, 6, 9, 0, 0, 0, time.UTC)
	if !nextUTC.Equal(wantUTC) {
		t.Errorf("UTC 下 next = %v，期望 %v", nextUTC, wantUTC)
	}

	// UTC+8 视角：本地 12:00 > 09:00，窗口已过 → 明天 09:00 +08。
	nextSH, err := NextStart(now, fixed, terminal.State{}, testLoc, rand.New(rand.NewSource(1)))
	if err != nil {
		t.Fatal(err)
	}
	wantSH := time.Date(2025, 9, 7, 9, 0, 0, 0, testLoc)
	if !nextSH.Equal(wantSH) {
		t.Errorf("UTC+8 下 next = %v，期望 %v", nextSH, wantSH)
	}
}

// TestNextStartImmediateInsideWindow 验证异常启动/重启语义（ticket 24；用户故事
// #14/#17）：当天无终态且 now 已在窗口内 → 立即返回 now（与 RNG 种子无关），不再
// 从 [now, 结束] 随机；窗口尚未开始仍为完整窗口内随机（常驻 daemon 每日随机调度
// 不变）；已有终态时仍排次日（终态门控优先）。
func TestNextStartImmediateInsideWindow(t *testing.T) {
	inside := at(2025, 9, 6, 23, 40) // 窗口 [23:30, 23:59] 内
	for _, seed := range []int64{1, 2, 3, 42, 99} {
		next, err := NextStart(inside, win(23*60+30, 23*60+59), terminal.State{}, testLoc, rand.New(rand.NewSource(seed)))
		if err != nil {
			t.Fatalf("NextStart 返回错误: %v", err)
		}
		if !next.Equal(inside) {
			t.Errorf("窗口内无终态应立即执行：next = %v，期望 %v（RNG 种子 %d）", next, inside, seed)
		}
	}

	// 窗口前启动仍为完整窗口内随机（常驻 daemon 每日随机保持不变）。
	next, err := NextStart(at(2025, 9, 6, 0, 30), win(60, 180), terminal.State{}, testLoc, rand.New(rand.NewSource(7)))
	if err != nil {
		t.Fatal(err)
	}
	assertInRange(t, next, at(2025, 9, 6, 1, 0), at(2025, 9, 6, 3, 0))

	// 窗口内但有终态：终态门控优先 → 排次日（立即执行仅限无终态的异常恢复）。
	next, err = NextStart(inside, win(23*60+30, 23*60+59), terminal.State{LastTaskDate: "2025-09-06", LastTaskResult: terminal.ResultFailed}, testLoc, rand.New(rand.NewSource(7)))
	if err != nil {
		t.Fatal(err)
	}
	if got := next.In(testLoc).Format(terminal.DayLayout); got != "2025-09-07" {
		t.Errorf("有终态时窗口内不得立即执行，应排次日；next = %v", next)
	}
}
