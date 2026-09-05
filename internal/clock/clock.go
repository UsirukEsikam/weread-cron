// Package clock 定义时间源抽象（ADR-0006：clock 是领域真实依赖，经构造器注入）。
//
// Clock 提供 Now 与 Sleep：Task 的 report 节奏、rt 计算（ADR-0004）与 Terminal State
// 日期都以此为准；生产使用 Real，测试使用 Fake（确定性推进）。
package clock

import "time"

// Clock 抽象时间流逝。
type Clock interface {
	// Now 返回当前时刻。
	Now() time.Time
	// Sleep 阻塞至少 d 的时间（ctx 取消不打断；Task 对取消的响应在别的层）。
	Sleep(d time.Duration)
}

// Real 是生产实现：直接使用 time.Now / time.Sleep。
type Real struct{}

// Now 返回 time.Now()。
func (Real) Now() time.Time { return time.Now() }

// Sleep 调用 time.Sleep。
func (Real) Sleep(d time.Duration) { time.Sleep(d) }
