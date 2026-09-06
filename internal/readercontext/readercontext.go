// Package readercontext 是 Reader Context 模块（spec 决策 #3）：从 Web Reader 页面
// 抓取 __INITIAL_STATE__ 并解析为 Reading Progress + Reader Context + 书名。
//
// ticket 05 交付 Fetch（Task 开始时建立）与 Refresh（有界恢复链的强制刷新）。
// ticket 06 引入 TTL 缓存与主动刷新：Fetch 在 TTL 内命中缓存（零网络请求）、过期时
// 重新抓取；Refresh 总是重新抓取。对上层接口保持不变（Fetch/Refresh 签名未变）。
package readercontext

import (
	"context"
	"fmt"
	"sync"
	"time"

	"weread-cron/internal/clock"
	"weread-cron/internal/weread"
)

// DefaultContextTTL 是 Reader Context 缓存的生存期。
// 参考默认值 ≈15 分钟（weread.koplugin 的 CONTEXT_TTL_SECONDS）；**不是已确认的
// 服务器协议常量**——表述遵守验证清单 #7/#8 与 ADR-0004 的同一口径，正式复核可修正。
const DefaultContextTTL = 15 * time.Minute

// State 是一次 Reader 页抓取的解析结果。
type State struct {
	// Progress 是 Reading Progress（位置不推进：始终来自服务端的页面状态）。
	Progress weread.ReadingProgress
	// Context 是 Reader Context（psvts/pclts/reader.token）。
	Context weread.ReaderContext
	// Title 是页面中的书名。
	Title string
}

// Provider 抓取并解析 Web Reader 页面状态；持有 TTL 缓存（ticket 06）。
// 缓存按单书槽位保存（一个 Task 只使用一本书；换书 = 缓存失效）。
type Provider struct {
	client *weread.Client
	clock  clock.Clock

	mu      sync.Mutex
	bookID  string
	cached  *State
	fetched time.Time
}

// Options 是 Provider 的可选依赖（零值 = 参考默认）。
type Options struct {
	// Clock 是时间源（缓存 TTL 判定；ADR-0006：clock 是领域真实依赖，测试经 app
	// seam 注入 Fake 以确定性推进越过 TTL）。
	Clock clock.Clock
}

// NewProvider 构造 Provider。
func NewProvider(client *weread.Client, opts Options) *Provider {
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	return &Provider{client: client, clock: opts.Clock}
}

// Fetch 返回 Reader Context 解析结果（Reading Progress + Context + 书名）：
// TTL（DefaultContextTTL）内命中缓存——零网络请求、且不会换新 token/psvts；
// 过期或换书时重新抓取并更新缓存（Task 建立与周期 timed report 前的主动刷新共用
// 本方法：上层无需感知缓存存在）。
func (p *Provider) Fetch(ctx context.Context, bookID string) (*State, error) {
	p.mu.Lock()
	if p.cached != nil && p.bookID == bookID && p.clock.Now().Sub(p.fetched) < DefaultContextTTL {
		st := *p.cached
		p.mu.Unlock()
		return &st, nil
	}
	p.mu.Unlock()
	return p.fetchAndStore(ctx, bookID)
}

// Refresh 强制重新抓取 Reader 页并更新缓存（有界恢复链的"刷新 Reader Context"步骤；
// ticket 05 语义保持：Refresh 总是重新抓取，绕过 TTL 缓存）。
func (p *Provider) Refresh(ctx context.Context, bookID string) (*State, error) {
	return p.fetchAndStore(ctx, bookID)
}

// fetchAndStore 抓取、解析并更新缓存（返回副本，调用方不得反向污染缓存）。
func (p *Provider) fetchAndStore(ctx context.Context, bookID string) (*State, error) {
	st, err := p.fetch(ctx, bookID)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.bookID, p.cached, p.fetched = bookID, st, p.clock.Now()
	p.mu.Unlock()
	out := *st
	return &out, nil
}

func (p *Provider) fetch(ctx context.Context, bookID string) (*State, error) {
	html, cookies, err := p.client.ReaderPage(ctx, bookID)
	if err != nil {
		return nil, fmt.Errorf("抓取 Reader 页失败: %w", err)
	}
	state, err := weread.ParseInitialState(html)
	if err != nil {
		return nil, fmt.Errorf("解析 __INITIAL_STATE__ 失败: %w", err)
	}
	progress, err := state.ReadingProgress(bookID)
	if err != nil {
		return nil, err
	}
	// 响应被接受（HTTP 200 + 解析成功）后才并入并持久化响应 Set-Cookie；失败的
	// 响应不改写持久化 Login Session（issue 16：与 renewal/report/Shelf 同语义）。
	if err := p.client.MergeResponseCookies(cookies); err != nil {
		return nil, fmt.Errorf("抓取 Reader 页失败: %w", err)
	}
	return &State{Progress: progress, Context: state.ReaderContext(), Title: state.BookTitle()}, nil
}
