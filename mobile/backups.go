package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const backupMaxCompressed = 64 << 20
const backupMaxUnpacked = 128 << 20
const backupMaxFiles = 512

type BackupFile struct {
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}
type BackupManifest struct {
	Format       int                   `json:"format"`
	Program      string                `json:"program"`
	Version      string                `json:"version"`
	Created      string                `json:"created"`
	Architecture string                `json:"architecture"`
	Complete     bool                  `json:"complete"`
	Files        map[string]BackupFile `json:"files"`
}
type BackupView struct {
	ID        string `json:"id"`
	Created   string `json:"created"`
	Bytes     int64  `json:"bytes"`
	Automatic bool   `json:"automatic"`
	Complete  bool   `json:"complete"`
}
type backupRestoreJournal struct {
	Version      int      `json:"version"`
	Before       string   `json:"before"`
	BeforeSHA256 string   `json:"beforeSHA256"`
	Targets      []string `json:"targets"`
	PreviousOn   bool     `json:"previousOn"`
}

var backupIDPattern = regexp.MustCompile(`^(manual|auto)-[0-9]{8}-[0-9]{6}-[a-f0-9]{8}\.tar\.gz$`)
var backupAssetPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+\.(dat|mmdb|metadb|mrs)$`)
var backupStateNames = map[string]bool{
	"settings.json": true, "profiles.json": true, "profiles-previous.json": true,
	"devices.json": true, "traffic.json": true, "smart.json": true, "recovery.json": true,
	"services.json": true, "platform-checks.json": true, "maintenance.json": true,
	"selectors.json": true, "device-layout.json": true,
	"previous.yaml": true, "devices.previous.json": true, "devices-rules.previous.json": true, "devices-v1-rules.json": true,
}

// Only data understood by this installation is restored. Backup-supplied shell
// scripts, hooks, absolute paths and executable directives are never installed.
func validBackupName(n string) bool {
	if n == "" || n != path.Clean(n) || strings.ContainsAny(n, "\\\x00") || strings.HasPrefix(n, "/") || strings.Contains(n, ":") {
		return false
	}
	if n == "manifest.json" || n == "installation.json" || n == "RESTORE.txt" || n == "router/config.yaml" || n == "programs/mihomo" || n == "programs/mobile-panel" {
		return true
	}
	if strings.HasPrefix(n, "state/") {
		s := strings.TrimPrefix(n, "state/")
		if backupStateNames[s] {
			return true
		}
		if strings.HasPrefix(s, "subscription-cache/") {
			return profileCachePattern.MatchString(strings.TrimPrefix(s, "subscription-cache/"))
		}
		return false
	}
	if strings.HasPrefix(n, "geodata/") {
		s := strings.TrimPrefix(n, "geodata/")
		return backupAssetPattern.MatchString(s)
	}
	return false
}
func restoreDataName(n string) bool {
	return n == "router/config.yaml" || strings.HasPrefix(n, "state/") || strings.HasPrefix(n, "geodata/")
}
func backupHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func (a *App) backupDir() string { return filepath.Join(a.store, "backups") }
func (a *App) backupTarget(n string) (string, error) {
	if !validBackupName(n) || !restoreDataName(n) {
		return "", errors.New("备份包含不支持的文件")
	}
	if n == "router/config.yaml" {
		return filepath.Join(a.dir, "config.yaml"), nil
	}
	if strings.HasPrefix(n, "geodata/") {
		return filepath.Join(a.dir, path.Base(n)), nil
	}
	return filepath.Join(a.store, filepath.FromSlash(strings.TrimPrefix(n, "state/"))), nil
}

func readBackupSource(p string, limit int64) ([]byte, error) {
	s, e := os.Lstat(p)
	if e != nil {
		return nil, e
	}
	if !s.Mode().IsRegular() || s.Size() > limit {
		return nil, errors.New("备份文件类型或大小不受支持")
	}
	f, e := os.Open(p)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil {
		return nil, e
	}
	if int64(len(b)) > limit {
		return nil, errors.New("备份文件过大")
	}
	return b, nil
}

func (a *App) collectBackupFiles(complete bool) (map[string][]byte, error) {
	files := map[string][]byte{}
	add := func(n, p string, required bool) error {
		b, e := readBackupSource(p, backupMaxUnpacked)
		if os.IsNotExist(e) && !required {
			return nil
		}
		if e != nil {
			return errors.New("读取备份所需文件失败，当前网络未修改")
		}
		files[n] = b
		return nil
	}
	if e := add("router/config.yaml", filepath.Join(a.dir, "config.yaml"), true); e != nil {
		return nil, e
	}
	for n := range backupStateNames {
		if e := add("state/"+n, filepath.Join(a.store, n), n == "settings.json"); e != nil {
			return nil, e
		}
	}
	entries, e := os.ReadDir(filepath.Join(a.store, "subscription-cache"))
	if e != nil && !os.IsNotExist(e) {
		return nil, e
	}
	for _, entry := range entries {
		if profileCachePattern.MatchString(entry.Name()) {
			if e := add("state/subscription-cache/"+entry.Name(), filepath.Join(a.store, "subscription-cache", entry.Name()), true); e != nil {
				return nil, e
			}
		}
	}
	if complete {
		entries, _ = os.ReadDir(a.dir)
		for _, entry := range entries {
			if backupAssetPattern.MatchString(entry.Name()) {
				if e := add("geodata/"+entry.Name(), filepath.Join(a.dir, entry.Name()), true); e != nil {
					return nil, e
				}
			}
		}
		files["installation.json"], _ = json.Marshal(map[string]any{"version": "4.0", "directory": a.dir, "panelAddress": a.host, "boot": a.bootStatus(), "restoreProgramsAutomatically": false})
		if e := add("programs/mihomo", filepath.Join(a.dir, "mihomo"), true); e != nil {
			return nil, e
		}
		program, e := os.Executable()
		if e != nil {
			return nil, e
		}
		if e = add("programs/mobile-panel", program, true); e != nil {
			return nil, e
		}
		files["RESTORE.txt"] = []byte("小米网络 v4 完整备份\n\n在面板 更多 → 备份与恢复 中上传本文件可恢复订阅、规则、服务分流、设备设置和统计数据。恢复会保留当前面板登录密码、程序和开机恢复入口。\nprograms/mihomo 与 programs/mobile-panel 为离线重新安装提供程序文件（ARM64 路由器）。不要在 Windows 上运行。路由器程序丢失时，使用随更新包提供的安装工具，或联系维护者按版本安装。\n本文件包含你的订阅和节点信息，请作为私人备份保存。\n")
	}
	return files, nil
}

func encodeBackup(w io.Writer, files map[string][]byte, complete bool, now time.Time) error {
	manifest := BackupManifest{Format: 1, Program: "xiaomi-private-mihomo-panel", Version: "4.0", Created: now.Format(time.RFC3339), Architecture: "linux-arm64", Complete: complete, Files: map[string]BackupFile{}}
	var total int64
	for n, b := range files {
		if !validBackupName(n) || n == "manifest.json" {
			return errors.New("备份文件名无效")
		}
		total += int64(len(b))
		manifest.Files[n] = BackupFile{SHA256: backupHash(b), Bytes: int64(len(b))}
	}
	if total > backupMaxUnpacked || len(files) > backupMaxFiles {
		return errors.New("备份内容过大，请先整理旧数据")
	}
	mb, _ := json.Marshal(manifest)
	all := make(map[string][]byte, len(files)+1)
	for n, b := range files {
		all[n] = b
	}
	all["manifest.json"] = mb
	keys := make([]string, 0, len(all))
	for n := range all {
		keys = append(keys, n)
	}
	sort.Strings(keys)
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, n := range keys {
		b := all[n]
		if e := tw.WriteHeader(&tar.Header{Name: n, Mode: 0600, Size: int64(len(b)), ModTime: now, Typeflag: tar.TypeReg}); e != nil {
			return e
		}
		if _, e := tw.Write(b); e != nil {
			return e
		}
	}
	if e := tw.Close(); e != nil {
		return e
	}
	return gz.Close()
}

func decodeBackup(r io.Reader) (map[string][]byte, BackupManifest, error) {
	var empty BackupManifest
	lr := &io.LimitedReader{R: r, N: backupMaxCompressed + 1}
	gz, e := gzip.NewReader(lr)
	if e != nil {
		return nil, empty, errors.New("请选择面板导出的 tar.gz 备份文件")
	}
	defer gz.Close()
	gz.Multistream(false)
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	var total int64
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, empty, errors.New("备份文件不完整或已损坏")
		}
		if !validBackupName(h.Name) || h.Typeflag != tar.TypeReg || h.Size < 0 || h.Size > backupMaxUnpacked || len(files) >= backupMaxFiles+1 {
			return nil, empty, errors.New("备份包含不支持的路径、链接或文件类型")
		}
		if _, dup := files[h.Name]; dup {
			return nil, empty, errors.New("备份包含重复文件，未执行恢复")
		}
		total += h.Size
		if total > backupMaxUnpacked {
			return nil, empty, errors.New("备份解压后的内容过大")
		}
		b, e := io.ReadAll(io.LimitReader(tr, h.Size+1))
		if e != nil || int64(len(b)) != h.Size {
			return nil, empty, errors.New("备份文件不完整")
		}
		files[h.Name] = b
	}
	// Consume gzip checksum/trailer even when tar ends at its zero blocks.
	if _, e = io.Copy(io.Discard, gz); e != nil || lr.N <= 0 {
		return nil, empty, errors.New("备份校验失败或文件过大")
	}
	var m BackupManifest
	if json.Unmarshal(files["manifest.json"], &m) != nil || m.Format != 1 || m.Program != "xiaomi-private-mihomo-panel" || m.Architecture != "linux-arm64" || m.Files == nil {
		return nil, empty, errors.New("备份版本或设备类型不兼容")
	}
	delete(files, "manifest.json")
	if len(m.Files) != len(files) {
		return nil, empty, errors.New("备份文件清单不完整")
	}
	for n, b := range files {
		f, ok := m.Files[n]
		if !ok || f.Bytes != int64(len(b)) || f.SHA256 != backupHash(b) {
			return nil, empty, errors.New("备份内容校验失败")
		}
	}
	if len(files["router/config.yaml"]) == 0 || len(files["state/settings.json"]) == 0 {
		return nil, empty, errors.New("备份缺少配置或设置")
	}
	return files, m, nil
}

func (a *App) listBackups() []BackupView {
	views := []BackupView{}
	entries, _ := os.ReadDir(a.backupDir())
	for _, entry := range entries {
		if !backupIDPattern.MatchString(entry.Name()) {
			continue
		}
		s, e := entry.Info()
		if e != nil || !s.Mode().IsRegular() {
			continue
		}
		views = append(views, BackupView{ID: entry.Name(), Created: s.ModTime().Format(time.RFC3339Nano), Bytes: s.Size(), Automatic: strings.HasPrefix(entry.Name(), "auto-"), Complete: strings.HasPrefix(entry.Name(), "manual-")})
	}
	sort.Slice(views, func(i, j int) bool {
		ti, _ := time.Parse(time.RFC3339Nano, views[i].Created)
		tj, _ := time.Parse(time.RFC3339Nano, views[j].Created)
		return ti.After(tj)
	})
	return views
}

func (a *App) pruneBackups() {
	if exists(filepath.Join(a.store, "backup-restore-pending.json")) {
		return
	}
	counts := map[bool]int{}
	for _, v := range a.listBackups() {
		counts[v.Automatic]++
		keep := 1
		if v.Automatic {
			keep = 3
		}
		if counts[v.Automatic] > keep {
			_ = os.Remove(filepath.Join(a.backupDir(), v.ID))
		}
	}
}

func (a *App) createBackup(automatic bool) (BackupView, error) {
	return a.createBackupWithAssets(automatic, nil)
}

func (a *App) createBackupWithAssets(automatic bool, assets map[string][]byte) (BackupView, error) {
	a.saveUsage(time.Now())
	files, e := a.collectBackupFiles(!automatic)
	if e != nil {
		return BackupView{}, e
	}
	for name, data := range assets {
		if !strings.HasPrefix(name, "geodata/") || !validBackupName(name) {
			return BackupView{}, errors.New("恢复备份中的数据库路径无效")
		}
		files[name] = data
	}
	if e = os.MkdirAll(a.backupDir(), 0700); e != nil {
		return BackupView{}, e
	}
	prefix := "manual"
	if automatic {
		prefix = "auto"
	}
	id := prefix + "-" + time.Now().Format("20060102-150405") + "-" + random()[:8] + ".tar.gz"
	// First compress in RAM-backed /tmp, then reserve the actual output size on
	// the small persistent partition. Repeated full backups remain possible.
	f, e := os.CreateTemp("", "mihomo-backup-*.tar.gz")
	if e != nil {
		return BackupView{}, e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	os.Chmod(tmp, 0600)
	e = encodeBackup(f, files, !automatic, time.Now())
	if e == nil {
		e = f.Sync()
	}
	if ce := f.Close(); e == nil {
		e = ce
	}
	if e != nil {
		os.Remove(tmp)
		return BackupView{}, errors.New("备份写入失败，当前网络未修改")
	}
	st, e := os.Stat(tmp)
	if e != nil {
		return BackupView{}, e
	}
	if st.Size() > backupMaxCompressed {
		os.Remove(tmp)
		return BackupView{}, errors.New("备份文件过大，未保存")
	}
	space := storageInfo(a.store)
	if space.Available && space.Free < uint64(st.Size())+2<<20 {
		return BackupView{}, errors.New("路由器空间不足以安全保存备份，请先下载并清理旧备份")
	}
	in, e := os.Open(tmp)
	if e != nil {
		return BackupView{}, e
	}
	defer in.Close()
	dst := filepath.Join(a.backupDir(), id+".new")
	out, e := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return BackupView{}, e
	}
	_, e = io.Copy(out, in)
	if e == nil {
		e = out.Sync()
	}
	if ce := out.Close(); e == nil {
		e = ce
	}
	if e != nil {
		os.Remove(dst)
		return BackupView{}, errors.New("备份保存失败，当前网络未修改")
	}
	if e = os.Rename(dst, filepath.Join(a.backupDir(), id)); e != nil {
		os.Remove(dst)
		return BackupView{}, e
	}
	v := BackupView{ID: id, Created: st.ModTime().Format(time.RFC3339), Bytes: st.Size(), Automatic: automatic, Complete: !automatic}
	a.pruneBackups()
	return v, nil
}

func (a *App) prepareAutomaticBackup(action string) error {
	switch action {
	case "sub-save", "sub-switch", "sub-update", "subscription", "restore", "rule-save", "rule-switch", "rule-remove", "rule-auto", "service-save", "service-settings", "services-save", "devices", "device-defaults":
		_, e := a.createBackup(true)
		if e != nil {
			return errors.New("修改前的安全备份未完成，当前设置已保留；请检查存储空间")
		}
		return nil
	}
	return nil
}

// Validate the complete staged data before any live file or network rule is
// changed. The Mihomo validator receives only a staged file, never a URL.
func (a *App) validateBackupState(files map[string][]byte) error {
	var c map[string]any
	if yaml.Unmarshal(files["router/config.yaml"], &c) != nil || c == nil {
		return errors.New("备份中的代理配置无法读取")
	}
	// Restoring this private panel is not a router migration. Keep the verified
	// LAN DNS, inbound ports, local API and disabled UDP/TUN design intact.
	if current, e := a.config(); e == nil {
		for _, key := range []string{"mixed-port", "redir-port", "tproxy-port", "port", "socks-port", "external-controller", "external-controller-tls", "bind-address", "interface-name", "routing-mark", "tun"} {
			if !reflect.DeepEqual(c[key], current[key]) {
				return errors.New("备份中的路由器端口或网络模式与当前安装不兼容，未修改网络")
			}
		}
		oldDNS, _ := current["dns"].(map[string]any)
		newDNS, _ := c["dns"].(map[string]any)
		if !reflect.DeepEqual(oldDNS["listen"], newDNS["listen"]) || !reflect.DeepEqual(oldDNS["enable"], newDNS["enable"]) {
			return errors.New("备份中的 DNS 监听与当前路由器不兼容，未修改网络")
		}
	}
	var s State
	if json.Unmarshal(files["state/settings.json"], &s) != nil {
		return errors.New("备份中的面板设置无法读取")
	}
	for _, raw := range []string{s.URL, s.PreviousURL} {
		if raw != "" {
			if _, e := cleanSubscriptionURL(raw); e != nil {
				return errors.New("备份中的订阅设置无效")
			}
		}
	}
	if b := files["state/devices.json"]; b != nil {
		var d DeviceConfig
		if json.Unmarshal(b, &d) != nil || d.Version != 2 || d.Devices == nil || validateDeviceConfig(&d) != nil {
			return errors.New("备份中的设备策略无效")
		}
	}
	if b := files["state/profiles.json"]; b != nil {
		var p ProfileStore
		if json.Unmarshal(b, &p) != nil || p.Version != 1 || len(p.Subscriptions) > 20 || len(p.Rules) > 30 {
			return errors.New("备份中的订阅或规则资料无效")
		}
		ids := map[string]bool{}
		for _, sub := range p.Subscriptions {
			if !profileIDPattern.MatchString(sub.ID) || ids[sub.ID] {
				return errors.New("备份中的订阅编号无效")
			}
			ids[sub.ID] = true
			if _, e := cleanSubscriptionURL(sub.URL); e != nil {
				return errors.New("备份中的订阅链接无效")
			}
		}
		if p.ActiveSub != "" && subIndex(&p, p.ActiveSub) < 0 {
			return errors.New("备份中的当前订阅不存在")
		}
		ids = map[string]bool{}
		for _, r := range p.Rules {
			if !profileIDPattern.MatchString(r.ID) || ids[r.ID] || len(r.Rules) == 0 || len(r.Rules) > 5000 {
				return errors.New("备份中的规则方案无效")
			}
			ids[r.ID] = true
			if validateProfileRules(r.Rules, c) != nil {
				return errors.New("备份中的规则引用不存在的分组或格式无效")
			}
		}
		if ruleIndex(&p, p.ActiveRule) < 0 {
			return errors.New("备份中的当前规则不存在")
		}
	}
	if b := files["state/selectors.json"]; b != nil {
		var selectors map[string]string
		if json.Unmarshal(b, &selectors) != nil || selectors == nil || len(selectors) > 128 {
			return errors.New("备份中的节点选择记录无效")
		}
		for k, v := range selectors {
			if len(k) > 1024 || len(v) > 1024 || strings.ContainsAny(k+v, "\x00\r\n") {
				return errors.New("备份中的节点选择记录无效")
			}
		}
	}
	if b := files["state/device-layout.json"]; b != nil {
		var l DeviceLayout
		if json.Unmarshal(b, &l) != nil || l.Version != 1 || l.Devices == nil || len(l.Devices) > 512 {
			return errors.New("备份中的设备整理设置无效")
		}
		for mac := range l.Devices {
			if canonical, e := canonicalMAC(mac); e != nil || canonical != mac {
				return errors.New("备份中的设备整理记录无效")
			}
		}
	}
	if b := files["state/traffic.json"]; b != nil {
		var l UsageLedger
		if json.Unmarshal(b, &l) != nil || l.Version != 1 || l.Devices == nil || len(l.Devices) > 4096 {
			return errors.New("备份中的用量历史无效")
		}
		for mac, v := range l.Devices {
			if _, e := canonicalMAC(mac); e != nil || v == nil {
				return errors.New("备份中的用量设备记录无效")
			}
		}
	}
	if b := files["state/smart.json"]; b != nil {
		var s smartSaved
		if json.Unmarshal(b, &s) != nil || s.Version != 1 || validSmartOverrides(s.Overrides) != nil {
			return errors.New("备份中的智能选择设置无效")
		}
	}
	if b := files["state/recovery.json"]; b != nil {
		var s RecoverySettings
		if json.Unmarshal(b, &s) != nil || s.Version != 1 {
			return errors.New("备份中的自动恢复设置无效")
		}
	}
	if b := files["state/services.json"]; b != nil {
		var s ServicesStore
		if json.Unmarshal(b, &s) != nil || validateServicesStore(&s) != nil {
			return errors.New("备份中的服务分流设置无效")
		}
	}
	if b := files["state/platform-checks.json"]; b != nil {
		var s PlatformStore
		if json.Unmarshal(b, &s) != nil || validatePlatformStore(&s) != nil {
			return errors.New("备份中的平台检查记录无效")
		}
	}
	for n, b := range files {
		if strings.HasPrefix(n, "state/") && strings.HasSuffix(n, ".json") && !json.Valid(b) {
			return errors.New("备份中的设置文件损坏")
		}
	}
	staged := filepath.Join(a.dir, "mobile-backup-candidate.yaml")
	if e := atomicWrite(staged, files["router/config.yaml"]); e != nil {
		return errors.New("无法暂存备份配置，当前网络未修改")
	}
	defer os.Remove(staged)
	return a.validate(staged)
}

func (a *App) restoreWriteFiles(files map[string][]byte, targets []string) error {
	// Settings are written last to keep the old credentials valid throughout.
	sort.SliceStable(targets, func(i, j int) bool { return targets[i] != "state/settings.json" && targets[j] == "state/settings.json" })
	for _, n := range targets {
		p, e := a.backupTarget(n)
		if e != nil {
			return e
		}
		if b, ok := files[n]; ok {
			if old, e := os.ReadFile(p); e == nil && bytes.Equal(old, b) {
				continue
			}
			if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
				return e
			}
			if e = atomicWrite(p, b); e != nil {
				return e
			}
		} else if e = os.Remove(p); e != nil && !os.IsNotExist(e) {
			return e
		}
	}
	return nil
}

func (a *App) reloadBackupState() error {
	b, e := os.ReadFile(filepath.Join(a.store, "settings.json"))
	if e != nil {
		return e
	}
	var s State
	if e = json.Unmarshal(b, &s); e != nil {
		return e
	}
	a.stateMu.Lock()
	a.state = s
	a.stateMu.Unlock()
	var d *DeviceConfig
	if b, e = os.ReadFile(filepath.Join(a.store, "devices.json")); e == nil {
		if e = json.Unmarshal(b, &d); e != nil {
			return e
		}
	}
	a.setDeviceConfig(d)
	var p *ProfileStore
	if b, e = os.ReadFile(filepath.Join(a.store, "profiles.json")); e == nil {
		if e = json.Unmarshal(b, &p); e != nil {
			return e
		}
	}
	a.setProfiles(p)
	a.usageMu.Lock()
	a.initUsage()
	a.usageViews = nil
	a.usageConnections = nil
	a.usageProxyLastSample = time.Time{}
	a.usageProxySpeedReady = false
	a.usageMu.Unlock()
	a.initSmart()
	a.smart.mu.Lock()
	saved := defaultSmartSaved()
	if b, e = os.ReadFile(filepath.Join(a.store, "smart.json")); e == nil {
		_ = json.Unmarshal(b, &saved)
	}
	a.smart.saved = saved
	a.smart.mu.Unlock()
	a.initRecovery()
	a.recovery.mu.Lock()
	rs := RecoverySettings{Version: 1, Enabled: true}
	if b, e = os.ReadFile(filepath.Join(a.store, "recovery.json")); e == nil {
		_ = json.Unmarshal(b, &rs)
	}
	if d != nil {
		rs.DesiredOn = d.Enabled
	}
	a.recovery.saved = rs
	a.recovery.attempts = 0
	a.recovery.next = time.Time{}
	a.recovery.mu.Unlock()
	return a.reloadServiceState()
}

// Prevent the periodic checkpoint from replacing an imported ledger with the
// pre-restore in-memory copy between the atomic file write and the reload.
func (a *App) restoreAndReloadBackupState(files map[string][]byte, targets []string) error {
	a.usageFileMu.Lock()
	defer a.usageFileMu.Unlock()
	if err := a.restoreWriteFiles(files, targets); err != nil {
		return err
	}
	return a.reloadBackupState()
}

func (a *App) restoreBackup(r io.Reader) error {
	if a.transactionPending() {
		return errors.New("还有一项配置操作未完成，请先修复或重试")
	}
	files, _, e := decodeBackup(r)
	if e != nil {
		return e
	}
	// Program files are verified by the archive reader but never installed by a
	// settings restore. Release them before spawning the memory-heavy validator.
	for name := range files {
		if !restoreDataName(name) {
			delete(files, name)
		}
	}
	debug.FreeOSMemory()
	if e = a.validateBackupState(files); e != nil {
		return e
	}
	// Login belongs to the installed panel. Restoring an old backup must not
	// invalidate the open browser session or unexpectedly change its password.
	var imported State
	_ = json.Unmarshal(files["state/settings.json"], &imported)
	auth := a.snapshot()
	imported.Salt = auth.Salt
	imported.Hash = auth.Hash
	imported.Token = auth.Token
	files["state/settings.json"], _ = json.Marshal(imported)
	current, e := a.collectBackupFiles(false)
	if e != nil {
		return e
	}
	// Ordinary changes need only a small settings snapshot. A full restore also
	// backs up precisely the geodata it will replace, preserving safe rollback
	// without retaining three duplicate databases on this small router partition.
	assets := map[string][]byte{}
	for name, data := range files {
		if !strings.HasPrefix(name, "geodata/") {
			continue
		}
		path, err := a.backupTarget(name)
		if err != nil {
			return err
		}
		old, err := readBackupSource(path, backupMaxUnpacked)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if bytes.Equal(old, data) {
			delete(files, name)
			continue
		}
		current[name] = old
		assets[name] = old
	}
	before, e := a.createBackupWithAssets(true, assets)
	if e != nil {
		return errors.New("恢复前安全备份失败，当前网络未修改")
	}
	beforePath := filepath.Join(a.backupDir(), before.ID)
	beforeBytes, e := os.ReadFile(beforePath)
	if e != nil {
		return e
	}
	targetSet := map[string]bool{}
	for n := range current {
		if restoreDataName(n) {
			targetSet[n] = true
		}
	}
	for n := range files {
		if restoreDataName(n) {
			targetSet[n] = true
		}
	}
	targets := []string{}
	var required uint64
	for n := range targetSet {
		targets = append(targets, n)
		if !bytes.Equal(current[n], files[n]) {
			required += uint64(len(files[n]))
		}
	}
	if space := storageInfo(a.store); space.Available && space.Free < required+2<<20 {
		return errors.New("恢复所需的安全空间不足，当前网络未修改")
	}
	prevOn := false
	if c := a.deviceSnapshot(); c != nil {
		prevOn = c.Enabled
	}
	j := backupRestoreJournal{Version: 1, Before: before.ID, BeforeSHA256: backupHash(beforeBytes), Targets: targets, PreviousOn: prevOn}
	journal := filepath.Join(a.store, "backup-restore-pending.json")
	if e = writeJSON(journal, j); e != nil {
		return e
	}
	rollback := func() error { return a.recoverBackupRestore() }
	if e = a.suspendDevices(); e == nil {
		e = a.restoreAndReloadBackupState(files, targets)
	}
	on := false
	if d := a.deviceSnapshot(); d != nil {
		on = d.Enabled
	}
	if e == nil {
		e = a.restart(on)
	}
	if e != nil {
		if rollback() == nil {
			return errors.New("恢复没有完成，已自动恢复原配置和网络")
		}
		return errors.New("恢复未完成，原配置已保留在安全备份中；当前尽量保持普通直连")
	}
	if e = os.Remove(journal); e != nil {
		return errors.New("设置已恢复，但恢复记录无法清理，请查看详细状态")
	}
	a.recovery.mu.Lock()
	a.recovery.degraded = false
	a.recovery.state = "healthy"
	a.recovery.message = "已从完整备份恢复"
	a.recovery.mu.Unlock()
	a.pruneBackups()
	return nil
}

func (a *App) recoverBackupRestore() error {
	p := filepath.Join(a.store, "backup-restore-pending.json")
	b, e := os.ReadFile(p)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	var j backupRestoreJournal
	if json.Unmarshal(b, &j) != nil || j.Version != 1 || !backupIDPattern.MatchString(j.Before) || len(j.Targets) > backupMaxFiles {
		return errors.New("备份恢复记录无效，已保留现有文件")
	}
	for _, n := range j.Targets {
		if !validBackupName(n) || !restoreDataName(n) {
			return errors.New("备份恢复路径无效")
		}
	}
	b, e = readBackupSource(filepath.Join(a.backupDir(), j.Before), backupMaxCompressed)
	if e != nil || backupHash(b) != j.BeforeSHA256 {
		return errors.New("恢复前的安全备份校验失败，已保留现有文件")
	}
	files, _, e := decodeBackup(bytes.NewReader(b))
	if e != nil {
		return e
	}
	if e = a.restoreAndReloadBackupState(files, j.Targets); e != nil {
		return e
	}
	if e = a.restart(j.PreviousOn); e != nil {
		_ = a.suspendDevices()
		return errors.New("原配置文件已恢复，代理尚未启动；安全备份仍然保留")
	}
	return os.Remove(p)
}

// This method is called inside the existing authentication/origin wrapper.
func (a *App) handleRecoveryHTTP(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/api/recovery":
		if r.Method != "GET" {
			fail(w, 405, "不支持的请求")
		} else {
			respond(w, 200, a.recoverySnapshot())
		}
		return true
	case "/api/backup/download":
		if r.Method != "GET" {
			fail(w, 405, "不支持的请求")
			return true
		}
		id := r.URL.Query().Get("id")
		if id == "" {
			select {
			case a.gate <- struct{}{}:
			default:
				fail(w, 409, "上一项操作还在进行，请稍等")
				return true
			}
			defer func() { <-a.gate }()
			unlock, e := acquireManagerLock()
			if e != nil {
				fail(w, 409, e.Error())
				return true
			}
			defer unlock()
			v, e := a.createBackup(false)
			if e != nil {
				fail(w, 400, e.Error())
				return true
			}
			id = v.ID
		}
		if !backupIDPattern.MatchString(id) {
			fail(w, 400, "备份编号无效")
			return true
		}
		p := filepath.Join(a.backupDir(), id)
		f, e := os.Open(p)
		if e != nil {
			fail(w, 404, "备份文件不存在")
			return true
		}
		defer f.Close()
		st, e := f.Stat()
		if e != nil || !st.Mode().IsRegular() {
			fail(w, 404, "备份文件不可读取")
			return true
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"xiaomi-network-%s\"", id))
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, id, st.ModTime(), f)
		return true
	case "/api/backup/restore":
		if r.Method != "POST" {
			fail(w, 405, "不支持的请求")
			return true
		}
		r.Body = http.MaxBytesReader(w, r.Body, backupMaxCompressed+1<<20)
		if e := r.ParseMultipartForm(1 << 20); e != nil {
			fail(w, 400, "备份文件无法读取或超过 64 MB")
			return true
		}
		if r.MultipartForm != nil {
			defer r.MultipartForm.RemoveAll()
		}
		f, _, e := r.FormFile("backup")
		if e != nil {
			fail(w, 400, "请选择备份文件")
			return true
		}
		defer f.Close()
		// Upload data is staged in tmp before a job starts; the private temporary
		// path is generated by the process, never supplied by the browser.
		tmp, e := os.CreateTemp("", "mihomo-restore-*.tar.gz")
		if e != nil {
			fail(w, 500, "无法暂存备份文件")
			return true
		}
		name := tmp.Name()
		os.Chmod(name, 0600)
		_, e = io.Copy(tmp, io.LimitReader(f, backupMaxCompressed+1))
		if e == nil {
			e = tmp.Sync()
		}
		tmp.Close()
		st, _ := os.Stat(name)
		if e != nil || st == nil || st.Size() > backupMaxCompressed {
			os.Remove(name)
			fail(w, 400, "备份文件无法读取或过大")
			return true
		}
		a.waitSmartBackground()
		a.waitServiceBackground()
		select {
		case a.gate <- struct{}{}:
		default:
			os.Remove(name)
			fail(w, 409, "上一项操作还在进行，请稍等")
			return true
		}
		unlock, e := acquireManagerLock()
		if e != nil {
			<-a.gate
			os.Remove(name)
			fail(w, 409, e.Error())
			return true
		}
		j := Job{ID: random()[:16], Action: "backup-restore", Running: true, Message: "正在检查并恢复备份"}
		a.jobMu.Lock()
		a.job = j
		a.jobMu.Unlock()
		go func() {
			defer unlock()
			defer func() { <-a.gate }()
			defer os.Remove(name)
			f, e := os.Open(name)
			if e == nil {
				e = a.restoreBackup(f)
				f.Close()
			}
			a.jobMu.Lock()
			defer a.jobMu.Unlock()
			a.job.Running = false
			a.job.Error = e != nil
			if e != nil {
				a.job.Message = e.Error()
			} else {
				a.job.Message = "备份已恢复，登录密码和已安装程序保持当前版本"
			}
		}()
		respond(w, 202, j)
		return true
	}
	return false
}
