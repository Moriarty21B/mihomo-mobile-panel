package main

// Smart selection deliberately uses its own loopback listener and selector. A
// probe must never rotate 节点选择, or ordinary household connections would be
// sent through every node during a check.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const smartGroup = "面板检测"
const smartListener = "mobile-panel-smart-check"
const smartPort = 19081
const smartInterval = 5 * time.Minute
const smartScanBudget = 40 * time.Second

type SmartEdit struct {
	Enabled               *bool              `json:"enabled,omitempty"`
	ExcludeHighMultiplier *bool              `json:"excludeHighMultiplier,omitempty"`
	Overrides             map[string]float64 `json:"overrides,omitempty"`
}

type SmartCandidate struct {
	Name            string  `json:"name"`
	Region          string  `json:"region"`
	Multiplier      float64 `json:"multiplier"`
	MultiplierKnown bool    `json:"multiplierKnown"`
	Override        bool    `json:"override"`
	Status          string  `json:"status"`
	Delay           int64   `json:"delay"`
	Reason          string  `json:"reason"`
	Checked         string  `json:"checked,omitempty"`
}

type SmartView struct {
	Enabled               bool               `json:"enabled"`
	ExcludeHighMultiplier bool               `json:"excludeHighMultiplier"`
	Overrides             map[string]float64 `json:"overrides"`
	Running               bool               `json:"running"`
	LastChecked           string             `json:"lastChecked"`
	NextCheck             string             `json:"nextCheck"`
	Message               string             `json:"message"`
	Selected              string             `json:"selected"`
	Candidates            []SmartCandidate   `json:"candidates"`
	GPTCheck              string             `json:"gptCheck"`
}

type smartSaved struct {
	Version               int                `json:"version"`
	Enabled               bool               `json:"enabled"`
	ExcludeHighMultiplier bool               `json:"excludeHighMultiplier"`
	Overrides             map[string]float64 `json:"overrides"`
	CatalogHash           string             `json:"catalogHash,omitempty"`
	LastChecked           string             `json:"lastChecked,omitempty"`
	LastSwitch            string             `json:"lastSwitch,omitempty"`
	Message               string             `json:"message,omitempty"`
	Selected              string             `json:"selected,omitempty"`
	Candidates            []SmartCandidate   `json:"candidates,omitempty"`
}

type smartProxy struct {
	Now     string   `json:"now"`
	All     []string `json:"all"`
	Type    string   `json:"type"`
	History []struct {
		Delay int64 `json:"delay"`
	} `json:"history"`
}

type SmartManager struct {
	mu sync.Mutex
	// All platform and smart scans share one loopback selector. Holding this
	// lock prevents a second scan from moving it underneath an HTTP request.
	probeMu    sync.Mutex
	saved      smartSaved
	next       time.Time
	running    bool
	background bool
	cancel     context.CancelFunc
	done       chan struct{}
	generation uint64
	cursor     int
	failures   map[string]int
	// Hooks keep tests independent from router processes and the public web.
	apiHook       func(context.Context, string, string, any) ([]byte, error)
	probeHook     func(context.Context, string) SmartCandidate
	provisionHook func() error
	healthHook    func() error
}

// This transaction is deliberately separate from applyConfig. Adding a private
// probe listener is not a user subscription change, so it must never overwrite
// previous.yaml, profiles-previous.json, or any State.Previous* field.
type smartProvisionJournal struct {
	Version      int               `json:"version"`
	Original     []byte            `json:"original"`
	OriginalHash string            `json:"originalHash"`
	On           bool              `json:"on"`
	Selected     map[string]string `json:"selected"`
}

func defaultSmartSaved() smartSaved {
	return smartSaved{Version: 1, ExcludeHighMultiplier: true, Overrides: map[string]float64{}, Candidates: []SmartCandidate{}, Message: "未开启，保留手动选择的节点"}
}

func (a *App) initSmart() {
	a.smartOnce.Do(func() {
		m := &SmartManager{saved: defaultSmartSaved(), failures: map[string]int{}}
		if a.store != "" {
			if b, e := os.ReadFile(filepath.Join(a.store, "smart.json")); e == nil {
				var s smartSaved
				if json.Unmarshal(b, &s) == nil && s.Version == 1 && validSmartOverrides(s.Overrides) == nil {
					m.saved = s
					if m.saved.Overrides == nil {
						m.saved.Overrides = map[string]float64{}
					}
					if m.saved.Candidates == nil {
						m.saved.Candidates = []SmartCandidate{}
					}
				} else {
					m.saved.Message = "智能设置无法读取，已保持关闭；可重新开启"
				}
			}
		}
		if hash, e := a.smartCatalogHash(); e == nil {
			if m.saved.CatalogHash != hash && len(m.saved.Overrides) > 0 {
				m.saved.Overrides = map[string]float64{}
				m.saved.Candidates = []SmartCandidate{}
				m.saved.LastChecked = ""
				m.saved.Message = "节点配置已变化，请重新确认未标明的倍率"
				m.saved.CatalogHash = hash
				_ = a.persistSmart(m.saved)
			} else {
				m.saved.CatalogHash = hash
			}
		} else if m.saved.Enabled {
			m.saved.Enabled = false
			m.saved.Message = "代理配置无法读取，智能优选已保持关闭"
		}
		m.next = time.Now().Add(25 * time.Second)
		a.smart = m
	})
}

func validSmartOverrides(overrides map[string]float64) error {
	if len(overrides) > 2000 {
		return errors.New("确认的节点过多")
	}
	for name, multiplier := range overrides {
		if strings.TrimSpace(name) == "" || len(name) > 1024 || math.IsNaN(multiplier) || math.IsInf(multiplier, 0) || multiplier < 0 || multiplier > 1000 {
			return errors.New("请填写有效节点名称和 0 到 1000 的倍率（0 表示取消确认）")
		}
	}
	return nil
}

func copySmartSaved(s smartSaved) smartSaved {
	s.Overrides = cloneSmartOverrides(s.Overrides)
	s.Candidates = append([]SmartCandidate{}, s.Candidates...)
	return s
}

func cloneSmartOverrides(in map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (a *App) persistSmart(s smartSaved) error {
	if a.store == "" {
		return errors.New("智能设置保存目录未准备好")
	}
	b, e := json.Marshal(s)
	if e != nil {
		return e
	}
	if e = atomicWrite(filepath.Join(a.store, "smart.json"), b); e != nil {
		return errors.New("智能设置保存失败，原开关状态未改变")
	}
	return nil
}

func (a *App) smartView() SmartView {
	a.initSmart()
	m := a.smart
	m.mu.Lock()
	defer m.mu.Unlock()
	s := copySmartSaved(m.saved)
	next := ""
	if s.Enabled && !m.next.IsZero() {
		next = m.next.Format(time.RFC3339)
	}
	return SmartView{Enabled: s.Enabled, ExcludeHighMultiplier: s.ExcludeHighMultiplier, Overrides: s.Overrides,
		Running: m.running, LastChecked: s.LastChecked, NextCheck: next, Message: s.Message,
		Selected: s.Selected, Candidates: s.Candidates,
		GPTCheck: "检查 ChatGPT 网页与登录入口可达性；不保证登录后每次聊天均可用"}
}

func (a *App) waitSmartBackground() {
	a.initSmart()
	m := a.smart
	m.mu.Lock()
	var done chan struct{}
	if m.running && m.background {
		done = m.done
		if m.cancel != nil {
			m.cancel()
		}
	}
	m.mu.Unlock()
	if done == nil {
		return
	}
	timer := time.NewTimer(1500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

func (a *App) pauseSmartOnManual() {
	a.initSmart()
	m := a.smart
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.saved.Enabled {
		return
	}
	if m.cancel != nil {
		m.cancel()
	}
	next := copySmartSaved(m.saved)
	next.Enabled = false
	next.Message = "已暂停智能优选，保留这次手动选择"
	if e := a.persistSmart(next); e != nil {
		// Safety takes precedence over silently undoing the user's manual node.
		m.saved.Enabled = false
		m.saved.Message = "已暂停；设置写入失败，请检查路由器空间"
		// Renaming needs no second full settings file. If the filesystem is
		// full, retain the old file as a backup rather than restart enabled.
		_ = os.Rename(filepath.Join(a.store, "smart.json"), filepath.Join(a.store, "smart-disabled.saved.json"))
	} else {
		m.saved = next
	}
	m.generation++
}

func (a *App) notifySmartConfigChanged() {
	a.initSmart()
	m := a.smart
	hash, _ := a.smartCatalogHash()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
	}
	next := copySmartSaved(m.saved)
	if hash != "" && next.CatalogHash != hash {
		next.CatalogHash = hash
		next.Overrides = map[string]float64{}
	}
	next.Candidates = []SmartCandidate{}
	next.LastChecked = ""
	if next.Enabled {
		next.Message = "节点配置已更新，等待重新检查"
	}
	m.next = time.Now().Add(10 * time.Second)
	m.generation++
	m.failures = map[string]int{}
	if a.persistSmart(next) == nil {
		m.saved = next
	} else {
		m.saved = next
		m.saved.Enabled = false
		m.saved.Message = "节点已更新，但智能设置保存失败；已暂停优选"
		_ = os.Rename(filepath.Join(a.store, "smart.json"), filepath.Join(a.store, "smart-disabled.saved.json"))
	}
}

func (a *App) smartCatalogHash() (string, error) {
	c, e := a.config()
	if e != nil {
		return "", e
	}
	b, e := yaml.Marshal(c["proxies"])
	if e != nil {
		return "", e
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func (a *App) performSmart(r Request) (string, any, error, bool) {
	if r.Action != "smart-settings" && r.Action != "smart-check" {
		return "", nil, nil, false
	}
	a.initSmart()
	m := a.smart
	if exists(a.smartProvisionPath()) {
		if e := a.recoverSmartProvision(); e != nil {
			return "", a.smartView(), errors.New("前次智能检查安装尚未恢复，请先在“更多”里重启服务后再试"), true
		}
	}
	if r.Action == "smart-check" {
		m.mu.Lock()
		enabled := m.saved.Enabled
		m.mu.Unlock()
		if !enabled {
			return "", nil, errors.New("请先开启智能优选，再检查节点"), true
		}
		msg, e := a.runSmartCheck(false)
		return msg, a.smartView(), e, true
	}
	if r.Smart == nil {
		return "", nil, errors.New("请填写智能优选设置"), true
	}
	if e := validSmartOverrides(r.Smart.Overrides); e != nil {
		return "", nil, e, true
	}
	m.mu.Lock()
	before := copySmartSaved(m.saved)
	m.mu.Unlock()
	next := copySmartSaved(before)
	if r.Smart.Enabled != nil {
		next.Enabled = *r.Smart.Enabled
	}
	if r.Smart.ExcludeHighMultiplier != nil {
		next.ExcludeHighMultiplier = *r.Smart.ExcludeHighMultiplier
	}
	for name, multiplier := range r.Smart.Overrides {
		if multiplier == 0 {
			delete(next.Overrides, name)
		} else {
			next.Overrides[name] = multiplier
		}
	}
	if next.Enabled {
		if e := a.ensureSmartInfrastructure(); e != nil {
			return "", a.smartView(), e, true
		}
		hash, e := a.smartCatalogHash()
		if e != nil {
			return "", nil, errors.New("节点配置无法读取，未开启智能优选"), true
		}
		if before.CatalogHash != "" && before.CatalogHash != hash {
			next.Overrides = cloneSmartOverrides(r.Smart.Overrides)
			for name, rate := range next.Overrides {
				if rate == 0 {
					delete(next.Overrides, name)
				}
			}
		}
		next.CatalogHash = hash
		next.Message = "已开启，正在后台检查港台新节点与 GPT 网页"
	} else {
		next.Message = "智能优选已关闭，当前节点保持不变"
	}
	if e := a.persistSmart(next); e != nil {
		return "", nil, e, true
	}
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
	}
	m.saved = next
	m.generation++
	m.next = time.Now()
	m.mu.Unlock()
	return next.Message, a.smartView(), nil, true
}

// addSmartProbeConfig is idempotent and changes no ports, rules or existing
// groups. Probe nodes are direct leaf names, never another automatic group.
func addSmartProbeConfig(original []byte) ([]byte, bool, error) {
	var c map[string]any
	if yaml.Unmarshal(original, &c) != nil || len(c) == 0 {
		return nil, false, errors.New("当前代理配置无法读取")
	}
	proxies, ok := c["proxies"].([]any)
	if !ok || len(proxies) == 0 {
		return nil, false, errors.New("没有可供检查的节点")
	}
	names := []any{}
	for _, item := range proxies {
		p, ok := item.(map[string]any)
		if !ok {
			return nil, false, errors.New("当前节点格式无法识别")
		}
		name, _ := p["name"].(string)
		if name != "" && !quotaNode(name) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, false, errors.New("没有可供检查的实际节点")
	}
	groups, ok := c["proxy-groups"].([]any)
	if !ok {
		return nil, false, errors.New("节点分组无法读取")
	}
	groupFound := false
	for _, item := range groups {
		g, ok := item.(map[string]any)
		if !ok {
			return nil, false, errors.New("节点分组格式无法读取")
		}
		if name, _ := g["name"].(string); name == smartGroup {
			if typ, _ := g["type"].(string); typ != "select" {
				return nil, false, errors.New("面板检测分组名称已被其他配置占用")
			}
			g["proxies"] = names
			g["hidden"] = true
			groupFound = true
		}
	}
	if !groupFound {
		groups = append(groups, map[string]any{"name": smartGroup, "type": "select", "proxies": names, "hidden": true})
	}
	c["proxy-groups"] = groups
	listeners := []any{}
	if v, exists := c["listeners"]; exists {
		var ok bool
		listeners, ok = v.([]any)
		if !ok {
			return nil, false, errors.New("已有独立监听配置无法读取")
		}
	}
	listenerFound := false
	for _, item := range listeners {
		l, ok := item.(map[string]any)
		if !ok {
			return nil, false, errors.New("已有监听配置无法识别")
		}
		name, _ := l["name"].(string)
		if name == smartListener {
			if !smartListenerMatches(l) {
				return nil, false, errors.New("面板检测监听名称已被其他配置占用")
			}
			listenerFound = true
		} else if fmt.Sprint(l["port"]) == strconv.Itoa(smartPort) {
			return nil, false, errors.New("智能检查端口已被占用，原配置未修改")
		}
	}
	if !listenerFound {
		listeners = append(listeners, map[string]any{"name": smartListener, "type": "mixed", "listen": "127.0.0.1", "port": smartPort, "proxy": smartGroup})
	}
	c["listeners"] = listeners
	b, e := yaml.Marshal(c)
	if e != nil {
		return nil, false, e
	}
	var old map[string]any
	_ = yaml.Unmarshal(original, &old)
	oldBytes, _ := yaml.Marshal(old)
	return b, string(b) != string(oldBytes), nil
}

func smartListenerMatches(l map[string]any) bool {
	return l["name"] == smartListener && l["type"] == "mixed" && l["listen"] == "127.0.0.1" && fmt.Sprint(l["port"]) == strconv.Itoa(smartPort) && l["proxy"] == smartGroup
}

func (a *App) ensureSmartInfrastructure() error {
	m := a.smart
	if m.provisionHook != nil {
		return m.provisionHook()
	}
	original, e := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
	if e != nil {
		return errors.New("读取代理配置失败，未开启智能优选")
	}
	candidate, changed, e := addSmartProbeConfig(original)
	if e != nil {
		return e
	}
	if !changed {
		return nil
	}
	// Reserve-check before changing configuration. The port only belongs to us
	// after this explicitly requested, validated configuration transaction.
	listener, e := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(smartPort))
	if e != nil {
		var c map[string]any
		_ = yaml.Unmarshal(original, &c)
		owned := false
		if ls, ok := c["listeners"].([]any); ok {
			for _, item := range ls {
				if l, ok := item.(map[string]any); ok && smartListenerMatches(l) {
					owned = true
				}
			}
		}
		if !owned {
			return errors.New("智能检查端口已被其他程序占用，未修改配置")
		}
	} else {
		_ = listener.Close()
	}
	selected := map[string]string{}
	proxies, e := a.smartProxies(context.Background())
	if e != nil || proxies[selectGroup].Now == "" {
		return errors.New("无法读取当前节点，未安装检测通道；请稍后重试")
	}
	for name, p := range proxies {
		if strings.EqualFold(p.Type, "Selector") && p.Now != "" {
			selected[name] = p.Now
		}
	}
	return a.applySmartInfrastructure(original, candidate, selected)
}

func smartHash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (a *App) smartProvisionPath() string {
	return filepath.Join(a.store, "smart-provision-pending.json")
}

func (a *App) smartInfrastructureHealth() error {
	if a.smart != nil && a.smart.healthHook != nil {
		return a.smart.healthHook()
	}
	deadline := time.Now().Add(2500 * time.Millisecond)
	for {
		conn, e := net.DialTimeout("tcp4", "127.0.0.1:"+strconv.Itoa(smartPort), 500*time.Millisecond)
		if e == nil {
			_ = conn.Close()
			break
		}
		if !time.Now().Before(deadline) {
			return errors.New("独立节点检查端口未就绪")
		}
		time.Sleep(100 * time.Millisecond)
	}
	proxies, e := a.smartProxies(context.Background())
	if e != nil || !strings.EqualFold(proxies[smartGroup].Type, "Selector") || len(proxies[smartGroup].All) == 0 {
		return errors.New("独立节点检查分组未就绪")
	}
	return nil
}

func (a *App) restoreSmartSelectors(selected map[string]string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	groups := []string{}
	for group := range selected {
		if group != smartGroup {
			groups = append(groups, group)
		}
	}
	sort.Strings(groups)
	var failure error
	for _, group := range groups {
		if _, e := a.smartAPI(ctx, "PUT", "/proxies/"+url.PathEscape(group), map[string]string{"name": selected[group]}); e != nil {
			failure = errors.New("原节点选择尚未完全恢复")
		}
	}
	return failure
}

func (a *App) applySmartInfrastructure(original, candidate []byte, selected map[string]string) error {
	if exists(a.smartProvisionPath()) || exists(filepath.Join(a.store, "pending.json")) || exists(filepath.Join(a.store, "profiles-pending.json")) {
		return errors.New("前次配置操作尚未恢复，未启用智能检查通道")
	}
	staged := filepath.Join(a.dir, "mobile-smart-candidate.yaml")
	if e := atomicWrite(staged, candidate); e != nil {
		return errors.New("保存智能检查配置失败，原配置未修改")
	}
	defer os.Remove(staged)
	if e := a.validate(staged); e != nil {
		return e
	}
	rules, e := a.rules()
	if e != nil {
		return errors.New("无法读取当前网络开关，原配置未修改")
	}
	wantOn := rules.Total > 0
	if c := a.deviceSnapshot(); c != nil {
		wantOn = c.Enabled
	}
	if a.recoveryDegraded() {
		wantOn = false
	}
	live, e := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
	if e != nil || smartHash(live) != smartHash(original) {
		return errors.New("代理配置刚刚发生变化，请刷新后重试；未覆盖当前配置")
	}
	journal := smartProvisionJournal{Version: 1, Original: original, OriginalHash: smartHash(original), On: wantOn, Selected: selected}
	b, e := json.Marshal(journal)
	if e != nil || atomicWrite(a.smartProvisionPath(), b) != nil {
		return errors.New("智能检查备份失败，原配置未修改")
	}
	rollback := func(cause error) error {
		if e := a.restoreSmartProvision(journal); e != nil {
			return errors.New("智能检查启用失败，已保留恢复记录；请在“更多”里重启服务")
		}
		return fmt.Errorf("智能检查启用未成功，已恢复原配置和节点。%s", cause)
	}
	if e := atomicWrite(filepath.Join(a.dir, "config.yaml"), candidate); e != nil {
		return rollback(errors.New("配置写入失败"))
	}
	if e := a.restart(wantOn); e != nil {
		return rollback(e)
	}
	if e := a.smartInfrastructureHealth(); e != nil {
		return rollback(e)
	}
	if e := a.restoreSmartSelectors(selected); e != nil {
		return rollback(e)
	}
	// Removing the private journal is the commit point. Incomplete operations
	// are always rolled back on startup, including a crash after core restart.
	if e := os.Remove(a.smartProvisionPath()); e != nil {
		return rollback(errors.New("检查通道事务未完成"))
	}
	return nil
}

func (a *App) restoreSmartProvision(j smartProvisionJournal) error {
	if j.Version != 1 || len(j.Original) == 0 || len(j.Original) > maxConfig || smartHash(j.Original) != j.OriginalHash {
		return errors.New("智能检查恢复记录无效，未覆盖当前代理配置")
	}
	staged := filepath.Join(a.dir, "mobile-smart-recovery.yaml")
	if e := atomicWrite(staged, j.Original); e != nil {
		return errors.New("智能检查恢复文件无法写入")
	}
	defer os.Remove(staged)
	if e := a.validate(staged); e != nil {
		return e
	}
	if e := atomicWrite(filepath.Join(a.dir, "config.yaml"), j.Original); e != nil {
		return errors.New("原代理配置恢复失败")
	}
	if e := a.restart(j.On); e != nil {
		return e
	}
	if e := a.restoreSmartSelectors(j.Selected); e != nil {
		return e
	}
	if e := os.Remove(a.smartProvisionPath()); e != nil && !os.IsNotExist(e) {
		return e
	}
	return nil
}

// Call during startup under the same exclusive lock used by recoverPending,
// before InitProfiles. No smart manager or settings initialization is required.
func (a *App) recoverSmartProvision() error {
	f, e := os.Open(a.smartProvisionPath())
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return errors.New("无法读取智能检查恢复记录")
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 4*maxConfig+1))
	_ = f.Close()
	if e != nil || len(b) > 4*maxConfig {
		return errors.New("智能检查恢复记录无法读取，未覆盖当前配置")
	}
	var j smartProvisionJournal
	if json.Unmarshal(b, &j) != nil {
		return errors.New("智能检查恢复记录损坏，未覆盖当前配置")
	}
	return a.restoreSmartProvision(j)
}

func (a *App) smartAPI(ctx context.Context, method, path string, body any) ([]byte, error) {
	m := a.smart
	if m != nil && m.apiHook != nil {
		return m.apiHook(ctx, method, path, body)
	}
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = strings.NewReader(string(b))
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, method, "http://127.0.0.1:9091"+path, reader)
	if e != nil {
		return nil, e
	}
	req.Header.Set("Content-Type", "application/json")
	if c, e := a.config(); e == nil {
		if secret, ok := c["secret"].(string); ok && secret != "" {
			req.Header.Set("Authorization", "Bearer "+secret)
		}
	}
	tr := &http.Transport{Proxy: nil}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}
	resp, e := client.Do(req)
	if e != nil {
		return nil, errors.New("代理服务暂时无法连接")
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if e != nil {
		return nil, e
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("节点检查服务返回 %d", resp.StatusCode)
	}
	return b, nil
}

func (a *App) smartProxies(ctx context.Context) (map[string]smartProxy, error) {
	b, e := a.smartAPI(ctx, "GET", "/proxies", nil)
	if e != nil {
		return nil, e
	}
	var value struct {
		Proxies map[string]smartProxy `json:"proxies"`
	}
	if json.Unmarshal(b, &value) != nil || value.Proxies == nil {
		return nil, errors.New("节点信息无法读取")
	}
	return value.Proxies, nil
}

var smartRateAfter = regexp.MustCompile(`(?i)(?:[x×✕＊*]|倍率\s*[:：]?)\s*([0-9]+(?:\.[0-9]+)?)`)
var smartRateBefore = regexp.MustCompile(`(?i)([0-9]+(?:\.[0-9]+)?)\s*(?:倍|[x×✕＊*])(?:[^a-z0-9.]|$)`)
var smartTW = regexp.MustCompile(`(?i)(^|[^a-z])tw(?:[^a-z]|$)`)
var smartSG = regexp.MustCompile(`(?i)(^|[^a-z])sg(?:[^a-z]|$)`)
var smartHK = regexp.MustCompile(`(?i)(^|[^a-z])hk(?:[^a-z]|$)`)

func smartRegion(name string) string {
	low := strings.ToLower(name)
	switch {
	case strings.Contains(low, "台湾") || strings.Contains(low, "台灣") || strings.Contains(low, "taiwan") || strings.Contains(low, "🇹🇼") || smartTW.MatchString(low):
		return "台湾"
	case strings.Contains(low, "新加坡") || strings.Contains(low, "singapore") || strings.Contains(low, "狮城") || strings.Contains(low, "獅城") || strings.Contains(low, "🇸🇬") || smartSG.MatchString(low):
		return "新加坡"
	case strings.Contains(low, "香港") || strings.Contains(low, "hong kong") || strings.Contains(low, "hongkong") || strings.Contains(low, "🇭🇰") || smartHK.MatchString(low):
		return "香港"
	}
	return ""
}

func smartMultiplier(name string) (float64, bool) {
	var found []float64
	for _, expression := range []*regexp.Regexp{smartRateAfter, smartRateBefore} {
		for _, match := range expression.FindAllStringSubmatch(name, -1) {
			if n, e := strconv.ParseFloat(match[1], 64); e == nil && n > 0 && n <= 1000 {
				found = append(found, n)
			}
		}
	}
	if len(found) == 0 {
		return 0, false
	}
	value := found[0]
	for _, n := range found[1:] {
		if math.Abs(n-value) > 0.00001 {
			return 0, false
		}
	}
	return value, true
}

func smartCandidates(proxies map[string]smartProxy, settings smartSaved) []SmartCandidate {
	rows := []SmartCandidate{}
	for _, name := range proxies[selectGroup].All {
		p, ok := proxies[name]
		if !ok || len(p.All) > 0 || quotaNode(name) || name == smartGroup || smartRegion(name) == "" {
			continue
		}
		rate, known := smartMultiplier(name)
		override := false
		if value, ok := settings.Overrides[name]; ok && value > 0 {
			rate, known, override = value, true, true
		}
		row := SmartCandidate{Name: name, Region: smartRegion(name), Multiplier: rate, MultiplierKnown: known, Override: override, Status: "untested", Reason: "等待检查"}
		if len(p.History) > 0 {
			row.Delay = p.History[len(p.History)-1].Delay
		}
		if settings.ExcludeHighMultiplier && !known {
			row.Status, row.Reason = "excluded", "倍率未标明，请先确认倍率"
		}
		if settings.ExcludeHighMultiplier && known && rate > 1 {
			row.Status, row.Reason = "excluded", "已排除高于 1 倍的节点"
		}
		rows = append(rows, row)
	}
	return rows
}

// A successful TCP/TLS connection or a generic 200/401 is not evidence of GPT
// availability. Require the public ChatGPT authentication endpoint's expected
// JSON shape; challenge HTML, unsupported-country errors and timeouts stay
// distinct. This is intentionally a web access check, not a logged-in chat test.
func classifyGPTResponse(status int, contentType string, body []byte) (string, string) {
	lower := strings.ToLower(string(body))
	for _, marker := range []string{"unsupported_country", "unsupported country", "country, region, or territory", "country or region", "not available in your country", "openai's services are not available"} {
		if strings.Contains(lower, marker) {
			return "restricted", "GPT 返回地区限制"
		}
	}
	if strings.Contains(lower, "cf-chl-") || strings.Contains(lower, "challenge-platform") || strings.Contains(lower, "just a moment") || strings.Contains(lower, "verify you are human") {
		return "unknown", "遇到网页人机验证，暂无法确认 GPT"
	}
	if status == 200 && strings.Contains(strings.ToLower(contentType), "application/json") {
		var providers map[string]struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			SigninURL string `json:"signinUrl"`
		}
		if json.Unmarshal(body, &providers) == nil {
			for _, key := range []string{"openai", "auth0", "oidc"} {
				p, ok := providers[key]
				if !ok || p.ID != key || (p.Type != "oauth" && p.Type != "oidc") {
					continue
				}
				u, e := url.Parse(p.SigninURL)
				if e == nil && u.Scheme == "https" && u.Hostname() == "chatgpt.com" && strings.HasPrefix(u.Path, "/api/auth/signin/") {
					return "pass", "GPT 网页检查通过"
				}
			}
		}
	}
	if status == 429 {
		return "unknown", "GPT 检查请求过多，稍后重试"
	}
	return "unknown", fmt.Sprintf("GPT 网页返回 %d，暂无法确认", status)
}

func probeSmartHTTP(ctx context.Context, name, proxyAddress, latencyURL, gptURL string) SmartCandidate {
	row := SmartCandidate{Name: name, Status: "unknown", Reason: "节点未能完成检查", Checked: time.Now().Format(time.RFC3339)}
	proxy, e := url.Parse(proxyAddress)
	if e != nil {
		return row
	}
	tr := &http.Transport{Proxy: http.ProxyURL(proxy), DisableKeepAlives: true, ForceAttemptHTTP2: false, TLSHandshakeTimeout: 4 * time.Second, ResponseHeaderTimeout: 5 * time.Second}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 6 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, e := http.NewRequestWithContext(ctx, "GET", latencyURL, nil)
	if e != nil {
		return row
	}
	start := time.Now()
	response, e := client.Do(request)
	if e != nil {
		row.Reason = "节点连接超时或暂不可达"
		return row
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 2048))
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		row.Reason = "连通性检查未返回预期结果"
		return row
	}
	row.Delay = time.Since(start).Milliseconds()
	if row.Delay < 1 {
		row.Delay = 1
	}
	request, e = http.NewRequestWithContext(ctx, "GET", gptURL, nil)
	if e != nil {
		return row
	}
	request.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux aarch64) AppleWebKit/537.36 Chrome/131.0 Safari/537.36")
	request.Header.Set("Accept", "application/json")
	response, e = client.Do(request)
	if e != nil {
		row.Reason = "GPT 网页检查超时或无法连接"
		return row
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, (128<<10)+1))
	_ = response.Body.Close()
	if readErr != nil || len(body) > 128<<10 {
		row.Reason = "GPT 网页响应无法确认"
		return row
	}
	row.Status, row.Reason = classifyGPTResponse(response.StatusCode, response.Header.Get("Content-Type"), body)
	return row
}

func (a *App) probeSmart(ctx context.Context, name string) SmartCandidate {
	if a.smart.probeHook != nil {
		return a.smart.probeHook(ctx, name)
	}
	return probeSmartHTTP(ctx, name, "http://127.0.0.1:"+strconv.Itoa(smartPort), "https://www.gstatic.com/generate_204", "https://chatgpt.com/api/auth/providers")
}

func (a *App) runSmartCheck(background bool) (string, error) {
	if background && (a.recoveryDegraded() || a.transactionPending()) {
		return "", errors.New("网络恢复期间已暂停后台节点检查")
	}
	a.initSmart()
	m := a.smart
	if !m.probeMu.TryLock() {
		return "", errors.New("平台检查正在进行，请稍后重试")
	}
	defer m.probeMu.Unlock()
	m.mu.Lock()
	if !m.saved.Enabled {
		m.mu.Unlock()
		return "", errors.New("智能优选已关闭")
	}
	if m.running {
		m.mu.Unlock()
		return "", errors.New("智能节点检查正在进行")
	}
	ctx, cancel := context.WithTimeout(context.Background(), smartScanBudget)
	m.running, m.background, m.cancel, m.done = true, background, cancel, make(chan struct{})
	settings := copySmartSaved(m.saved)
	generation, cursor := m.generation, m.cursor
	m.mu.Unlock()
	defer func() {
		cancel()
		m.mu.Lock()
		m.running, m.background, m.cancel = false, false, nil
		close(m.done)
		m.next = time.Now().Add(smartInterval)
		m.mu.Unlock()
	}()
	proxies, e := a.smartProxies(ctx)
	if e != nil {
		return a.finishSmartError(generation, "智能检查暂时无法连接代理服务；当前节点保留", e)
	}
	probe, hasProbe := proxies[smartGroup]
	if !hasProbe || len(probe.All) == 0 {
		return a.finishSmartError(generation, "独立检测通道未就绪，请关闭后重新开启智能优选；当前节点保留", errors.New("独立检测通道未就绪"))
	}
	current := proxies[selectGroup].Now
	rows := smartCandidates(proxies, settings)
	eligible := []int{}
	for i := range rows {
		if rows[i].Status != "excluded" {
			eligible = append(eligible, i)
		}
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		a, b := rows[eligible[i]], rows[eligible[j]]
		if a.Name == current {
			return true
		}
		if b.Name == current {
			return false
		}
		di, dj := a.Delay, b.Delay
		if di <= 0 {
			di = 1 << 30
		}
		if dj <= 0 {
			dj = 1 << 30
		}
		return di < dj
	})
	// Rotate the non-current portion if a previous round exhausted its time
	// budget. No candidate is silently advertised as checked without a probe.
	start := 0
	if len(eligible) > 0 && rows[eligible[0]].Name == current {
		start = 1
	}
	if n := len(eligible) - start; n > 1 && cursor > 0 {
		offset := cursor % n
		copyOrder := append([]int{}, eligible[start:]...)
		copy(eligible[start:], append(copyOrder[offset:], copyOrder[:offset]...))
	}
	rotatedProgress := 0
	probeMembers := map[string]bool{}
	for _, name := range probe.All {
		probeMembers[name] = true
	}
	for _, index := range eligible {
		if ctx.Err() != nil {
			break
		}
		// Reserve time for re-reading the active selector and the single
		// final switch; otherwise an exhausted scan deadline would discard
		// every successful result from a slow round.
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < 15*time.Second {
			break
		}
		name := rows[index].Name
		if !probeMembers[name] {
			rows[index].Status, rows[index].Reason = "unknown", "检测通道尚未包含这个节点，请重新开启智能优选"
			continue
		}
		if _, e = a.smartAPI(ctx, "PUT", "/proxies/"+url.PathEscape(smartGroup), map[string]string{"name": name}); e != nil {
			rows[index].Status, rows[index].Reason = "unknown", "独立检测通道暂时不可用"
			if ctx.Err() != nil {
				break
			}
			continue
		}
		nodeCtx, stop := context.WithTimeout(ctx, 7*time.Second)
		result := a.probeSmart(nodeCtx, name)
		stop()
		rows[index].Status, rows[index].Reason, rows[index].Delay, rows[index].Checked = result.Status, result.Reason, result.Delay, result.Checked
		if name != current || start == 0 {
			rotatedProgress++
		}
		m.mu.Lock()
		if m.generation == generation {
			m.saved.Candidates = append([]SmartCandidate{}, rows...)
		}
		m.mu.Unlock()
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return "检查已暂停，当前节点保持不变", nil
	}
	m.mu.Lock()
	if m.generation != generation || !m.saved.Enabled {
		m.mu.Unlock()
		return "检查已暂停，当前节点保持不变", nil
	}
	next := copySmartSaved(m.saved)
	next.Candidates = rows
	next.LastChecked = time.Now().Format(time.RFC3339)
	next.Selected = current
	m.cursor = (cursor + rotatedProgress) % max(1, len(eligible)-start)
	best, currentRow := -1, -1
	for i := range rows {
		if rows[i].Name == current {
			currentRow = i
		}
		if rows[i].Status == "pass" && (best == -1 || rows[i].Delay < rows[best].Delay) {
			best = i
		}
	}
	if currentRow >= 0 && rows[currentRow].Checked != "" {
		if rows[currentRow].Status == "pass" {
			m.failures[current] = 0
		} else {
			m.failures[current]++
		}
	}
	message := "没有同时满足地区、倍率与 GPT 网页检查的节点，已保留当前节点"
	if best >= 0 {
		bestRow := rows[best]
		switchNow := current != bestRow.Name
		if currentRow >= 0 && rows[currentRow].Status == "pass" {
			improvement := rows[currentRow].Delay - bestRow.Delay
			threshold := int64(math.Max(25, float64(rows[currentRow].Delay)*0.20))
			lastSwitch, _ := time.Parse(time.RFC3339, settings.LastSwitch)
			if improvement < threshold || (!lastSwitch.IsZero() && time.Since(lastSwitch) < 5*time.Minute) {
				switchNow = false
			}
		} else if currentRow >= 0 && rows[currentRow].Status != "excluded" && settings.LastChecked != "" && m.failures[current] < 2 {
			switchNow = false
		}
		message = "GPT 网页检查通过，当前节点已保持"
		if switchNow {
			// Re-read the selector immediately before the one household change.
			// A foreign client may have manually switched without our UI.
			m.mu.Unlock()
			fresh, fe := a.smartProxies(ctx)
			m.mu.Lock()
			if fe != nil || fresh[selectGroup].Now != current || m.generation != generation || !m.saved.Enabled {
				message = "检查完成，当前选择已变化，本轮不自动切换"
				if fe != nil {
					message = "检查完成，但暂时无法确认当前选择，本轮未切换节点"
				} else if fresh[selectGroup].Now != "" {
					next.Selected = fresh[selectGroup].Now
				}
			} else {
				m.mu.Unlock()
				_, se := a.smartAPI(ctx, "PUT", "/proxies/"+url.PathEscape(selectGroup), map[string]string{"name": bestRow.Name})
				if se == nil {
					if pe := a.rememberDefaultSelector(bestRow.Name); pe != nil {
						_, _ = a.smartAPI(context.Background(), "PUT", "/proxies/"+url.PathEscape(selectGroup), map[string]string{"name": current})
						se = pe
					}
				}
				m.mu.Lock()
				if se == nil {
					next.Selected, next.LastSwitch = bestRow.Name, time.Now().Format(time.RFC3339)
					message = "已优选 " + bestRow.Name + "，GPT 网页检查通过"
				} else {
					message = "检查完成，但节点切换未成功；保留原选择"
				}
			}
		}
	}
	if m.generation != generation {
		m.mu.Unlock()
		return "检查已暂停，当前节点保持不变", nil
	}
	next.Message = message
	if e := a.persistSmart(next); e != nil {
		message += "；本轮结果暂未保存"
		next.Message = message
	}
	m.saved = next
	m.mu.Unlock()
	return message, nil
}

func (a *App) finishSmartError(generation uint64, message string, cause error) (string, error) {
	m := a.smart
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.generation != generation {
		return "检查已暂停，当前节点保持不变", nil
	}
	next := copySmartSaved(m.saved)
	next.LastChecked, next.Message = time.Now().Format(time.RFC3339), message
	_ = a.persistSmart(next)
	m.saved = next
	return message, cause
}

func (a *App) smartMonitor() {
	a.initSmart()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if a.recoveryDegraded() || a.transactionPending() {
			continue
		}
		m := a.smart
		m.mu.Lock()
		due := m.saved.Enabled && !m.running && !time.Now().Before(m.next)
		m.mu.Unlock()
		if !due {
			continue
		}
		select {
		case a.gate <- struct{}{}:
		default:
			continue
		}
		release, e := acquireManagerLock()
		if e != nil {
			<-a.gate
			continue
		}
		if a.recoveryDegraded() || a.transactionPending() {
			release()
			<-a.gate
			continue
		}
		_, _ = a.runSmartCheck(true)
		release()
		<-a.gate
	}
}
