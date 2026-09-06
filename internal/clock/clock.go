// Package clock 定义时间源抽象（ADR-0006：clock 是领域真实依赖，经构造器注入）。
//
// Clock 提供 Now 与 Sleep：Task 的 report 节奏、rt 计算（ADR-0004）与 Terminal State
// 日期都以此为准；生产使用 Real，测试使用 Fake（确定性推进）。
package clock

import (
	"context"
	"time"
)

// Clock 抽象时间流逝。
type Clock interface {
	// Now 返回当前时刻。
	Now() time.Time
	// Sleep 阻塞至少 d 的时间（ctx 取消不打断；调用方对取消的响应在别的层——
	// 见 WaitUntil）。
	Sleep(d time.Duration)
}

// Real 是生产实现：直接使用 time.Now / time.Sleep。
type Real struct{}

// Now 返回 time.Now()。
func (Real) Now() time.Time { return time.Now() }

// Sleep 调用 time.Sleep。
func (Real) Sleep(d time.Duration) { time.Sleep(d) }

// WaitChunk 是 WaitUntil 的分片上限（内部默认，ADR-0005）：Clock.Sleep 不感知
// ctx，分片 + 每片检查把取消（SIGINT/SIGTERM）响应延迟约束在 ≤ WaitChunk
// （2s 在取消延迟与唤醒频率之间取平衡）。daemon 睡眠与 Task 的 timed report
// 间隔等待共用同一上界（ticket 18 评审跟进：原两处逐字重复实现合并于此）。
const WaitChunk = 2 * time.Second

// WaitUntil 阻塞到 until，以 WaitChunk 分片睡眠、每次醒来检查 ctx：ctx 取消
// → 返回该错误（取消响应延迟 ≤ WaitChunk）；到点 → 返回 nil。Clock.Sleep 本身
// 不感知 ctx（本接口文档），分片检查由本函数承担——scheduler 的睡眠与 task 的
// timed report 间隔等待共用（原 sleepUntil / waitUntil 为逐字重复的私有实现，
// ticket 18 评审跟进合并）。
func WaitUntil(ctx context.Context, c Clock, until time.Time) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		d := until.Sub(c.Now())
		if d <= 0 {
			return nil
		}
		if d > WaitChunk {
			d = WaitChunk
		}
		c.Sleep(d)
	}
}
