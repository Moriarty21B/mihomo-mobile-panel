package main

import (
	"errors"
	"os"
	"syscall"
)

// An advisory kernel lock is tied to this open file description. It releases on
// exit, SIGKILL and power loss, unlike a directory used as a lock sentinel.
func acquireFileLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("另一项管理操作正在进行，请稍后重试")
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
