package session

import (
	"os"
	"path/filepath"
	"testing"
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
