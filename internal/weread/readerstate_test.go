package weread

import (
	"fmt"
	"testing"
	"time"
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

// TestParseInitialStateExplicitZeroChapterFields 断言 currentChapter 显式零值
// （chapterIdx=0 / chapterOffset=0）按字面采用，不被 progress.book 的回退值覆盖；
// 数字与字符串两种 JSON 形式均适用，且解析按字段独立（chapterUid 缺失时 chapterIdx
// 显式 0 仍按字面采用、chapterUid 回退 progress.book）。
func TestParseInitialStateExplicitZeroChapterFields(t *testing.T) {
	cases := []struct {
		name              string
		cc                string // currentChapter 的 JSON
		wantChapterUID    uint64
		wantChapterIdx    int
		wantChapterOffset int
	}{
		{
			name:              "数字零值",
			cc:                `{"chapterUid":112,"chapterIdx":0,"chapterOffset":0}`,
			wantChapterUID:    112,
			wantChapterIdx:    0,
			wantChapterOffset: 0,
		},
		{
			name:              "字符串零值",
			cc:                `{"chapterUid":112,"chapterIdx":"0","chapterOffset":"0"}`,
			wantChapterUID:    112,
			wantChapterIdx:    0,
			wantChapterOffset: 0,
		},
		{
			name:              "uid 缺失时 idx/offset 显式零仍采用",
			cc:                `{"chapterIdx":0,"chapterOffset":0}`,
			wantChapterUID:    555, // chapterUid 缺失 → 回退 progress.book
			wantChapterIdx:    0,
			wantChapterOffset: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := `{"reader":{"psvts":"p1","token":"t","bookInfo":{"bookId":"9","title":"书"},` +
				`"currentChapter":` + tc.cc + `,` +
				`"progress":{"book":{"chapterUid":555,"chapterIdx":7,"chapterOffset":9,"progress":88,"summary":"s"}}}}`
			st, err := ParseInitialState(samplePage(state))
			if err != nil {
				t.Fatal(err)
			}
			p, err := st.ReadingProgress("9")
			if err != nil {
				t.Fatal(err)
			}
			if p.ChapterUID != tc.wantChapterUID || p.ChapterIdx != tc.wantChapterIdx || p.ChapterOffset != tc.wantChapterOffset {
				t.Errorf("ReadingProgress = %+v（期望 uid=%d idx=%d offset=%d）", p, tc.wantChapterUID, tc.wantChapterIdx, tc.wantChapterOffset)
			}
		})
	}
}

// TestParseInitialStateChapterUIDZeroMeansNoPosition 断言 chapterUid 的 0 仍视为
// 无位置：currentChapter 显式 chapterUid=0（或 null）时回退 progress.book；两源皆无
// （含显式 0）时维持"无法构造合法上报"的错误语义（现有错误判定不变）。
func TestParseInitialStateChapterUIDZeroMeansNoPosition(t *testing.T) {
	cases := []struct {
		name    string
		cc      string // currentChapter 的 JSON
		pb      string // progress.book 的 JSON
		want    uint64
		wantErr bool
	}{
		{
			name: "cc 显式 0 回退 pb",
			cc:   `{"chapterUid":0,"chapterIdx":3,"chapterOffset":1}`,
			pb:   `{"chapterUid":555,"chapterIdx":2,"chapterOffset":0,"progress":10,"summary":""}`,
			want: 555,
		},
		{
			name: "cc null 视为缺失回退 pb",
			cc:   `{"chapterUid":null}`,
			pb:   `{"chapterUid":555,"chapterIdx":2,"chapterOffset":0,"progress":10,"summary":""}`,
			want: 555,
		},
		{
			name:    "两源皆显式 0 报错",
			cc:      `{"chapterUid":0,"chapterIdx":0,"chapterOffset":0}`,
			pb:      `{"chapterUid":0,"chapterIdx":0,"chapterOffset":0}`,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := `{"reader":{"psvts":"p1","token":"t","bookInfo":{"bookId":"9","title":"书"},` +
				`"currentChapter":` + tc.cc + `,` +
				`"progress":{"book":` + tc.pb + `}}}`
			st, err := ParseInitialState(samplePage(state))
			if err != nil {
				t.Fatal(err)
			}
			p, err := st.ReadingProgress("9")
			if tc.wantErr {
				if err == nil {
					t.Errorf("应报无章节位置错误，得到 %+v", p)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.ChapterUID != tc.want {
				t.Errorf("ChapterUID = %d（期望 %d）", p.ChapterUID, tc.want)
			}
		})
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

// TestParseInitialStatePcltsFlexibility 断言 pclts 兼容 JSON 数字与字符串等各形态，
// 且非法类型报错，并在 payload 构造时触发正确的 pc fallback。
func TestParseInitialStatePcltsFlexibility(t *testing.T) {
	cases := []struct {
		name      string
		pcltsJSON string // e.g. `"pclts":0`，为空表示字段缺失
		wantPclts string
		wantErr   bool
	}{
		{
			name:      "数字 0（受控真实验证观察值）",
			pcltsJSON: `"pclts":0,`,
			wantPclts: "0",
		},
		{
			name:      "非零数字时间戳",
			pcltsJSON: `"pclts":1744333820,`,
			wantPclts: "1744333820",
		},
		{
			name:      "字符串 0",
			pcltsJSON: `"pclts":"0",`,
			wantPclts: "0",
		},
		{
			name:      "已编码字符串",
			pcltsJSON: `"pclts":"aab32e207a65a466g010615",`,
			wantPclts: "aab32e207a65a466g010615",
		},
		{
			name:      "null 显式空值",
			pcltsJSON: `"pclts":null,`,
			wantPclts: "",
		},
		{
			name:      "字段缺失",
			pcltsJSON: "",
			wantPclts: "",
		},
		{
			name:      "非法类型（布尔值）报错",
			pcltsJSON: `"pclts":true,`,
			wantErr:   true,
		},
		{
			name:      "非法类型（数组）报错",
			pcltsJSON: `"pclts":[0],`,
			wantErr:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := fmt.Sprintf(`{"reader":{"psvts":"5cb328e07aa94b32g0140f1",%s"token":"tok-1",`+
				`"bookInfo":{"bookId":"9","title":"书"},`+
				`"currentChapter":{"chapterUid":112,"chapterIdx":3,"chapterOffset":0}}}`, tc.pcltsJSON)
			st, err := ParseInitialState(samplePage(state))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望解析报错，但成功返回: %+v", st)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseInitialState 失败: %v", err)
			}
			rc := st.ReaderContext()
			if rc.Pclts != tc.wantPclts {
				t.Errorf("ReaderContext.Pclts = %q, want %q", rc.Pclts, tc.wantPclts)
			}

			// 验证下游 payload 构造中的 pc 决策（issue 30）：ResolvePC 在 Reading
			// Session 建立（fixedNow）时解析一次会话级 pc，构造层原样携带。
			fixedNow := time.Unix(1744333820, 0)
			progress, err := st.ReadingProgress("9")
			if err != nil {
				t.Fatalf("ReadingProgress 失败: %v", err)
			}
			pc := ResolvePC(rc, fixedNow)
			enterPayload := EnterReportPayload(progress, rc, pc, fixedNow, "test-ua")
			timedPayload := TimedReportPayload(progress, rc, pc, fixedNow, "test-ua", 30, fixedNow.UnixMilli(), 12345)
			expectedFallback := EncodeID("1744333820")

			if tc.wantPclts == "" || tc.wantPclts == "0" {
				if enterPayload["pc"] != expectedFallback {
					t.Errorf("enterPayload pc = %q, 期望回退为 e(会话建立时刻) %q", enterPayload["pc"], expectedFallback)
				}
				if timedPayload["pc"] != expectedFallback {
					t.Errorf("timedPayload pc = %q, 期望回退为 e(会话建立时刻) %q", timedPayload["pc"], expectedFallback)
				}
			} else {
				if enterPayload["pc"] != tc.wantPclts {
					t.Errorf("enterPayload pc = %q, 期望保留原值 %q", enterPayload["pc"], tc.wantPclts)
				}
				if timedPayload["pc"] != tc.wantPclts {
					t.Errorf("timedPayload pc = %q, 期望保留原值 %q", timedPayload["pc"], tc.wantPclts)
				}
			}
		})
	}
}

