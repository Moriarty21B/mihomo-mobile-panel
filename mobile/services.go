package main

// Services own only their prefixed selector groups and narrowly scoped domain
// rules. Airport credentials, LAN networking and direct rules remain local.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const serviceGroupPrefix = "服务 · "

type ServiceDefinition struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Note    string `json:"note"`
	domains []string
}

var serviceDefinitions = []ServiceDefinition{
	{ID: "chatgpt", Name: "ChatGPT", Note: "网页及登录入口；不代表登录后每次聊天均可用", domains: []string{"chatgpt.com", "openai.com", "oaistatic.com", "oaiusercontent.com"}},
	{ID: "tiktok", Name: "TikTok", Note: "检查网页可达性；账号、应用和视频播放仍受其他条件影响", domains: []string{"tiktok.com", "tiktokv.com", "tiktokcdn.com", "tiktokcdn-us.com", "musical.ly", "muscdn.com", "ttwstatic.com"}},
	{ID: "youtube", Name: "YouTube", Note: "检查网站页面；不代表会员内容或所有视频可播放", domains: []string{"youtube.com", "youtu.be", "googlevideo.com", "ytimg.com", "youtubei.googleapis.com", "youtube.googleapis.com", "youtube-nocookie.com", "ggpht.com"}},
	{ID: "twitter", Name: "X / Twitter", Note: "检查公开网页；未登录或人机验证时显示暂无法确认", domains: []string{"x.com", "twitter.com", "twimg.com", "t.co", "tweetdeck.com"}},
	{ID: "netflix", Name: "Netflix", Note: "检查网站及公开片目页；不代表会员片库或视频播放已解锁", domains: []string{"netflix.com", "netflix.net", "nflxvideo.net", "nflximg.net", "nflximg.com", "nflxso.net", "nflxext.com"}},
}

type ServiceEdit struct {
	ID   string `json:"id"`
	Mode string `json:"mode"`
	Node string `json:"node"`
}
type ServiceChoice struct {
	Mode string `json:"mode"`
	Node string `json:"node,omitempty"`
}
type ServicesStore struct {
	Version     int                      `json:"version"`
	Revision    uint64                   `json:"revision"`
	Choices     map[string]ServiceChoice `json:"choices"`
	LastSwitch  map[string]string        `json:"lastSwitch,omitempty"`
	LastChecked map[string]string        `json:"lastChecked,omitempty"`
	Messages    map[string]string        `json:"messages,omitempty"`
	Cursor      map[string]int           `json:"cursor,omitempty"`
}
type ServiceView struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Group        string `json:"group"`
	Mode         string `json:"mode"`
	Node         string `json:"node"`
	Current      string `json:"current"`
	Missing      bool   `json:"missing"`
	Message      string `json:"message"`
	SmartAllowed bool   `json:"smartAllowed"`
	Checked      string `json:"checked"`
}
type ServicesView struct {
	Provisioned bool          `json:"provisioned"`
	Error       string        `json:"error"`
	Items       []ServiceView `json:"items"`
	Nodes       []string      `json:"nodes"`
	RuleAuto    bool          `json:"ruleAuto"`
}
type servicesJournal struct {
	Version       int               `json:"version"`
	Original      []byte            `json:"original"`
	OriginalHash  string            `json:"originalHash"`
	Before        []byte            `json:"before,omitempty"`
	BeforeExists  bool              `json:"beforeExists"`
	On            bool              `json:"on"`
	ConfigChanged bool              `json:"configChanged"`
	Selected      map[string]string `json:"selected"`
}

func serviceDefinition(id string) (ServiceDefinition, bool) {
	for _, d := range serviceDefinitions {
		if d.ID == id {
			return d, true
		}
	}
	return ServiceDefinition{}, false
}
func serviceGroup(id string) string {
	if d, ok := serviceDefinition(id); ok {
		// A slash in a selector name is decoded before routing by some core
		// builds, even when PathEscape is used. Keep the internal name simple.
		if id == "twitter" {
			return serviceGroupPrefix + "Twitter"
		}
		return serviceGroupPrefix + d.Name
	}
	return ""
}
func defaultServicesStore() ServicesStore {
	s := ServicesStore{Version: 1, Choices: map[string]ServiceChoice{}, LastSwitch: map[string]string{}, LastChecked: map[string]string{}, Messages: map[string]string{}, Cursor: map[string]int{}}
	for _, d := range serviceDefinitions {
		s.Choices[d.ID] = ServiceChoice{Mode: "default"}
	}
	return s
}
func validateServicesStore(s *ServicesStore) error {
	if s.Version != 1 || len(s.Choices) > len(serviceDefinitions) || len(s.LastSwitch) > len(serviceDefinitions) || len(s.LastChecked) > len(serviceDefinitions) || len(s.Messages) > len(serviceDefinitions) || len(s.Cursor) > len(serviceDefinitions) {
		return errors.New("服务分流记录格式不正确")
	}
	for id, c := range s.Choices {
		if _, ok := serviceDefinition(id); !ok {
			return errors.New("服务分流记录包含未知服务")
		}
		if c.Mode != "default" && c.Mode != "manual" && c.Mode != "smart" {
			return errors.New("服务分流方式不正确")
		}
		if len(c.Node) > 1024 || strings.ContainsAny(c.Node, "\x00\r\n") || c.Mode == "manual" && strings.TrimSpace(c.Node) == "" {
			return errors.New("服务节点记录不正确")
		}
	}
	for id, at := range s.LastSwitch {
		if _, ok := serviceDefinition(id); !ok {
			return errors.New("服务切换时间不正确")
		}
		if _, e := time.Parse(time.RFC3339, at); e != nil {
			return errors.New("服务切换时间不正确")
		}
	}
	for id, at := range s.LastChecked {
		if _, ok := serviceDefinition(id); !ok {
			return errors.New("服务检测记录不正确")
		}
		if _, e := time.Parse(time.RFC3339, at); e != nil {
			return errors.New("服务检测时间不正确")
		}
	}
	for id, message := range s.Messages {
		if _, ok := serviceDefinition(id); !ok || len(message) > 4096 {
			return errors.New("服务检测信息不正确")
		}
	}
	for id, cursor := range s.Cursor {
		if _, ok := serviceDefinition(id); !ok || cursor < 0 || cursor > 2000 {
			return errors.New("服务检测进度不正确")
		}
	}
	return nil
}
func (a *App) servicesPath() string { return filepath.Join(a.store, "services.json") }
func (a *App) servicesSnapshot() (ServicesStore, error) {
	a.serviceMu.Lock()
	defer a.serviceMu.Unlock()
	return a.readServices()
}
func (a *App) readServices() (ServicesStore, error) {
	s := defaultServicesStore()
	b, e := os.ReadFile(a.servicesPath())
	if os.IsNotExist(e) {
		return s, nil
	}
	if e != nil {
		return s, e
	}
	if json.Unmarshal(b, &s) != nil || validateServicesStore(&s) != nil {
		return defaultServicesStore(), errors.New("服务分流记录无法读取，现有网络配置保留")
	}
	if s.Choices == nil {
		s.Choices = map[string]ServiceChoice{}
	}
	if s.LastSwitch == nil {
		s.LastSwitch = map[string]string{}
	}
	if s.LastChecked == nil {
		s.LastChecked = map[string]string{}
	}
	if s.Messages == nil {
		s.Messages = map[string]string{}
	}
	if s.Cursor == nil {
		s.Cursor = map[string]int{}
	}
	for _, d := range serviceDefinitions {
		if _, ok := s.Choices[d.ID]; !ok {
			s.Choices[d.ID] = ServiceChoice{Mode: "default"}
		}
	}
	return s, nil
}
func (a *App) writeServices(s ServicesStore) error {
	if e := validateServicesStore(&s); e != nil {
		return e
	}
	b, e := json.Marshal(s)
	if e != nil {
		return e
	}
	return atomicWrite(a.servicesPath(), b)
}
func serviceLeafNames(c map[string]any) []string {
	result := []string{}
	if ps, ok := c["proxies"].([]any); ok {
		for _, item := range ps {
			if p, ok := item.(map[string]any); ok {
				if n, ok := p["name"].(string); ok && n != "" && !quotaNode(n) {
					result = append(result, n)
				}
			}
		}
	}
	return result
}
func containsString(items []string, s string) bool {
	for _, v := range items {
		if v == s {
			return true
		}
	}
	return false
}
func serviceProvisioned(c map[string]any) bool {
	found := map[string]bool{}
	if gs, ok := c["proxy-groups"].([]any); ok {
		for _, item := range gs {
			if g, ok := item.(map[string]any); ok {
				n, _ := g["name"].(string)
				found[n] = g["type"] == "select"
			}
		}
	}
	for _, d := range serviceDefinitions {
		if !found[serviceGroup(d.ID)] {
			return false
		}
	}
	return true
}
func serviceOwnTarget(target string) bool {
	for _, d := range serviceDefinitions {
		if target == serviceGroup(d.ID) {
			return true
		}
	}
	return false
}
func serviceRuleTarget(parts []string) int {
	if len(parts) >= 2 && strings.EqualFold(strings.TrimSpace(parts[0]), "MATCH") {
		return 1
	}
	if len(parts) >= 3 {
		return 2
	}
	return -1
}
func directRuleTarget(target string) bool {
	switch strings.ToUpper(strings.TrimSpace(target)) {
	case "DIRECT", "REJECT", "REJECT-DROP", "PASS":
		return true
	}
	return false
}
func serviceDomainMatches(rule, domain string) bool {
	parts := strings.Split(rule, ",")
	if len(parts) < 3 {
		return false
	}
	kind, key := strings.ToUpper(strings.TrimSpace(parts[0])), strings.ToLower(strings.TrimSpace(parts[1]))
	switch kind {
	case "DOMAIN", "DOMAIN-SUFFIX":
		return key == domain || strings.HasSuffix(key, "."+domain)
	}
	return false
}

// Service rules are inserted after the leading explicit DIRECT/REJECT rules,
// before broad GeoIP/MATCH rules. Existing domain DIRECT rules anywhere are
// retained and promoted ahead of generated rules if they overlap a service.
func compileServiceConfig(body []byte, s ServicesStore) ([]byte, error) {
	if e := validateServicesStore(&s); e != nil {
		return nil, e
	}
	var c map[string]any
	if yaml.Unmarshal(body, &c) != nil {
		return nil, errors.New("代理配置无法读取")
	}
	groups, ok := c["proxy-groups"].([]any)
	if !ok {
		return nil, errors.New("节点分组无法读取")
	}
	nodes := serviceLeafNames(c)
	if len(nodes) == 0 {
		return nil, errors.New("没有可用节点，保留现有配置")
	}
	nodeSet := map[string]bool{}
	for _, n := range nodes {
		nodeSet[n] = true
	}
	for _, d := range serviceDefinitions {
		if nodeSet[serviceGroup(d.ID)] {
			return nil, errors.New("节点名称与服务分组冲突")
		}
	}
	members := []any{selectGroup}
	for _, n := range nodes {
		members = append(members, n)
	}
	for _, d := range serviceDefinitions {
		found := false
		for _, item := range groups {
			g, ok := item.(map[string]any)
			if !ok {
				return nil, errors.New("节点分组格式无法读取")
			}
			if g["name"] == serviceGroup(d.ID) {
				if g["type"] != "select" || g["hidden"] != true {
					return nil, errors.New("服务分组名称已被占用")
				}
				g["proxies"] = append([]any{}, members...)
				g["hidden"] = true
				found = true
			}
		}
		if !found {
			groups = append(groups, map[string]any{"name": serviceGroup(d.ID), "type": "select", "hidden": true, "proxies": append([]any{}, members...)})
		}
	}
	c["proxy-groups"] = groups
	raw, ok := c["rules"].([]any)
	if !ok {
		return nil, errors.New("当前规则无法读取")
	}
	base := []string{}
	for _, item := range raw {
		line, ok := item.(string)
		if !ok {
			return nil, errors.New("当前规则格式无法读取")
		}
		parts := strings.Split(line, ",")
		ti := serviceRuleTarget(parts)
		if ti >= 0 && serviceOwnTarget(strings.TrimSpace(parts[ti])) {
			continue
		}
		base = append(base, line)
	}
	generated := []string{}
	protected := []string{}
	remaining := []string{}
	for _, line := range base {
		parts := strings.Split(line, ",")
		ti := serviceRuleTarget(parts)
		protect := false
		if ti >= 0 && directRuleTarget(parts[ti]) {
			for _, d := range serviceDefinitions {
				for _, domain := range d.domains {
					if serviceDomainMatches(line, domain) {
						protect = true
					}
				}
			}
		}
		if protect {
			protected = append(protected, line)
		} else {
			remaining = append(remaining, line)
		}
	}
	for _, d := range serviceDefinitions {
		choice := s.Choices[d.ID]
		if choice.Mode == "" || choice.Mode == "default" {
			continue
		}
		for _, domain := range d.domains {
			generated = append(generated, "DOMAIN-SUFFIX,"+domain+","+serviceGroup(d.ID))
		}
	}
	// Keep all leading download/direct exceptions at their original priority.
	cut := 0
	for cut < len(remaining) {
		p := strings.Split(remaining[cut], ",")
		ti := serviceRuleTarget(p)
		if ti < 0 || !directRuleTarget(p[ti]) {
			break
		}
		cut++
	}
	result := append([]string{}, remaining[:cut]...)
	result = append(result, protected...)
	result = append(result, generated...)
	result = append(result, remaining[cut:]...)
	// Generated rules may already exist after a previous application. Remove
	// duplicate exact entries without disturbing first-match priority.
	unique := make([]string, 0, len(result))
	seen := map[string]bool{}
	for _, line := range result {
		if !seen[line] {
			unique = append(unique, line)
			seen[line] = true
		}
	}
	c["rules"] = unique
	return yaml.Marshal(c)
}

func (a *App) servicesView() ServicesView {
	v := ServicesView{Items: []ServiceView{}, Nodes: []string{}, RuleAuto: true}
	s, e := a.servicesSnapshot()
	if e != nil {
		v.Error = e.Error()
	}
	c, ce := a.config()
	if ce == nil {
		v.Provisioned = serviceProvisioned(c)
		v.Nodes = serviceLeafNames(c)
	} else if v.Error == "" {
		v.Error = "代理配置暂时无法读取"
	}
	if p := a.profileSnapshot(); p != nil {
		v.RuleAuto = profileAutoApply(p)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	proxies, _ := a.smartProxies(ctx)
	for _, d := range serviceDefinitions {
		choice := s.Choices[d.ID]
		row := ServiceView{ID: d.ID, Name: d.Name, Group: serviceGroup(d.ID), Mode: choice.Mode, Node: choice.Node, Current: proxies[serviceGroup(d.ID)].Now, SmartAllowed: true, Checked: s.LastChecked[d.ID]}
		if row.Mode == "" {
			row.Mode = "default"
		}
		switch row.Mode {
		case "default":
			row.Message = "跟随默认节点"
			if row.Current == "" {
				row.Current = selectGroup
			}
		case "manual":
			row.Missing = !containsString(v.Nodes, choice.Node)
			if row.Missing {
				row.Message = "原节点已不在当前订阅，暂时跟随默认节点"
			} else {
				row.Message = "使用指定节点"
			}
		case "smart":
			row.Message = "仅选择对应平台检查通过的低延迟节点"
			if message := s.Messages[d.ID]; message != "" {
				row.Message = message
			}
			if choice.Node == "" {
				row.Message = "等待平台检查通过，暂时跟随默认节点"
			}
			if choice.Node != "" && !containsString(v.Nodes, choice.Node) {
				row.Missing = true
				row.Message = "优选节点已不在当前订阅，等待重新检查"
			}
		}
		if row.Current == "" {
			row.Current = selectGroup
		}
		v.Items = append(v.Items, row)
	}
	return v
}

func (a *App) servicesJournalPath() string {
	return filepath.Join(a.store, "services-provision-pending.json")
}
func (a *App) captureServiceSelectors() (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ps, e := a.smartProxies(ctx)
	if e != nil || ps[selectGroup].Now == "" {
		return nil, errors.New("无法读取当前节点，现有配置保留")
	}
	result := map[string]string{}
	for name, p := range ps {
		if strings.EqualFold(p.Type, "Selector") && p.Now != "" && name != smartGroup {
			result[name] = p.Now
		}
	}
	return result, nil
}
func (a *App) restoreAvailableSelectors(selected map[string]string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	ps, e := a.smartProxies(ctx)
	if e != nil {
		return e
	}
	keys := []string{}
	for g := range selected {
		keys = append(keys, g)
	}
	sort.Strings(keys)
	for _, g := range keys {
		p, ok := ps[g]
		if !ok || !containsString(p.All, selected[g]) {
			continue
		}
		if _, e = a.smartAPI(ctx, "PUT", "/proxies/"+url.PathEscape(g), map[string]string{"name": selected[g]}); e != nil {
			return errors.New("节点选择尚未完全恢复")
		}
	}
	return nil
}
func (a *App) setServiceSelections(s ServicesStore) error {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	ps, e := a.smartProxies(ctx)
	if e != nil {
		return e
	}
	for _, d := range serviceDefinitions {
		group := serviceGroup(d.ID)
		p, exists := ps[group]
		if !exists {
			return errors.New("服务分组尚未就绪")
		}
		choice := s.Choices[d.ID]
		want := selectGroup
		if choice.Mode != "default" && containsString(p.All, choice.Node) {
			want = choice.Node
		}
		if choice.Mode == "smart" {
			if cached, valid := a.cachedPlatform(choice.Node, d.ID, 10*time.Minute); !valid || cached.Status != "pass" {
				want = selectGroup
			}
		}
		if p.Now == want {
			continue
		}
		if _, e = a.smartAPI(ctx, "PUT", "/proxies/"+url.PathEscape(group), map[string]string{"name": want}); e != nil {
			return fmt.Errorf("%s 节点暂时未切换成功", d.Name)
		}
	}
	return nil
}
func (a *App) restoreServiceSelections() error {
	s, e := a.servicesSnapshot()
	if e != nil {
		return e
	}
	c, e := a.config()
	if e != nil || !serviceProvisioned(c) {
		return e
	}
	return a.setServiceSelections(s)
}

func (a *App) applyServices(s ServicesStore) error {
	if exists(a.servicesJournalPath()) || exists(a.smartProvisionPath()) || exists(filepath.Join(a.store, "pending.json")) || exists(filepath.Join(a.store, "profiles-pending.json")) {
		return errors.New("前次修改尚未恢复，请稍后重试")
	}
	before, e := os.ReadFile(a.servicesPath())
	beforeExists := e == nil
	if e != nil && !os.IsNotExist(e) {
		return errors.New("无法备份服务分流设置")
	}
	original, e := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
	if e != nil {
		return e
	}
	candidate, e := compileServiceConfig(original, s)
	if e != nil {
		return e
	}
	var old, new map[string]any
	_ = yaml.Unmarshal(original, &old)
	_ = yaml.Unmarshal(candidate, &new)
	changed := !reflect.DeepEqual(old, new)
	if changed {
		staged := filepath.Join(a.dir, "mobile-services-candidate.yaml")
		if e = atomicWrite(staged, candidate); e != nil {
			return errors.New("服务分流配置写入失败，现有网络保留")
		}
		defer os.Remove(staged)
		if e = a.validate(staged); e != nil {
			return e
		}
	}
	selected, e := a.captureServiceSelectors()
	if e != nil {
		return e
	}
	rules, e := a.rules()
	if e != nil {
		return e
	}
	wantOn := rules.Total > 0
	if d := a.deviceSnapshot(); d != nil {
		wantOn = d.Enabled
	}
	if a.recoveryDegraded() {
		wantOn = false
	}
	j := servicesJournal{Version: 1, Original: original, OriginalHash: smartHash(original), Before: before, BeforeExists: beforeExists, On: wantOn, ConfigChanged: changed, Selected: selected}
	jb, _ := json.Marshal(j)
	if e = atomicWrite(a.servicesJournalPath(), jb); e != nil {
		return errors.New("服务分流备份失败，未修改网络")
	}
	rollback := func(cause error) error {
		if re := a.restoreServicesProvision(j); re != nil {
			return errors.New("服务分流未完成，已保留恢复记录；请查看状态")
		}
		return fmt.Errorf("服务分流未完成，已恢复原配置。%s", cause)
	}
	if changed {
		if e = atomicWrite(filepath.Join(a.dir, "config.yaml"), candidate); e != nil {
			return rollback(e)
		}
		if e = a.restart(wantOn); e != nil {
			return rollback(e)
		}
		if e = a.restoreAvailableSelectors(selected); e != nil {
			return rollback(e)
		}
	}
	if e = a.setServiceSelections(s); e != nil {
		return rollback(e)
	}
	s.Revision++
	if e = a.writeServices(s); e != nil {
		return rollback(errors.New("分流设置保存失败"))
	}
	if e = os.Remove(a.servicesJournalPath()); e != nil {
		return rollback(e)
	}
	return nil
}
func (a *App) restoreServicesProvision(j servicesJournal) error {
	if j.Version != 1 || len(j.Original) == 0 || len(j.Original) > maxConfig || smartHash(j.Original) != j.OriginalHash {
		return errors.New("服务分流恢复记录无效，未覆盖配置")
	}
	if j.BeforeExists {
		var before ServicesStore
		if json.Unmarshal(j.Before, &before) != nil || validateServicesStore(&before) != nil {
			return errors.New("原服务分流设置无效，未覆盖配置")
		}
	}
	if j.ConfigChanged {
		staged := filepath.Join(a.dir, "mobile-services-recovery.yaml")
		if e := atomicWrite(staged, j.Original); e != nil {
			return e
		}
		defer os.Remove(staged)
		if e := a.validate(staged); e != nil {
			return e
		}
		if e := atomicWrite(filepath.Join(a.dir, "config.yaml"), j.Original); e != nil {
			return e
		}
		if e := a.restart(j.On); e != nil {
			return e
		}
	}
	if e := a.restoreAvailableSelectors(j.Selected); e != nil {
		return e
	}
	if j.BeforeExists {
		if e := atomicWrite(a.servicesPath(), j.Before); e != nil {
			return e
		}
	} else {
		if e := os.Remove(a.servicesPath()); e != nil && !os.IsNotExist(e) {
			return e
		}
	}
	if e := os.Remove(a.servicesJournalPath()); e != nil && !os.IsNotExist(e) {
		return e
	}
	return nil
}
func (a *App) recoverServicesProvision() error {
	b, e := os.ReadFile(a.servicesJournalPath())
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	var j servicesJournal
	if len(b) > 4*maxConfig || json.Unmarshal(b, &j) != nil {
		return errors.New("服务分流恢复记录无法读取")
	}
	return a.restoreServicesProvision(j)
}

func (a *App) performServices(r Request) (string, any, error, bool) {
	if r.Action != "service-save" && r.Action != "service-smart-check" {
		return "", nil, nil, false
	}
	if r.Service == nil {
		return "", nil, errors.New("请选择服务和分流方式"), true
	}
	d, ok := serviceDefinition(r.Service.ID)
	if !ok {
		return "", nil, errors.New("这个服务不存在"), true
	}
	s, e := a.servicesSnapshot()
	if e != nil {
		return "", nil, e, true
	}
	if r.Action == "service-smart-check" {
		message, e := a.runServiceSmart(d.ID, true)
		return message, a.servicesView(), e, true
	}
	choice := ServiceChoice{Mode: r.Service.Mode, Node: strings.TrimSpace(r.Service.Node)}
	if choice.Mode == "default" {
		choice.Node = ""
	}
	if choice.Mode == "smart" {
		choice.Node = ""
		if old := s.Choices[d.ID]; old.Mode == "smart" {
			choice.Node = old.Node
		}
	}
	s.Choices[d.ID] = choice
	if e = validateServicesStore(&s); e != nil {
		return "", nil, e, true
	}
	if choice.Mode == "manual" {
		c, ce := a.config()
		if ce != nil || !containsString(serviceLeafNames(c), choice.Node) {
			return "", nil, errors.New("这个节点已不在当前订阅，请刷新后重选"), true
		}
	}
	if e = a.applyServices(s); e != nil {
		return "", a.servicesView(), e, true
	}
	message := d.Name + " 分流已保存"
	if choice.Mode == "smart" {
		message = "已开启 " + d.Name + " 智能选择；后台检查通过后切换"
	}
	return message, a.servicesView(), nil, true
}

// Called at boot/update before the engine is started. It reuses the selected
// saved rule profile rather than any rules supplied by the airport.
func (a *App) savedRulesAndServices(body []byte) ([]byte, error) {
	p := a.profileSnapshot()
	var e error
	if p != nil && profileAutoApply(p) {
		i := ruleIndex(p, p.ActiveRule)
		if i < 0 {
			return nil, errors.New("当前规则方案不存在")
		}
		body, e = applyRulesToConfig(body, p.Rules[i].Rules)
		if e != nil {
			return nil, e
		}
	}
	s, e := a.servicesSnapshot()
	if e != nil {
		return nil, e
	}
	if !exists(a.servicesPath()) {
		var c map[string]any
		_ = yaml.Unmarshal(body, &c)
		if !serviceProvisioned(c) {
			return body, nil
		}
	}
	return compileServiceConfig(body, s)
}

func (a *App) reloadServiceState() error {
	if _, e := a.servicesSnapshot(); e != nil {
		return e
	}
	return a.reloadPlatforms()
}

func (a *App) finishServiceSmart(s ServicesStore, id, message string) (string, error) {
	s.LastChecked[id] = time.Now().Format(time.RFC3339)
	s.Messages[id] = message
	s.Revision++
	if e := a.writeServices(s); e != nil {
		return message + "；本轮结果暂未保存", nil
	}
	return message, nil
}

func (a *App) runServiceSmart(id string, force bool) (string, error) {
	return a.runServiceSmartContext(context.Background(), id, force)
}

func (a *App) runServiceSmartContext(parent context.Context, id string, force bool) (string, error) {
	d, ok := serviceDefinition(id)
	if !ok {
		return "", errors.New("服务不存在")
	}
	s, e := a.servicesSnapshot()
	if e != nil {
		return "", e
	}
	choice := s.Choices[id]
	if choice.Mode != "smart" {
		return "", errors.New("请先开启这个服务的智能选择")
	}
	a.initSmart()
	m := a.smart
	if !m.probeMu.TryLock() {
		return "", errors.New("节点检查正在进行，请稍后重试")
	}
	defer m.probeMu.Unlock()
	a.initPlatforms()
	pm := a.platforms
	pm.mu.Lock()
	pm.running = true
	pm.message = "正在检查 " + d.Name + " 候选节点"
	pm.mu.Unlock()
	defer func() { pm.mu.Lock(); pm.running = false; pm.mu.Unlock() }()
	if e = a.ensureSmartInfrastructure(); e != nil {
		return "", e
	}
	ctx, cancel := context.WithTimeout(parent, smartScanBudget)
	defer cancel()
	ps, e := a.smartProxies(ctx)
	if e != nil {
		return "", e
	}
	m.mu.Lock()
	settings := copySmartSaved(m.saved)
	m.mu.Unlock()
	candidates := smartCandidates(ps, settings)
	// A rotating bounded subset keeps an unhealthy airport from monopolising
	// the router. Cached passes are reusable only for ten minutes.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Name == choice.Node {
			return true
		}
		if candidates[j].Name == choice.Node {
			return false
		}
		di, dj := candidates[i].Delay, candidates[j].Delay
		if di <= 0 {
			di = 1 << 30
		}
		if dj <= 0 {
			dj = 1 << 30
		}
		return di < dj
	})
	eligible := []SmartCandidate{}
	for _, c := range candidates {
		if c.Status != "excluded" {
			eligible = append(eligible, c)
		}
	}
	start := 0
	if len(eligible) > 0 && eligible[0].Name == choice.Node {
		start = 1
	}
	if n := len(eligible) - start; n > 1 {
		offset := s.Cursor[id] % n
		tail := append([]SmartCandidate{}, eligible[start:]...)
		copy(eligible[start:], append(tail[offset:], tail[:offset]...))
	}
	results := []PlatformResult{}
	checked := 0
	progress := 0
	// Compare all still-fresh cached candidates, including those outside this
	// round's probe budget; rotate the uncached part on subsequent rounds.
	if !force {
		for _, c := range eligible {
			if cached, valid := a.cachedPlatform(c.Name, id, 10*time.Minute); valid {
				results = append(results, cached)
			}
		}
	}
	for _, c := range eligible {
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < 8*time.Second {
			break
		}
		if checked >= 4 {
			break
		}
		cached, valid := a.cachedPlatform(c.Name, id, 10*time.Minute)
		if !force && valid {
			_ = cached
			continue
		}
		if c.Name != choice.Node || start == 0 {
			progress++
		}
		if !containsString(ps[smartGroup].All, c.Name) {
			continue
		}
		if _, e = a.smartAPI(ctx, "PUT", "/proxies/"+url.PathEscape(smartGroup), map[string]string{"name": c.Name}); e != nil {
			continue
		}
		nodeCtx, stop := context.WithTimeout(ctx, 7*time.Second)
		result := a.probePlatform(nodeCtx, c.Name, id)
		stop()
		a.recordPlatform(result)
		results = append(results, result)
		checked++
	}
	s.Cursor[id] = (s.Cursor[id] + progress) % max(1, len(eligible)-start)
	if ctx.Err() != nil {
		return "后台检查已暂停，保留当前节点", ctx.Err()
	}
	best := PlatformResult{}
	current := PlatformResult{}
	for _, r := range results {
		if r.Node == choice.Node {
			current = r
		}
		if r.Status == "pass" && (best.Node == "" || r.Delay < best.Delay) {
			best = r
		}
	}
	if best.Node == "" {
		return a.finishServiceSmart(s, id, d.Name+" 暂无平台检查通过的候选，保留当前选择")
	}
	if best.Node == choice.Node {
		return a.finishServiceSmart(s, id, d.Name+" 当前节点检查通过，已保持")
	}
	last, _ := time.Parse(time.RFC3339, s.LastSwitch[id])
	if current.Status == "pass" {
		threshold := int64(math.Max(25, float64(current.Delay)*.2))
		if current.Delay-best.Delay < threshold || !last.IsZero() && time.Since(last) < 5*time.Minute {
			return a.finishServiceSmart(s, id, d.Name+" 当前节点可用，暂不为小幅延迟变化切换")
		}
	}
	// Apply only the dedicated service selector, never the household selector.
	if !containsString(ps[serviceGroup(id)].All, best.Node) {
		return "", errors.New("服务分组尚未包含候选节点，请重新保存分流设置")
	}
	fresh, fe := a.smartProxies(ctx)
	if fe != nil || fresh[serviceGroup(id)].Now != ps[serviceGroup(id)].Now {
		return a.finishServiceSmart(s, id, "检查期间服务节点已变化，本轮保留当前选择")
	}
	if _, e = a.smartAPI(ctx, "PUT", "/proxies/"+url.PathEscape(serviceGroup(id)), map[string]string{"name": best.Node}); e != nil {
		return "", e
	}
	choice.Node = best.Node
	s.Choices[id] = choice
	s.LastSwitch[id] = time.Now().Format(time.RFC3339)
	s.LastChecked[id] = time.Now().Format(time.RFC3339)
	s.Messages[id] = d.Name + " 已选择 " + best.Node + "，网页检查通过"
	s.Revision++
	if e = a.writeServices(s); e != nil {
		old := selectGroup
		if containsString(ps[serviceGroup(id)].All, ps[serviceGroup(id)].Now) {
			old = ps[serviceGroup(id)].Now
		}
		_, _ = a.smartAPI(context.Background(), "PUT", "/proxies/"+url.PathEscape(serviceGroup(id)), map[string]string{"name": old})
		return "", errors.New("智能选择未保存，已尝试恢复原服务节点")
	}
	return d.Name + " 已选择 " + best.Node + "，网页检查通过", nil
}

// Foreground switches and backup restores take priority over background probes.
func (a *App) waitServiceBackground() {
	a.serviceCancelMu.Lock()
	cancel, done := a.serviceCancel, a.serviceDone
	a.serviceCancelMu.Unlock()
	if cancel == nil || done == nil {
		return
	}
	cancel()
	timer := time.NewTimer(1500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

func (a *App) serviceBackgroundTick(id string) bool {
	if a.recoveryDegraded() || a.transactionPending() {
		return false
	}
	select {
	case a.gate <- struct{}{}:
	default:
		return false
	}
	lock := acquireManagerLock
	if a.recovery != nil && a.recovery.lockHook != nil {
		lock = a.recovery.lockHook
	}
	release, err := lock()
	if err != nil {
		<-a.gate
		return false
	}
	if a.recoveryDegraded() || a.transactionPending() {
		release()
		<-a.gate
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	a.serviceCancelMu.Lock()
	a.serviceCancel = cancel
	a.serviceDone = done
	a.serviceCancelMu.Unlock()
	defer func() {
		cancel()
		release()
		<-a.gate
		a.serviceCancelMu.Lock()
		a.serviceCancel = nil
		a.serviceDone = nil
		a.serviceCancelMu.Unlock()
		close(done)
	}()
	_, _ = a.runServiceSmartContext(ctx, id, false)
	return true
}

func (a *App) serviceMonitor() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	next := map[string]time.Time{}
	for range ticker.C {
		if a.recoveryDegraded() || a.transactionPending() {
			continue
		}
		s, err := a.servicesSnapshot()
		if err != nil {
			continue
		}
		for _, d := range serviceDefinitions {
			if s.Choices[d.ID].Mode != "smart" || time.Now().Before(next[d.ID]) {
				continue
			}
			if a.serviceBackgroundTick(d.ID) {
				next[d.ID] = time.Now().Add(5 * time.Minute)
				break
			}
		}
	}
}
