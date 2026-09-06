package clock

import (
	"sync"
	"time"
)

// Fake 是测试注入的确定性时钟：
//   - Sleep 立即把时间推进 d（不真实阻塞），因此按固定节奏的循环在测试中瞬时完成；
//   - Advance 用于模拟睡眠/挂起造成的时间跳变（report 循环之外的时钟前进），
//     使下一次 rt 计算能观察到超阈值间隔（ADR-0004 的异常间隔重建路径）。
type Fake struct {
	mu sync.Mutex
	t  time.Time
}

// NewFake 创建从 start 开始的假时钟。
func NewFake(start time.Time) *Fake {
	return &Fake{t: start}
}

// Now 返回当前假时间。
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

// Sleep 把时间推进 d（立即返回）。
func (f *Fake) Sleep(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// Advance 把时间跳进 d（模拟挂起/暂停）。
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// Capped 是带时间上限的 Fake：Sleep 推进到 cap 后停止推进（后续 Sleep 在轻微
// 真实等待后原样返回，防止忙等）。用于 daemon 测试：自动推进的 Fake 会瞬时越过
// 整段长睡眠——测试的取消/终态写入与 daemon 醒来之间的顺序无法确定；Capped 把
// 睡眠钉在上限，测试可在任意时刻确定地取消。
//
// 生产代码不感知 Capped；它是与 Fake 同类的测试注入时钟（ADR-0006：clock 是
// 领域真实依赖，测试经构造器注入确定性实现）。
type Capped struct {
	*Fake
	cap  time.Time
	hook func(time.Time) // 每次推进后调用（测试注入：写入终态等）
}

// NewCapped 创建从 start 开始、至多推进到 cap 的时钟。
func NewCapped(start, cap time.Time, hook func(time.Time)) *Capped {
	return &Capped{Fake: NewFake(start), cap: cap, hook: hook}
}

// Sleep 推进 d（同 Fake），但不越过 cap：越过时推进到 cap 并返回（此后不再前进，
// 直到外部取消——clock.WaitUntil 每轮检查 ctx，取消延迟 ≤ 一个分片加轻微等待）。
func (c *Capped) Sleep(d time.Duration) {
	now := c.Fake.Now()
	if next := now.Add(d); next.After(c.cap) {
		c.Fake.Sleep(c.cap.Sub(now))
		time.Sleep(time.Millisecond) // 防忙等；daemon 的 clock.WaitUntil 每次醒来检查 ctx
		return
	}
	c.Fake.Sleep(d)
	if c.hook != nil {
		c.hook(c.Fake.Now())
	}
}
