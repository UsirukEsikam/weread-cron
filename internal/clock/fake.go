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
