package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed common-rules.yaml
var commonRules []byte

// Credentials and cached node files are private router files. Views below never
// expose the raw subscription URL, the node credentials, or executable config.
type SubscriptionProfile struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Updated   string `json:"updated,omitempty"`
	Checked   string `json:"checked,omitempty"`
	NodeCount int    `json:"nodeCount"`
	Quota     *Quota `json:"quota,omitempty"`
	Error     string `json:"error,omitempty"`
}
type RuleProfile struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Rules   []string `json:"rules"`
	Builtin bool     `json:"builtin"`
	Updated string   `json:"updated"`
}
type ProfileStore struct {
	Version       int                   `json:"version"`
	Revision      uint64                `json:"revision"`
	ActiveSub     string                `json:"activeSub"`
	ActiveRule    string                `json:"activeRule"`
	Subscriptions []SubscriptionProfile `json:"subscriptions"`
	Rules         []RuleProfile         `json:"rules"`
	// Nil migrates v3 records to the safe default: airport updates retain the
	// user's selected local rule profile, never rules from a subscription.
	AutoApply *bool `json:"autoApply,omitempty"`
}
type SubscriptionView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Active    bool   `json:"active"`
	Cached    bool   `json:"cached"`
	Updated   string `json:"updated"`
	Checked   string `json:"checked"`
	NodeCount int    `json:"nodeCount"`
	Quota     *Quota `json:"quota,omitempty"`
	Error     string `json:"error,omitempty"`
	Stale     bool   `json:"stale"`
}
type ProfilesView struct {
	Version      int                `json:"version"`
	ActiveID     string             `json:"activeId"`
	ActiveRuleID string             `json:"activeRuleId"`
	Items        []SubscriptionView `json:"items"`
}
type RuleProfileView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Count   int    `json:"count"`
	Active  bool   `json:"active"`
	Builtin bool   `json:"builtin"`
	Updated string `json:"updated"`
	Content string `json:"content"`
}
type RuleProfilesView struct {
	ActiveID  string            `json:"activeId"`
	Items     []RuleProfileView `json:"items"`
	AutoApply bool              `json:"autoApply"`
}

func profileAutoApply(p *ProfileStore) bool { return p != nil && (p.AutoApply == nil || *p.AutoApply) }

type profileJournal struct {
	Before        ProfileStore `json:"before"`
	After         ProfileStore `json:"after"`
	Previous      []byte       `json:"previous,omitempty"`
	CandidateHash string       `json:"candidateHash"`
	StateURL      string       `json:"stateUrl"`
	StateUpdated  string       `json:"stateUpdated"`
}

var profileIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
var profileCachePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}-[a-f0-9]{16}\.yaml$`)

func cloneProfiles(p *ProfileStore) *ProfileStore {
	if p == nil {
		return nil
	}
	b, _ := json.Marshal(p)
	var result ProfileStore
	_ = json.Unmarshal(b, &result)
	return &result
}
func (a *App) profileSnapshot() *ProfileStore {
	a.profileMu.RLock()
	defer a.profileMu.RUnlock()
	return cloneProfiles(a.profiles)
}
func (a *App) setProfiles(p *ProfileStore) {
	a.profileMu.Lock()
	a.profiles = cloneProfiles(p)
	a.profileMu.Unlock()
}
func (a *App) profilePath() string { return filepath.Join(a.store, "profiles.json") }
func (a *App) profileCache(id string) string {
	if !profileIDPattern.MatchString(id) {
		return ""
	}
	p := a.profileSnapshot()
	if p == nil {
		return ""
	}
	i := subIndex(p, id)
	if i < 0 {
		return ""
	}
	return a.profileCacheFor(p.Subscriptions[i])
}
func (a *App) profileCacheFor(p SubscriptionProfile) string {
	if !profileIDPattern.MatchString(p.ID) {
		return ""
	}
	// A failed edit of an active URL must not overwrite the old URL's cache.
	return filepath.Join(a.store, "subscription-cache", p.ID+"-"+profileHash([]byte(p.URL))[:16]+".yaml")
}
func (a *App) writeProfiles(p *ProfileStore) error {
	b, e := json.Marshal(p)
	if e != nil {
		return e
	}
	return atomicWrite(a.profilePath(), b)
}
func (a *App) saveProfiles(p *ProfileStore) error {
	p.Revision++
	if e := a.writeProfiles(p); e != nil {
		return errors.New("保存失败，请检查路由器存储空间；当前网络未修改")
	}
	a.setProfiles(p)
	a.cleanupProfileCaches()
	return nil
}

// URL edits use different cache files so failed changes retain working nodes.
// Keep only the current profiles and the restore snapshot after a commit; never
// delete a cache while a profile/core transaction is pending.
func (a *App) cleanupProfileCaches() {
	if exists(filepath.Join(a.store, "profiles-pending.json")) {
		return
	}
	keep := map[string]bool{}
	remember := func(p *ProfileStore) {
		if p == nil {
			return
		}
		for _, sub := range p.Subscriptions {
			if path := a.profileCacheFor(sub); path != "" {
				keep[filepath.Base(path)] = true
			}
		}
	}
	remember(a.profileSnapshot())
	if b, e := os.ReadFile(filepath.Join(a.store, "profiles-previous.json")); e == nil {
		var previous ProfileStore
		if json.Unmarshal(b, &previous) == nil && previous.Version == 1 {
			remember(&previous)
		}
	}
	dir := filepath.Join(a.store, "subscription-cache")
	entries, e := os.ReadDir(dir)
	if e != nil {
		return
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() && profileCachePattern.MatchString(entry.Name()) && !keep[entry.Name()] {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}
func maskedSubscription(raw string) string {
	u, e := url.Parse(raw)
	if e != nil || u.Hostname() == "" {
		return ""
	}
	return u.Scheme + "://" + u.Hostname() + "/…"
}
func cleanSubscriptionURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "。")
	u, e := url.Parse(raw)
	if e != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || len(raw) > 8192 {
		return "", errors.New("请粘贴完整的 http:// 或 https:// 订阅链接")
	}
	return raw, nil
}
func profileName(raw, fallback string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = fallback
	}
	if len([]rune(raw)) > 48 || strings.ContainsAny(raw, "\x00\r\n") {
		return "", errors.New("名称请控制在 48 个字以内，不要换行")
	}
	return raw, nil
}
func subIndex(p *ProfileStore, id string) int {
	for i := range p.Subscriptions {
		if p.Subscriptions[i].ID == id {
			return i
		}
	}
	return -1
}
func ruleIndex(p *ProfileStore, id string) int {
	for i := range p.Rules {
		if p.Rules[i].ID == id {
			return i
		}
	}
	return -1
}
func configRules(body []byte) ([]string, error) {
	var c map[string]any
	if yaml.Unmarshal(body, &c) != nil {
		return nil, errors.New("当前规则无法读取")
	}
	items, ok := c["rules"].([]any)
	if !ok || len(items) == 0 {
		return nil, errors.New("当前配置没有可保存的规则")
	}
	rules := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, errors.New("规则必须是文本列表")
		}
		rules = append(rules, s)
	}
	return rules, nil
}
func (a *App) InitProfiles() error {
	if e := os.MkdirAll(filepath.Join(a.store, "subscription-cache"), 0700); e != nil {
		return e
	}
	if e := a.recoverProfiles(); e != nil {
		return e
	}
	if b, e := os.ReadFile(a.profilePath()); e == nil {
		var p ProfileStore
		if json.Unmarshal(b, &p) != nil || p.Version != 1 {
			return errors.New("订阅和规则方案记录无法读取，现有代理仍保留")
		}
		if len(p.Subscriptions) > 20 || len(p.Rules) > 30 {
			return errors.New("方案记录异常，现有代理仍保留")
		}
		ids := map[string]bool{}
		for _, s := range p.Subscriptions {
			if !profileIDPattern.MatchString(s.ID) || ids[s.ID] {
				return errors.New("订阅记录异常")
			}
			ids[s.ID] = true
		}
		if p.ActiveSub != "" && subIndex(&p, p.ActiveSub) < 0 {
			return errors.New("当前订阅记录缺失")
		}
		ids = map[string]bool{}
		for _, r := range p.Rules {
			if !profileIDPattern.MatchString(r.ID) || ids[r.ID] || len(r.Rules) == 0 || len(r.Rules) > 5000 {
				return errors.New("规则方案记录异常")
			}
			ids[r.ID] = true
		}
		if ruleIndex(&p, p.ActiveRule) < 0 {
			return errors.New("当前规则记录缺失")
		}
		a.setProfiles(&p)
		a.cleanupProfileCaches()
		return nil
	} else if !os.IsNotExist(e) {
		return e
	}
	current, e := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
	if e != nil {
		return e
	}
	rules, e := configRules(current)
	if e != nil {
		return e
	}
	common, e := configRules(commonRules)
	if e != nil {
		return e
	}
	now := time.Now().Format(time.RFC3339)
	p := &ProfileStore{Version: 1, ActiveRule: "current", Subscriptions: []SubscriptionProfile{}, Rules: []RuleProfile{
		{ID: "current", Name: "原有规则", Rules: rules, Updated: now},
		{ID: "common", Name: "我的常用规则", Rules: common, Builtin: true, Updated: now},
	}}
	if reflect.DeepEqual(rules, common) {
		p.ActiveRule = "common"
	}
	s := a.snapshot()
	if s.URL != "" {
		_, count, _ := mergeNodes(current, current)
		p.Subscriptions = append(p.Subscriptions, SubscriptionProfile{ID: "existing", Name: "当前机场", URL: s.URL, Updated: s.Updated, NodeCount: count, Quota: s.Quota})
		p.ActiveSub = "existing"
		if e = atomicWrite(a.profileCacheFor(p.Subscriptions[0]), current); e != nil {
			return e
		}
	}
	// The existing restore button must also keep working immediately after the
	// upgrade, before the first v3 switch creates a new profile-aware backup.
	if previous, readErr := os.ReadFile(filepath.Join(a.store, "previous.yaml")); readErr == nil {
		if oldRules, ruleErr := configRules(previous); ruleErr == nil {
			old := cloneProfiles(p)
			old.ActiveSub = ""
			if s.PreviousURL != "" {
				_, count, _ := mergeNodes(previous, previous)
				prior := SubscriptionProfile{ID: "legacy_previous", Name: "上一版机场", URL: s.PreviousURL, Updated: s.PreviousUpdated, NodeCount: count, Quota: s.PreviousQuota}
				if s.PreviousURL == s.URL {
					prior.ID = "existing"
					old.Subscriptions[0] = prior
				} else {
					old.Subscriptions = append(old.Subscriptions, prior)
				}
				old.ActiveSub = prior.ID
				if e = atomicWrite(a.profileCacheFor(prior), previous); e != nil {
					return e
				}
			}
			old.ActiveRule = "legacy_previous_rules"
			if reflect.DeepEqual(oldRules, common) {
				old.ActiveRule = "common"
			} else if reflect.DeepEqual(oldRules, rules) {
				old.ActiveRule = "current"
			} else {
				old.Rules = append(old.Rules, RuleProfile{ID: old.ActiveRule, Name: "上一版规则", Rules: oldRules, Updated: s.PreviousUpdated})
			}
			b, _ := json.Marshal(old)
			if e = atomicWrite(filepath.Join(a.store, "profiles-previous.json"), b); e != nil {
				return e
			}
		}
	}
	return a.saveProfiles(p)
}
func (a *App) profilesView() any {
	p := a.profileSnapshot()
	v := ProfilesView{Version: 1, Items: []SubscriptionView{}}
	if p == nil {
		return v
	}
	v.ActiveID, v.ActiveRuleID = p.ActiveSub, p.ActiveRule
	for _, s := range p.Subscriptions {
		stale := s.Error != ""
		if s.Quota != nil {
			checked, _ := time.Parse(time.RFC3339, s.Quota.Updated)
			stale = stale || checked.IsZero() || time.Since(checked) > time.Hour
		}
		v.Items = append(v.Items, SubscriptionView{s.ID, s.Name, maskedSubscription(s.URL), s.ID == p.ActiveSub, exists(a.profileCacheFor(s)), s.Updated, s.Checked, s.NodeCount, s.Quota, s.Error, stale})
	}
	return v
}
func (a *App) rulesProfilesView() any {
	p := a.profileSnapshot()
	v := RuleProfilesView{Items: []RuleProfileView{}, AutoApply: true}
	if p == nil {
		return v
	}
	v.ActiveID = p.ActiveRule
	v.AutoApply = profileAutoApply(p)
	for _, r := range p.Rules {
		b, _ := yaml.Marshal(map[string]any{"rules": r.Rules})
		v.Items = append(v.Items, RuleProfileView{r.ID, r.Name, len(r.Rules), r.ID == p.ActiveRule, r.Builtin, r.Updated, string(b)})
	}
	return v
}

// Rule imports have a deliberately narrow surface: a rules list, never scripts,
// DNS, listeners, subscription URLs or provider declarations from a full config.
func parseRuleImport(content string) ([]string, error) {
	if len(content) > 256<<10 {
		return nil, errors.New("规则文件超过 256 KB")
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, errors.New("请粘贴规则内容，或选择规则文件")
	}
	var node yaml.Node
	d := yaml.NewDecoder(strings.NewReader(content))
	if e := d.Decode(&node); e != nil {
		return nil, errors.New("规则格式无法读取，请使用 rules: 列表或逐行规则")
	}
	var extra yaml.Node
	if e := d.Decode(&extra); e != io.EOF {
		return nil, errors.New("规则只支持一个文本文件")
	}
	if len(node.Content) != 1 {
		return nil, errors.New("规则内容为空")
	}
	n := node.Content[0]
	if n.Kind == yaml.MappingNode {
		if len(n.Content) != 2 || n.Content[0].Value != "rules" {
			return nil, errors.New("这里只导入 rules 规则列表，请去掉节点、DNS、脚本等配置")
		}
		n = n.Content[1]
	}
	var rules []string
	if n.Kind == yaml.SequenceNode {
		for _, line := range n.Content {
			if line.Kind != yaml.ScalarNode || line.Tag != "!!str" {
				return nil, errors.New("每条规则必须是文本")
			}
			rules = append(rules, strings.TrimSpace(line.Value))
		}
	} else if n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				rules = append(rules, line)
			}
		}
	} else {
		return nil, errors.New("请使用 rules: 列表或逐行规则")
	}
	if len(rules) == 0 || len(rules) > 5000 {
		return nil, errors.New("规则数量需要在 1 至 5000 条之间")
	}
	return rules, nil
}
func validateProfileRules(rules []string, c map[string]any) error {
	targets := map[string]bool{"DIRECT": true, "REJECT": true, "REJECT-DROP": true, "PASS": true, "COMPATIBLE": true}
	if groups, ok := c["proxy-groups"].([]any); ok {
		for _, g := range groups {
			if m, ok := g.(map[string]any); ok {
				if s, ok := m["name"].(string); ok {
					targets[s] = true
				}
			}
		}
	}
	types := map[string]bool{"DOMAIN": true, "DOMAIN-SUFFIX": true, "DOMAIN-KEYWORD": true, "DOMAIN-REGEX": true, "IP-CIDR": true, "IP-CIDR6": true, "IP-SUFFIX": true, "SRC-IP-CIDR": true, "SRC-IP-SUFFIX": true, "GEOIP": true, "GEOSITE": true, "SRC-GEOIP": true, "SRC-ASN": true, "IP-ASN": true, "SRC-PORT": true, "DST-PORT": true, "NETWORK": true, "PROCESS-NAME": true, "PROCESS-PATH": true, "PROCESS-NAME-REGEX": true, "PROCESS-PATH-REGEX": true, "IN-TYPE": true, "IN-PORT": true, "IN-USER": true, "UID": true, "DSCP": true, "RULE-SET": true}
	for i, line := range rules {
		if len(line) > 4096 || strings.ContainsAny(line, "\n\r\x00") {
			return fmt.Errorf("第 %d 条规则过长或包含换行", i+1)
		}
		parts := strings.Split(line, ",")
		for j := range parts {
			parts[j] = strings.TrimSpace(parts[j])
		}
		if len(parts) < 2 {
			return fmt.Errorf("第 %d 条规则格式不完整", i+1)
		}
		kind := strings.ToUpper(parts[0])
		targetIndex := 2
		if kind == "MATCH" {
			if i != len(rules)-1 || len(parts) != 2 {
				return errors.New("MATCH 兜底规则必须放在最后，而且只能有一条")
			}
			targetIndex = 1
		} else {
			if !types[kind] || len(parts) < 3 || len(parts) > 4 || parts[1] == "" {
				return fmt.Errorf("第 %d 条规则不支持；请使用常规域名、IP 或端口规则", i+1)
			}
			if len(parts) == 4 && parts[3] != "no-resolve" {
				return fmt.Errorf("第 %d 条规则的附加参数不支持", i+1)
			}
			if kind == "RULE-SET" {
				providers, _ := c["rule-providers"].(map[string]any)
				if _, ok := providers[parts[1]]; !ok {
					return fmt.Errorf("第 %d 条规则引用了不存在的规则集", i+1)
				}
			}
		}
		if !targets[parts[targetIndex]] {
			return fmt.Errorf("第 %d 条规则引用了不存在的分组；建议使用 DIRECT 或 节点选择", i+1)
		}
	}
	if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(rules[len(rules)-1])), "MATCH,") {
		return errors.New("请在最后添加 MATCH,节点选择 作为兜底规则")
	}
	return nil
}
func applyRulesToConfig(body []byte, rules []string) ([]byte, error) {
	var c map[string]any
	if yaml.Unmarshal(body, &c) != nil {
		return nil, errors.New("当前配置无法读取")
	}
	if e := validateProfileRules(rules, c); e != nil {
		return nil, e
	}
	c["rules"] = rules
	return yaml.Marshal(c)
}
func (a *App) fetchProfile(raw string) ([]byte, *Quota, error) {
	if a.profileFetchHook != nil {
		return a.profileFetchHook(raw)
	}
	return downloadSubscription(raw)
}
func (a *App) cacheProfile(p *SubscriptionProfile, body []byte, quota *Quota) error {
	current, e := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
	if e != nil {
		return e
	}
	_, count, e := mergeNodes(current, body)
	if e != nil {
		return e
	}
	if e = atomicWrite(a.profileCacheFor(*p), body); e != nil {
		return errors.New("节点缓存保存失败；现有代理保留")
	}
	p.NodeCount = count
	p.Updated = time.Now().Format(time.RFC3339)
	p.Checked = p.Updated
	p.Error = ""
	// Never manufacture a provider balance from sampled device traffic.
	if quota != nil && quota.Available {
		p.Quota = quota
	} else if p.Quota == nil {
		p.Quota = quota
	} else {
		p.Error = "订阅这次未提供套餐信息，保留上次数据"
	}
	return nil
}
func profileHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func (a *App) recoverProfiles() error {
	path := filepath.Join(a.store, "profiles-pending.json")
	b, e := os.ReadFile(path)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	var j profileJournal
	if json.Unmarshal(b, &j) != nil {
		return errors.New("方案事务记录无法读取；未修改现有网络")
	}
	current, e := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
	if e != nil {
		return e
	}
	s := a.snapshot()
	committed := !exists(filepath.Join(a.store, "pending.json")) && profileHash(current) == j.CandidateHash && s.URL == j.StateURL && s.Updated == j.StateUpdated
	want := &j.Before
	if committed {
		want = &j.After
	} else if len(j.Previous) > 0 {
		if e = atomicWrite(filepath.Join(a.store, "profiles-previous.json"), j.Previous); e != nil {
			return e
		}
	} else {
		os.Remove(filepath.Join(a.store, "profiles-previous.json"))
	}
	if e = a.writeProfiles(want); e != nil {
		return e
	}
	return os.Remove(path)
}
func (a *App) applyProfileConfig(candidate []byte, next State, before, after *ProfileStore) error {
	// Regenerate service exceptions after any explicit rule/profile change.
	// This never edits the saved base rules or adopts airport networking.
	if exists(a.servicesPath()) {
		s, e := a.servicesSnapshot()
		if e != nil {
			return e
		}
		candidate, e = compileServiceConfig(candidate, s)
		if e != nil {
			return e
		}
	}
	next.Updated = time.Now().Format(time.RFC3339Nano)
	after.Revision = before.Revision + 1
	previous, _ := os.ReadFile(filepath.Join(a.store, "profiles-previous.json"))
	j := profileJournal{Before: *before, After: *after, Previous: previous, CandidateHash: profileHash(candidate), StateURL: next.URL, StateUpdated: next.Updated}
	b, _ := json.Marshal(j)
	path := filepath.Join(a.store, "profiles-pending.json")
	if e := atomicWrite(path, b); e != nil {
		return errors.New("方案备份失败，未修改网络")
	}
	old, _ := json.Marshal(before)
	if e := atomicWrite(filepath.Join(a.store, "profiles-previous.json"), old); e != nil {
		os.Remove(path)
		return errors.New("方案备份失败，未修改网络")
	}
	if e := a.writeProfiles(after); e != nil {
		_ = a.recoverProfiles()
		return errors.New("方案保存失败，未修改网络")
	}
	if e := a.applyConfig(candidate, next); e != nil {
		if !exists(filepath.Join(a.store, "pending.json")) {
			if re := a.recoverProfiles(); re != nil {
				return errors.New("网络已恢复，方案记录恢复失败；请保留文件并重新打开面板")
			}
		}
		return e
	}
	a.setProfiles(after)
	if e := os.Remove(path); e != nil {
		return errors.New("方案已应用，事务清理失败；请查看详细状态")
	}
	a.cleanupProfileCaches()
	return nil
}
func (a *App) applySubscription(p *ProfileStore, index int, body []byte) (int, error) {
	before := a.profileSnapshot()
	current, e := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
	if e != nil {
		return 0, e
	}
	merged, count, e := mergeNodes(current, body)
	if e != nil {
		return 0, e
	}
	ri := ruleIndex(p, p.ActiveRule)
	if ri < 0 {
		return 0, errors.New("当前规则方案不存在，未修改")
	}
	if profileAutoApply(p) {
		merged, e = applyRulesToConfig(merged, p.Rules[ri].Rules)
		if e != nil {
			return 0, e
		}
	}
	p.ActiveSub = p.Subscriptions[index].ID
	next := a.snapshot()
	next.URL = p.Subscriptions[index].URL
	next.Quota = p.Subscriptions[index].Quota
	e = a.applyProfileConfig(merged, next, before, p)
	return count, e
}
func (a *App) profileRestore() (string, any, error) {
	body, e := os.ReadFile(filepath.Join(a.store, "previous.yaml"))
	if e != nil {
		return "", nil, errors.New("还没有可恢复的上一版配置")
	}
	b, e := os.ReadFile(filepath.Join(a.store, "profiles-previous.json"))
	if e != nil {
		return "", nil, errors.New("没有对应的上一版方案记录，未修改现有配置")
	}
	var p ProfileStore
	if json.Unmarshal(b, &p) != nil || p.Version != 1 {
		return "", nil, errors.New("上一版方案记录无法读取")
	}
	next := a.snapshot()
	next.URL = next.PreviousURL
	next.Quota = next.PreviousQuota
	e = a.applyProfileConfig(body, next, a.profileSnapshot(), &p)
	return "已恢复上一版订阅和规则方案", nil, e
}
func (a *App) performProfiles(r Request) (string, any, error, bool) {
	supported := map[string]bool{"sub-save": true, "sub-update": true, "sub-switch": true, "sub-remove": true, "sub-quota": true, "rule-save": true, "rule-switch": true, "rule-remove": true, "rule-auto": true, "subscription": true, "restore": true, "quota": true}
	if !supported[r.Action] {
		return "", nil, nil, false
	}
	p := a.profileSnapshot()
	if p == nil {
		return "", nil, nil, false
	} // Existing tests and pre-migration callers retain their legacy behavior.
	msg, result, e := a.doProfiles(p, r)
	return msg, result, e, true
}
func (a *App) doProfiles(p *ProfileStore, r Request) (string, any, error) {
	if r.Action == "rule-auto" {
		if r.RuleAuto == nil {
			return "", nil, errors.New("请选择是否自动保留我的规则")
		}
		before := a.profileSnapshot()
		p.AutoApply = r.RuleAuto
		if !*r.RuleAuto {
			if e := a.saveProfiles(p); e != nil {
				return "", nil, e
			}
			return "已关闭自动应用；当前规则仍保留", a.rulesProfilesView(), nil
		}
		i := ruleIndex(p, p.ActiveRule)
		if i < 0 {
			return "", nil, errors.New("当前规则方案不存在")
		}
		current, e := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
		if e != nil {
			return "", nil, e
		}
		candidate, e := applyRulesToConfig(current, p.Rules[i].Rules)
		if e != nil {
			return "", nil, e
		}
		if exists(a.servicesPath()) {
			s, se := a.servicesSnapshot()
			if se != nil {
				return "", nil, se
			}
			candidate, e = compileServiceConfig(candidate, s)
			if e != nil {
				return "", nil, e
			}
		}
		var oldConfig, newConfig map[string]any
		_ = yaml.Unmarshal(current, &oldConfig)
		_ = yaml.Unmarshal(candidate, &newConfig)
		if reflect.DeepEqual(oldConfig, newConfig) {
			e = a.saveProfiles(p)
		} else {
			e = a.applyProfileConfig(candidate, a.snapshot(), before, p)
		}
		return "已开启自动应用；更新订阅和开机恢复会保留当前规则方案", a.rulesProfilesView(), e
	}
	if r.Action == "restore" {
		return a.profileRestore()
	}
	legacyApply := r.Action == "subscription"
	if legacyApply {
		r.Action = "sub-update"
		r.ID = p.ActiveSub
		if r.URL != "" {
			raw, _ := cleanSubscriptionURL(r.URL)
			i := subIndex(p, p.ActiveSub)
			if i < 0 || raw != p.Subscriptions[i].URL {
				r.Action = "sub-save"
				r.Name = ""
			}
		}
	}
	if r.Action == "quota" {
		r.Action = "sub-quota"
	}
	if strings.HasPrefix(r.Action, "sub-") {
		id := r.ID
		if id == "" && r.Action != "sub-save" {
			id = p.ActiveSub
		}
		i := subIndex(p, id)
		if r.Action == "sub-save" {
			if id != "" && i < 0 {
				return "", nil, errors.New("这个订阅已不存在，请刷新页面")
			}
			if i < 0 && len(p.Subscriptions) >= 20 {
				return "", nil, errors.New("最多保存 20 个订阅，请先移除不用的订阅")
			}
			fallback := fmt.Sprintf("机场 %d", len(p.Subscriptions)+1)
			if i >= 0 {
				fallback = p.Subscriptions[i].Name
			}
			name, e := profileName(r.Name, fallback)
			if e != nil {
				return "", nil, e
			}
			raw := r.URL
			if raw == "" && i >= 0 {
				raw = p.Subscriptions[i].URL
			}
			raw, e = cleanSubscriptionURL(raw)
			if e != nil {
				return "", nil, e
			}
			if i >= 0 && raw == p.Subscriptions[i].URL {
				p.Subscriptions[i].Name = name
				if e = a.saveProfiles(p); e != nil {
					return "", nil, e
				}
				return "订阅名称已保存", a.profilesView(), nil
			}
			if i < 0 {
				i = len(p.Subscriptions)
				p.Subscriptions = append(p.Subscriptions, SubscriptionProfile{ID: random()[:16], Name: name})
			}
			p.Subscriptions[i].Name = name
			p.Subscriptions[i].URL = raw
			p.Subscriptions[i].Quota = nil
			p.Subscriptions[i].NodeCount = 0
			p.Subscriptions[i].Updated = ""
			body, q, fetchErr := a.fetchProfile(raw)
			if fetchErr == nil {
				fetchErr = a.cacheProfile(&p.Subscriptions[i], body, q)
			}
			if p.Subscriptions[i].ID == p.ActiveSub || legacyApply {
				if fetchErr != nil {
					return "", nil, errors.New("新链接暂时无法使用，原订阅和配置已保留")
				}
				count, e := a.applySubscription(p, i, body)
				return fmt.Sprintf("订阅链接已更新，共 %d 个节点；当前规则已保留", count), a.profilesView(), e
			}
			if fetchErr != nil {
				p.Subscriptions[i].Error = "暂时无法读取订阅，请检查链接；已保存，稍后可重试"
				p.Subscriptions[i].Checked = time.Now().Format(time.RFC3339)
				os.Remove(a.profileCacheFor(p.Subscriptions[i]))
			}
			if e = a.saveProfiles(p); e != nil {
				return "", nil, e
			}
			return "订阅已保存；当前使用的机场未改变", a.profilesView(), nil
		}
		if i < 0 {
			return "", nil, errors.New("请先保存一个订阅，或刷新页面重试")
		}
		s := &p.Subscriptions[i]
		switch r.Action {
		case "sub-remove":
			if s.ID == p.ActiveSub {
				return "", nil, errors.New("请先切换到另一个机场，再移除当前订阅")
			}
			cache := a.profileCacheFor(*s)
			p.Subscriptions = append(p.Subscriptions[:i], p.Subscriptions[i+1:]...)
			if e := a.saveProfiles(p); e != nil {
				return "", nil, e
			}
			os.Remove(cache)
			return "订阅已移除，当前网络不受影响", a.profilesView(), nil
		case "sub-quota":
			e := a.refreshProfileQuota(s.ID)
			if e != nil {
				return "", a.profilesView(), e
			}
			return "套餐流量已刷新，当前节点不变", a.profilesView(), nil
		case "sub-update", "sub-switch":
			var body []byte
			var q *Quota
			var e error
			cached := false
			if r.Action == "sub-switch" {
				body, e = os.ReadFile(a.profileCacheFor(*s))
				if e == nil && len(body) > 0 {
					q = s.Quota
					cached = true
				}
			}
			if len(body) == 0 || r.Action == "sub-update" {
				body, q, e = a.fetchProfile(s.URL)
				if e != nil {
					s.Error = "订阅读取失败，已保留上次数据和当前配置"
					s.Checked = time.Now().Format(time.RFC3339)
					_ = a.saveProfiles(p)
					return "", a.profilesView(), errors.New(s.Error)
				}
			}
			if !cached {
				if e = a.cacheProfile(s, body, q); e != nil {
					return "", nil, e
				}
			}
			if r.Action == "sub-switch" || s.ID == p.ActiveSub {
				count, e := a.applySubscription(p, i, body)
				return fmt.Sprintf("已启用 %s，共 %d 个节点；当前规则已保留", s.Name, count), a.profilesView(), e
			}
			if e = a.saveProfiles(p); e != nil {
				return "", nil, e
			}
			return "订阅节点与套餐信息已更新，当前机场未切换", a.profilesView(), nil
		}
	}
	if strings.HasPrefix(r.Action, "rule-") {
		i := ruleIndex(p, r.ID)
		switch r.Action {
		case "rule-save":
			if r.ID != "" && i < 0 {
				return "", nil, errors.New("这个规则方案已不存在")
			}
			if i >= 0 && p.Rules[i].Builtin {
				return "", nil, errors.New("常用规则保留原版；请另存为一个新方案")
			}
			if i < 0 && len(p.Rules) >= 30 {
				return "", nil, errors.New("最多保存 30 个规则方案")
			}
			rules, e := parseRuleImport(r.Content)
			if e != nil {
				return "", nil, e
			}
			c, e := a.config()
			if e != nil {
				return "", nil, e
			}
			if e = validateProfileRules(rules, c); e != nil {
				return "", nil, e
			}
			name, e := profileName(r.Name, "自定义规则")
			if e != nil {
				return "", nil, e
			}
			if i < 0 {
				i = len(p.Rules)
				p.Rules = append(p.Rules, RuleProfile{ID: random()[:16]})
			}
			p.Rules[i].Name = name
			p.Rules[i].Rules = rules
			p.Rules[i].Updated = time.Now().Format(time.RFC3339)
			if p.Rules[i].ID == p.ActiveRule {
				current, e := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
				if e != nil {
					return "", nil, e
				}
				body, e := applyRulesToConfig(current, rules)
				if e != nil {
					return "", nil, e
				}
				e = a.applyProfileConfig(body, a.snapshot(), a.profileSnapshot(), p)
				return "规则已保存并应用", a.rulesProfilesView(), e
			}
			if e = a.saveProfiles(p); e != nil {
				return "", nil, e
			}
			return "规则方案已保存，点击使用即可切换", a.rulesProfilesView(), nil
		case "rule-switch":
			if i < 0 {
				return "", nil, errors.New("请先选择一个规则方案")
			}
			current, e := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
			if e != nil {
				return "", nil, e
			}
			body, e := applyRulesToConfig(current, p.Rules[i].Rules)
			if e != nil {
				return "", nil, e
			}
			p.ActiveRule = p.Rules[i].ID
			e = a.applyProfileConfig(body, a.snapshot(), a.profileSnapshot(), p)
			return "已切换为 " + p.Rules[i].Name + "，订阅和设备策略已保留", a.rulesProfilesView(), e
		case "rule-remove":
			if i < 0 {
				return "", nil, errors.New("规则方案已不存在")
			}
			if p.Rules[i].Builtin || p.Rules[i].ID == p.ActiveRule {
				return "", nil, errors.New("常用规则和当前正在使用的方案不能移除")
			}
			p.Rules = append(p.Rules[:i], p.Rules[i+1:]...)
			if e := a.saveProfiles(p); e != nil {
				return "", nil, e
			}
			return "规则方案已移除", a.rulesProfilesView(), nil
		}
	}
	return "", nil, errors.New("不支持的方案操作")
}
func (a *App) refreshProfileQuota(id string) error {
	p := a.profileSnapshot()
	if p == nil {
		return errors.New("订阅管理尚未准备好")
	}
	i := subIndex(p, id)
	if i < 0 {
		return errors.New("订阅已不存在")
	}
	s := &p.Subscriptions[i]
	_, q, e := a.fetchProfile(s.URL)
	s.Checked = time.Now().Format(time.RFC3339)
	if e != nil {
		s.Error = "暂时无法刷新套餐信息，保留上次数据"
	} else if q == nil || !q.Available {
		s.Error = "订阅未提供套餐流量信息，保留上次数据"
	} else {
		s.Quota = q
		s.Error = ""
	}
	if se := a.saveProfiles(p); se != nil {
		return se
	}
	if s.Error != "" {
		return errors.New(s.Error)
	}
	if s.ID == p.ActiveSub {
		state := a.snapshot()
		if state.URL == s.URL {
			state.Quota = q
			if e = a.save(state); e != nil {
				return errors.New("套餐信息已读取，但主界面缓存保存失败")
			}
		}
	}
	return nil
}

// Called after fetching outside the command gate. A URL change or any intervening
// profile edit invalidates the result; inactive subscriptions cannot change State.
func (a *App) saveBackgroundProfileQuota(before *ProfileStore, id string, q *Quota, fetchErr error) {
	select {
	case a.gate <- struct{}{}:
		defer func() { <-a.gate }()
	default:
		return
	}
	release, e := acquireManagerLock()
	if e != nil {
		return
	}
	defer release()
	p := a.profileSnapshot()
	if p == nil || p.Revision != before.Revision {
		return
	}
	i := subIndex(p, id)
	old := subIndex(before, id)
	if i < 0 || old < 0 || p.Subscriptions[i].URL != before.Subscriptions[old].URL {
		return
	}
	s := &p.Subscriptions[i]
	s.Checked = time.Now().Format(time.RFC3339)
	if fetchErr != nil {
		s.Error = "暂时无法刷新套餐信息，保留上次数据"
	} else if q == nil || !q.Available {
		s.Error = "订阅未提供套餐流量信息，保留上次数据"
	} else {
		s.Quota = q
		s.Error = ""
	}
	if a.saveProfiles(p) != nil {
		return
	}
	if s.ID == p.ActiveSub && s.Error == "" {
		state := a.snapshot()
		if state.URL == s.URL {
			state.Quota = q
			_ = a.save(state)
		}
	}
}

// Keep this comparison helper out of API views; useful when verifying an import
// preserves all router-local values, including secrets, DNS and interface binds.
func sameProfileConfigExceptRules(a, b []byte) bool {
	var left, right map[string]any
	if yaml.Unmarshal(a, &left) != nil || yaml.Unmarshal(b, &right) != nil {
		return false
	}
	delete(left, "rules")
	delete(right, "rules")
	x, _ := yaml.Marshal(left)
	y, _ := yaml.Marshal(right)
	return bytes.Equal(x, y)
}
