package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestSmartRegionsAndMultiplierAreConservative(t *testing.T) {
	for _, row := range []struct {
		name, region string
		rate         float64
		known        bool
	}{
		{"🇹🇼台灣04 x0.8", "台湾", .8, true},
		{"SG新加坡01|1.0倍", "新加坡", 1, true},
		{"HK02 2X", "香港", 2, true},
		{"Hong Kong 倍率: 0.5", "香港", .5, true},
		{"Singapore 1x | x2", "新加坡", 0, false},
		{"台湾01", "台湾", 0, false},
		{"🇯🇵日本04 x0.5", "", .5, true},
		{"TW04 / 0.5×", "台湾", .5, true},
		{"sg01 | x1", "新加坡", 1, true},
	} {
		rate, known := smartMultiplier(row.name)
		if rate != row.rate || known != row.known || smartRegion(row.name) != row.region {
			t.Errorf("%s => region %s rate %v known %v", row.name, smartRegion(row.name), rate, known)
		}
	}
	settings := defaultSmartSaved()
	proxies := map[string]smartProxy{selectGroup: {All: []string{"TW 台湾 x0.5", "SG 新加坡 x2", "HK 香港", "US 美国 x0.1", autoGroup}}, autoGroup: {All: []string{"TW 台湾 x0.5"}}}
	for _, name := range proxies[selectGroup].All {
		if name != autoGroup {
			proxies[name] = smartProxy{Type: "Shadowsocks"}
		}
	}
	rows := smartCandidates(proxies, settings)
	if len(rows) != 3 || rows[0].Status != "untested" || rows[1].Status != "excluded" || rows[2].Status != "excluded" {
		t.Fatalf("unexpected candidate filter: %+v", rows)
	}
	settings.Overrides["HK 香港"] = 1
	rows = smartCandidates(proxies, settings)
	if !rows[2].MultiplierKnown || !rows[2].Override || rows[2].Status != "untested" {
		t.Fatal("explicit multiplier confirmation not applied", rows[2])
	}
	settings.ExcludeHighMultiplier = false
	if rows = smartCandidates(proxies, settings); rows[1].Status == "excluded" {
		t.Fatal("filter toggle did not disable multiplier exclusion")
	}
}

func TestSmartProbeConfigIsIsolatedIdempotentAndPreservesRouter(t *testing.T) {
	b, changed, e := addSmartProbeConfig([]byte(original))
	if e != nil || !changed {
		t.Fatal(changed, e)
	}
	var before, after map[string]any
	_ = yaml.Unmarshal([]byte(original), &before)
	_ = yaml.Unmarshal(b, &after)
	for k, v := range before {
		if k != "proxy-groups" && !reflect.DeepEqual(v, after[k]) {
			t.Fatal("changed router setting", k)
		}
	}
	beforeGroups, afterGroups := before["proxy-groups"].([]any), after["proxy-groups"].([]any)
	if len(afterGroups) != len(beforeGroups)+1 {
		t.Fatal("did not add exactly one isolated group")
	}
	for i, group := range beforeGroups {
		if !reflect.DeepEqual(group, afterGroups[i]) {
			t.Fatal("modified household selector", i)
		}
	}
	listeners := after["listeners"].([]any)
	if len(listeners) != 1 || !smartListenerMatches(listeners[0].(map[string]any)) {
		t.Fatal("listener exposed or incorrectly routed", listeners)
	}
	b2, changed, e := addSmartProbeConfig(b)
	if e != nil || changed || string(b2) != string(b) {
		t.Fatal("probe config not idempotent", changed, e)
	}
	incoming := []byte("proxies:\n - {name: 'SG新加坡 x1', type: ss, server: private.example, port: 443}\n")
	merged, _, e := mergeNodes(b, incoming)
	if e != nil {
		t.Fatal(e)
	}
	_ = yaml.Unmarshal(merged, &after)
	for _, item := range after["proxy-groups"].([]any) {
		g := item.(map[string]any)
		if g["name"] == smartGroup && !reflect.DeepEqual(g["proxies"], []any{"SG新加坡 x1"}) {
			t.Fatal("subscription switch retained stale probe nodes", g)
		}
		if g["name"] == selectGroup {
			for _, node := range g["proxies"].([]any) {
				if node == smartGroup {
					t.Fatal("probe selector leaked into household choices")
				}
			}
		}
	}
	for _, bad := range []string{
		original + "listeners: [{name: unrelated, type: mixed, port: 19081, listen: 0.0.0.0}]\n",
		original + "listeners: [{name: mobile-panel-smart-check, type: mixed, port: 19081, listen: 0.0.0.0, proxy: 面板检测}]\n",
	} {
		if _, _, e := addSmartProbeConfig([]byte(bad)); e == nil {
			t.Fatal("unsafe port collision accepted")
		}
	}
}

const smartValidProviders = `{"openai":{"id":"openai","name":"OpenAI","type":"oauth","signinUrl":"https://chatgpt.com/api/auth/signin/openai"}}`

func TestSmartGPTRequiresExpectedJSONRatherThanHTTPStatusAlone(t *testing.T) {
	for _, row := range []struct {
		status         int
		ct, body, want string
	}{
		{200, "application/json; charset=utf-8", smartValidProviders, "pass"},
		{200, "text/html", "<html>generic OK</html>", "unknown"},
		{200, "application/json", `{"ok":true}`, "unknown"},
		{401, "application/json", `{"detail":"Unauthorized"}`, "unknown"},
		{403, "application/json", `{"error":{"code":"unsupported_country_region_territory"}}`, "restricted"},
		{403, "text/html", "<html>Just a moment... cf-chl-</html>", "unknown"},
		{200, "application/json", strings.ReplaceAll(smartValidProviders, "https://chatgpt.com", "https://attacker.example"), "unknown"},
		{429, "application/json", `{}`, "unknown"},
	} {
		if status, _ := classifyGPTResponse(row.status, row.ct, []byte(row.body)); status != row.want {
			t.Errorf("HTTP %d %s => %s want %s", row.status, row.body, status, row.want)
		}
	}
}

func TestSmartProbeChecksBodyAnd204ThroughItsOwnHTTPProxy(t *testing.T) {
	gptStatus, latencyStatus := 200, 204
	gptBody := smartValidProviders
	var requests []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.String())
		if r.URL.Path == "/generate_204" {
			w.WriteHeader(latencyStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(gptStatus)
		_, _ = w.Write([]byte(gptBody))
	}))
	defer s.Close()
	probe := func() SmartCandidate {
		return probeSmartHTTP(context.Background(), "SG新加坡 x1", s.URL, "http://latency.invalid/generate_204", "http://chatgpt.invalid/api/auth/providers")
	}
	if got := probe(); got.Status != "pass" || got.Delay < 1 || len(requests) != 2 {
		t.Fatal(got, requests)
	}
	gptStatus, gptBody = 403, `{"error":{"code":"unsupported_country_region_territory"}}`
	if got := probe(); got.Status != "restricted" {
		t.Fatal("country block was counted as GPT available", got)
	}
	latencyStatus = 200
	requests = nil
	if got := probe(); got.Status != "unknown" || len(requests) != 1 {
		t.Fatal("200 connectivity response bypassed strict 204 check", got, requests)
	}
}

type smartTestHarness struct {
	mu           sync.Mutex
	proxies      map[string]smartProxy
	mainChanges  []string
	probeChanges []string
	results      map[string]SmartCandidate
	stopProbe    func(context.Context)
}

func newSmartHarness(t *testing.T) (*App, *smartTestHarness) {
	t.Helper()
	a := newTestApp(t)
	_ = os.WriteFile(filepath.Join(a.dir, "config.yaml"), []byte(original), 0600)
	a.initSmart()
	names := []string{"TW台湾 x1", "SG新加坡 x0.8", "HK香港 x2", "香港未标明", "US美国 x0.5"}
	h := &smartTestHarness{proxies: map[string]smartProxy{selectGroup: {Type: "Selector", Now: names[0], All: names}, smartGroup: {Type: "Selector", Now: names[0], All: names}}, results: map[string]SmartCandidate{}}
	for _, name := range names {
		h.proxies[name] = smartProxy{Type: "Shadowsocks"}
		h.results[name] = SmartCandidate{Status: "pass", Delay: 80, Reason: "GPT 网页检查通过", Checked: time.Now().Format(time.RFC3339)}
	}
	h.results[names[0]] = SmartCandidate{Status: "pass", Delay: 150, Checked: time.Now().Format(time.RFC3339)}
	h.results[names[1]] = SmartCandidate{Status: "pass", Delay: 40, Checked: time.Now().Format(time.RFC3339)}
	a.smart.apiHook = func(ctx context.Context, method, path string, body any) ([]byte, error) {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if method == "GET" && path == "/proxies" {
			return json.Marshal(map[string]any{"proxies": h.proxies})
		}
		if method == "PUT" && strings.HasPrefix(path, "/proxies/") {
			group, _ := url.PathUnescape(strings.TrimPrefix(path, "/proxies/"))
			name := body.(map[string]string)["name"]
			p := h.proxies[group]
			p.Now = name
			h.proxies[group] = p
			if group == smartGroup {
				h.probeChanges = append(h.probeChanges, name)
			} else if group == selectGroup {
				h.mainChanges = append(h.mainChanges, name)
			} else {
				t.Error("changed unrelated group", group)
			}
			return []byte(`{}`), nil
		}
		return nil, errors.New("unexpected core call")
	}
	a.smart.probeHook = func(ctx context.Context, name string) SmartCandidate {
		if h.stopProbe != nil {
			h.stopProbe(ctx)
		}
		return h.results[name]
	}
	a.smart.provisionHook = func() error { return nil }
	a.smart.saved.Enabled = true
	return a, h
}

func TestSmartChoosesEligibleGPTNodeWithoutCyclingHouseholdGroup(t *testing.T) {
	a, h := newSmartHarness(t)
	if _, e := a.runSmartCheck(false); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(h.mainChanges, []string{"SG新加坡 x0.8"}) {
		t.Fatal("household selector changed during probes", h.mainChanges)
	}
	if len(h.probeChanges) != 2 {
		t.Fatal("unknown/high multiplier/wrong region node probed", h.probeChanges)
	}
	v := a.smartView()
	if v.Selected != "SG新加坡 x0.8" || v.Running || v.LastChecked == "" {
		t.Fatal(v)
	}
	a.pauseSmartOnManual()
	if a.smartView().Enabled {
		t.Fatal("manual selection did not pause intelligent selection")
	}
	if _, e := a.runSmartCheck(false); e == nil {
		t.Fatal("disabled intelligent selector still ran")
	}
	b := newTestApp(t)
	b.store = a.store
	b.dir = a.dir
	b.initSmart()
	if b.smartView().Enabled {
		t.Fatal("pause was not persisted across panel restart")
	}
}

func TestSmartMinimumHoldAvoidsFlappingAndManualPauseSurvivesFullStore(t *testing.T) {
	a, h := newSmartHarness(t)
	if _, e := a.runSmartCheck(false); e != nil {
		t.Fatal(e)
	}
	h.results["TW台湾 x1"] = SmartCandidate{Status: "pass", Delay: 1, Checked: time.Now().Format(time.RFC3339)}
	if _, e := a.runSmartCheck(false); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(h.mainChanges, []string{"SG新加坡 x0.8"}) {
		t.Fatal("healthy node switched inside minimum hold", h.mainChanges)
	}
	// Force the next atomic write to fail; the old enabled settings must not
	// silently restart smart selection after a successful manual node choice.
	if e := os.Mkdir(filepath.Join(a.store, "smart.json.new"), 0700); e != nil {
		t.Fatal(e)
	}
	a.pauseSmartOnManual()
	if a.smartView().Enabled {
		t.Fatal("manual pause was undone by storage error")
	}
	b := newTestApp(t)
	b.store, b.dir = a.store, a.dir
	b.initSmart()
	if b.smartView().Enabled {
		t.Fatal("autoselection restarted from stale enabled file")
	}
}

func TestSmartNoQualifiedCandidateNeverFallsBackToWrongRegionOrMultiplier(t *testing.T) {
	a, h := newSmartHarness(t)
	for name := range h.results {
		h.results[name] = SmartCandidate{Status: "restricted", Delay: 30, Checked: time.Now().Format(time.RFC3339)}
	}
	if _, e := a.runSmartCheck(false); e != nil {
		t.Fatal(e)
	}
	if len(h.mainChanges) != 0 || !strings.Contains(a.smartView().Message, "保留当前") {
		t.Fatal(h.mainChanges, a.smartView())
	}
}

func TestSmartHysteresisKeepsNearEquivalentNodeAndRequiresRepeatedFailure(t *testing.T) {
	a, h := newSmartHarness(t)
	h.results["TW台湾 x1"] = SmartCandidate{Status: "pass", Delay: 50, Checked: time.Now().Format(time.RFC3339)}
	if _, e := a.runSmartCheck(false); e != nil {
		t.Fatal(e)
	}
	if len(h.mainChanges) != 0 {
		t.Fatal("10 ms improvement caused a switch")
	}
	h.results["TW台湾 x1"] = SmartCandidate{Status: "unknown", Delay: 50, Checked: time.Now().Format(time.RFC3339)}
	if _, e := a.runSmartCheck(false); e != nil {
		t.Fatal(e)
	}
	if len(h.mainChanges) != 0 {
		t.Fatal("single transient failure caused a switch")
	}
	if _, e := a.runSmartCheck(false); e != nil {
		t.Fatal(e)
	}
	if len(h.mainChanges) != 1 || h.mainChanges[0] != "SG新加坡 x0.8" {
		t.Fatal("repeated failed node not replaced", h.mainChanges)
	}
}

func TestSmartBackgroundCancellationDoesNotChangeHouseholdNode(t *testing.T) {
	a, h := newSmartHarness(t)
	started := make(chan struct{})
	var once sync.Once
	h.stopProbe = func(ctx context.Context) { once.Do(func() { close(started) }); <-ctx.Done() }
	done := make(chan struct{})
	go func() { _, _ = a.runSmartCheck(true); close(done) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background check did not start")
	}
	start := time.Now()
	a.waitSmartBackground()
	if time.Since(start) > time.Second {
		t.Fatal("background cancellation blocked the control UI")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("background check did not stop")
	}
	if len(h.mainChanges) != 0 {
		t.Fatal("cancelled probe changed household node", h.mainChanges)
	}
}

func TestSmartSettingsDefaultOffAtomicFailureAndSubscriptionConfirmationScope(t *testing.T) {
	a, h := newSmartHarness(t)
	a.smart.saved = defaultSmartSaved()
	if a.smartView().Enabled || !a.smartView().ExcludeHighMultiplier {
		t.Fatal("unsafe initial toggle defaults")
	}
	on := true
	a.smart.provisionHook = func() error { return errors.New("configuration check failed") }
	_, _, e, handled := a.performSmart(Request{Action: "smart-settings", Smart: &SmartEdit{Enabled: &on}})
	if !handled || e == nil || a.smartView().Enabled || len(h.mainChanges) > 0 {
		t.Fatal("failed provisioning enabled autonomous selection", e)
	}
	a.smart.provisionHook = func() error { return nil }
	_, _, e, _ = a.performSmart(Request{Action: "smart-settings", Smart: &SmartEdit{Enabled: &on, Overrides: map[string]float64{"香港未标明": 1}}})
	if e != nil || !a.smartView().Enabled || a.smartView().Overrides["香港未标明"] != 1 {
		t.Fatal(e, a.smartView())
	}
	// A different airport may reuse a node name with a different billing rate.
	_ = os.WriteFile(filepath.Join(a.dir, "config.yaml"), []byte(strings.ReplaceAll(original, "example.com", "new-airport.example")), 0600)
	a.notifySmartConfigChanged()
	if len(a.smartView().Overrides) != 0 {
		t.Fatal("unconfirmed multiplier carried into another subscription")
	}
	off := false
	_, _, e, _ = a.performSmart(Request{Action: "smart-settings", Smart: &SmartEdit{Enabled: &off}})
	if e != nil || a.smartView().Enabled || len(h.mainChanges) > 0 {
		t.Fatal("disable changed current node", e, h.mainChanges)
	}
}

func newSmartProvisionFixture(t *testing.T) (*App, []byte, map[string][]byte) {
	t.Helper()
	a := mockTransactionApp(t)
	before := a.snapshot()
	before.URL, before.PreviousURL = "https://current.example/private-current", "https://previous.example/private-previous"
	before.Updated, before.PreviousUpdated = "2001-10-23T08:00:00+08:00", "2001-10-22T08:00:00+08:00"
	before.Quota, before.PreviousQuota = &Quota{Available: true, Total: 100, Remaining: 70}, &Quota{Available: true, Total: 80, Remaining: 50}
	if e := a.save(before); e != nil {
		t.Fatal(e)
	}
	history := map[string][]byte{
		"previous.yaml":          []byte(strings.ReplaceAll(original, "old", "previous")),
		"profiles-previous.json": []byte(`{"version":1,"activeSub":"previous-airport","activeRule":"common"}`),
		"profiles.json":          []byte(`{"version":1,"activeSub":"current-airport","activeRule":"common"}`),
	}
	for name, b := range history {
		if e := os.WriteFile(filepath.Join(a.store, name), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	b, _ := os.ReadFile(filepath.Join(a.store, "settings.json"))
	history["settings.json"] = b
	a.initSmart()
	a.smart.healthHook = func() error { return nil }
	a.smart.apiHook = func(context.Context, string, string, any) ([]byte, error) { return []byte(`{}`), nil }
	a.restartHook = func(on bool) error {
		if !on {
			t.Fatal("lost enabled global network state")
		}
		return nil
	}
	candidate, _, e := addSmartProbeConfig([]byte(original))
	if e != nil {
		t.Fatal(e)
	}
	return a, candidate, history
}

func assertSmartProvisionHistory(t *testing.T, a *App, history map[string][]byte) {
	t.Helper()
	for name, want := range history {
		got, e := os.ReadFile(filepath.Join(a.store, name))
		if e != nil || string(got) != string(want) {
			t.Fatalf("smart plumbing changed user restore history %s: %v", name, e)
		}
	}
	state, _ := json.Marshal(a.snapshot())
	if string(state) != string(history["settings.json"]) {
		t.Fatal("smart plumbing changed in-memory subscription URLs/quota/restore metadata")
	}
}

func TestSmartProvisionSuccessPreservesSubscriptionRestoreIdentityAndQuota(t *testing.T) {
	a, candidate, history := newSmartProvisionFixture(t)
	if e := a.applySmartInfrastructure([]byte(original), candidate, map[string]string{selectGroup: "old"}); e != nil {
		t.Fatal(e)
	}
	assertSmartProvisionHistory(t, a, history)
	got, _ := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
	if string(got) != string(candidate) {
		t.Fatal("probe infrastructure not committed")
	}
	if exists(a.smartProvisionPath()) || exists(filepath.Join(a.store, "pending.json")) {
		t.Fatal("committed plumbing left a journal or wrote generic subscription journal")
	}
}

func TestSmartProvisionPostApplyFailureRestoresActiveConfigAndUserHistory(t *testing.T) {
	for _, failure := range []string{"restart", "listener", "selector"} {
		t.Run(failure, func(t *testing.T) {
			a, candidate, history := newSmartProvisionFixture(t)
			restarts, selectorCalls := 0, 0
			a.restartHook = func(on bool) error {
				restarts++
				if !on {
					t.Fatal("lost global on state")
				}
				if failure == "restart" && restarts == 1 {
					return errors.New("simulated core start failure")
				}
				return nil
			}
			if failure == "listener" {
				a.smart.healthHook = func() error { return errors.New("simulated missing loopback listener") }
			}
			a.smart.apiHook = func(_ context.Context, method, path string, body any) ([]byte, error) {
				if method != "PUT" {
					t.Fatal("unexpected API operation")
				}
				selectorCalls++
				if failure == "selector" && selectorCalls == 1 {
					return nil, errors.New("simulated selector restore failure")
				}
				return []byte(`{}`), nil
			}
			if e := a.applySmartInfrastructure([]byte(original), candidate, map[string]string{selectGroup: "old"}); e == nil || !strings.Contains(e.Error(), "已恢复") {
				t.Fatal("failed provisioning did not report rollback", e)
			}
			assertSmartProvisionHistory(t, a, history)
			got, _ := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
			if string(got) != original || restarts != 2 || selectorCalls < 1 {
				t.Fatal("failed plumbing did not restore active config and selector", restarts, selectorCalls)
			}
			if exists(a.smartProvisionPath()) {
				t.Fatal("completed rollback left stale recovery record")
			}
		})
	}
}

func TestSmartProvisionInterruptedRestartRecoveryPreservesRestoreHistory(t *testing.T) {
	a, candidate, history := newSmartProvisionFixture(t)
	j := smartProvisionJournal{Version: 1, Original: []byte(original), OriginalHash: smartHash([]byte(original)), On: true, Selected: map[string]string{selectGroup: "old"}}
	b, _ := json.Marshal(j)
	if e := atomicWrite(a.smartProvisionPath(), b); e != nil {
		t.Fatal(e)
	}
	if e := atomicWrite(filepath.Join(a.dir, "config.yaml"), candidate); e != nil {
		t.Fatal(e)
	}
	// Simulate a new process: recovery must work before initSmart/InitProfiles.
	a.smart = nil
	restarts := 0
	a.restartHook = func(bool) error { restarts++; return nil }
	// No selectors in this fixture's initial recovery avoids a real controller
	// dependency while still checking the pre-initialization recovery path.
	j.Selected = nil
	b, _ = json.Marshal(j)
	_ = atomicWrite(a.smartProvisionPath(), b)
	if e := a.recoverSmartProvision(); e != nil {
		t.Fatal(e)
	}
	assertSmartProvisionHistory(t, a, history)
	got, _ := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
	if string(got) != original || restarts != 1 || exists(a.smartProvisionPath()) {
		t.Fatal("incomplete probe installation not rolled back on restart")
	}
}

func TestSmartProvisionFailedRollbackRetainsRecoveryRecordAndHistory(t *testing.T) {
	a, candidate, history := newSmartProvisionFixture(t)
	a.restartHook = func(bool) error { return errors.New("simulated engine failure") }
	if e := a.applySmartInfrastructure([]byte(original), candidate, map[string]string{selectGroup: "old"}); e == nil {
		t.Fatal("failed recovery was ignored")
	}
	if !exists(a.smartProvisionPath()) {
		t.Fatal("failed rollback lost its restart recovery record")
	}
	assertSmartProvisionHistory(t, a, history)
	a.restartHook = func(bool) error { return nil }
	if e := a.recoverSmartProvision(); e != nil {
		t.Fatal(e)
	}
	if exists(a.smartProvisionPath()) {
		t.Fatal("successful second recovery left stale marker")
	}
	assertSmartProvisionHistory(t, a, history)
}
