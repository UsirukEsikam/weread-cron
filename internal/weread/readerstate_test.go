package weread

import (
	"testing"
)

// samplePage 生成模拟 Reader 页（结构参照真实页面：
// window.__INITIAL_STATE__ = {json}; (function ...)()）。
func samplePage(stateJSON string) string {
	return `<html><head><title>x</title></head><body><script>window.__INITIAL_STATE__ = ` +
		stateJSON + `; (function(){var a=1;return a;})();</script></body></html>`
}

func TestParseInitialState(t *testing.T) {
	state := `{"reader":{"psvts":"4ee326507a65a465g015fae","pclts":"aab32e207a65a466g010615","token":"tok-9",` +
		`"bookInfo":{"bookId":"695233","title":"三体全集"},` +
		`"currentChapter":{"chapterUid":112,"chapterIdx":3,"chapterOffset":"1234"},` +
		`"progress":{"book":{"chapterUid":112,"chapterIdx":3,"chapterOffset":1234,"progress":35,"summary":"太空是无尽黑暗的"}}}}`
	st, err := ParseInitialState(samplePage(state))
	if err != nil {
		t.Fatalf("ParseInitialState 失败: %v", err)
	}
	if st.BookTitle() != "三体全集" {
		t.Errorf("BookTitle = %q", st.BookTitle())
	}
	rc := st.ReaderContext()
	if rc.Psvts != "4ee326507a65a465g015fae" || rc.Pclts != "aab32e207a65a466g010615" || rc.Token != "tok-9" {
		t.Errorf("ReaderContext = %+v", rc)
	}
	p, err := st.ReadingProgress("695233")
	if err != nil {
		t.Fatalf("ReadingProgress 失败: %v", err)
	}
	if p.ChapterUID != 112 || p.ChapterIdx != 3 || p.ChapterOffset != 1234 ||
		p.Progress != 35 || p.Summary != "太空是无尽黑暗的" || p.BookID != "695233" {
		t.Errorf("ReadingProgress = %+v", p)
	}
}

// TestParseInitialStatePrecedence 断言 koplugin apply_to_book 的字段优先级：
// currentChapter 的章节位置优先于 progress.book。
func TestParseInitialStatePrecedence(t *testing.T) {
	state := `{"reader":{"psvts":"p1","token":"t",` +
		`"bookInfo":{"bookId":"9","title":"书"},` +
		`"currentChapter":{"chapterUid":112,"chapterIdx":3,"chapterOffset":42},` +
		`"progress":{"book":{"chapterUid":999,"chapterIdx":7,"chapterOffset":9,"progress":88,"summary":"s"}}}}`
	st, err := ParseInitialState(samplePage(state))
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.ReadingProgress("9")
	if err != nil {
		t.Fatal(err)
	}
	if p.ChapterUID != 112 || p.ChapterIdx != 3 || p.ChapterOffset != 42 {
		t.Errorf("currentChapter 应优先：%+v", p)
	}
	if p.Progress != 88 || p.Summary != "s" {
		t.Errorf("progress 应取自 progress.book：%+v", p)
	}
}

// TestParseInitialStateProgressFieldFallback 断言无 currentChapter 时回退 progress.book。
func TestParseInitialStateProgressFieldFallback(t *testing.T) {
	state := `{"reader":{"psvts":"p1","token":"t","bookInfo":{"bookId":"9","title":"书"},` +
		`"progress":{"book":{"chapterUid":555,"chapterIdx":2,"chapterOffset":0,"progress":10,"summary":""}}}}`
	st, err := ParseInitialState(samplePage(state))
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.ReadingProgress("9")
	if err != nil {
		t.Fatal(err)
	}
	if p.ChapterUID != 555 || p.ChapterIdx != 2 {
		t.Errorf("应回退 progress.book：%+v", p)
	}
}

// TestParseInitialStateOffsetAsNumber 断言 chapterOffset 数字形式同样可解析。
func TestParseInitialStateOffsetAsNumber(t *testing.T) {
	state := `{"reader":{"psvts":"p1","token":"t","bookInfo":{"bookId":"9","title":"书"},` +
		`"currentChapter":{"chapterUid":112,"chapterIdx":3,"chapterOffset":77},` +
		`"progress":{"book":{"chapterUid":112,"chapterIdx":3,"chapterOffset":77,"progress":1,"summary":""}}}}`
	st, err := ParseInitialState(samplePage(state))
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.ReadingProgress("9")
	if err != nil {
		t.Fatal(err)
	}
	if p.ChapterOffset != 77 {
		t.Errorf("ChapterOffset = %d", p.ChapterOffset)
	}
}

func TestParseInitialStateMissingMarker(t *testing.T) {
	if _, err := ParseInitialState("<html>no state here</html>"); err == nil {
		t.Error("无 __INITIAL_STATE__ 应报错")
	}
}

func TestParseInitialStateMalformedJSON(t *testing.T) {
	html := samplePage(`{"reader":{`)
	if _, err := ParseInitialState(html); err == nil {
		t.Error("畸形 JSON 应报错")
	}
}

// TestParseInitialStateNoChapter 断言完全没有章节位置时报错。
func TestParseInitialStateNoChapter(t *testing.T) {
	state := `{"reader":{"psvts":"p1","token":"t","bookInfo":{"bookId":"9","title":"书"}}}`
	st, err := ParseInitialState(samplePage(state))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReadingProgress("9"); err == nil {
		t.Error("无章节位置应报错")
	}
}

// TestFlexIntsRejectGarbage 断言非法数字字符串在解析时报错（不被静默当 0）。
func TestFlexIntsRejectGarbage(t *testing.T) {
	state := `{"reader":{"currentChapter":{"chapterUid":"abc"},"progress":{"book":{"progress":35}}}}`
	if _, err := ParseInitialState(samplePage(state)); err == nil {
		t.Error("非法 chapterUid 应报错")
	}
}
