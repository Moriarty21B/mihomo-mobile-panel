package main

import (
	"bytes"
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed web/*
var assets embed.FS

//go:embed control.sh
var control string

const selectGroup = "节点选择"
const autoGroup = "自动选择"
const maxConfig = 2 << 20

type State struct {
	Salt            string `json:"salt"`
	Hash            string `json:"hash"`
	Token           string `json:"token"`
	URL             string `json:"url"`
	Updated         string `json:"updated"`
	PreviousURL     string `json:"previousUrl"`
	PreviousUpdated string `json:"previousUpdated"`
	Quota           *Quota `json:"quota,omitempty"`
	PreviousQuota   *Quota `json:"previousQuota,omitempty"`
}
type Job struct {
	ID      string `json:"id"`
	Action  string `json:"action"`
	Running bool   `json:"running"`
	Message string `json:"message"`
	Error   bool   `json:"error"`
	Result  any    `json:"result,omitempty"`
}
type App struct {
	dir, store, host     string
	state                State
	stateMu              sync.RWMutex
	job                  Job
	jobMu                sync.RWMutex
	gate                 chan struct{}
	loginMu              sync.Mutex
	attempts             int
	loginSince           time.Time
	validateHook         func(string) error
	restartHook          func(bool) error
	rulesHook            func() (Rules, error)
	deviceMu             sync.RWMutex
	deviceConfig         *DeviceConfig
	deviceError          string
	inventoryHook        func() []DeviceView
	ownedHook            func() (OwnedRules, error)
	commitHook           func(OwnedRules) error
	deviceHealthHook     func(bool) error
	usageMu              sync.RWMutex
	usageFileMu          sync.Mutex
	usage                *UsageLedger
	usageViews           []DeviceView
	usageUpdated         string
	usageError           string
	usageProxyOK         bool
	usageProxyLastSample time.Time
	usageProxySpeedReady bool
	usageConnections     map[string]ConnectionCursor
	usageBoot            string
	usageSessionStarted  time.Time
	maintenanceMu        sync.Mutex
	maintenance          Maintenance
	profileMu            sync.RWMutex
	profiles             *ProfileStore
	profileFetchHook     func(string) ([]byte, *Quota, error)
	smartOnce            sync.Once
	smart                *SmartManager
	runtimeMu            sync.RWMutex
	runtimeConnections   runtimeConnectionSample
	layoutMu             sync.Mutex
	startupPreferences   atomic.Bool
	recoveryOnce         sync.Once
	recovery             *RecoveryManager
	serviceMu            sync.Mutex
	serviceCancelMu      sync.Mutex
	serviceCancel        context.CancelFunc
	serviceDone          chan struct{}
	platformOnce         sync.Once
	platforms            *PlatformManager
}

func random() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func passwordHash(p, s string) string {
	b, _ := pbkdf2.Key(sha256.New, p, []byte(s), 120000, 32)
	return hex.EncodeToString(b)
}
func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
func atomicWrite(path string, b []byte) error {
	tmp := path + ".new"
	f, e := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		os.Remove(tmp)
		return e
	}
	if e = os.Rename(tmp, path); e != nil {
		os.Remove(tmp)
	}
	return e
}
func (a *App) save(s State) error {
	b, _ := json.Marshal(s)
	if e := atomicWrite(filepath.Join(a.store, "settings.json"), b); e != nil {
		return e
	}
	a.stateMu.Lock()
	a.state = s
	a.stateMu.Unlock()
	return nil
}
func (a *App) snapshot() State { a.stateMu.RLock(); defer a.stateMu.RUnlock(); return a.state }
func (a *App) config() (map[string]any, error) {
	b, e := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
	if e != nil {
		return nil, e
	}
	var c map[string]any
	e = yaml.Unmarshal(b, &c)
	return c, e
}
func (a *App) api(method, path string, body any) ([]byte, error) {
	return a.apiTimeout(method, path, body, 70*time.Second)
}
func (a *App) apiTimeout(method, path string, body any, timeout time.Duration) ([]byte, error) {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, method, "http://127.0.0.1:9091"+path, r)
	if e != nil {
		return nil, e
	}
	req.Header.Set("Content-Type", "application/json")
	if c, e := a.config(); e == nil {
		if secret, ok := c["secret"].(string); ok && secret != "" {
			req.Header.Set("Authorization", "Bearer "+secret)
		}
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: timeout}
	defer client.CloseIdleConnections()
	resp, e := client.Do(req)
	if e != nil {
		return nil, errors.New("暂时连不上代理服务，请在“更多”里尝试重启服务")
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if e != nil {
		return nil, e
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == 504 && strings.Contains(path, "/delay") {
			return nil, errors.New("节点测速超时（504）。请换可用节点或稍后重试；代理配置仍保留")
		}
		return nil, fmt.Errorf("代理服务返回 %d，请查看详细状态", resp.StatusCode)
	}
	return b, nil
}
func (a *App) sh(body string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	out, e := exec.CommandContext(ctx, "/bin/sh", "-c", control+"\n"+body).CombinedOutput()
	if e != nil {
		return string(out), fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

type Rules struct {
	TCP     int  `json:"tcp"`
	DNSUDP  int  `json:"dnsUdp"`
	DNSTCP  int  `json:"dnsTcp"`
	Total   int  `json:"total"`
	Enabled bool `json:"enabled"`
	Partial bool `json:"partial"`
}

func parseRules(s string) Rules {
	var r Rules
	for _, l := range strings.Split(s, "\n") {
		if !strings.HasPrefix(l, "-A PREROUTING ") {
			continue
		}
		isTCP := strings.Contains(l, "-j MIHOMO_TCP")
		isDNS := strings.Contains(l, "--dport 53 ") && strings.Contains(l, "--to-ports 1053") && strings.Contains(l, "-j REDIRECT")
		if !isTCP && !isDNS {
			continue
		}
		r.Total++
		if !strings.Contains(l, "-i br-lan ") || strings.Contains(l, " -s ") {
			continue
		}
		if isTCP && strings.Contains(l, "-p tcp ") {
			r.TCP++
		}
		if isDNS && strings.Contains(l, "-p tcp ") {
			r.DNSTCP++
		}
		if isDNS && strings.Contains(l, "-p udp ") {
			r.DNSUDP++
		}
	}
	r.Enabled = r.Total == 3 && r.TCP == 1 && r.DNSTCP == 1 && r.DNSUDP == 1
	r.Partial = r.Total > 0 && !r.Enabled
	return r
}
func (a *App) rules() (Rules, error) {
	if a.rulesHook != nil {
		return a.rulesHook()
	}
	b, e := exec.Command("iptables", "-t", "nat", "-S", "PREROUTING").Output()
	if bytes.Contains(b, []byte("-j MIHOMO_DEVICE")) {
		chain, ce := exec.Command("iptables", "-t", "nat", "-S", deviceChain).Output()
		if ce != nil {
			return Rules{Partial: true}, ce
		}
		r := Rules{}
		for _, l := range strings.Split(string(chain), "\n") {
			if strings.Contains(l, "--mac-source") {
				continue
			}
			if strings.Contains(l, "-j MIHOMO_TCP") {
				r.TCP++
			}
			if strings.Contains(l, "--dport 53") && strings.Contains(l, "--to-ports 1053") {
				if strings.Contains(l, "-p tcp ") {
					r.DNSTCP++
				}
				if strings.Contains(l, "-p udp ") {
					r.DNSUDP++
				}
			}
		}
		r.Total = r.TCP + r.DNSTCP + r.DNSUDP
		r.Enabled = r.TCP == 1 && r.DNSTCP == 1 && r.DNSUDP == 1
		r.Partial = r.Total > 0 && !r.Enabled
		return r, e
	}
	r := parseRules(string(b))
	if r.Enabled {
		chain, ce := exec.Command("iptables", "-t", "nat", "-S", "MIHOMO_TCP").Output()
		if ce != nil || !bytes.Contains(chain, []byte("-j REDIRECT --to-ports 7892")) {
			r.Enabled = false
			r.Partial = true
		}
	}
	return r, e
}
func (a *App) status() map[string]any {
	rules, re := a.rules()
	state := a.snapshot()
	result := map[string]any{"rules": rules, "updated": state.Updated, "hasSubscription": state.URL != "", "online": false, "nodes": []any{}, "selected": "", "actual": "", "quota": state.Quota, "maintenance": a.maintenanceStatus()}
	if re != nil {
		result["rulesError"] = "读取网络开关失败"
	}
	b, e := a.api("GET", "/proxies", nil)
	if e != nil {
		result["error"] = e.Error()
		return result
	}
	var data struct {
		Proxies map[string]struct {
			Now     string   `json:"now"`
			All     []string `json:"all"`
			Type    string   `json:"type"`
			History []struct {
				Delay int `json:"delay"`
			} `json:"history"`
		} `json:"proxies"`
	}
	if json.Unmarshal(b, &data) != nil {
		result["error"] = "节点信息暂时无法读取"
		return result
	}
	result["online"] = true
	result["selected"] = data.Proxies[selectGroup].Now
	result["actual"] = data.Proxies[autoGroup].Now
	nodes := []map[string]any{}
	for _, n := range data.Proxies[selectGroup].All {
		if quotaNode(n) {
			continue
		}
		p := data.Proxies[n]
		d := 0
		if len(p.History) > 0 {
			d = p.History[len(p.History)-1].Delay
		}
		nodes = append(nodes, map[string]any{"name": n, "delay": d, "type": p.Type})
	}
	result["nodes"] = nodes
	result["hasBackup"] = exists(filepath.Join(a.store, "previous.yaml"))
	return result
}
func exists(p string) bool { _, e := os.Stat(p); return e == nil }
func (a *App) validate(path string) error {
	if a.validateHook != nil {
		return a.validateHook(path)
	}
	key := a.validationKey(path)
	cache := filepath.Join(a.store, "validated-input.sha256")
	if previous, err := os.ReadFile(cache); key != "" && err == nil && string(previous) == key {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	out, e := exec.CommandContext(ctx, filepath.Join(a.dir, "mihomo"), "-t", "-d", a.dir, "-f", path).CombinedOutput()
	if e != nil { // Keep raw parser errors out of the page: they can contain subscription credentials.
		_ = atomicWrite(filepath.Join(a.store, "validation.log"), out)
		return errors.New("配置检查未通过，原配置没有被替换；请确认订阅是 Clash / Mihomo 格式")
	}
	if key != "" {
		_ = atomicWrite(cache, []byte(key))
	}
	return nil
}
func (a *App) enginePIDs() []int {
	entries, _ := filepath.Glob("/proc/[0-9]*/exe")
	var pids []int
	for _, f := range entries {
		p, e := os.Readlink(f)
		if e == nil && (p == filepath.Join(a.dir, "mihomo") || p == filepath.Join(a.dir, "mihomo")+" (deleted)") {
			pid, _ := strconv.Atoi(filepath.Base(filepath.Dir(f)))
			pids = append(pids, pid)
		}
	}
	return pids
}
func (a *App) restart(wantOn bool) error {
	if a.restartHook != nil {
		return a.restartHook(wantOn)
	}
	if e := a.reapplySavedConfig(); e != nil {
		return e
	}
	if e := a.validate(filepath.Join(a.dir, "config.yaml")); e != nil {
		return e
	}
	selected, e := a.selectorsForRestart(a.captureServiceSelectors)
	if e != nil {
		return e
	}
	if e := a.suspendDevices(); e != nil {
		return e
	}
	for _, pid := range a.enginePIDs() {
		p, e := os.FindProcess(pid)
		if e == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	}
	deadline := time.Now().Add(12 * time.Second)
	for len(a.enginePIDs()) > 0 && time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
	}
	for _, pid := range a.enginePIDs() {
		p, e := os.FindProcess(pid)
		if e == nil {
			_ = p.Kill()
		}
	}
	time.Sleep(500 * time.Millisecond)
	if len(a.enginePIDs()) > 0 {
		return errors.New("仍有其他程序在启动代理，已保持直连，请查看详细状态")
	}
	// Do not compete with any unrelated listener or an external watchdog.
	if _, e := a.sh("for p in 7890 7892 7893 1053 9091; do if has_port \"$p\" '^(tcp|udp)'; then echo '代理端口仍被占用，未重复启动'; exit 1; fi; done"); e != nil {
		return e
	}
	logPath := "/tmp/mihomo-mobile-engine.log"
	f, e := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|os.O_APPEND, 0600)
	if e != nil {
		return e
	}
	cmd := exec.Command(filepath.Join(a.dir, "mihomo"), "-d", a.dir)
	cmd.Dir = a.dir
	cmd.Stdout = f
	cmd.Stderr = f
	detach(cmd)
	e = cmd.Start()
	f.Close()
	if e != nil {
		return errors.New("代理服务启动失败，已保持直连")
	}
	go func() { _ = cmd.Wait() }()
	deadline = time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if _, e = a.sh("ready && has_port 7890 '^tcp' && has_port 9091 '^tcp'"); e == nil && len(a.enginePIDs()) == 1 {
			if _, e = a.api("GET", "/version", nil); e == nil {
				if e = a.restoreAvailableSelectors(selected); e != nil {
					return e
				}
				if e = a.restoreServiceSelections(); e != nil {
					return e
				}
				a.startupPreferences.Store(false)
				e = a.resumeDevices(wantOn)
				return e
			}
		}
		time.Sleep(time.Second)
	}
	return errors.New("代理未通过启动检查，已保持直连；请查看日志")
}

// A subscription supplies nodes, never router networking settings or executable directives.
func mergeNodes(current, incoming []byte) ([]byte, int, error) {
	var c, n map[string]any
	if yaml.Unmarshal(current, &c) != nil || yaml.Unmarshal(incoming, &n) != nil || len(c) == 0 {
		return nil, 0, errors.New("订阅格式无法识别，请使用 Clash / Mihomo 订阅链接")
	}
	proxies, ok := n["proxies"].([]any)
	if !ok || len(proxies) == 0 {
		return nil, 0, errors.New("订阅里没有节点列表。请使用包含节点的 Clash / Mihomo 订阅，不支持通用 Base64 或仅 provider 的链接")
	}
	if len(proxies) > 2000 {
		return nil, 0, errors.New("订阅节点过多，请使用较小的订阅")
	}
	if p, ok := c["proxy-providers"].(map[string]any); ok && len(p) > 0 {
		return nil, 0, errors.New("当前配置使用独立节点提供器，暂不直接替换，请先联系维护者适配")
	}
	groups, ok := c["proxy-groups"].([]any)
	if !ok {
		return nil, 0, errors.New("当前配置缺少节点分组，未修改")
	}
	keep := map[string]bool{"DIRECT": true, "REJECT": true, "REJECT-DROP": true, "PASS": true, "COMPATIBLE": true}
	for _, g := range groups {
		m, ok := g.(map[string]any)
		if !ok {
			return nil, 0, errors.New("当前节点分组格式异常")
		}
		name, _ := m["name"].(string)
		keep[name] = true
	}
	if !keep[selectGroup] || !keep[autoGroup] {
		return nil, 0, errors.New("当前配置缺少节点选择或自动选择组，未修改")
	}
	names := []any{}
	seen := map[string]bool{}
	for _, p := range proxies {
		m, ok := p.(map[string]any)
		if !ok {
			return nil, 0, errors.New("订阅中存在无效节点")
		}
		name, _ := m["name"].(string)
		typ, _ := m["type"].(string)
		if name == "" || typ == "" || seen[name] || keep[name] {
			return nil, 0, errors.New("订阅节点名称为空、重复或与分组冲突，未修改")
		}
		seen[name] = true
		names = append(names, name)
	}
	for _, g := range groups {
		m := g.(map[string]any)
		if m["name"] == "面板检测" {
			m["proxies"] = append([]any{}, names...)
			continue
		}
		list := []any{}
		used := map[string]bool{}
		if old, ok := m["proxies"].([]any); ok {
			for _, p := range old {
				s, ok := p.(string)
				if ok && keep[s] && !used[s] {
					list = append(list, s)
					used[s] = true
				}
			}
		}
		m["proxies"] = append(list, names...)
	}
	c["proxies"] = proxies
	b, e := yaml.Marshal(c)
	return b, len(proxies), e
}
func download(raw string) ([]byte, error) {
	b, _, e := downloadSubscription(raw)
	return b, e
}
func downloadSubscription(raw string) ([]byte, *Quota, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "。")
	u, e := url.Parse(raw)
	if e != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return nil, nil, errors.New("请粘贴完整的 http:// 或 https:// 订阅链接")
	}
	if len(raw) > 8192 {
		return nil, nil, errors.New("订阅链接过长")
	}
	for _, proxy := range []string{"", "http://127.0.0.1:7890"} {
		tr := &http.Transport{TLSHandshakeTimeout: 8 * time.Second, ResponseHeaderTimeout: 20 * time.Second}
		if proxy != "" {
			p, _ := url.Parse(proxy)
			tr.Proxy = http.ProxyURL(p)
		}
		client := &http.Client{Transport: tr, Timeout: 35 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 4 {
				return errors.New("重定向过多")
			}
			if req.URL.Scheme != "https" && req.URL.Scheme != "http" {
				return errors.New("不支持的链接")
			}
			return nil
		}}
		req, _ := http.NewRequest("GET", raw, nil)
		req.Header.Set("User-Agent", "Clash.Meta/1.19")
		resp, e := client.Do(req)
		if e != nil {
			tr.CloseIdleConnections()
			continue
		}
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, maxConfig+1))
		resp.Body.Close()
		tr.CloseIdleConnections()
		if len(b) > maxConfig {
			return nil, nil, errors.New("订阅文件超过 2 MB，未修改")
		}
		page := strings.ToLower(strings.TrimSpace(string(b)))
		if strings.HasPrefix(page, "<!doctype html") || strings.HasPrefix(page, "<html") {
			continue
		}
		if resp.StatusCode == 200 && readErr == nil {
			return b, parseQuota(resp.Header.Get("Subscription-Userinfo"), b, time.Now()), nil
		}
	}
	return nil, nil, errors.New("订阅下载失败，原配置未改动。请检查链接是否过期，或先换一个可用节点")
}

func (a *App) applyConfig(candidate []byte, next State) error {
	staged := filepath.Join(a.dir, "mobile-candidate.yaml")
	if e := atomicWrite(staged, candidate); e != nil {
		return errors.New("保存新配置失败，可能是路由器空间不足")
	}
	defer os.Remove(staged)
	if e := a.validate(staged); e != nil {
		return e
	}
	old, e := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
	if e != nil {
		return e
	}
	rules, e := a.rules()
	if e != nil {
		return e
	}
	wantOn := rules.Total > 0
	if c := a.deviceSnapshot(); c != nil {
		wantOn = c.Enabled
	}
	before := a.snapshot()
	journal := struct {
		State State `json:"state"`
		On    bool  `json:"on"`
	}{before, wantOn}
	jb, _ := json.Marshal(journal)
	if e = atomicWrite(filepath.Join(a.store, "rollback.yaml"), old); e != nil {
		return errors.New("备份失败，未修改当前配置")
	}
	if e = atomicWrite(filepath.Join(a.store, "pending.json"), jb); e != nil {
		return e
	}
	rollback := func(cause error) error {
		if e := atomicWrite(filepath.Join(a.dir, "config.yaml"), old); e != nil {
			return errors.New("恢复配置失败，请保留当前文件并查看详细状态")
		}
		re := a.restart(wantOn)
		se := a.save(before)
		if re == nil && se == nil {
			os.Remove(filepath.Join(a.store, "pending.json"))
			return fmt.Errorf("更新未成功，已恢复原配置和开关状态。%s", cause)
		}
		return errors.New("已恢复原配置文件，但服务未恢复，请在更多里重启服务；网络已尽量保持直连")
	}
	if e = atomicWrite(filepath.Join(a.dir, "config.yaml"), candidate); e != nil {
		return rollback(errors.New("配置写入失败"))
	}
	if e = a.restart(wantOn); e != nil {
		return rollback(e)
	}
	next.PreviousURL = before.URL
	next.PreviousUpdated = before.Updated
	next.PreviousQuota = before.Quota
	if e = atomicWrite(filepath.Join(a.store, "previous.yaml"), old); e != nil {
		return rollback(errors.New("保存上一版配置失败"))
	}
	if e = a.save(next); e != nil {
		return rollback(errors.New("保存订阅设置失败"))
	}
	if e = os.Remove(filepath.Join(a.store, "pending.json")); e != nil {
		return errors.New("配置已更新，但事务记录清理失败，请查看详细状态")
	}
	os.Remove(filepath.Join(a.store, "rollback.yaml"))
	return nil
}
func (a *App) recoverPending() error {
	p := filepath.Join(a.store, "pending.json")
	if !exists(p) {
		return nil
	}
	var j struct {
		State State `json:"state"`
		On    bool  `json:"on"`
	}
	b, e := os.ReadFile(p)
	if e != nil {
		return e
	}
	if e = json.Unmarshal(b, &j); e != nil {
		return e
	}
	old, e := os.ReadFile(filepath.Join(a.store, "rollback.yaml"))
	if e != nil {
		return e
	}
	if e = atomicWrite(filepath.Join(a.dir, "config.yaml"), old); e != nil {
		return e
	}
	if e = a.restart(j.On); e != nil {
		return e
	}
	if e = a.save(j.State); e != nil {
		return e
	}
	return os.Remove(p)
}
func checkURL(raw, proxy string) map[string]any {
	t := time.Now()
	tr := &http.Transport{TLSHandshakeTimeout: 6 * time.Second}
	if proxy != "" {
		u, _ := url.Parse(proxy)
		tr.Proxy = http.ProxyURL(u)
	}
	c := &http.Client{Transport: tr, Timeout: 15 * time.Second}
	defer tr.CloseIdleConnections()
	resp, e := c.Get(raw)
	if e != nil {
		return map[string]any{"ok": false, "label": "暂时无法连接"}
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	ok := resp.StatusCode >= 200 && resp.StatusCode < 400
	label := "连接正常"
	if !ok {
		label = "网站返回限制，请稍后重试"
	}
	return map[string]any{"ok": ok, "label": label, "ms": time.Since(t).Milliseconds(), "http": resp.StatusCode}
}

type Request struct {
	Action   string        `json:"action"`
	Name     string        `json:"name"`
	URL      string        `json:"url"`
	ID       string        `json:"id"`
	Content  string        `json:"content"`
	Smart    *SmartEdit    `json:"smart,omitempty"`
	Devices  []DeviceEdit  `json:"devices"`
	Revision int           `json:"revision"`
	MAC      string        `json:"mac,omitempty"`
	Pinned   *bool         `json:"pinned,omitempty"`
	Archived *bool         `json:"archived,omitempty"`
	Enabled  *bool         `json:"enabled,omitempty"`
	Service  *ServiceEdit  `json:"service,omitempty"`
	Platform *PlatformEdit `json:"platform,omitempty"`
	RuleAuto *bool         `json:"ruleAuto,omitempty"`
}

func (a *App) perform(r Request) (string, any, error) {
	if msg, result, err, handled := a.performRecovery(r); handled {
		return msg, result, err
	}
	if msg, result, err, handled := a.performServices(r); handled {
		return msg, result, err
	}
	if msg, result, err, handled := a.performPlatforms(r); handled {
		return msg, result, err
	}
	switch r.Action {
	case "connection-close":
		return "连接已断开；应用需要时会重新连接", nil, a.closeConnection(r.ID)
	case "device-meta":
		return "设备整理设置已保存，策略与历史用量已保留", nil, a.saveDeviceLayout(r.MAC, r.Pinned, r.Archived)
	}
	if msg, result, err, handled := a.performSmart(r); handled {
		return msg, result, err
	}
	if msg, result, err, handled := a.performProfiles(r); handled {
		if err == nil {
			switch r.Action {
			case "sub-save", "sub-switch", "sub-update", "rule-switch", "subscription", "restore":
				a.notifySmartConfigChanged()
			}
		}
		return msg, result, err
	}
	switch r.Action {
	case "on":
		e := a.setWholeHouse(true)
		if e == nil {
			e = a.recordDesiredProxy(true)
		}
		return "全屋代理已开启，设备策略已生效", nil, e
	case "off":
		e := a.setWholeHouse(false)
		if e == nil {
			e = a.recordDesiredProxy(false)
		}
		return "全屋代理已关闭，所有设备普通直连；必要时重连 Wi-Fi 刷新旧 DNS", nil, e
	case "repair":
		c := a.deviceSnapshot()
		if c == nil {
			return "", nil, errors.New("设备策略未准备好，请稍后重试")
		}
		e := a.applyDevices(c)
		return "网络规则已修复，原来的开关状态已保留", nil, e
	case "devices", "device-defaults":
		e := a.saveDeviceEdits(r.Devices, r.Action == "device-defaults", r.Revision)
		return "设备策略已保存并生效；切换为直连后如有旧网页异常，请重连 Wi-Fi", nil, e
	case "select":
		a.pauseSmartOnManual()
		if quotaNode(r.Name) {
			return "", nil, errors.New("这是一条套餐信息，请选择实际地区节点")
		}
		b, e := a.api("GET", "/proxies/"+url.PathEscape(selectGroup), nil)
		if e != nil {
			return "", nil, e
		}
		var g struct {
			All []string `json:"all"`
			Now string   `json:"now"`
		}
		json.Unmarshal(b, &g)
		found := false
		for _, n := range g.All {
			if n == r.Name {
				found = true
			}
		}
		if !found {
			return "", nil, errors.New("这个节点已不存在，请刷新列表")
		}
		_, e = a.api("PUT", "/proxies/"+url.PathEscape(selectGroup), map[string]string{"name": r.Name})
		if e == nil {
			if e = a.rememberDefaultSelector(r.Name); e != nil {
				if g.Now != "" {
					_, _ = a.api("PUT", "/proxies/"+url.PathEscape(selectGroup), map[string]string{"name": g.Now})
				}
				e = errors.New("节点选择保存失败，已尝试恢复原节点")
			}
		}
		return "已切换为 " + r.Name, nil, e
	case "delay", "auto":
		group := selectGroup
		if r.Action == "auto" {
			if _, _, e := a.perform(Request{Action: "select", Name: autoGroup}); e != nil {
				return "", nil, e
			}
			group = autoGroup
		}
		b, e := a.api("GET", "/group/"+url.PathEscape(group)+"/delay?timeout=5000&url="+url.QueryEscape("https://www.gstatic.com/generate_204"), nil)
		if e != nil {
			return "", nil, e
		}
		var results map[string]int
		if json.Unmarshal(b, &results) != nil {
			return "", nil, errors.New("测速结果无法读取")
		}
		type Row struct {
			Name  string `json:"name"`
			Delay int    `json:"delay"`
		}
		rows := []Row{}
		for n, d := range results {
			if quotaNode(n) {
				continue
			}
			rows = append(rows, Row{n, d})
		}
		sort.Slice(rows, func(i, j int) bool {
			di, dj := rows[i].Delay, rows[j].Delay
			if di <= 0 {
				di = 1 << 30
			}
			if dj <= 0 {
				dj = 1 << 30
			}
			return di < dj
		})
		return "测速完成，已按延迟排序", rows, nil
	case "restart":
		rules, e := a.rules()
		if e != nil {
			return "", nil, e
		}
		wantOn := rules.Total > 0
		if c := a.deviceSnapshot(); c != nil {
			wantOn = c.Enabled
		}
		e = a.restart(wantOn)
		return "代理服务已重启", nil, e
	case "test":
		var domestic, foreign map[string]any
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); domestic = checkURL("https://www.baidu.com", "") }()
		go func() {
			defer wg.Done()
			foreign = checkURL("https://www.google.com/generate_204", "http://127.0.0.1:7890")
		}()
		wg.Wait()
		return "网络检查完成", map[string]any{"domestic": domestic, "foreign": foreign}, nil
	case "subscription":
		raw := strings.TrimSpace(r.URL)
		if raw == "" {
			raw = a.snapshot().URL
		}
		if raw == "" {
			return "", nil, errors.New("请先粘贴订阅链接")
		}
		b, quota, e := downloadSubscription(raw)
		if e != nil {
			return "", nil, e
		}
		current, e := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
		if e != nil {
			return "", nil, e
		}
		merged, count, e := mergeNodes(current, b)
		if e != nil {
			return "", nil, e
		}
		next := a.snapshot()
		next.URL = raw
		next.Quota = quota
		next.Updated = time.Now().Format(time.RFC3339)
		if e = a.applyConfig(merged, next); e != nil {
			return "", nil, e
		}
		return fmt.Sprintf("订阅已更新，共 %d 个节点；路由器网络设置已保留", count), nil, nil
	case "restore":
		b, e := os.ReadFile(filepath.Join(a.store, "previous.yaml"))
		if e != nil {
			return "", nil, errors.New("还没有可恢复的上一版订阅")
		}
		next := a.snapshot()
		next.URL = next.PreviousURL
		next.Updated = next.PreviousUpdated
		next.Quota = next.PreviousQuota
		e = a.applyConfig(b, next)
		return "已恢复上一版订阅", nil, e
	case "quota":
		e := a.refreshQuota()
		return "套餐流量已刷新，节点与网络规则保持不变", nil, e
	case "clean-logs":
		a.cleanLogs(time.Now())
		m := a.maintenanceStatus()
		if m.Error != "" {
			return "", nil, errors.New(m.Error)
		}
		return "日志已检查并清理，最近记录已保留", m, nil
	default:
		return "", nil, errors.New("不支持的操作")
	}
}
func respond(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
func fail(w http.ResponseWriter, status int, message string) {
	respond(w, status, map[string]string{"error": message})
}
func (a *App) handler() http.Handler {
	web, _ := fs.Sub(assets, "web")
	static := http.FileServer(http.FS(web))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("Cache-Control", "no-store")
		if r.Host != a.host {
			fail(w, 403, "请使用路由器的管理页面地址访问")
			return
		}
		if o := r.Header.Get("Origin"); o != "" && o != "http://"+a.host {
			fail(w, 403, "请从管理页面操作")
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			if r.Method != "GET" && r.Method != "HEAD" {
				fail(w, 405, "不支持的请求")
				return
			}
			static.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/login" {
			if r.Method != "POST" {
				fail(w, 405, "请填写登录信息")
				return
			}
			a.loginMu.Lock()
			if time.Since(a.loginSince) > time.Minute {
				a.loginSince = time.Now()
				a.attempts = 0
			}
			a.attempts++
			blocked := a.attempts > 8
			a.loginMu.Unlock()
			if blocked {
				fail(w, 429, "尝试次数较多，请一分钟后重试")
				return
			}
			var v struct {
				Password string `json:"password"`
			}
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&v) != nil {
				fail(w, 400, "请输入密码")
				return
			}
			s := a.snapshot()
			if !equal(passwordHash(v.Password, s.Salt), s.Hash) {
				fail(w, 401, "密码不对，请输入安装时使用的路由器密码")
				return
			}
			respond(w, 200, map[string]string{"token": s.Token})
			return
		}
		if !equal(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), a.snapshot().Token) {
			fail(w, 401, "请先登录")
			return
		}
		if a.handleRecoveryHTTP(w, r) {
			return
		}
		switch r.URL.Path {
		case "/api/services":
			if r.Method != "GET" {
				fail(w, 405, "不支持的请求")
				return
			}
			respond(w, 200, a.servicesView())
		case "/api/platforms":
			if r.Method != "GET" {
				fail(w, 405, "不支持的请求")
				return
			}
			respond(w, 200, a.platformsView())
		case "/api/connections":
			if r.Method != "GET" {
				fail(w, 405, "不支持的请求")
				return
			}
			view, err := a.connectionsView(r.URL.Query())
			if err != nil {
				fail(w, 400, err.Error())
				return
			}
			respond(w, 200, view)
		case "/api/status":
			if r.Method != "GET" {
				fail(w, 405, "不支持的请求")
				return
			}
			respond(w, 200, a.status())
		case "/api/devices":
			if r.Method != "GET" {
				fail(w, 405, "不支持的请求")
				return
			}
			respond(w, 200, a.devicesResponse())
		case "/api/subscription":
			if r.Method != "GET" {
				fail(w, 405, "不支持的请求")
				return
			}
			s := a.snapshot()
			display := ""
			if u, e := url.Parse(s.URL); e == nil && u.Hostname() != "" {
				display = u.Scheme + "://" + u.Hostname() + "/…"
			}
			respond(w, 200, map[string]any{"url": display, "hasURL": s.URL != "", "updated": s.Updated, "hasBackup": exists(filepath.Join(a.store, "previous.yaml")), "quota": s.Quota})
		case "/api/profiles":
			respond(w, 200, a.profilesView())
		case "/api/rule-profiles":
			respond(w, 200, a.rulesProfilesView())
		case "/api/smart":
			respond(w, 200, a.smartView())
		case "/api/traffic":
			view, err := a.trafficView(r.URL.Query().Get("range"))
			if err != nil {
				fail(w, 400, err.Error())
				return
			}
			respond(w, 200, view)
		case "/api/job":
			a.jobMu.RLock()
			j := a.job
			a.jobMu.RUnlock()
			respond(w, 200, j)
		case "/api/action":
			if r.Method != "POST" {
				fail(w, 405, "不支持的请求")
				return
			}
			var v Request
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 131072)).Decode(&v) != nil {
				fail(w, 400, "填写的信息无法读取")
				return
			}
			a.waitSmartBackground()
			a.waitServiceBackground()
			select {
			case a.gate <- struct{}{}:
			default:
				fail(w, 409, "上一项操作还在进行，请稍等")
				return
			}
			release, e := acquireManagerLock()
			if e != nil {
				<-a.gate
				fail(w, 409, "有其他管理工具正在操作路由器，请稍后再试")
				return
			}
			j := Job{ID: random()[:16], Action: v.Action, Running: true, Message: "正在处理，请稍候"}
			a.jobMu.Lock()
			a.job = j
			a.jobMu.Unlock()
			go func() {
				defer func() { release(); <-a.gate }()
				var msg string
				var result any
				e := a.prepareAutomaticBackup(v.Action)
				if e == nil {
					msg, result, e = a.perform(v)
				}
				a.jobMu.Lock()
				defer a.jobMu.Unlock()
				a.job.Running = false
				a.job.Result = result
				a.job.Error = e != nil
				if e != nil {
					a.job.Message = e.Error()
				} else {
					a.job.Message = msg
				}
			}()
			respond(w, 202, j)
		case "/api/details":
			out, e := a.sh("echo '监听端口'; ports | awk '$1 ~ /^(tcp|udp)/ && $4 ~ /:(7890|7892|7893|1053|9091)$/ {print}'; echo '代理入口'; iptables -t nat -S PREROUTING; echo '命中计数'; iptables -t nat -L MIHOMO_TCP -n -v; echo '进程'; pids; echo '存储（第一行是代理实际所在分区）'; df -h \"$DIR\" /data /tmp")
			if e != nil {
				out += "\n" + e.Error()
			}
			respond(w, 200, map[string]string{"text": out})
		case "/api/logs":
			path := a.engineLogPath()
			f, e := os.Open(path)
			if e != nil {
				fail(w, 404, "暂时没有日志")
				return
			}
			defer f.Close()
			if st, e := f.Stat(); e == nil && st.Size() > 256<<10 {
				f.Seek(-(256 << 10), io.SeekEnd)
			}
			b, _ := io.ReadAll(io.LimitReader(f, 256<<10))
			lines := strings.Split(string(b), "\n")
			if r.URL.Query().Get("errors") == "1" {
				filtered := []string{}
				for _, l := range lines {
					s := strings.ToLower(l)
					if strings.Contains(s, "error") || strings.Contains(s, "fatal") || strings.Contains(s, "panic") {
						filtered = append(filtered, l)
					}
				}
				lines = filtered
			}
			if len(lines) > 100 {
				lines = lines[len(lines)-100:]
			}
			respond(w, 200, map[string]string{"text": strings.Join(lines, "\n")})
		default:
			fail(w, 404, "未找到这个功能")
		}
	})
}
func main() {
	dir := flag.String("dir", "/data/other/mihomo", "Mihomo directory")
	listen := flag.String("listen", "192.168.31.1:9080", "LAN address")
	init := flag.Bool("init", false, "Initialize credentials from stdin")
	rollbackDevices := flag.Bool("rollback-devices", false, "Restore pre-device managed rules before reinstalling v1")
	installBoot := flag.String("install-boot", "", "Install recovery in an explicitly verified persistent crontab")
	bootSSH := flag.Bool("boot-ssh", false, "Explicitly preserve existing SSH host key and restore LAN SSH; requires -install-boot")
	flag.Parse()
	if *bootSSH && *installBoot == "" {
		log.Fatal("-boot-ssh requires explicit -install-boot")
	}
	a := &App{dir: *dir, store: filepath.Join(*dir, "mobile-panel"), host: *listen, gate: make(chan struct{}, 1)}
	if e := os.MkdirAll(a.store, 0700); e != nil {
		log.Fatal(e)
	}
	settings := filepath.Join(a.store, "settings.json")
	if *init {
		var v struct {
			Password string `json:"password"`
		}
		if json.NewDecoder(io.LimitReader(os.Stdin, 4096)).Decode(&v) != nil || v.Password == "" {
			log.Fatal("Password required")
		}
		s := State{Salt: random(), Token: random()}
		s.Hash = passwordHash(v.Password, s.Salt)
		if exists(settings) {
			log.Fatal("Already initialized; existing login preserved")
		}
		if e := a.save(s); e != nil {
			log.Fatal(e)
		}
		fmt.Println("Login initialized")
		return
	}
	b, e := os.ReadFile(settings)
	if e != nil {
		log.Fatal("Initialize login first")
	}
	if json.Unmarshal(b, &a.state) != nil || a.state.Token == "" {
		log.Fatal("Invalid settings")
	}
	if *installBoot != "" {
		executable, e := os.Executable()
		if e != nil {
			log.Fatal(e)
		}
		if e = a.configureBootRecovery(executable, *installBoot, *bootSSH); e != nil {
			log.Fatal(e)
		}
		fmt.Println("Persistent recovery installed; existing cron jobs preserved")
		return
	}
	if *rollbackDevices {
		b, e := os.ReadFile(filepath.Join(a.store, "devices-v1-rules.json"))
		if e != nil {
			log.Fatal(e)
		}
		var old OwnedRules
		if e = json.Unmarshal(b, &old); e != nil {
			log.Fatal(e)
		}
		if e = a.commitOwned(old); e != nil {
			log.Fatal(e)
		}
		fmt.Println("Pre-device managed rules restored")
		return
	}
	if b, e := os.ReadFile(filepath.Join(a.store, "devices.json")); e == nil {
		var c DeviceConfig
		if json.Unmarshal(b, &c) == nil && validateDeviceConfig(&c) == nil {
			a.setDeviceConfig(&c)
		}
	}
	listener, e := net.Listen("tcp4", *listen)
	if e != nil {
		log.Fatal(e)
	}
	defer listener.Close()
	a.startupPreferences.Store(true)
	a.initRecovery()
	if release, e := acquireManagerLock(); e == nil {
		if e = a.recoverStartupTransactions(); e != nil {
			a.setDeviceError(e)
			log.Print("Startup recovery requires attention; management page remains available")
		}
		release()
	} else {
		a.setDeviceError(e)
	}
	if e := a.InitProfiles(); e != nil {
		log.Printf("Profile manager: %v", e)
	}
	a.initSmart()
	a.initUsage()
	go func() {
		select {
		case a.gate <- struct{}{}:
		}
		if release, e := acquireManagerLock(); e == nil {
			a.recoveryStep(time.Now())
			release()
		}
		<-a.gate
		a.deviceTick()
		for range time.Tick(20 * time.Second) {
			a.deviceTick()
		}
	}()
	go a.usageMonitor()
	go a.maintenanceMonitor()
	go a.quotaMonitor()
	go a.smartMonitor()
	go a.serviceMonitor()
	go a.recoveryMonitor()
	server := &http.Server{Handler: a.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 100 * time.Second, IdleTimeout: 45 * time.Second, MaxHeaderBytes: 8192}
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-quit
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		select {
		case a.gate <- struct{}{}:
			a.saveUsage(time.Now())
			<-a.gate
		case <-ctx.Done():
			a.saveUsage(time.Now())
		}
		os.Exit(0)
	}()
	log.Printf("Mobile panel ready: http://%s", *listen)
	if e := server.Serve(listener); e != nil && !errors.Is(e, http.ErrServerClosed) {
		log.Fatal(e)
	}
	select {}
}
