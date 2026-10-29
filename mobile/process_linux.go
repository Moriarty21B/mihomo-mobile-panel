package main

import (
	"os/exec"
	"syscall"
)

func detach(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

func storageInfo(path string) StorageInfo {
	var s syscall.Statfs_t
	if syscall.Statfs(path, &s) != nil {
		return StorageInfo{}
	}
	total, free := s.Blocks*uint64(s.Bsize), s.Bavail*uint64(s.Bsize)
	return StorageInfo{Total: total, Free: free, Available: true, Low: free < 8<<20}
}
