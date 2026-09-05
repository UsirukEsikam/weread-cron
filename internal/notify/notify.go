// Package notify 是通知模块（spec 决策 #3/#10）：Bark 与企业微信机器人，渠道独立启用、
// 发送失败不影响 Task 结果与终态（用户故事 #37/#38）。
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Success 是成功通知的内容（spec 决策 #10：完成、计划时长、实际累计时长、report 次数、书名）。
type Success struct {
	// Date 是 Task 日期（cfg.TZ 下 YYYY-MM-DD）。
	Date string
	// BookTitle 是书名。
	BookTitle string
	// BookID 是 bookId。
	BookID string
	// Planned 是当天 Target Duration（计划时长）。
	Planned time.Duration
	// Actual 是本地累计的实际阅读时长（汇总被接受的 rt）。
	Actual time.Duration
	// Reports 是成功接受的 timed report 次数。
	Reports int
}

// LoginInvalid 是登录失效通知的内容（spec 决策 #10：固定文案明确提示更新初始 Cookie）。
type LoginInvalid struct {
	// Date 是 Task 日期（cfg.TZ 下 YYYY-MM-DD）。
	Date string
	// Cause 是重建失败的原因摘要（固定文案之外的附加信息，可空）。
	Cause string
}

// LoginInvalidPrompt 是登录失效通知的固定提示文案（spec 决策 #10：明确提示更新初始 Cookie）。
const LoginInvalidPrompt = "请更新初始 Cookie（WEREAD_CRON_COOKIE）后重试：重启服务或执行 weread-cron run。"

// FailureTitle 是失败通知的标题（spec 决策 #10；与成功/登录失效通知文案区分）。
const FailureTitle = "微信读书阅读任务失败"

// Failure 是失败通知的内容（spec 决策 #10：失败阶段、主要错误、已尝试恢复动作）。
type Failure struct {
	// Date 是 Task 日期（cfg.TZ 下 YYYY-MM-DD）。
	Date string
	// Stage 是失败阶段（如 task 包的 StageTimedReport/StageEnterReport）。
	Stage string
	// Error 是主要错误（恢复链耗尽时的最终错误摘要）。
	Error string
	// Actions 是已尝试的恢复动作（按发生顺序；由 task 恢复链记录）。
	Actions []string
}

// Notifier 发送一类通知；错误仅表示该渠道失败。
type Notifier interface {
	// NotifySuccess 发送成功通知。
	NotifySuccess(ctx context.Context, s Success) error
	// NotifyLoginInvalid 发送登录失效通知（固定文案：明确提示更新初始 Cookie）。
	NotifyLoginInvalid(ctx context.Context, l LoginInvalid) error
	// NotifyFailure 发送失败通知（失败阶段、主要错误、已尝试恢复动作；登录失效通知不混入）。
	NotifyFailure(ctx context.Context, f Failure) error
}

// HTTPClient 是通知渠道使用的 HTTP 客户端（超时等内部默认由装配层提供）。
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Multi 按配置的渠道顺序逐个发送；各渠道独立、互不影响，错误聚合返回（调用方忽略即可）。
type Multi struct {
	channels []Notifier
}

// NewMulti 组合多个渠道；空渠道列表 = 不通知（用户故事 #37："都无"合法）。
func NewMulti(channels ...Notifier) *Multi {
	return &Multi{channels: channels}
}

// NotifySuccess 逐个渠道发送并聚合错误（任一渠道失败不影响其他渠道）。
func (m *Multi) NotifySuccess(ctx context.Context, s Success) error {
	var errs []error
	for _, ch := range m.channels {
		if err := ch.NotifySuccess(ctx, s); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// NotifyLoginInvalid 逐个渠道发送并聚合错误（任一渠道失败不影响其他渠道）。
func (m *Multi) NotifyLoginInvalid(ctx context.Context, l LoginInvalid) error {
	var errs []error
	for _, ch := range m.channels {
		if err := ch.NotifyLoginInvalid(ctx, l); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// NotifyFailure 逐个渠道发送并聚合错误（任一渠道失败不影响其他渠道）。
func (m *Multi) NotifyFailure(ctx context.Context, f Failure) error {
	var errs []error
	for _, ch := range m.channels {
		if err := ch.NotifyFailure(ctx, f); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Bark 通过 Bark 服务推送（Bark 官方 API：POST 到设备 key URL，JSON body）。
type Bark struct {
	// URL 是配置的 Bark 地址（BARK URL，形如 https://api.day.app/<key>）。
	URL    string
	Client HTTPClient
}

// NotifySuccess 发送 Bark 通知（title + body 由 Success 生成）。
func (b *Bark) NotifySuccess(ctx context.Context, s Success) error {
	body := map[string]string{
		"title": "微信读书阅读任务完成",
		"body":  formatMessage(s),
		"group": "weread-cron",
	}
	return postJSON(ctx, b.Client, b.URL, body)
}

// NotifyLoginInvalid 发送 Bark 登录失效通知（固定文案 + 原因摘要）。
func (b *Bark) NotifyLoginInvalid(ctx context.Context, l LoginInvalid) error {
	body := map[string]string{
		"title": "微信读书登录已失效",
		"body":  formatLoginInvalid(l),
		"group": "weread-cron",
	}
	return postJSON(ctx, b.Client, b.URL, body)
}

// NotifyFailure 发送 Bark 失败通知（失败阶段/主要错误/已尝试恢复动作）。
func (b *Bark) NotifyFailure(ctx context.Context, f Failure) error {
	body := map[string]string{
		"title": FailureTitle,
		"body":  formatFailure(f),
		"group": "weread-cron",
	}
	return postJSON(ctx, b.Client, b.URL, body)
}

// WeCom 通过企业微信机器人 webhook 推送。
type WeCom struct {
	// URL 是配置的 Webhook 地址（WEREAD_CRON_WECOM_WEBHOOK_URL）。
	URL    string
	Client HTTPClient
}

// NotifySuccess 发送企业微信文本消息。
func (w *WeCom) NotifySuccess(ctx context.Context, s Success) error {
	body := map[string]any{
		"msgtype": "text",
		"text": map[string]string{
			"content": formatMessage(s),
		},
	}
	return postJSON(ctx, w.Client, w.URL, body)
}

// NotifyLoginInvalid 发送企业微信文本消息（固定文案 + 原因摘要）。
func (w *WeCom) NotifyLoginInvalid(ctx context.Context, l LoginInvalid) error {
	body := map[string]any{
		"msgtype": "text",
		"text": map[string]string{
			"content": formatLoginInvalid(l),
		},
	}
	return postJSON(ctx, w.Client, w.URL, body)
}

// NotifyFailure 发送企业微信文本消息（失败阶段/主要错误/已尝试恢复动作）。
func (w *WeCom) NotifyFailure(ctx context.Context, f Failure) error {
	body := map[string]any{
		"msgtype": "text",
		"text": map[string]string{
			"content": formatFailure(f),
		},
	}
	return postJSON(ctx, w.Client, w.URL, body)
}

// formatMessage 生成统一的成功通知文本。
func formatMessage(s Success) string {
	return fmt.Sprintf(
		"微信读书阅读任务完成（%s）\n书名：%s（%s）\n计划时长：%s\n实际累计：%s\n上报次数：%d",
		s.Date, s.BookTitle, s.BookID,
		FormatDuration(s.Planned), FormatDuration(s.Actual), s.Reports)
}

// formatLoginInvalid 生成登录失效通知文本：固定文案 + 可选原因（spec 决策 #10）。
func formatLoginInvalid(l LoginInvalid) string {
	msg := fmt.Sprintf("微信读书登录已失效：Task 未能完成（%s）\n%s", l.Date, LoginInvalidPrompt)
	if l.Cause != "" {
		msg += "\n原因：" + l.Cause
	}
	return msg
}

// formatFailure 生成失败通知文本：失败阶段、主要错误、已尝试恢复动作（spec 决策 #10）。
func formatFailure(f Failure) string {
	msg := fmt.Sprintf("微信读书阅读任务失败（%s）\n失败阶段：%s\n主要错误：%s", f.Date, f.Stage, f.Error)
	if len(f.Actions) > 0 {
		msg += "\n已尝试恢复：" + strings.Join(f.Actions, " → ")
	}
	return msg
}

// FormatDuration 输出中文时长（如 "40 分钟"、"40 分钟 30 秒"、"30 秒"）。
func FormatDuration(d time.Duration) string {
	total := int(d / time.Second)
	m, sec := total/60, total%60
	switch {
	case m == 0:
		return fmt.Sprintf("%d 秒", sec)
	case sec == 0:
		return fmt.Sprintf("%d 分钟", m)
	default:
		return fmt.Sprintf("%d 分钟 %d 秒", m, sec)
	}
}

// postJSON 向 url POST JSON body；非 2xx 视为失败。
func postJSON(ctx context.Context, client HTTPClient, url string, body any) error {
	if client == nil {
		client = http.DefaultClient
	}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("序列化通知失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("构造通知请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("发送通知失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("通知端点返回 HTTP %d", resp.StatusCode)
	}
	return nil
}
