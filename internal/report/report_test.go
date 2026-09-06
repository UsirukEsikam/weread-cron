// report 包的 Sender 测试（issue 16）：响应 Set-Cookie 只在 report 被业务接受
// （protocol.IsAccepted）后并入；被拒响应不并入；合并失败报错语义保持。
package report

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"weread-cron/internal/weread"
)

// TestSenderMergesCookiesOnlyAfterAcceptance 断言 enter/timed report 的 Set-Cookie
// 只在被接受后并入（issue 16 / F6）：被拒响应不触发合并且返回 ErrRejected；接受
// 响应触发合并且 Cookie 原样交付。
func TestSenderMergesCookiesOnlyAfterAcceptance(t *testing.T) {
	cases := []struct {
		name         string
		timed        bool
		body         string
		wantErr      bool
		wantRejected bool
		wantMerge    int
	}{
		{name: "enter 被拒携带 Set-Cookie", body: `{"errCode":-2014,"errMsg":"err"}`, wantErr: true, wantRejected: true, wantMerge: 0},
		{name: "timed 被拒携带 Set-Cookie", timed: true, body: `{"errCode":-2014,"errMsg":"err"}`, wantErr: true, wantRejected: true, wantMerge: 0},
		{name: "enter 接受携带 Set-Cookie", body: `{"succ":1}`, wantMerge: 1},
		{name: "timed 接受携带 Set-Cookie", timed: true, body: `{"succ":1}`, wantMerge: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.SetCookie(w, &http.Cookie{Name: "wr_gid", Value: "ok123", Path: "/"})
				w.Write([]byte(tc.body))
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
			s := NewSender(client)

			now := time.Unix(1744264311, 0)
			pos := weread.ReadingProgress{
				BookID: "695233", ChapterUID: 112, ChapterIdx: 3, ChapterOffset: 1234,
				Progress: 35, Summary: "太空是无尽黑暗的",
			}
			rc := weread.ReaderContext{Psvts: "ps", Pclts: "pc", Token: "tok"}
			pc := weread.ResolvePC(rc, now)
			var err error
			if tc.timed {
				err = s.Timed(context.Background(), "695233", pos, rc, pc, now, 15, 1744264311434, 466)
			} else {
				err = s.Enter(context.Background(), "695233", pos, rc, pc, now)
			}
			if tc.wantErr != (err != nil) {
				t.Fatalf("发送 err = %v，wantErr = %v", err, tc.wantErr)
			}
			if tc.wantRejected && !errors.Is(err, ErrRejected) {
				t.Errorf("被拒响应应包装 ErrRejected，实际: %v", err)
			}
			if merged != tc.wantMerge {
				t.Errorf("合并回调调用次数 = %d，期望 %d", merged, tc.wantMerge)
			}
			if tc.wantMerge == 1 {
				if len(got) != 1 || got[0].Name != "wr_gid" || got[0].Value != "ok123" {
					t.Errorf("合并的 Cookie = %+v，期望 wr_gid=ok123", got)
				}
			}
		})
	}
}

// TestSenderMergeFailurePreservesErrorSemantics 断言接受响应但合并/持久化失败时
// 发送仍报错且包装"上报请求失败: 并入响应 Cookie 失败"（issue 16 验收：合并失败
// 报错行为不回归；不归类为 ErrRejected——被接受后失败是本地错误）。
func TestSenderMergeFailurePreservesErrorSemantics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "wr_gid", Value: "x", Path: "/"})
		w.Write([]byte(`{"succ":1}`))
	}))
	defer srv.Close()
	client := weread.NewClient(srv.URL, srv.Client(), weread.DefaultUserAgent,
		func() string { return "" },
		func([]*http.Cookie) error { return errors.New("磁盘故障") })
	s := NewSender(client)

	now := time.Unix(1744264311, 0)
	pos := weread.ReadingProgress{BookID: "695233", ChapterUID: 112}
	rc := weread.ReaderContext{Psvts: "ps", Pclts: "pc", Token: "tok"}
	err := s.Enter(context.Background(), "695233", pos, rc, weread.ResolvePC(rc, now), now)
	if err == nil {
		t.Fatal("合并失败时 Enter 应报错")
	}
	if !strings.Contains(err.Error(), "上报请求失败") || !strings.Contains(err.Error(), "并入响应 Cookie 失败") {
		t.Errorf("错误应包装「上报请求失败: 并入响应 Cookie 失败」，实际: %v", err)
	}
	if errors.Is(err, ErrRejected) {
		t.Errorf("合并失败不得归类为 ErrRejected（响应已被接受）: %v", err)
	}
}
