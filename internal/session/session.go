// Package session 管理 Login Session 的持久化与恢复。
//
// ticket 01 只提供"是否存在可恢复的 Login Session"这一最小接口，供首次启动
// 无初始 Cookie 时快速失败；完整序列化/恢复/原子持久化由 ticket 04 交付。
package session

import (
	"errors"
	"os"
	"path/filepath"
)

// loginSessionFileName 是持久化 Login Session 的文件名（ticket 04 定义其内容格式）。
const loginSessionFileName = "login_session.json"

// Store 抽象 Login Session 的持久化存储（ADR-0006：经构造器注入）。
type Store interface {
	// HasLoginSession 报告是否存在可恢复的持久化 Login Session。
	HasLoginSession() (bool, error)
}

// FileStore 将 Login Session 存储在 DataDir 下。
type FileStore struct {
	dir string
}

// NewFileStore 创建基于目录 dir 的文件存储。
func NewFileStore(dir string) *FileStore {
	return &FileStore{dir: dir}
}

// HasLoginSession 检查 DataDir 下是否存在 Login Session 文件。
func (s *FileStore) HasLoginSession() (bool, error) {
	_, err := os.Stat(filepath.Join(s.dir, loginSessionFileName))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}
