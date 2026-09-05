package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "file.json")
	if err := WriteFile(path, []byte(`{"a":1}`)); err != nil {
		t.Fatalf("WriteFile 失败: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"a":1}` {
		t.Errorf("内容 = %q", data)
	}
	// 无残留临时文件。
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".tmp-*"))
	if len(leftovers) > 0 {
		t.Errorf("残留临时文件: %v", leftovers)
	}
}

func TestWriteFileOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := WriteFile(path, []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "new" {
		t.Errorf("内容 = %q", data)
	}
}

func TestWriteFileCreatesDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "c")
	if err := WriteFile(path, []byte("x")); err != nil {
		t.Fatalf("应自动创建父目录: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error(err)
	}
}

// TestWriteFileFailureLeavesNoPartialFile 断言写入失败时目标文件不被破坏。
func TestWriteFileFailureLeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	// 目标已存在。
	path := filepath.Join(dir, "f")
	if err := WriteFile(path, []byte("original")); err != nil {
		t.Fatal(err)
	}
	// 目录只读（posix 权限近似；写入失败路径至少不崩溃）。
	_ = os.Chmod(dir, 0o555)
	err := WriteFile(path, []byte("partial"))
	_ = os.Chmod(dir, 0o755)
	if err == nil {
		// 某些系统/用户下仍然可写（root），跳过严格要求。
		return
	}
	data, _ := os.ReadFile(path)
	if string(data) != "original" {
		t.Errorf("写入失败后目标被破坏: %q", data)
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, ".tmp-*"))
	if len(leftovers) > 0 {
		t.Errorf("写入失败后残留临时文件: %v", leftovers)
	}
}
