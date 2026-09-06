// readercontext 包的 Provider 测试（issue 16）：Reader 页响应的 Set-Cookie 只在
// 页面解析与位置提取成功（State 完整可建，业务接受）后并入；失败响应不并入；
// 合并失败报错语义保持。
package readercontext

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"weread-cron/internal/weread"
)

// probePage 生成带 __INITIAL_STATE__ 的模拟 Reader 页（结构与
// internal/weread/readerstate_test.go 的 samplePage 同构；位置字段齐全可建 State）。
func probePage(token string) string {
	state := `{"reader":{"psvts":"p1","pclts":"p2","token":"` + token + `",` +
		`"bookInfo":{"bookId":"695233","title":"三体全集"},` +
		`"currentChapter":{"chapterUid":112,"chapterIdx":3,"chapterOffset":1234},` +
		`"progress":{"book":{"chapterUid":112,"chapterIdx":3,"chapterOffset":1234,"progress":35,"summary":"s"}}}}`
	return `<html><head><title>x</title></head><body><script>window.__INITIAL_STATE__ = ` +
		state + `; (function(){return 1;})();</script></body></html>`
}

// TestProviderMergesCookiesOnlyAfterAcceptance 断言 Reader 页 Set-Cookie 只在业务
// 接受后并入（issue 16 一致语义）：无法解析的页面不触发合并；成功触发且 Cookie
// 原样交付、State 完整可建；HTTP 500 响应不触发合并。
func TestProviderMergesCookiesOnlyAfterAcceptance(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		html      string
		wantErr   bool
		wantMerge int
	}{
		{name: "HTTP 500 携带 Set-Cookie", status: 500, html: "boom", wantErr: true, wantMerge: 0},
		{name: "页面无法解析携带 Set-Cookie", html: `<html><body>这本书不可阅读</body></html>`, wantErr: true, wantMerge: 0},
		{name: "成功携带 Set-Cookie", html: probePage("tok-1"), wantMerge: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.SetCookie(w, &http.Cookie{Name: "wr_gid", Value: "ok123", Path: "/"})
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				w.Write([]byte(tc.html))
			}))
			defer srv.Close()

			merged := 0
			var got []*http.Cookie
			client := weread.NewClient(srv.URL, srv.Client(), weread.DefaultUserAgent,
				func() string { return "" },
				func(cookies []*http.Cookie) error {
					merged++
					got = cookies
					return nil
				})
			p := NewProvider(client, Options{})
			st, err := p.Fetch(context.Background(), "695233")
			if tc.wantErr != (err != nil) {
				t.Fatalf("Fetch() err = %v，wantErr = %v", err, tc.wantErr)
			}
			if merged != tc.wantMerge {
				t.Errorf("合并回调调用次数 = %d，期望 %d", merged, tc.wantMerge)
			}
			if tc.wantMerge == 1 {
				if len(got) != 1 || got[0].Name != "wr_gid" || got[0].Value != "ok123" {
					t.Errorf("合并的 Cookie = %+v，期望 wr_gid=ok123", got)
				}
				if st == nil || st.Title != "三体全集" || st.Context.Token != "tok-1" {
					t.Errorf("State 应完整可建: %+v", st)
				}
			}
		})
	}
}

// TestProviderMergeFailurePreservesErrorSemantics 断言接受响应但合并/持久化失败时
// Fetch 仍报错且包装"抓取 Reader 页失败: 并入响应 Cookie 失败"（issue 16 验收：
// 合并失败报错行为不回归）。
func TestProviderMergeFailurePreservesErrorSemantics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "wr_gid", Value: "x", Path: "/"})
		w.Write([]byte(probePage("tok-1")))
	}))
	defer srv.Close()
	client := weread.NewClient(srv.URL, srv.Client(), weread.DefaultUserAgent,
		func() string { return "" },
		func([]*http.Cookie) error { return errors.New("磁盘故障") })
	p := NewProvider(client, Options{})

	_, err := p.Fetch(context.Background(), "695233")
	if err == nil {
		t.Fatal("合并失败时 Fetch 应报错")
	}
	if !strings.Contains(err.Error(), "抓取 Reader 页失败") || !strings.Contains(err.Error(), "并入响应 Cookie 失败") {
		t.Errorf("错误应包装「抓取 Reader 页失败: 并入响应 Cookie 失败」，实际: %v", err)
	}
}

// TestProviderAcceptsNumericPclts 断言 Provider 在真实服务端返回数字 pclts（如受控真机验证观察到的 0）
// 时能够正常建立 Reader Context，不报反序列化错误。
func TestProviderAcceptsNumericPclts(t *testing.T) {
	pageHTML := `<html><head><title>x</title></head><body><script>window.__INITIAL_STATE__ = ` +
		`{"reader":{"psvts":"5cb328e07aa94b32g0140f1","pclts":0,"token":"tok-test",` +
		`"bookInfo":{"bookId":"695233","title":"三体全集"},` +
		`"currentChapter":{"chapterUid":112,"chapterIdx":3,"chapterOffset":1234},` +
		`"progress":{"book":{"chapterUid":112,"chapterIdx":3,"chapterOffset":1234,"progress":35,"summary":"s"}}}};` +
		` (function(){return 1;})();</script></body></html>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(pageHTML))
	}))
	defer srv.Close()

	client := weread.NewClient(srv.URL, srv.Client(), weread.DefaultUserAgent,
		func() string { return "" },
		func([]*http.Cookie) error { return nil })
	p := NewProvider(client, Options{})

	st, err := p.Fetch(context.Background(), "695233")
	if err != nil {
		t.Fatalf("Fetch() 失败: %v", err)
	}
	if st.Context.Psvts != "5cb328e07aa94b32g0140f1" {
		t.Errorf("Psvts = %q, want %q", st.Context.Psvts, "5cb328e07aa94b32g0140f1")
	}
	if st.Context.Pclts != "0" {
		t.Errorf("Pclts = %q, want %q", st.Context.Pclts, "0")
	}
	if st.Context.Token != "tok-test" {
		t.Errorf("Token = %q, want %q", st.Context.Token, "tok-test")
	}
}

