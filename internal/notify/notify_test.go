package notify

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// failClient 模拟传输层失败（DNS / TLS / 连接）：Do 返回与 net/http 同形态的
// *url.Error（Op + 完整请求 URL + 内层错误），并记录请求 URL 供断言——脱敏只应
// 作用于返回的错误，实际发出的请求 URL 不得被改写。
type failClient struct {
	inner error
	urls  []string
}

func (c *failClient) Do(req *http.Request) (*http.Response, error) {
	c.urls = append(c.urls, req.URL.String())
	return nil, &url.Error{Op: "Post", URL: req.URL.String(), Err: c.inner}
}

// statusClient 返回固定 HTTP 状态码（模拟通知端点非 2xx 响应）。
type statusClient struct {
	status int
}

func (c *statusClient) Do(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: c.status,
		Status:     http.StatusText(c.status),
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}, nil
}

// TestBarkTransportErrorRedactsKey 断言 Bark key（位于 URL path）在传输层失败
// （DNS / 连接）的错误串中不出现（issue 19 / F9 验收 1/3）：错误保持 url.Error
// 形态（Op/Err 保留、URL 脱敏为仅 scheme://host），unwrap 语义贯通；实际发送的
// 请求 URL 不被改写。Task 编排层对通知错误原样记日志（finalize 的 Warn），因此
// 断言返回错误的字符串即断言日志内容。
func TestBarkTransportErrorRedactsKey(t *testing.T) {
	const (
		key = "barkSecretDeviceKey123"
		raw = "https://api.day.app/" + key
	)
	for name, inner := range map[string]error{
		"DNS 解析失败": &net.DNSError{Err: "no such host", Name: "api.day.app"},
		"连接失败":     errors.New("dial tcp 1.2.3.4:443: connect: connection refused"),
		"TLS 握手失败": errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority"),
		"客户端上下文取消": context.Canceled,
	} {
		t.Run(name, func(t *testing.T) {
			c := &failClient{inner: inner}
			b := &Bark{URL: raw, Client: c}
			err := b.NotifySuccess(context.Background(), Success{})
			if err == nil {
				t.Fatal("传输层失败应返回错误")
			}
			msg := err.Error()
			if strings.Contains(msg, key) {
				t.Errorf("错误暴露 Bark key: %v", msg)
			}
			if !strings.Contains(msg, "https://api.day.app") {
				t.Errorf("应保留主机便于定位问题: %v", msg)
			}
			if len(c.urls) != 1 || c.urls[0] != raw {
				t.Errorf("实际请求 URL 不应被脱敏: %v", c.urls)
			}
			var ue *url.Error
			if !errors.As(err, &ue) {
				t.Fatalf("应保持 url.Error 形态，实际: %T", err)
			}
			if ue.URL != "https://api.day.app" {
				t.Errorf("url.Error.URL = %q，期望脱敏为仅主机", ue.URL)
			}
			if !errors.Is(err, inner) {
				t.Errorf("unwrap 语义应贯通（errors.Is(err, inner)）: %v", err)
			}
		})
	}
}

// TestWeComTransportErrorRedactsKey 断言企业微信 robot key（位于 URL query）在
// 各类传输层失败的错误串中不出现（issue 19 / F9 验收 2/3）：错误保持 url.Error
// 形态（Op/Err 保留、URL 脱敏为仅 scheme://host），unwrap 语义与请求 URL 不变性
// 断言与 Bark 用例一致。
func TestWeComTransportErrorRedactsKey(t *testing.T) {
	const (
		key = "wecomRobotSecretKey456"
		raw = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=" + key
	)
	for name, inner := range map[string]error{
		"DNS 解析失败": &net.DNSError{Err: "no such host", Name: "qyapi.weixin.qq.com"},
		"连接失败":     errors.New("dial tcp 1.2.3.4:443: connect: connection refused"),
		"TLS 握手失败": errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority"),
		"客户端上下文取消": context.Canceled,
	} {
		t.Run(name, func(t *testing.T) {
			c := &failClient{inner: inner}
			w := &WeCom{URL: raw, Client: c}
			err := w.NotifyFailure(context.Background(), Failure{})
			if err == nil {
				t.Fatal("传输层失败应返回错误")
			}
			msg := err.Error()
			if strings.Contains(msg, key) {
				t.Errorf("错误暴露企业微信 robot key: %v", msg)
			}
			if !strings.Contains(msg, "https://qyapi.weixin.qq.com") {
				t.Errorf("应保留主机便于定位问题: %v", msg)
			}
			if len(c.urls) != 1 || c.urls[0] != raw {
				t.Errorf("实际请求 URL 不应被脱敏: %v", c.urls)
			}
			var ue *url.Error
			if !errors.As(err, &ue) {
				t.Fatalf("应保持 url.Error 形态，实际: %T", err)
			}
			if ue.URL != "https://qyapi.weixin.qq.com" {
				t.Errorf("url.Error.URL = %q，期望脱敏为仅主机", ue.URL)
			}
			if !errors.Is(err, inner) {
				t.Errorf("unwrap 语义应贯通（errors.Is(err, inner)）: %v", err)
			}
		})
	}
}

// TestNotifyHTTPStatusErrorUnchanged 断言 HTTP 状态错误（非 2xx）行为不回归
// （issue 19 验收 4）：错误仍为"通知端点返回 HTTP %d"、不含 URL，三种通知出口一致。
func TestNotifyHTTPStatusErrorUnchanged(t *testing.T) {
	send := []struct {
		name string
		fn   func(ctx context.Context, client HTTPClient, url string) error
	}{
		{name: "Success", fn: func(ctx context.Context, client HTTPClient, url string) error {
			return (&Bark{URL: url, Client: client}).NotifySuccess(ctx, Success{})
		}},
		{name: "LoginInvalid", fn: func(ctx context.Context, client HTTPClient, url string) error {
			return (&WeCom{URL: url, Client: client}).NotifyLoginInvalid(ctx, LoginInvalid{})
		}},
		{name: "Failure", fn: func(ctx context.Context, client HTTPClient, url string) error {
			return (&WeCom{URL: url, Client: client}).NotifyFailure(ctx, Failure{})
		}},
	}
	for _, tc := range send {
		t.Run(tc.name, func(t *testing.T) {
			client := &statusClient{status: http.StatusInternalServerError}
			err := tc.fn(context.Background(), client, "https://api.day.app/barkSecretDeviceKey123")
			if err == nil {
				t.Fatal("非 2xx 应返回错误")
			}
			if got, want := err.Error(), "通知端点返回 HTTP 500"; got != want {
				t.Errorf("错误 = %q，期望 %q", got, want)
			}
		})
	}
}

// TestNotifyRequestConstructionErrorRedacted 断言请求构造失败（URL 解析错误）的
// 错误串同样不暴露凭据（issue 19 验收 3 兜底路径）：非 url.Error 的错误串中出现
// 完整 URL 时整体替换为脱敏形态。
func TestNotifyRequestConstructionErrorRedacted(t *testing.T) {
	const key = "barkSecretDeviceKey123"
	raw := "https://api.day.app/" + key + "\x7f" // 控制字符使 url.Parse 失败
	b := &Bark{URL: raw, Client: &failClient{inner: context.Canceled}}
	err := b.NotifySuccess(context.Background(), Success{})
	if err == nil {
		t.Fatal("请求构造失败应返回错误")
	}
	if msg := err.Error(); strings.Contains(msg, key) {
		t.Errorf("错误暴露 Bark key: %v", msg)
	}
}
