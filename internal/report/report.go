// Package report 是上报模块（spec 决策 #3）：enter/timed report 的发送与成功判定。
// payload 构造复用 internal/weread 协议层；是否被接受按 protocol.IsAccepted
// （succ==1 或 synckey 存在，checklist #9 注释见 protocol.go）。
package report

import (
	"context"
	"errors"
	"fmt"
	"time"

	"weread-cron/internal/weread"
)

// ErrRejected 表示服务器明确拒绝了上报（区别于传输错误）。
// 有界恢复链（ticket 05）用 errors.Is(err, ErrRejected) 区分拒绝与网络故障。
var ErrRejected = errors.New("服务器拒绝上报")

// Sender 依据 Reader Context 与 Reading Progress 构造并发送 enter/timed report。
type Sender struct {
	client *weread.Client
}

// NewSender 构造 Sender。
func NewSender(client *weread.Client) *Sender {
	return &Sender{client: client}
}

// Enter 发送 enter report（不带计时字段：rt/ts/rn/sg；见 protocol.EnterReportPayload）。
// pc 是会话级 pc（issue 30：调用方在 Reading Session 建立时经 weread.ResolvePC 解析
// 一次并传入，会话内 enter 与全部 timed reports 复用）。
// 返回 nil = 被接受；服务器拒绝时返回包装 ErrRejected 的错误（非传输错误）。
// 响应 Set-Cookie 只在被接受后并入会话（issue 16）。
func (s *Sender) Enter(ctx context.Context, bookID string, p weread.ReadingProgress, rc weread.ReaderContext, pc string, now time.Time) error {
	payload := weread.EnterReportPayload(p, rc, pc, now, s.client.UserAgent)
	body, cookies, err := s.client.Report(ctx, payload, bookID)
	if err != nil {
		return err
	}
	if !weread.IsAccepted(body) {
		return fmt.Errorf("%w: 响应 %v", ErrRejected, body)
	}
	// 响应被业务接受（succ==1 或 synckey 存在）后才并入并持久化响应 Set-Cookie；
	// 被拒的响应不改写持久化 Login Session（issue 16 的事务预期）。
	if err := s.client.MergeResponseCookies(cookies); err != nil {
		return fmt.Errorf("上报请求失败: %w", err)
	}
	return nil
}

// Timed 发送 timed report（携带 rt/ts/rn/sg；rt 语义由调用方按 ADR-0004 传入）。
// pc 是会话级 pc（同 Enter；issue 30）。
// 返回 nil = 被接受；服务器拒绝时返回包装 ErrRejected 的错误（非传输错误）。
// 响应 Set-Cookie 只在被接受后并入会话（issue 16）。
func (s *Sender) Timed(ctx context.Context, bookID string, p weread.ReadingProgress, rc weread.ReaderContext, pc string, now time.Time, rtSec int, tsMs int64, rn int) error {
	payload := weread.TimedReportPayload(p, rc, pc, now, s.client.UserAgent, rtSec, tsMs, rn)
	body, cookies, err := s.client.Report(ctx, payload, bookID)
	if err != nil {
		return err
	}
	if !weread.IsAccepted(body) {
		return fmt.Errorf("%w: 响应 %v", ErrRejected, body)
	}
	// 响应被业务接受后才并入并持久化响应 Set-Cookie；被拒的响应不改写持久化
	// Login Session（issue 16 的事务预期）。
	if err := s.client.MergeResponseCookies(cookies); err != nil {
		return fmt.Errorf("上报请求失败: %w", err)
	}
	return nil
}
