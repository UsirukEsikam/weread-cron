// 微信读书 Web Reader 页面 __INITIAL_STATE__ 解析（纯逻辑，无 HTTP）。
//
// 结构与字段优先级按 weread.koplugin 的 reader_state.lua（ReaderState.extract /
// apply_to_book）实现：
//   - 页面包含 window.__INITIAL_STATE__ = <json>; (function ...)()；JSON 中 reader 子对象
//     携带 psvts/pclts/token/bookInfo/currentChapter/progress；
//   - ReadingProgress 字段优先级：章节位置以 currentChapter 优先、回退 progress.book；
//     进度百分比与摘要取自 progress.book；
//   - 章节编号等字段在真实页面中可能是数字或字符串（chapterOffset 常见为 "0" 字符串），
//     flexInt/flexUint64 兼容两者。
package weread

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// initialStateMarker 匹配 "window.__INITIAL_STATE__ = {...}; (function" 片段
// （koplugin 的 Lua 模式 window%.__INITIAL_STATE__%s*=%s*(.-)%s*;%s*%(function 等价）。
var initialStateMarker = regexp.MustCompile(`(?s)window\.__INITIAL_STATE__\s*=\s*(.*?)\s*;\s*\(function`)

// InitialState 是解析后的 __INITIAL_STATE__（只保留本工具关心的字段）。
type InitialState struct {
	Reader struct {
		Psvts string `json:"psvts"`
		Pclts string `json:"pclts"`
		Token string `json:"token"`
		BookInfo struct {
			BookID string `json:"bookId"`
			Title  string `json:"title"`
		} `json:"bookInfo"`
		CurrentChapter struct {
			ChapterUID    flexUint64 `json:"chapterUid"`
			ChapterIdx    flexInt    `json:"chapterIdx"`
			ChapterOffset flexInt    `json:"chapterOffset"`
		} `json:"currentChapter"`
		Progress struct {
			Book struct {
				ChapterUID    flexUint64 `json:"chapterUid"`
				ChapterIdx    flexInt    `json:"chapterIdx"`
				ChapterOffset flexInt    `json:"chapterOffset"`
				Progress      flexInt    `json:"progress"`
				Summary       string     `json:"summary"`
			} `json:"book"`
		} `json:"progress"`
	} `json:"reader"`
}

// ParseInitialState 从 Reader 页 HTML 提取并解析 __INITIAL_STATE__。
func ParseInitialState(html string) (*InitialState, error) {
	m := initialStateMarker.FindStringSubmatch(html)
	if m == nil {
		return nil, fmt.Errorf("页面中未找到 window.__INITIAL_STATE__（%d 字节）", len(html))
	}
	raw := strings.TrimSpace(m[1])
	var st InitialState
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.UseNumber()
	if err := dec.Decode(&st); err != nil {
		return nil, fmt.Errorf("解析 __INITIAL_STATE__ JSON 失败: %w", err)
	}
	return &st, nil
}

// BookTitle 返回页面中的书名（可能为空）。
func (s *InitialState) BookTitle() string { return s.Reader.BookInfo.Title }

// ReadingProgress 按 koplugin apply_to_book 的优先级构造 ReadingProgress。
// 找不到任何章节位置时返回错误（无法构造合法上报）。
func (s *InitialState) ReadingProgress(bookID string) (ReadingProgress, error) {
	cc := s.Reader.CurrentChapter
	pb := s.Reader.Progress.Book

	chapterUID := cc.ChapterUID.Uint64()
	if chapterUID == 0 {
		chapterUID = pb.ChapterUID.Uint64()
	}
	chapterIdx := cc.ChapterIdx.Int()
	if chapterIdx == 0 {
		chapterIdx = pb.ChapterIdx.Int()
	}
	chapterOffset := cc.ChapterOffset.Int()
	if chapterOffset == 0 {
		chapterOffset = pb.ChapterOffset.Int()
	}
	if chapterUID == 0 {
		return ReadingProgress{}, fmt.Errorf("__INITIAL_STATE__ 无章节位置（currentChapter/progress.book 均缺 chapterUid）")
	}
	return ReadingProgress{
		BookID:        bookID,
		ChapterUID:    chapterUID,
		ChapterIdx:    chapterIdx,
		ChapterOffset: chapterOffset,
		Progress:      pb.Progress.Int(),
		Summary:       pb.Summary,
	}, nil
}

// ReaderContext 返回用于构造 payload 的 Reader Context（psvts/pclts/token；
// Pclts 为空时由 payload 构造层回退 e(now)）。
func (s *InitialState) ReaderContext() ReaderContext {
	return ReaderContext{
		Psvts: s.Reader.Psvts,
		Pclts: s.Reader.Pclts,
		Token: s.Reader.Token,
	}
}

// flexInt 解析 JSON 数字或数字字符串为 int（真实页面 chapterOffset 等字段两种形式都有）。
type flexInt struct {
	n int
}

// UnmarshalJSON 接受数字字面量或带引号的数字字符串。
func (f *flexInt) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		f.n = 0
		return nil
	}
	s := string(b)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return err
	}
	f.n = int(n)
	return nil
}

// Int 返回数值。
func (f flexInt) Int() int { return f.n }

// flexUint64 解析 JSON 数字或数字字符串为 uint64（chapterUid 在大数场景需保持精度）。
type flexUint64 struct {
	n uint64
}

// UnmarshalJSON 接受数字字面量或带引号的数字字符串。
func (f *flexUint64) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		f.n = 0
		return nil
	}
	s := string(b)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return err
	}
	f.n = n
	return nil
}

// Uint64 返回数值。
func (f flexUint64) Uint64() uint64 { return f.n }
