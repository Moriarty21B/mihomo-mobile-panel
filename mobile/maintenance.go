package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Maintenance struct {
	Checked    string      `json:"checked"`
	Cleaned    string      `json:"cleaned"`
	Freed      uint64      `json:"freed"`
	TotalFreed uint64      `json:"totalFreed"`
	LogBytes   uint64      `json:"logBytes"`
	Storage    StorageInfo `json:"storage"`
	Error      string      `json:"error,omitempty"`
}
type StorageInfo struct {
	Total     uint64 `json:"total"`
	Free      uint64 `json:"free"`
	Available bool   `json:"available"`
	Low       bool   `json:"low"`
}
type LogTarget struct {
	Path, Kind  string
	Limit, Keep int64
	Expire      bool
}
type LogResult struct {
	Freed, Total uint64
	Changed      bool
	Error        string
}

func safeLog(path string) (os.FileInfo, bool) {
	s, e := os.Lstat(path)
	return s, e == nil && s.Mode().IsRegular()
}
func fileTail(path string, n int64) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	s, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if s.Size() > n {
		f.Seek(-n, io.SeekEnd)
	}
	b, e := io.ReadAll(io.LimitReader(f, n))
	if len(b) > 0 && s.Size() > n {
		if i := strings.IndexByte(string(b), '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	return b, e
}
func openLogAppend(path string) (bool, bool) {
	active, appendOnly := false, true
	paths, _ := filepath.Glob("/proc/[0-9]*/fd/*")
	for _, p := range paths {
		dest, e := os.Readlink(p)
		if e != nil || dest != path {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(p)), "fdinfo", filepath.Base(p)))
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) != 2 || f[0] != "flags:" {
				continue
			}
			flags, e := strconv.ParseUint(f[1], 8, 64)
			if e != nil {
				continue
			}
			if flags&3 == 0 {
				continue
			}
			active = true
			if flags&1024 == 0 {
				appendOnly = false
			}
		}
	}
	return active, appendOnly
}
func managedArchives(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var paths []string
	for _, e := range entries {
		n := e.Name()
		if (strings.HasPrefix(n, "engine-") || strings.HasPrefix(n, "panel-") || strings.HasPrefix(n, "legacy-")) && strings.HasSuffix(n, ".log.gz") {
			p := filepath.Join(dir, n)
			if _, ok := safeLog(p); ok {
				paths = append(paths, p)
			}
		}
	}
	return paths
}
func maintainLogs(targets []LogTarget, archive string, now time.Time, opened func(string) (bool, bool)) LogResult {
	r := LogResult{}
	archiveSize := func() uint64 {
		var n uint64
		for _, p := range managedArchives(archive) {
			s, _ := os.Stat(p)
			n += uint64(s.Size())
		}
		return n
	}
	before := archiveSize()
	for _, t := range targets {
		if s, ok := safeLog(t.Path); ok {
			before += uint64(s.Size())
		}
	}
	for _, t := range targets {
		s, ok := safeLog(t.Path)
		if !ok {
			continue
		}
		active, appendOnly := opened(t.Path)
		if t.Expire && !active && now.Sub(s.ModTime()) > 7*24*time.Hour {
			if os.Remove(t.Path) == nil {
				r.Changed = true
			}
			continue
		}
		if s.Size() <= t.Limit {
			continue
		}
		if active && !appendOnly {
			r.Error = "发现旧日志仍在写入，已保留文件；可在更多里重启代理后清理"
			continue
		}
		old, e := fileTail(t.Path, t.Limit)
		if e != nil {
			r.Error = "有日志暂时无法读取，下次会自动重试"
			continue
		}
		if e = os.MkdirAll(archive, 0700); e != nil {
			r.Error = "日志备份目录无法写入"
			continue
		}
		// Archive the last bounded segment in RAM storage; never unlink an open log.
		p := filepath.Join(archive, fmt.Sprintf("%s-%d.log.gz", t.Kind, now.UnixNano()))
		f, e := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			r.Error = "日志备份失败，原日志已保留"
			continue
		}
		z := gzip.NewWriter(f)
		_, e = z.Write(old)
		ce := z.Close()
		fe := f.Close()
		if e != nil || ce != nil || fe != nil {
			os.Remove(p)
			r.Error = "日志备份失败，原日志已保留"
			continue
		}
		keep, e := fileTail(t.Path, t.Keep)
		if e != nil {
			continue
		}
		f, e = os.OpenFile(t.Path, os.O_WRONLY, 0600)
		if e == nil {
			e = f.Truncate(0)
			if e == nil {
				_, e = f.WriteAt(keep, 0)
			}
			f.Close()
		}
		if e != nil {
			r.Error = "日志缩减失败，下次会自动重试"
		} else {
			r.Changed = true
		}
	}
	paths := managedArchives(archive)
	sort.Slice(paths, func(i, j int) bool {
		a, _ := os.Stat(paths[i])
		b, _ := os.Stat(paths[j])
		return a.ModTime().Before(b.ModTime())
	})
	for _, p := range paths {
		s, _ := os.Stat(p)
		if now.Sub(s.ModTime()) > 7*24*time.Hour {
			if os.Remove(p) == nil {
				r.Changed = true
			}
		}
	}
	var total uint64
	for _, t := range targets {
		if s, ok := safeLog(t.Path); ok {
			total += uint64(s.Size())
		}
	}
	total += archiveSize()
	for _, p := range paths {
		if total <= 10<<20 {
			break
		}
		s, ok := safeLog(p)
		if !ok {
			continue
		}
		if os.Remove(p) == nil {
			total -= uint64(s.Size())
			r.Changed = true
		}
	}
	r.Total = total
	if before > total {
		r.Freed = before - total
	}
	return r
}
func (a *App) cleanLogs(now time.Time) {
	a.maintenanceMu.Lock()
	defer a.maintenanceMu.Unlock()
	r := maintainLogs([]LogTarget{
		{"/tmp/mihomo-mobile-engine.log", "engine", 2 << 20, 512 << 10, false},
		{"/tmp/mihomo-mobile.log", "panel", 512 << 10, 128 << 10, false},
		{"/tmp/mihomo-mobile-panel.log", "panel-boot", 512 << 10, 128 << 10, false},
		{"/tmp/mihomo-mobile-boot.log", "boot", 512 << 10, 128 << 10, false},
		{filepath.Join(a.dir, "mihomo.log"), "legacy", 2 << 20, 256 << 10, true},
	}, "/tmp/mihomo-mobile-log-archive", now, openLogAppend)
	a.maintenance.Checked = now.Format(time.RFC3339)
	a.maintenance.Error = r.Error
	a.maintenance.LogBytes = r.Total
	if r.Changed {
		a.maintenance.Cleaned = now.Format(time.RFC3339)
		a.maintenance.Freed = r.Freed
		a.maintenance.TotalFreed += r.Freed
		b, _ := json.Marshal(a.maintenance)
		if atomicWrite(filepath.Join(a.store, "maintenance.json"), b) != nil {
			a.maintenance.Error = "日志已清理，但清理记录暂时无法保存"
		}
	}
}
func (a *App) maintenanceStatus() Maintenance {
	a.maintenanceMu.Lock()
	m := a.maintenance
	a.maintenanceMu.Unlock()
	m.Storage = storageInfo(a.dir)
	return m
}
func (a *App) maintenanceMonitor() {
	a.maintenanceMu.Lock()
	b, _ := os.ReadFile(filepath.Join(a.store, "maintenance.json"))
	_ = json.Unmarshal(b, &a.maintenance)
	a.maintenanceMu.Unlock()
	a.cleanLogs(time.Now())
	for range time.Tick(time.Minute) {
		a.cleanLogs(time.Now())
	}
}
