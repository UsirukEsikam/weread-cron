// 微信读书 HTTP 客户端（ticket 03/09）：renewal、Shelf 抓取、Reader 页抓取、
// enter/timed report 发送。
// 本层不持有状态：Cookie header 与 Set-Cookie 合并经注入回调接入 Login Session
// （ADR-0006：HTTP endpoints 与 session store 在应用边界注入；fake server 即测试 seam）。
//
// 请求形态按 weread.koplugin 的 client.lua 实现：
//   - renewal：POST /web/login/renewal，body {"rq":"%2Fweb%2Fbook%2Fread","ql":false}，
//     成功判定 succ==1（spec 决策 #5）；响应 Set-Cookie 在接受后并入会话（issue 16）；
//   - Shelf：GET /web/shelf/sync，纯 Cookie 鉴权；响应解析见 shelf.go（checklist #1）；
//   - Reader 页：GET /web/reader/{_e(bookId)}，HTML 中解析 __INITIAL_STATE__；
//   - report：POST /web/book/read，payload 为 JSON，enter/timed 字段集与签名由
//     protocol.go 构造；ci/co/pr/ct/rt/ts/rn 在线上为数字（见 protocol.go 注释）。
//
// 未实测项（checklist #1/#5/#6/#9）遵循 protocol.go 与 shelf.go 的标注，不在本层改写语义。
package weread

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
)

// 微信读书官网基址（App 装配时可经 Deps 注入 fake server）。
const DefaultBaseURL = "https://weread.qq.com"

// DefaultUserAgent 是内部默认 UA（spec 决策 #13：UA 为内部参数，不暴露为配置）。
// 与 weread.koplugin 当前值一致（桌面 Chrome/135），appId 由 UA 派生（protocol.AppID）。
const DefaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36 Edg/135.0.0.0"

// numericFields 是线上必须为 JSON 数字的字段（protocol.go 注释：官方 JS 与抓包样本均如此）。
var numericFields = map[string]bool{
	"ci": true, "co": true, "pr": true, "ct": true,
	"rt": true, "ts": true, "rn": true,
}

// ErrLoginInvalid 标记"凭明确证据判定登录失效"（spec 决策 #7；checklist #2）。
// 当前实现的判别特征（与清单假设一致，不做额外推断）：
//   - 明确证据：renewal 请求完成（HTTP 200）且响应为合法 JSON、succ 存在且不为 true/1——
//     服务器明确拒绝了 renewal（登录失效的判别特征之一）；
//   - 非证据：传输失败、HTTP 非 200、响应解析失败、响应体不含 succ 字段——普通/暂时性
//     失败，当日可再调度，不触发从初始 Cookie 重建。
//
// 清单 #2 的其余判别特征（其他 HTTP 状态/响应体形态）未经实测，不在此实现；
// 若实测与假设冲突，按验证清单处理原则在受影响 ticket 提出讨论。
var ErrLoginInvalid = errors.New("登录已失效")

// Client 是微信读书 HTTP 客户端。
type Client struct {
	// BaseURL 是服务基址（生产 https://weread.qq.com；测试为 httptest server）。
	BaseURL string
	// HTTP 是底层 HTTP 客户端。
	HTTP *http.Client
	// UserAgent 是所有请求的 UA（协议层以它派生 appId）。
	UserAgent string
	// CookieHeader 返回发送给每种请求的 Cookie header（可为空串）。
	CookieHeader func() string
	// MergeCookies 接收响应 Set-Cookie（并入 Login Session；issue 16：接收方只在
	// 响应被业务接受后调用本回调，失败的响应不改写持久化会话）。
	MergeCookies func([]*http.Cookie) error
}

// NewClient 构造客户端。
func NewClient(baseURL string, hc *http.Client, userAgent string, cookieHeader func() string, mergeCookies func([]*http.Cookie) error) *Client {
	return &Client{
		BaseURL:      baseURL,
		HTTP:         hc,
		UserAgent:    userAgent,
		CookieHeader: cookieHeader,
		MergeCookies: mergeCookies,
	}
}

// ReaderURL 返回某书的 Reader 页地址（koplugin reader_url：/web/reader/{_e(bookId)}）。
func (c *Client) ReaderURL(bookID string) string {
	return c.BaseURL + "/web/reader/" + EncodeID(bookID)
}

// Renewal 在 Task 开始时刷新登录：POST /web/login/renewal（spec 决策 #5）。
// 响应 Set-Cookie 只在响应被接受后（HTTP 200 且 succ 确认）并入会话并持久化
// （issue 16：失败的 renewal 不改写持久化 Login Session）。
// 明确失败（HTTP 200 + 合法 JSON + succ 存在且不为 true/1）返回包装 ErrLoginInvalid
// 的错误（登录失效的明确证据，checklist #2）；传输/非 200/解析失败/响应不含 succ
// 字段不归为证据（暂时性失败）。
func (c *Client) Renewal(ctx context.Context) error {
	body := []byte(`{"rq":"%2Fweb%2Fbook%2Fread","ql":false}`)
	resp, cookies, err := c.do(ctx, http.MethodPost, c.BaseURL+"/web/login/renewal", c.BaseURL, body)
	if err != nil {
		return fmt.Errorf("renewal 请求失败: %w", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(resp, &decoded); err != nil {
		return fmt.Errorf("renewal 响应解析失败: %w", err)
	}
	if v, ok := decoded["succ"]; ok && !succIsTrue(v) {
		return fmt.Errorf("%w（renewal 未被接受）: %s", ErrLoginInvalid, truncate(string(resp), 200))
	}
	if !IsSucc(decoded) {
		// 响应不含 succ=1：不是"明确拒绝"的证据形态（如 errCode 错误体/空体），
		// 按暂时性失败处理（checklist #2 其余特征待实测，不静默加入判别集）。
		return fmt.Errorf("renewal 响应无法确认为成功（succ 缺失）: %s", truncate(string(resp), 200))
	}
	// 响应被接受（HTTP 200 + succ 确认）后才并入并持久化响应 Set-Cookie。
	if err := c.MergeResponseCookies(cookies); err != nil {
		return fmt.Errorf("renewal 请求失败: %w", err)
	}
	return nil
}

// ReaderPage 抓取 Reader 页 HTML（用于解析 __INITIAL_STATE__ 与 Reading Progress）。
// 返回页面正文与响应 Set-Cookie；调用方在解析成功（业务接受）后经
// MergeResponseCookies 并入会话（issue 16：失败响应不改写持久化 Login Session）。
func (c *Client) ReaderPage(ctx context.Context, bookID string) (string, []*http.Cookie, error) {
	url := c.ReaderURL(bookID)
	resp, cookies, err := c.do(ctx, http.MethodGet, url, url, nil)
	if err != nil {
		return "", nil, fmt.Errorf("抓取 Reader 页失败: %w", err)
	}
	return string(resp), cookies, nil
}

// Report 发送 enter/timed report（POST /web/book/read）。
// 返回解码后的响应体与响应 Set-Cookie；是否被接受由 protocol.IsAccepted 判定
// （本层不裁决语义），接受后由调用方经 MergeResponseCookies 并入会话（issue 16）。
func (c *Client) Report(ctx context.Context, payload map[string]string, bookID string) (map[string]any, []*http.Cookie, error) {
	body, err := marshalPayload(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("序列化上报 payload 失败: %w", err)
	}
	resp, cookies, err := c.do(ctx, http.MethodPost, c.BaseURL+"/web/book/read", c.ReaderURL(bookID), body)
	if err != nil {
		return nil, nil, fmt.Errorf("上报请求失败: %w", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(resp, &decoded); err != nil {
		return nil, nil, fmt.Errorf("上报响应解析失败: %w", err)
	}
	return decoded, cookies, nil
}

// MergeResponseCookies 在响应被业务接受后并入响应 Set-Cookie（renewal / report /
// Reader / Shelf 各业务调用方在成功判定后触发；issue 16：失败的响应不改写持久化
// Login Session）。合并/持久化失败返回包装"并入响应 Cookie 失败"的错误（语义保持）。
func (c *Client) MergeResponseCookies(cookies []*http.Cookie) error {
	if c.MergeCookies == nil || len(cookies) == 0 {
		return nil
	}
	if err := c.MergeCookies(cookies); err != nil {
		return fmt.Errorf("并入响应 Cookie 失败: %w", err)
	}
	return nil
}

// do 执行带统一 header（UA/Cookie/Content-Type/Origin/Referer）的请求并返回 body
// 与响应 Set-Cookie。本层不并入 Cookie：响应是否被接受由各业务调用方判定，接受后
// 经 MergeResponseCookies 并入会话。非 200 响应的 Set-Cookie 不返回（响应未被接受）。
func (c *Client) do(ctx context.Context, method, url, referer string, body []byte) ([]byte, []*http.Cookie, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	if c.CookieHeader != nil {
		if h := c.CookieHeader(); h != "" {
			req.Header.Set("Cookie", h)
		}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json;charset=UTF-8")
		req.Header.Set("Origin", c.BaseURL)
	}
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("读取响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return raw, resp.Cookies(), nil
}

// marshalPayload 把 payload 序列化为 JSON：numericFields 中的键输出数字，其余输出字符串。
// 键按字典序输出（确定性；JSON 对象顺序对服务端无意义，签名 s 基于排序查询串而非 JSON 顺序）。
func marshalPayload(p map[string]string) ([]byte, error) {
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(k)
		b.WriteString(`":`)
		if numericFields[k] {
			b.WriteString(p[k])
		} else {
			enc, err := json.Marshal(p[k])
			if err != nil {
				return nil, err
			}
			b.Write(enc)
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
