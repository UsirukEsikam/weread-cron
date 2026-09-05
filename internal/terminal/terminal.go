// Package terminal 管理 Terminal State（CONTEXT.md：Task 结束时落盘的最小记录：
// last_task_date + last_task_result）。
//
// spec 决策 #9/#10：终态先落盘、再发通知；原子写（临时文件 + rename）。
// success 与 failed 都阻止当天再次自动执行；failed 允许 run 手动重试（规则在
// ticket 07/08 调度与 run 层，本包只负责持久化）。
package terminal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"weread-cron/internal/atomicfile"
)

const (
	// FileName 是 Terminal State 文件名（与 Login Session 一样位于 DataDir）。
	FileName = "terminal_state.json"
	// ResultSuccess / ResultFailed 是 last_task_result 的取值。
	ResultSuccess = "success"
	ResultFailed  = "failed"
	// DayLayout 是 Terminal State 日期与调度日期的格式（cfg.TZ 下 YYYY-MM-DD）；
	// 日期匹配的唯一定义（scheduler/nextstart、app 门控、task 落盘共用）。
	DayLayout = "2006-01-02"
)

// TodayKey 返回 now 在 tz 下的当天日期键（YYYY-MM-DD）。
func TodayKey(now time.Time, tz *time.Location) string {
	return now.In(tz).Format(DayLayout)
}

// IsToday 报告 st 是否为 now 当天（tz 下）的终态——终态日期匹配的唯一定义
// （scheduler 的 todayTerminal 谓词与 app 的终态规则门控共用）。
func IsToday(st State, now time.Time, tz *time.Location) bool {
	return st.LastTaskDate == TodayKey(now, tz)
}

// State 是当日 Task 的终态记录。
type State struct {
	// LastTaskDate 是 Task 所属日期（cfg.TZ 下的 YYYY-MM-DD）。
	LastTaskDate string `json:"last_task_date"`
	// LastTaskResult 为 success 或 failed。
	LastTaskResult string `json:"last_task_result"`
}

// Store 抽象 Terminal State 持久化（写入 /data；经临时目录注入测试）。
type Store interface {
	// Save 原子写入终态。
	Save(s State) error
	// Load 读取终态；(State, true, nil) 表示存在。
	Load() (State, bool, error)
}

// FileStore 把 Terminal State 存放在 DataDir 下。
type FileStore struct {
	dir string
}

// NewFileStore 创建基于目录 dir 的文件存储。
func NewFileStore(dir string) *FileStore {
	return &FileStore{dir: dir}
}

func (s *FileStore) path() string { return filepath.Join(s.dir, FileName) }

// Save 原子写入终态（写入失败显式返回，调用方决定是否继续）。
func (s *FileStore) Save(st State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 Terminal State 失败: %w", err)
	}
	return atomicfile.WriteFile(s.path(), data)
}

// Load 读取终态；文件不存在返回 (State{}, false, nil)。
func (s *FileStore) Load() (State, bool, error) {
	data, err := os.ReadFile(s.path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, false, nil
		}
		return State{}, false, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, false, fmt.Errorf("解析 Terminal State 失败: %w", err)
	}
	return st, true, nil
}
