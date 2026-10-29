package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestServiceRulesPreserveDirectExceptionsAndRemainIdempotent(t *testing.T) {
	body := strings.Replace(original, "  - MATCH,节点选择", "  - DOMAIN-SUFFIX,steamcontent.com,DIRECT\n  - DOMAIN,keep.openai.com,DIRECT\n  - DOMAIN-SUFFIX,openai.com,节点选择\n  - GEOIP,CN,DIRECT\n  - MATCH,节点选择", 1)
	s := defaultServicesStore()
	s.Choices["chatgpt"] = ServiceChoice{Mode: "manual", Node: "old"}
	s.Choices["tiktok"] = ServiceChoice{Mode: "manual", Node: "old"}
	got, e := compileServiceConfig([]byte(body), s)
	if e != nil {
		t.Fatal(e)
	}
	again, e := compileServiceConfig(got, s)
	if e != nil || string(again) != string(got) {
		t.Fatal("service compilation is not idempotent", e)
	}
	var before, after map[string]any
	_ = yaml.Unmarshal([]byte(body), &before)
	_ = yaml.Unmarshal(got, &after)
	for k, v := range before {
		if k != "rules" && k != "proxy-groups" && !reflect.DeepEqual(v, after[k]) {
			t.Fatal("service changed router setting", k)
		}
	}
	rules, _ := configRules(got)
	direct, proxy, match := -1, -1, -1
	for i, r := range rules {
		if r == "DOMAIN,keep.openai.com,DIRECT" {
			direct = i
		}
		if r == "DOMAIN-SUFFIX,openai.com,"+serviceGroup("chatgpt") {
			proxy = i
		}
		if r == "MATCH,节点选择" {
			match = i
		}
		if strings.Contains(r, "pstatp.com,") || strings.Contains(r, "ibytedtos.com,") {
			t.Fatal("shared domestic ByteDance domains captured", r)
		}
	}
	if direct < 0 || proxy < direct || match != len(rules)-1 {
		t.Fatal("DIRECT/MATCH priority changed", rules)
	}
	s.Choices["chatgpt"] = ServiceChoice{Mode: "default"}
	got, e = compileServiceConfig(got, s)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(got), "DOMAIN-SUFFIX,openai.com,"+serviceGroup("chatgpt")) {
		t.Fatal("returning to default retained generated service rule")
	}
	bad := strings.Replace(body, "  - {name: 故障转移, type: fallback, proxies: [old]}", "  - {name: '服务 · ChatGPT', type: select, proxies: [old]}\n  - {name: 故障转移, type: fallback, proxies: [old]}", 1)
	if _, e = compileServiceConfig([]byte(bad), s); e == nil {
		t.Fatal("foreign group overwritten")
	}
}

type serviceHarness struct {
	proxies   map[string]smartProxy
	puts      []string
	restarts  int
	failGroup string
	failOnce  bool
}

func attachServiceHarness(t *testing.T, a *App) *serviceHarness {
	t.Helper()
	a.initSmart()
	h := &serviceHarness{}
	refresh := func() {
		c, e := a.config()
		if e != nil {
			t.Fatal(e)
		}
		h.proxies = map[string]smartProxy{}
		for _, n := range serviceLeafNames(c) {
			h.proxies[n] = smartProxy{Type: "Shadowsocks"}
		}
		for _, item := range c["proxy-groups"].([]any) {
			g := item.(map[string]any)
			name := g["name"].(string)
			p := smartProxy{Type: "Selector"}
			if g["type"] != "select" {
				p.Type = "URLTest"
			}
			for _, member := range g["proxies"].([]any) {
				p.All = append(p.All, member.(string))
			}
			if len(p.All) > 0 {
				p.Now = p.All[0]
			}
			h.proxies[name] = p
		}
	}
	refresh()
	a.smart.apiHook = func(ctx context.Context, method, path string, body any) ([]byte, error) {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		if method == "GET" && path == "/proxies" {
			return json.Marshal(map[string]any{"proxies": h.proxies})
		}
		if method == "PUT" && strings.HasPrefix(path, "/proxies/") {
			group, _ := url.PathUnescape(strings.TrimPrefix(path, "/proxies/"))
			node := body.(map[string]string)["name"]
			p, ok := h.proxies[group]
			if !ok || !containsString(p.All, node) {
				return nil, errors.New("missing test selector/member")
			}
			if h.failOnce && group == h.failGroup {
				h.failOnce = false
				return nil, errors.New("simulated selector failure")
			}
			p.Now = node
			h.proxies[group] = p
			h.puts = append(h.puts, group+"="+node)
			return []byte(`{}`), nil
		}
		return nil, errors.New("unexpected API call")
	}
	a.restartHook = func(bool) error { h.restarts++; refresh(); return nil }
	a.smart.provisionHook = func() error { return nil }
	return h
}

func TestServiceTransactionFastSelectRollbackAndMissingSubscriptionNode(t *testing.T) {
	a := mockTransactionApp(t)
	merged, _, e := mergeNodes([]byte(original), []byte(profileNodes))
	if e != nil {
		t.Fatal(e)
	}
	_ = os.WriteFile(filepath.Join(a.dir, "config.yaml"), merged, 0600)
	h := attachServiceHarness(t, a)
	previous := []byte("private-original-subscription-restore")
	_ = os.WriteFile(filepath.Join(a.store, "previous.yaml"), previous, 0600)
	state := a.snapshot()
	request := Request{Action: "service-save", Service: &ServiceEdit{ID: "chatgpt", Mode: "manual", Node: "TW台湾 0.5x"}}
	if _, _, e, handled := a.performServices(request); e != nil || !handled {
		t.Fatal(e)
	}
	if h.restarts != 1 || h.proxies[serviceGroup("chatgpt")].Now != "TW台湾 0.5x" {
		t.Fatal("provisioning did not apply dedicated service", h)
	}
	request.Service.Node = "SG新加坡 1x"
	if _, _, e, _ = a.performServices(request); e != nil {
		t.Fatal(e)
	}
	if h.restarts != 1 {
		t.Fatal("ordinary service switch restarted the proxy")
	}
	if h.proxies[selectGroup].Now != autoGroup {
		t.Fatal("service switch changed household selector")
	}
	h.failGroup = serviceGroup("chatgpt")
	h.failOnce = true
	request.Service.Node = "TW台湾 0.5x"
	if _, _, e, _ = a.performServices(request); e == nil {
		t.Fatal("failed API selection reported success")
	}
	s, _ := a.servicesSnapshot()
	if s.Choices["chatgpt"].Node != "SG新加坡 1x" || h.proxies[serviceGroup("chatgpt")].Now != "SG新加坡 1x" || exists(a.servicesJournalPath()) {
		t.Fatal("service selection rollback failed", s)
	}
	got, _ := os.ReadFile(filepath.Join(a.store, "previous.yaml"))
	if string(got) != string(previous) || !reflect.DeepEqual(state, a.snapshot()) {
		t.Fatal("service switch overwrote subscription restore/password")
	}
	curr, _ := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
	replacement := []byte("proxies:\n - {name: '🇹🇼台湾新 O’Brien x1', type: ss, server: new.example, port: 443}\n")
	newBody, _, e := mergeNodes(curr, replacement)
	if e != nil {
		t.Fatal(e)
	}
	newBody, e = compileServiceConfig(newBody, s)
	if e != nil {
		t.Fatal(e)
	}
	_ = os.WriteFile(filepath.Join(a.dir, "config.yaml"), newBody, 0600)
	_ = a.restartHook(true)
	if e = a.restoreServiceSelections(); e != nil {
		t.Fatal(e)
	}
	if h.proxies[serviceGroup("chatgpt")].Now != selectGroup {
		t.Fatal("missing manual node did not fall back")
	}
	s, _ = a.servicesSnapshot()
	if s.Choices["chatgpt"].Node != "SG新加坡 1x" {
		t.Fatal("subscription swap erased user's preference")
	}
	view := a.servicesView()
	if !view.Items[0].Missing {
		t.Fatal("missing node fallback hidden from user")
	}
}

func TestSavedRuleAutoSettingAndServicesSurviveSubscriptionReplacement(t *testing.T) {
	a := profileTestApp(t)
	_ = attachServiceHarness(t, a)
	s := defaultServicesStore()
	s.Choices["youtube"] = ServiceChoice{Mode: "manual", Node: "old"}
	if e := a.writeServices(s); e != nil {
		t.Fatal(e)
	}
	profileAction(t, a, Request{Action: "rule-switch", ID: "common"})
	if !a.rulesProfilesView().(RuleProfilesView).AutoApply {
		t.Fatal("rule auto migration default was off")
	}
	got, _ := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
	if !strings.Contains(string(got), "DOMAIN-SUFFIX,youtube.com,"+serviceGroup("youtube")) {
		t.Fatal("profile switch discarded service rule")
	}
	value := false
	profileAction(t, a, Request{Action: "rule-auto", RuleAuto: &value})
	if profileAutoApply(a.profileSnapshot()) {
		t.Fatal("rule auto off was not persisted")
	}
	value = true
	profileAction(t, a, Request{Action: "rule-auto", RuleAuto: &value})
	if !profileAutoApply(a.profileSnapshot()) {
		t.Fatal("rule auto on was not persisted")
	}
	p := a.profileSnapshot()
	if len(p.Rules[ruleIndex(p, "common")].Rules) != 220 {
		t.Fatal("service routing edited saved common rule base")
	}
	candidate, e := a.savedRulesAndServices(got)
	if e != nil {
		t.Fatal(e)
	}
	again, e := a.savedRulesAndServices(candidate)
	if e != nil || string(candidate) != string(again) {
		t.Fatal("boot/config rule reconciliation not idempotent", e)
	}
}

func TestServiceSmartUsesOnlyPlatformPassedEligibleNodesAndNeverChangesDefault(t *testing.T) {
	a := mockTransactionApp(t)
	nodes := []byte("proxies:\n - {name: 'TW台湾 x1', type: ss, server: a.example, port: 443}\n - {name: 'SG新加坡 x0.8', type: ss, server: b.example, port: 443}\n - {name: 'HK香港 x2', type: ss, server: c.example, port: 443}\n - {name: '香港无倍率', type: ss, server: d.example, port: 443}\n")
	body, _, _ := mergeNodes([]byte(original), nodes)
	s := defaultServicesStore()
	s.Choices["chatgpt"] = ServiceChoice{Mode: "smart"}
	body, _ = compileServiceConfig(body, s)
	body, _, _ = addSmartProbeConfig(body)
	_ = os.WriteFile(filepath.Join(a.dir, "config.yaml"), body, 0600)
	if e := a.writeServices(s); e != nil {
		t.Fatal(e)
	}
	h := attachServiceHarness(t, a)
	a.initPlatforms()
	probed := []string{}
	a.platforms.probeHook = func(ctx context.Context, node, id string) PlatformResult {
		probed = append(probed, node)
		status, delay := "pass", int64(80)
		if strings.HasPrefix(node, "TW") {
			status = "unknown"
			delay = 10
		}
		return PlatformResult{Node: node, Platform: id, Status: status, Delay: delay, Checked: time.Now().Format(time.RFC3339), Reason: "test"}
	}
	if _, e := a.runServiceSmart("chatgpt", true); e != nil {
		t.Fatal(e)
	}
	if h.proxies[serviceGroup("chatgpt")].Now != "SG新加坡 x0.8" {
		t.Fatal("did not select only platform pass", h.proxies)
	}
	if len(probed) != 2 {
		t.Fatal("high/unknown multiplier candidate probed", probed)
	}
	for _, put := range h.puts {
		if strings.HasPrefix(put, selectGroup+"=") {
			t.Fatal("platform checking changed wholehouse node", h.puts)
		}
	}
	a.smart.probeMu.Lock()
	if _, e := a.runServiceSmart("chatgpt", true); e == nil {
		t.Fatal("simultaneous scan acquired shared selector")
	}
	a.smart.probeMu.Unlock()
}

func TestForegroundActionCancelsBackgroundServiceProbe(t *testing.T) {
	a := mockTransactionApp(t)
	a.initRecovery()
	a.recovery.lockHook = func() (func(), error) { return func() {}, nil }
	nodes := []byte("proxies:\n - {name: 'TW台湾 x1', type: ss, server: a.example, port: 443}\n")
	body, _, _ := mergeNodes([]byte(original), nodes)
	s := defaultServicesStore()
	s.Choices["chatgpt"] = ServiceChoice{Mode: "smart"}
	body, _ = compileServiceConfig(body, s)
	body, _, _ = addSmartProbeConfig(body)
	os.WriteFile(filepath.Join(a.dir, "config.yaml"), body, 0600)
	a.writeServices(s)
	attachServiceHarness(t, a)
	a.initPlatforms()
	started := make(chan struct{})
	a.platforms.probeHook = func(ctx context.Context, node, id string) PlatformResult {
		close(started)
		<-ctx.Done()
		return PlatformResult{Node: node, Platform: id, Status: "unknown"}
	}
	finished := make(chan struct{})
	go func() { a.serviceBackgroundTick("chatgpt"); close(finished) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("background did not start")
	}
	a.waitServiceBackground()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("foreground remained blocked by probe")
	}
	select {
	case a.gate <- struct{}{}:
		<-a.gate
	default:
		t.Fatal("cancelled probe retained operation gate")
	}
}
