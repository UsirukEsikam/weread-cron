package terminal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileStoreSaveLoad(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)

	// 初始无终态。
	if _, ok, err := store.Load(); err != nil || ok {
		t.Fatalf("空目录 Load = (%v, %v)", ok, err)
	}

	want := State{LastTaskDate: "2025-09-06", LastTaskResult: ResultSuccess}
	if err := store.Save(want); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	got, ok, err := store.Load()
	if err != nil || !ok {
		t.Fatalf("Load = (%v, %v)", ok, err)
	}
	if got != want {
		t.Errorf("Load = %+v，期望 %+v", got, want)
	}

	// 原子写：无残留临时文件。
	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".tmp-*")); len(leftovers) > 0 {
		t.Errorf("残留临时文件: %v", leftovers)
	}
	// 文件是合法 JSON（终态先落盘再通知，内容可核对）。
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	var check State
	if err := json.Unmarshal(data, &check); err != nil {
		t.Errorf("终态文件不是合法 JSON: %v", err)
	}
}

func TestFileStoreOverwrite(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	if err := store.Save(State{LastTaskDate: "2025-09-06", LastTaskResult: ResultFailed}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(State{LastTaskDate: "2025-09-06", LastTaskResult: ResultSuccess}); err != nil {
		t.Fatal(err)
	}
	got, _, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.LastTaskResult != ResultSuccess {
		t.Errorf("覆盖后 = %+v", got)
	}
}

func TestFileStoreMissingDir(t *testing.T) {
	store := NewFileStore(filepath.Join(t.TempDir(), "nested", "data"))
	if _, ok, err := store.Load(); err != nil || ok {
		t.Fatalf("目录缺失 Load = (%v, %v)", ok, err)
	}
	if err := store.Save(State{LastTaskDate: "2025-09-06", LastTaskResult: ResultSuccess}); err != nil {
		t.Fatalf("目录缺失时应自动创建: %v", err)
	}
	if _, ok, err := store.Load(); err != nil || !ok {
		t.Fatalf("Save 后 Load = (%v, %v)", ok, err)
	}
}

func TestCorruptFileErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewFileStore(dir).Load(); err == nil {
		t.Error("损坏的终态文件应报错而非静默")
	}
}

// TestIsToday 覆盖终态日期匹配的唯一定义（scheduler 谓词 / app 门控共用）：
// 同一天匹配、跨天不匹配、TZ 决定日期边界。
func TestIsToday(t *testing.T) {
	var loc = time.FixedZone("Asia/Shanghai", 8*3600)
	now := time.Date(2025, 9, 6, 23, 30, 0, 0, loc)
	cases := []struct {
		name string
		st   State
		want bool
	}{
		{"当天", State{LastTaskDate: "2025-09-06", LastTaskResult: ResultSuccess}, true},
		{"昨天", State{LastTaskDate: "2025-09-05", LastTaskResult: ResultSuccess}, false},
		{"明天", State{LastTaskDate: "2025-09-07", LastTaskResult: ResultFailed}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsToday(tc.st, now, loc); got != tc.want {
				t.Errorf("IsToday(%+v) = %v，期望 %v", tc.st, got, tc.want)
			}
		})
	}
	// TZ 边界：同一 UTC 时刻在上海已过午夜（次日），UTC 仍是当天。
	utcNow := time.Date(2025, 9, 6, 16, 30, 0, 0, time.UTC) // = 上海 9-7 00:30
	st := State{LastTaskDate: "2025-09-07", LastTaskResult: ResultSuccess}
	if !IsToday(st, utcNow, loc) {
		t.Errorf("上海 00:30 应属于 9-7")
	}
	if IsToday(st, utcNow, time.UTC) {
		t.Errorf("UTC 16:30 应属于 9-6")
	}
	if got := TodayKey(utcNow, loc); got != "2025-09-07" {
		t.Errorf("TodayKey(上海) = %q，期望 2025-09-07", got)
	}
}
