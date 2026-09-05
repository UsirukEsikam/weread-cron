// Package atomicfile 提供原子写文件的工具：临时文件 + rename，避免崩溃留下半写状态。
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFile 把 data 原子写入 path（spec 决策 #9：临时文件 + rename）。
// 目录不存在时先创建（含父目录）；rename 为同文件系统操作，失败时会清理临时文件。
func WriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("同步临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename 到 %s 失败: %w", path, err)
	}
	tmpName = "" // rename 成功，临时文件已不存在
	return nil
}
