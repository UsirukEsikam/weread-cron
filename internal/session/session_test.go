package session

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileStoreHasLoginSession(t *testing.T) {
	dir := t.TempDir()

	// 空目录：无 Login Session
	store := NewFileStore(dir)
	got, err := store.HasLoginSession()
	if err != nil {
		t.Fatalf("HasLoginSession() 意外失败: %v", err)
	}
	if got {
		t.Error("空目录应报告无 Login Session")
	}

	// 存在登录会话文件
	if err := os.WriteFile(filepath.Join(dir, loginSessionFileName), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = store.HasLoginSession()
	if err != nil {
		t.Fatalf("HasLoginSession() 意外失败: %v", err)
	}
	if !got {
		t.Error("存在登录会话文件时应报告有 Login Session")
	}
}

func TestFileStoreMissingDirIsNoSession(t *testing.T) {
	store := NewFileStore(filepath.Join(t.TempDir(), "does-not-exist"))
	got, err := store.HasLoginSession()
	if err != nil {
		t.Fatalf("目录不存在应视为无 Login Session 而非错误: %v", err)
	}
	if got {
		t.Error("目录不存在时应报告无 Login Session")
	}
}

func TestJarFromHeader(t *testing.T) {
	j, err := NewJarFromHeader("wr_skey=abc; wr_gid=123")
	if err != nil {
		t.Fatal(err)
	}
	// 按名字排序输出。
	if got := j.CookieHeader(); got != "wr_gid=123; wr_skey=abc" {
		t.Errorf("CookieHeader = %q", got)
	}
	// 值内允许 "="。
	j2, err := NewJarFromHeader("a=1=2; b=3")
	if err != nil {
		t.Fatal(err)
	}
	if j2.CookieHeader() != "a=1=2; b=3" {
		t.Errorf("CookieHeader = %q", j2.CookieHeader())
	}
	// 空名字报错。
	if _, err := NewJarFromHeader("=v"); err == nil {
		t.Error("空名字应报错")
	}
}

func TestJarMergeSetCookiesReplacesByName(t *testing.T) {
	j, _ := NewJarFromHeader("wr_gid=old; wr_skey=abc")
	j.MergeSetCookies([]*http.Cookie{
		{Name: "wr_gid", Value: "new123", Path: "/", Domain: ".weread.qq.com", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode},
	})
	if got := j.CookieHeader(); got != "wr_gid=new123; wr_skey=abc" {
		t.Errorf("CookieHeader = %q", got)
	}
	// 属性完整保留。
	for _, c := range j.Snapshot() {
		if c.Name == "wr_gid" {
			if c.Path != "/" || c.Domain != ".weread.qq.com" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode {
				t.Errorf("wr_gid 属性丢失: %+v", c)
			}
		}
	}
}

func TestFileStoreSaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	j := NewJar()
	j.MergeSetCookies([]*http.Cookie{
		{Name: "wr_gid", Value: "g", Path: "/", Domain: ".weread.qq.com", HttpOnly: true},
		{Name: "wr_skey", Value: "s", Expires: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
	})
	if err := store.SaveJar(j); err != nil {
		t.Fatalf("SaveJar 失败: %v", err)
	}
	got, err := store.LoadJar()
	if err != nil {
		t.Fatalf("LoadJar 失败: %v", err)
	}
	if got.CookieHeader() != "wr_gid=g; wr_skey=s" {
		t.Errorf("CookieHeader = %q", got.CookieHeader())
	}
	for _, c := range got.Snapshot() {
		switch c.Name {
		case "wr_gid":
			if c.Path != "/" || c.Domain != ".weread.qq.com" || !c.HttpOnly {
				t.Errorf("wr_gid 属性未持久化: %+v", c)
			}
		case "wr_skey":
			if !c.Expires.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
				t.Errorf("wr_skey Expires 未持久化: %v", c.Expires)
			}
		}
	}
	// 原子写：无残留临时文件，文件内容是合法 JSON。
	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".tmp-*")); len(leftovers) > 0 {
		t.Errorf("残留临时文件: %v", leftovers)
	}
	data, err := os.ReadFile(filepath.Join(dir, loginSessionFileName))
	if err != nil {
		t.Fatal(err)
	}
	var f sessionFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Errorf("会话文件不是合法 JSON: %v", err)
	}
}

// TestNewRestorePreference 断言有持久化会话时优先恢复（而非使用初始 Cookie）。
func TestNewRestorePreference(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	persisted := NewJar()
	persisted.MergeSetCookies([]*http.Cookie{{Name: "wr_gid", Value: "from-disk"}})
	if err := store.SaveJar(persisted); err != nil {
		t.Fatal(err)
	}
	s, err := New(store, "wr_gid=from-cookie")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.CookieHeader(); got != "wr_gid=from-disk" {
		t.Errorf("应恢复持久化会话，got %q", got)
	}
}

// TestNewEstablishesFromInitialCookie 断言无持久化会话时用初始 Cookie 建立并落盘。
func TestNewEstablishesFromInitialCookie(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	s, err := New(store, "wr_skey=abc; wr_gid=123")
	if err != nil {
		t.Fatal(err)
	}
	if s.CookieHeader() != "wr_gid=123; wr_skey=abc" {
		t.Errorf("CookieHeader = %q", s.CookieHeader())
	}
	data, err := os.ReadFile(filepath.Join(dir, loginSessionFileName))
	if err != nil {
		t.Fatalf("初始 Login Session 未落盘: %v", err)
	}
	if !strings.Contains(string(data), `"value": "abc"`) {
		t.Errorf("会话文件缺少初始 Cookie: %s", data)
	}
}

func TestNewWithoutSessionAndCookieFails(t *testing.T) {
	if _, err := New(NewFileStore(t.TempDir()), ""); err == nil {
		t.Error("无持久化会话且无初始 Cookie 应报错")
	}
}

// TestMergeAndSave 断言并入新 Cookie 后原子落盘（renewal 后重启不回退）。
func TestMergeAndSave(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	s, err := New(store, "wr_gid=old")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MergeAndSaveCookies([]*http.Cookie{{Name: "wr_gid", Value: "new"}}); err != nil {
		t.Fatal(err)
	}
	// 磁盘与内存都已是新值。
	loaded, err := store.LoadJar()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CookieHeader() != "wr_gid=new" {
		t.Errorf("磁盘会话 = %q", loaded.CookieHeader())
	}
	if s.CookieHeader() != "wr_gid=new" {
		t.Errorf("内存会话 = %q", s.CookieHeader())
	}
}
