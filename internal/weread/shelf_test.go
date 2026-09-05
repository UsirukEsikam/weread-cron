package weread

import (
	"strings"
	"testing"
)

// TestParseShelfResponseBooks 断言正常响应解析：books[] 条目字段
// （bookId/title/finishReading 数字、布尔、"1" 字符串三种形态）。
func TestParseShelfResponseBooks(t *testing.T) {
	data := `{
		"books": [
			{"bookId": "601111", "title": "未读完的书", "author": "作者甲", "finishReading": 0},
			{"bookId": "602222", "title": "已读完的书", "author": "作者乙", "finishReading": 1},
			{"bookId": "603333", "title": "布尔标记", "finishReading": true},
			{"bookId": "604444", "title": "字符串标记", "finishReading": "1"}
		],
		"bookCount": 4
	}`
	books, err := ParseShelfResponse([]byte(data))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(books) != 4 {
		t.Fatalf("books 数 = %d，期望 4", len(books))
	}
	if books[0] != (ShelfBook{BookID: "601111", Title: "未读完的书", FinishReading: false}) {
		t.Errorf("books[0] = %+v", books[0])
	}
	if books[1] != (ShelfBook{BookID: "602222", Title: "已读完的书", FinishReading: true}) {
		t.Errorf("books[1] = %+v", books[1])
	}
	if !books[2].FinishReading || !books[3].FinishReading {
		t.Errorf("布尔/字符串 finishReading 未识别: %+v", books[2:])
	}
}

// TestParseShelfResponseIgnoresNonBookEntries 断言 albums/mp 等非 books[] 条目被忽略。
func TestParseShelfResponseIgnoresNonBookEntries(t *testing.T) {
	data := `{
		"books": [{"bookId": "601111", "title": "唯一书", "finishReading": 0}],
		"albums": [{"albumInfo": {"albumId": "a1"}}],
		"mp": {"bookId": "mp1"}
	}`
	books, err := ParseShelfResponse([]byte(data))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(books) != 1 || books[0].BookID != "601111" {
		t.Errorf("books = %+v，期望仅 1 条普通书", books)
	}
}

// TestParseShelfResponseErrCode 断言 errCode != 0 信封错误为解析错误（含 errMsg）。
func TestParseShelfResponseErrCode(t *testing.T) {
	_, err := ParseShelfResponse([]byte(`{"errCode":-2010,"errMsg":"用户不存在","info":""}`))
	if err == nil {
		t.Fatal("errCode != 0 应返回错误")
	}
	for _, want := range []string{"-2010", "用户不存在"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误缺少 %q；实际: %v", want, err)
		}
	}
}

// TestParseShelfResponseErrCodeZeroIsSuccess 断言 errCode==0（显式成功）正常解析。
func TestParseShelfResponseErrCodeZeroIsSuccess(t *testing.T) {
	books, err := ParseShelfResponse([]byte(`{"errCode":0,"books":[{"bookId":"601111","title":"书"}]}`))
	if err != nil {
		t.Fatalf("errCode==0 应成功: %v", err)
	}
	if len(books) != 1 || books[0].BookID != "601111" {
		t.Errorf("books = %+v", books)
	}
}

// TestParseShelfResponseEmptyShelf 断言空书架（books 缺失/空数组）返回空列表而非错误。
func TestParseShelfResponseEmptyShelf(t *testing.T) {
	for _, data := range []string{`{"books":[]}`, `{"books":null}`, `{}`} {
		books, err := ParseShelfResponse([]byte(data))
		if err != nil {
			t.Fatalf("空书架 %s 应成功: %v", data, err)
		}
		if len(books) != 0 {
			t.Errorf("空书架解析出 %d 本: %s", len(books), data)
		}
	}
}

// TestParseShelfResponseMalformed 断言畸形响应（非 JSON/空体）返回解析错误。
func TestParseShelfResponseMalformed(t *testing.T) {
	for _, data := range []string{``, `{oops`, `<html>404</html>`} {
		if _, err := ParseShelfResponse([]byte(data)); err == nil {
			t.Errorf("畸形响应 %q 应返回错误", data)
		}
	}
}

// TestShelfURL 断言 Shelf 请求 URL（/web/shelf/sync，纯 Cookie 接口）。
func TestShelfURL(t *testing.T) {
	c := NewClient("https://weread.qq.com", nil, DefaultUserAgent, nil, nil)
	if got := c.BaseURL + "/web/shelf/sync"; got != "https://weread.qq.com/web/shelf/sync" {
		t.Errorf("Shelf URL = %q", got)
	}
}
