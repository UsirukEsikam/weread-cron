// Package report 是上报模块（spec 决策 #3）：enter/timed report 的发送与成功判定。
// payload 构造复用 internal/weread 协议层；是否被接受按 protocol.IsAccepted
//（succ==1 或 synckey 存在，checklist #9 注释见 protocol.go）。
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
// 返回 nil = 被接受；服务器拒绝时返回包装 ErrRejected 的错误（非传输错误）。
func (s *Sender) Enter(ctx context.Context, bookID string, p weread.ReadingProgress, rc weread.ReaderContext, now time.Time) error {
	payload := weread.EnterReportPayload(p, rc, now, s.client.UserAgent)
	body, err := s.client.Report(ctx, payload, bookID)
	if err != nil {
		return err
	}
	if !weread.IsAccepted(body) {
		return fmt.Errorf("%w: 响应 %v", ErrRejected, body)
	}
	return nil
}

// Timed 发送 timed report（携带 rt/ts/rn/sg；rt 语义由调用方按 ADR-0004 传入）。
// 返回 nil = 被接受；服务器拒绝时返回包装 ErrRejected 的错误（非传输错误）。
func (s *Sender) Timed(ctx context.Context, bookID string, p weread.ReadingProgress, rc weread.ReaderContext, now time.Time, rtSec int, tsMs int64, rn int) error {
	payload := weread.TimedReportPayload(p, rc, now, s.client.UserAgent, rtSec, tsMs, rn)
	body, err := s.client.Report(ctx, payload, bookID)
	if err != nil {
		return err
	}
	if !weread.IsAccepted(body) {
		return fmt.Errorf("%w: 响应 %v", ErrRejected, body)
	}
	return nil
}
