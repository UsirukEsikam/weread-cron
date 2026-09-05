// Package readercontext 是 Reader Context 模块（spec 决策 #3）：从 Web Reader 页面
// 抓取 __INITIAL_STATE__ 并解析为 Reading Progress + Reader Context + 书名。
//
// ticket 05 交付 Fetch（Task 开始时建立）与 Refresh（有界恢复链的强制刷新）；当前
// 实现无缓存、两者等价。ticket 06 将在此包引入 TTL 缓存与主动刷新（Fetch 命中缓存、
// Refresh 总是重新抓取），对上层接口保持不变。
package readercontext

import (
	"context"
	"fmt"

	"weread-cron/internal/weread"
)

// State 是一次 Reader 页抓取的解析结果。
type State struct {
	// Progress 是 Reading Progress（位置不推进：始终来自服务端的页面状态）。
	Progress weread.ReadingProgress
	// Context 是 Reader Context（psvts/pclts/reader.token）。
	Context weread.ReaderContext
	// Title 是页面中的书名。
	Title string
}

// Provider 抓取并解析 Web Reader 页面状态。
type Provider struct {
	client *weread.Client
}

// NewProvider 构造 Provider。
func NewProvider(client *weread.Client) *Provider {
	return &Provider{client: client}
}

// Fetch 抓取并解析 Reader 页（Task 建立时使用）。
func (p *Provider) Fetch(ctx context.Context, bookID string) (*State, error) {
	return p.fetch(ctx, bookID)
}

// Refresh 强制重新抓取 Reader 页（有界恢复链的"刷新 Reader Context"步骤）。
// 当前与 Fetch 等价；ticket 06 引入缓存后 Refresh 保持"总是重新抓取"语义。
func (p *Provider) Refresh(ctx context.Context, bookID string) (*State, error) {
	return p.fetch(ctx, bookID)
}

func (p *Provider) fetch(ctx context.Context, bookID string) (*State, error) {
	html, err := p.client.ReaderPage(ctx, bookID)
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
	return &State{Progress: progress, Context: state.ReaderContext(), Title: state.BookTitle()}, nil
}
