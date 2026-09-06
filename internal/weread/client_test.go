package weread

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRenewalMergesCookiesOnlyAfterAcceptance 断言响应 Set-Cookie 只在 renewal 被
// 接受后并入（issue 16 / F6）：失败响应（HTTP 非 200 / succ=0 / succ 缺失）不触发
// 合并回调；成功响应触发且 Cookie 原样交付（无 Set-Cookie 时零调用）。
func TestRenewalMergesCookiesOnlyAfterAcceptance(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		withCookie bool
		wantErr    bool
		wantLogin  bool // 期望包装 ErrLoginInvalid
		wantMerge  int
	}{
		{name: "HTTP 500 携带 Set-Cookie", status: 500, body: `{"errCode":500,"errMsg":"boom"}`, withCookie: true, wantErr: true, wantMerge: 0},
		{name: "succ=0 携带 Set-Cookie", body: `{"succ":0,"errMsg":"expired"}`, withCookie: true, wantErr: true, wantLogin: true, wantMerge: 0},
		{name: "succ 缺失携带 Set-Cookie", body: `{"errCode":-2012,"errMsg":"error"}`, withCookie: true, wantErr: true, wantMerge: 0},
		{name: "成功携带 Set-Cookie", body: `{"succ":1}`, withCookie: true, wantMerge: 1},
		{name: "成功无 Set-Cookie", body: `{"succ":1}`, wantMerge: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.withCookie {
					http.SetCookie(w, &http.Cookie{Name: "wr_gid", Value: "ok123", Path: "/"})
				}
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			merged := 0
			var got []*http.Cookie
			c := NewClient(srv.URL, srv.Client(), DefaultUserAgent,
				func() string { return "" },
				func(cookies []*http.Cookie) error {
					merged++
					got = cookies
					return nil
				})
			err := c.Renewal(context.Background())
			if tc.wantErr != (err != nil) {
				t.Fatalf("Renewal() err = %v，wantErr = %v", err, tc.wantErr)
			}
			if tc.wantLogin && !errors.Is(err, ErrLoginInvalid) {
				t.Errorf("应包装 ErrLoginInvalid，实际: %v", err)
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

// TestRenewalMergeFailurePreservesErrorSemantics 断言合并/持久化失败仍使 renewal
// 失败且包装"并入响应 Cookie 失败"（issue 16 验收：合并失败报错行为不回归；
// 合并失败不是登录失效证据）。
func TestRenewalMergeFailurePreservesErrorSemantics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "wr_gid", Value: "x", Path: "/"})
		w.Write([]byte(`{"succ":1}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, srv.Client(), DefaultUserAgent,
		func() string { return "" },
		func([]*http.Cookie) error { return errors.New("磁盘故障") })
	err := c.Renewal(context.Background())
	if err == nil {
		t.Fatal("合并失败时 Renewal 应报错")
	}
	if !strings.Contains(err.Error(), "并入响应 Cookie 失败") {
		t.Errorf("错误应包装「并入响应 Cookie 失败」，实际: %v", err)
	}
	if errors.Is(err, ErrLoginInvalid) {
		t.Errorf("合并失败不得归类为登录失效: %v", err)
	}
}
