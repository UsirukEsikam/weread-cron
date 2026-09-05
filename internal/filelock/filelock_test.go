// filelock 单元测试：flock 语义的进程内可测部分——排他性（同一进程内两次独立
// open 也互相冲突，与跨进程互斥同一内核机制）、释放后可再获取、不同目录互不
// 干扰、错误路径（目录不存在 → 非 ErrLocked 的明确错误）。
package filelock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestTryLockExclusiveWithTwoOpens 断言 flock 排他性绑定 open file description：
// 同一进程内第二次独立 open 拿不到锁（跨进程互斥依赖的同一机制；进程内回归
// 测试的降级基础）。
func TestTryLockExclusiveWithTwoOpens(t *testing.T) {
	dir := t.TempDir()
	l1, err := TryLock(dir)
	if err != nil {
		t.Fatalf("首次 TryLock 失败: %v", err)
	}

	// 第二次独立 open + flock：非阻塞拒绝。
	if _, err := TryLock(dir); !errors.Is(err, ErrLocked) {
		t.Errorf("持锁期间第二次 TryLock = %v，期望 ErrLocked", err)
	}

	if err := l1.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}

	// 释放后可再获取（正常退出/释放路径）。
	l2, err := TryLock(dir)
	if err != nil {
		t.Fatalf("释放后 TryLock 失败: %v", err)
	}
	l2.Close()
}

// TestTryLockDifferentDirsIndependent 断言锁文件按目录隔离：不同目录（不同
// deployment）同时持锁互不干扰。
func TestTryLockDifferentDirsIndependent(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	lA, err := TryLock(dirA)
	if err != nil {
		t.Fatalf("dirA TryLock 失败: %v", err)
	}
	lB, err := TryLock(dirB)
	if err != nil {
		t.Fatalf("dirB TryLock 失败（不同目录应互不干扰）: %v", err)
	}
	lA.Close()
	lB.Close()
}

// TestTryLockCreatesLockFile 断言锁文件在 DataDir 下创建（文件常驻，内容不使用）。
func TestTryLockCreatesLockFile(t *testing.T) {
	dir := t.TempDir()
	l, err := TryLock(dir)
	if err != nil {
		t.Fatalf("TryLock 失败: %v", err)
	}
	defer l.Close()

	fi, err := os.Stat(filepath.Join(dir, LockFileName))
	if err != nil {
		t.Fatalf("锁文件未创建: %v", err)
	}
	if fi.Size() != 0 {
		t.Errorf("锁文件大小 = %d，期望 0（内容不使用）", fi.Size())
	}
}

// TestTryLockMissingDirFails 断言目录不存在时返回非 ErrLocked 的明确错误
// （保守失败：无法保证互斥时不得静默放行）。
func TestTryLockMissingDirFails(t *testing.T) {
	_, err := TryLock(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("目录不存在时应失败")
	}
	if errors.Is(err, ErrLocked) {
		t.Errorf("目录不存在不得报 ErrLocked: %v", err)
	}
}
