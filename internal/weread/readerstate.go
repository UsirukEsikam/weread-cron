// 微信读书 Web Reader 页面 __INITIAL_STATE__ 解析（纯逻辑，无 HTTP）。
//
// 结构与字段优先级按 weread.koplugin 的 reader_state.lua（ReaderState.extract /
// apply_to_book）实现：
//   - 页面包含 window.__INITIAL_STATE__ = <json>; (function ...)()；JSON 中 reader 子对象
//     携带 psvts/pclts/token/bookInfo/currentChapter/progress；
//   - ReadingProgress 字段优先级：章节位置以 currentChapter 优先、回退 progress.book；
//     chapterIdx/chapterOffset 区分"字段缺失"与"显式零值"——字段存在（含显式 0）时按字面
//     采用，仅在字段缺失时回退；chapterUid 的 0 仍视为无位置（两源皆无时无法构造合法上报）；
//   - 章节编号等字段在真实页面中可能是数字或字符串（chapterOffset 常见为 "0" 字符串），
//     flexInt/flexUint64 兼容两者并记录字段存在性。
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
		Psvts    string     `json:"psvts"`
		Pclts    flexString `json:"pclts"`
		Token    string     `json:"token"`
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
// chapterIdx/chapterOffset 区分"字段缺失"与"显式零值"：字段存在（含显式 0）时采用
// currentChapter 值，仅在字段缺失时回退 progress.book；chapterUid 的 0 仍视为无位置
// （显式 0 与缺失同样回退，两源皆无时报错——无法构造合法上报）。
func (s *InitialState) ReadingProgress(bookID string) (ReadingProgress, error) {
	cc := s.Reader.CurrentChapter
	pb := s.Reader.Progress.Book

	chapterUID := cc.ChapterUID.Uint64()
	if chapterUID == 0 {
		chapterUID = pb.ChapterUID.Uint64()
	}
	chapterIdx := cc.ChapterIdx.Int()
	if !cc.ChapterIdx.Present() {
		chapterIdx = pb.ChapterIdx.Int()
	}
	chapterOffset := cc.ChapterOffset.Int()
	if !cc.ChapterOffset.Present() {
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
		Pclts: s.Reader.Pclts.String(),
		Token: s.Reader.Token,
	}
}

// flexString 解析 JSON 字符串或数字为 string（真实页面 reader.pclts 存在 0 或字符串等形态）。
type flexString struct {
	s string
}

// UnmarshalJSON 接受字符串字面量或数字字面量。
func (f *flexString) UnmarshalJSON(b []byte) error {
	f.s = ""
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		f.s = str
		return nil
	}
	var num json.Number
	if err := json.Unmarshal(b, &num); err == nil {
		f.s = num.String()
		return nil
	}
	return fmt.Errorf("flexString: 无法将 %s 解析为字符串或数字", string(b))
}

// String 返回字符串值。
func (f flexString) String() string { return f.s }

// flexInt 解析 JSON 数字或数字字符串为 int（真实页面 chapterOffset 等字段两种形式都有）。
// 记录字段存在性：present=false 表示字段缺失（或 JSON null），present=true 表示字段存在
// （含显式零值）。
//
// flexInt 是值类型（结构体字段直接嵌入）；字段缺失时 UnmarshalJSON 不会被调用，
// 零值 present=false 即"缺失"，JSON null 同样记为缺失。
type flexInt struct {
	n       int
	present bool
}

// UnmarshalJSON 接受数字字面量或带引号的数字字符串。
func (f *flexInt) UnmarshalJSON(b []byte) error {
	f.n = 0
	f.present = false
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
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
	f.present = true
	return nil
}

// Present 返回字段是否存在于 JSON 中（显式零值也视为存在）。
func (f flexInt) Present() bool { return f.present }

// Int 返回数值。
func (f flexInt) Int() int { return f.n }

// flexUint64 解析 JSON 数字或数字字符串为 uint64（chapterUid 在大数场景需保持精度）。
// 存在性语义同 flexInt：present=false 表示字段缺失（或 JSON null）。
type flexUint64 struct {
	n       uint64
	present bool
}

// UnmarshalJSON 接受数字字面量或带引号的数字字符串。
func (f *flexUint64) UnmarshalJSON(b []byte) error {
	f.n = 0
	f.present = false
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
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
	f.present = true
	return nil
}

// Present 返回字段是否存在于 JSON 中（显式零值也视为存在）。
func (f flexUint64) Present() bool { return f.present }

// Uint64 返回数值。
func (f flexUint64) Uint64() uint64 { return f.n }
