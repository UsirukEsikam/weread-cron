// Package filelock 提供基于 flock(2) 的跨进程文件锁（ticket 11；ADR-0008）。
//
// 选 flock 而非其他载体的理由（ADR-0008 记录取舍）：
//   - 内核自动释放：锁绑定 open file description，随持有进程退出/崩溃自动释放，
//     不存在"残留锁文件导致永久卡死"；
//   - 非阻塞获取（LOCK_EX|LOCK_NB）：保持"已有 Task 运行时即拒绝、不等待"的
//     TryLock 语义；
//   - 排他性作用于整把锁：同一进程内两次独立 open 也互相冲突（进程内守卫的
//     回归测试依赖同一语义），无需引入进程号协商。
package filelock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// LockFileName 是跨进程锁文件名（位于 DataDir 下）。文件内容不使用——flock 只
// 需要一个 inode，锁状态完全由内核维护；文件常驻 /data（体积为零），不随锁释放
// 删除。
const LockFileName = "task.lock"

// ErrLocked 表示锁已被其他进程持有（非阻塞拒绝语义；app 层映射为 ErrTaskRunning）。
var ErrLocked = errors.New("跨进程锁已被其他进程持有")

// Lock 是持锁句柄。进程退出（含崩溃）时内核自动释放，无需也无法"手动持久化释放"。
type Lock struct {
	f *os.File
}

// TryLock 在 dir 下以非阻塞方式获取跨进程锁。
//
// 锁已被持有 → 返回 ErrLocked（调用方无需释放任何资源）；其他失败（锁文件创建
// 失败、flock 系统错误）→ 返回描述性错误。调用方持锁期间必须保持返回的 Lock
// 不被 GC 回收（锁随 fd 关闭释放）。
func TryLock(dir string) (*Lock, error) {
	path := filepath.Join(dir, LockFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开跨进程锁文件 %s 失败: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("获取跨进程锁 %s 失败: %w", path, err)
	}
	return &Lock{f: f}, nil
}

// Close 释放锁：显式 flock LOCK_UN 后关闭 fd（关闭 fd 本身也会释放锁，双保险；
// LOCK_UN 失败不阻止关闭）。
func (l *Lock) Close() error {
	unlockErr := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	closeErr := l.f.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
