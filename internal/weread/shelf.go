// 微信读书 Shelf（书架）协议层（ticket 09）：纯 Cookie 获取当前账号书架。
//
// # 验证状态（checklist #1）
//
// 端点：GET {base}/web/shelf/sync，纯 Cookie 鉴权（公开探测：无 Cookie 访问返回
// {"errCode":-2010,"errMsg":"用户不存在"}，即按登录身份鉴权、非公开接口；与 Web 端
// /web/shelf/* 接口族一致）。返回结构按 skill gateway /shelf/sync 的同构假设解析：
// books[] 数组，条目含 bookId/title/finishReading（finishReading==1 = 已读完）。
// 该结构未经真实账号实测确认；若实测与假设冲突，按验证清单处理原则在 ticket 09
// 提出讨论，不静默替换字段语义。
//
// 登录失效不在此层判别（checklist #2：renewal 失败是已实现的明确证据之一；
// Shelf 的 errCode!=0 响应不是已验证的证据形态，按普通错误向上传播，由调用方
// 在 renewal 环节判定登录失效）。
package weread

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// ShelfBook 是书架条目（checklist #1 假设字段的解析子集）。
type ShelfBook struct {
	// BookID 是书 ID（数字串或 MP_WXS_ 前缀等）。
	BookID string
	// Title 是书名。
	Title string
	// FinishReading 是否已读完（finishReading==1）。
	FinishReading bool
}

// shelfEnvelope 是 /web/shelf/sync 的响应信封（errCode 缺失视为成功；
// errCode!=0 为业务错误，如未登录 -2010）。
type shelfEnvelope struct {
	ErrCode json.Number  `json:"errCode"`
	ErrMsg  string       `json:"errMsg"`
	Books   []shelfEntry `json:"books"`
}

type shelfEntry struct {
	BookID        string        `json:"bookId"`
	Title         string        `json:"title"`
	FinishReading flexShelfBool `json:"finishReading"`
}

// ParseShelfResponse 解析 /web/shelf/sync 响应为 ShelfBook 列表。
// errCode 存在且不为 0 时返回错误（含 errMsg）；books 缺失/为空 = 空书架。
func ParseShelfResponse(data []byte) ([]ShelfBook, error) {
	var env shelfEnvelope
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&env); err != nil {
		return nil, fmt.Errorf("不是合法 JSON 响应: %w", err)
	}
	if env.ErrCode != "" && env.ErrCode.String() != "0" {
		return nil, fmt.Errorf("Shelf 接口返回错误 (errCode %s): %s", env.ErrCode.String(), env.ErrMsg)
	}
	out := make([]ShelfBook, 0, len(env.Books))
	for _, e := range env.Books {
		out = append(out, ShelfBook{
			BookID:        e.BookID,
			Title:         e.Title,
			FinishReading: e.FinishReading.Bool(),
		})
	}
	return out, nil
}

// Shelf 获取当前账号书架：GET /web/shelf/sync（纯 Cookie 鉴权，UA/Cookie 由客户端
// 统一注入，Referer 指向书架页）。传输/HTTP/解析失败均为普通错误（登录失效判别在
// renewal 环节，不在此层）。响应 Set-Cookie 在业务接受（errCode==0）后才并入并
// 持久化（issue 16：失败响应不改写持久化 Login Session）。
func (c *Client) Shelf(ctx context.Context) ([]ShelfBook, error) {
	resp, cookies, err := c.do(ctx, http.MethodGet, c.BaseURL+"/web/shelf/sync", c.BaseURL+"/web/shelf", nil)
	if err != nil {
		return nil, fmt.Errorf("抓取 Shelf 失败: %w", err)
	}
	books, err := ParseShelfResponse(resp)
	if err != nil {
		return nil, err // 错误已自描述（业务错误含 errCode/errMsg；解析错误含原因）
	}
	if err := c.MergeResponseCookies(cookies); err != nil {
		return nil, fmt.Errorf("抓取 Shelf 失败: %w", err)
	}
	return books, nil
}

// flexShelfBool 接受数字（0/1）、布尔、数字字符串三种形态的布尔标记
// （finishReading 在真实响应中形态未实测，按常见 JSON 形态宽容解析；checklist #1）。
type flexShelfBool struct {
	b bool
}

func (f *flexShelfBool) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		f.b = false
		return nil
	}
	s := string(data)
	if s == "true" {
		f.b = true
		return nil
	}
	if s == "false" {
		f.b = false
		return nil
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	f.b = n != 0
	return nil
}

// Bool 返回布尔值。
func (f flexShelfBool) Bool() bool { return f.b }
