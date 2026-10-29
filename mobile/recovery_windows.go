package main

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

var lockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
var unlockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("UnlockFileEx")

func acquireFileLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	var ov syscall.Overlapped
	ok, _, _ := lockFileEx.Call(f.Fd(), 3, 0, 1, 0, uintptr(unsafe.Pointer(&ov)))
	if ok == 0 {
		f.Close()
		return nil, errors.New("另一项管理操作正在进行，请稍后重试")
	}
	return func() { unlockFileEx.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&ov))); _ = f.Close() }, nil
}
