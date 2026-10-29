package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type PlatformEdit struct {
	Node      string   `json:"node"`
	Platforms []string `json:"platforms"`
	Force     bool     `json:"force"`
}
type PlatformResult struct {
	Node     string `json:"node"`
	Platform string `json:"platform"`
	Status   string `json:"status"`
	Delay    int64  `json:"delay"`
	Checked  string `json:"checked"`
	Reason   string `json:"reason"`
	Stale    bool   `json:"stale,omitempty"`
}
type PlatformStore struct {
	Version     int              `json:"version"`
	Items       []PlatformResult `json:"items"`
	CatalogHash string           `json:"catalogHash,omitempty"`
}
type PlatformsView struct {
	Running   bool                `json:"running"`
	Message   string              `json:"message"`
	Items     []PlatformResult    `json:"items"`
	Platforms []ServiceDefinition `json:"platforms"`
}
type PlatformManager struct {
	mu        sync.Mutex
	saved     PlatformStore
	running   bool
	message   string
	probeHook func(context.Context, string, string) PlatformResult
}

func validatePlatformStore(s *PlatformStore) error {
	if s.Version != 1 || len(s.Items) > 10000 || len(s.CatalogHash) > 128 {
		return errors.New("平台检查缓存格式不正确")
	}
	for _, r := range s.Items {
		if _, ok := serviceDefinition(r.Platform); !ok {
			return errors.New("平台检查缓存包含未知平台")
		}
		if r.Node == "" || len(r.Node) > 1024 || strings.ContainsAny(r.Node, "\x00\r\n") || r.Delay < 0 || len(r.Reason) > 2048 {
			return errors.New("平台检查缓存内容不正确")
		}
		switch r.Status {
		case "unchecked", "pass", "fail", "restricted", "unknown":
		default:
			return errors.New("平台检查状态不正确")
		}
		if r.Checked != "" {
			if _, e := time.Parse(time.RFC3339, r.Checked); e != nil {
				return errors.New("平台检查缓存时间不正确")
			}
		}
	}
	return nil
}
func (a *App) readPlatforms() (*PlatformManager, error) {
	m := &PlatformManager{saved: PlatformStore{Version: 1, Items: []PlatformResult{}}, message: "检查公开网页可达性；不代表登录后聊天或视频播放已经解锁"}
	b, e := os.ReadFile(filepath.Join(a.store, "platform-checks.json"))
	if os.IsNotExist(e) {
		return m, nil
	}
	if e != nil {
		return m, e
	}
	if json.Unmarshal(b, &m.saved) != nil || validatePlatformStore(&m.saved) != nil {
		return m, errors.New("平台检查缓存无法读取，可重新检查")
	}
	if m.saved.Items == nil {
		m.saved.Items = []PlatformResult{}
	}
	if hash, e := a.smartCatalogHash(); e == nil && m.saved.CatalogHash != "" && hash != m.saved.CatalogHash {
		m.saved.Items = []PlatformResult{}
		m.saved.CatalogHash = hash
		m.message = "订阅节点配置已变化，请重新检查平台"
	}
	return m, nil
}
func (a *App) initPlatforms() {
	a.platformOnce.Do(func() {
		m, e := a.readPlatforms()
		if e != nil {
			m.saved = PlatformStore{Version: 1, Items: []PlatformResult{}}
			m.message = e.Error()
		}
		a.platforms = m
	})
}
func (a *App) reloadPlatforms() error {
	m, e := a.readPlatforms()
	if e != nil {
		return e
	}
	a.initPlatforms()
	a.platforms.mu.Lock()
	defer a.platforms.mu.Unlock()
	if a.platforms.running {
		return errors.New("平台检查正在进行，请稍后恢复")
	}
	a.platforms.saved = m.saved
	a.platforms.message = m.message
	return nil
}
func (a *App) platformsView() PlatformsView {
	a.initPlatforms()
	m := a.platforms
	m.mu.Lock()
	defer m.mu.Unlock()
	items := append([]PlatformResult{}, m.saved.Items...)
	message := m.message
	if hash, e := a.smartCatalogHash(); e == nil && m.saved.CatalogHash != "" && hash != m.saved.CatalogHash {
		items = []PlatformResult{}
		message = "订阅节点配置已变化，请重新检查平台"
	}
	for i := range items {
		checked, e := time.Parse(time.RFC3339, items[i].Checked)
		items[i].Stale = e != nil || checked.After(time.Now().Add(time.Minute)) || time.Since(checked) > 10*time.Minute
	}
	return PlatformsView{Running: m.running, Message: message, Items: items, Platforms: append([]ServiceDefinition{}, serviceDefinitions...)}
}
func (a *App) cachedPlatform(node, id string, age time.Duration) (PlatformResult, bool) {
	a.initPlatforms()
	m := a.platforms
	m.mu.Lock()
	defer m.mu.Unlock()
	hash, e := a.smartCatalogHash()
	if e != nil || m.saved.CatalogHash != hash {
		return PlatformResult{}, false
	}
	for _, r := range m.saved.Items {
		if r.Node == node && r.Platform == id {
			checked, e := time.Parse(time.RFC3339, r.Checked)
			if e == nil && !checked.After(time.Now().Add(time.Minute)) && time.Since(checked) < age {
				return r, true
			}
		}
	}
	return PlatformResult{}, false
}
func (a *App) recordPlatform(r PlatformResult) {
	a.initPlatforms()
	m := a.platforms
	m.mu.Lock()
	defer m.mu.Unlock()
	hash, e := a.smartCatalogHash()
	if e != nil {
		return
	}
	if m.saved.CatalogHash != hash {
		m.saved.Items = []PlatformResult{}
		m.saved.CatalogHash = hash
	}
	r.Checked = time.Now().Format(time.RFC3339)
	found := false
	for i, old := range m.saved.Items {
		if old.Node == r.Node && old.Platform == r.Platform {
			m.saved.Items[i] = r
			found = true
			break
		}
	}
	if !found {
		m.saved.Items = append(m.saved.Items, r)
	}
	if len(m.saved.Items) > 1000 {
		sort.SliceStable(m.saved.Items, func(i, j int) bool { return m.saved.Items[i].Checked > m.saved.Items[j].Checked })
		m.saved.Items = m.saved.Items[:1000]
	}
	b, e := json.Marshal(m.saved)
	if e == nil {
		if e = atomicWrite(filepath.Join(a.store, "platform-checks.json"), b); e != nil {
			m.message = "检查结果暂未保存，请检查路由器空间"
		}
	}
}
func classifyPlatformResponse(id string, status int, contentType string, body []byte) (string, string) {
	if id == "chatgpt" {
		return classifyGPTResponse(status, contentType, body)
	}
	d, ok := serviceDefinition(id)
	if !ok {
		return "unknown", "未知平台"
	}
	lower := strings.ToLower(string(body))
	for _, marker := range []string{"not available in your country", "not available in your region", "unavailable in your location", "unavailable in your country", "geo_restricted", "geo-restricted", "region restricted", "proxy or unblocker", "unblocker or proxy", "检测到您正在使用解锁工具或代理"} {
		if strings.Contains(lower, marker) {
			return "restricted", d.Name + " 返回地区或代理限制"
		}
	}
	if status == 451 {
		return "restricted", d.Name + " 返回访问地区限制"
	}
	for _, marker := range []string{"cf-chl-", "challenge-platform", "just a moment", "verify you are human", "captcha", "access denied", "enable javascript and cookies", "verify to continue"} {
		if strings.Contains(lower, marker) {
			return "unknown", d.Name + " 遇到验证或访问检查，暂无法确认"
		}
	}
	if status == 429 {
		return "unknown", d.Name + " 请求过多，稍后重试"
	}
	if status >= 500 {
		return "fail", fmt.Sprintf("%s 网站暂时返回 %d", d.Name, status)
	}
	if status != 200 {
		return "unknown", fmt.Sprintf("%s 网页返回 %d，暂无法确认", d.Name, status)
	}
	if !strings.Contains(strings.ToLower(contentType), "text/html") {
		return "unknown", d.Name + " 没有返回预期网页"
	}
	markers := map[string][]string{"tiktok": {"sigi_state", "__universal_data_for_rehydration__", "tiktok-webapp", "<title>tiktok"}, "youtube": {"ytinitialdata", "ytcfg.set", "ytd-app"}, "twitter": {"<title>x</title>", "<title>twitter", "window.__initial_state__", "responsive-web/client-web"}, "netflix": {"netflix", "nfcontext"}}
	matched := false
	for _, marker := range markers[id] {
		if strings.Contains(lower, marker) {
			matched = true
			break
		}
	}
	if !matched {
		return "unknown", d.Name + " 网页内容无法确认"
	}
	return "pass", d.Name + " 公开网页可访问；未检测登录或视频播放"
}
func platformProbeURL(id string) string {
	switch id {
	case "chatgpt":
		return "https://chatgpt.com/api/auth/providers"
	case "tiktok":
		return "https://www.tiktok.com/"
	case "youtube":
		return "https://www.youtube.com/"
	case "twitter":
		return "https://x.com/"
	case "netflix":
		return "https://www.netflix.com/title/80018499"
	}
	return ""
}
func probePlatformHTTP(ctx context.Context, node, id, proxyAddress, target string) PlatformResult {
	r := PlatformResult{Node: node, Platform: id, Status: "unknown", Reason: "检查尚未完成", Checked: time.Now().Format(time.RFC3339)}
	proxy, e := url.Parse(proxyAddress)
	if e != nil || target == "" {
		return r
	}
	tr := &http.Transport{Proxy: http.ProxyURL(proxy), DisableKeepAlives: true, ForceAttemptHTTP2: false, TLSHandshakeTimeout: 4 * time.Second, ResponseHeaderTimeout: 5 * time.Second}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 6 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return http.ErrUseLastResponse
		}
		first := via[0].URL.Hostname()
		host := req.URL.Hostname()
		if host != first && !strings.HasSuffix(host, "."+strings.TrimPrefix(first, "www.")) && !strings.HasSuffix(first, "."+host) {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	req, e := http.NewRequestWithContext(ctx, "GET", target, nil)
	if e != nil {
		return r
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux aarch64) AppleWebKit/537.36 Chrome/131.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/json;q=0.9,*/*;q=0.8")
	start := time.Now()
	response, e := client.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			r.Reason = "检查超时或已暂停"
		} else {
			r.Status = "fail"
			r.Reason = "节点无法连接这个平台"
		}
		return r
	}
	r.Delay = max(1, time.Since(start).Milliseconds())
	body, re := io.ReadAll(io.LimitReader(response.Body, 512<<10))
	_ = response.Body.Close()
	if re != nil {
		r.Reason = "网页响应读取不完整，暂无法确认"
		return r
	}
	r.Status, r.Reason = classifyPlatformResponse(id, response.StatusCode, response.Header.Get("Content-Type"), body)
	return r
}
func (a *App) probePlatform(ctx context.Context, node, id string) PlatformResult {
	a.initPlatforms()
	m := a.platforms
	if m.probeHook != nil {
		return m.probeHook(ctx, node, id)
	}
	return probePlatformHTTP(ctx, node, id, "http://127.0.0.1:19081", platformProbeURL(id))
}
func (a *App) performPlatforms(r Request) (string, any, error, bool) {
	if r.Action != "platform-check" {
		return "", nil, nil, false
	}
	if r.Platform == nil {
		return "", nil, errors.New("请选择节点和要检查的平台"), true
	}
	a.initPlatforms()
	a.initSmart()
	m := a.platforms
	if len(r.Platform.Platforms) == 0 || len(r.Platform.Platforms) > 5 {
		return "", nil, errors.New("请选择 1 至 5 个平台"), true
	}
	ids := []string{}
	for _, id := range r.Platform.Platforms {
		if _, ok := serviceDefinition(id); !ok {
			return "", nil, errors.New("平台不存在"), true
		}
		if !containsString(ids, id) {
			ids = append(ids, id)
		}
	}
	c, e := a.config()
	if e != nil || !containsString(serviceLeafNames(c), r.Platform.Node) {
		return "", nil, errors.New("节点已不在当前订阅，请刷新后重选"), true
	}
	if !a.smart.probeMu.TryLock() {
		return "", nil, errors.New("节点检查正在进行，请稍后重试"), true
	}
	defer a.smart.probeMu.Unlock()
	m.mu.Lock()
	m.running = true
	m.message = "正在通过独立通道检查，家庭当前节点保持"
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.running = false; m.mu.Unlock() }()
	if e = a.ensureSmartInfrastructure(); e != nil {
		return "", nil, e, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	ps, e := a.smartProxies(ctx)
	if e != nil || !containsString(ps[smartGroup].All, r.Platform.Node) {
		return "", nil, errors.New("独立检测分组尚未就绪"), true
	}
	if _, e = a.smartAPI(ctx, "PUT", "/proxies/"+url.PathEscape(smartGroup), map[string]string{"name": r.Platform.Node}); e != nil {
		return "", nil, e, true
	}
	results := []PlatformResult{}
	for _, id := range ids {
		if !r.Platform.Force {
			if cached, ok := a.cachedPlatform(r.Platform.Node, id, 10*time.Minute); ok {
				results = append(results, cached)
				continue
			}
		}
		nodeCtx, stop := context.WithTimeout(ctx, 7*time.Second)
		result := a.probePlatform(nodeCtx, r.Platform.Node, id)
		stop()
		a.recordPlatform(result)
		results = append(results, result)
		if ctx.Err() != nil {
			break
		}
	}
	m.mu.Lock()
	m.message = "检查完成；通过仅表示公开网页可达，登录聊天和视频播放请实际确认"
	m.mu.Unlock()
	return "平台检查完成", results, nil, true
}
