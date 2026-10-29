package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const profileNodes = `dns: {listen: '0.0.0.0:53'}
mixed-port: 9999
external-controller: 0.0.0.0:9090
rules: [MATCH,REJECT]
proxies:
  - {name: 'TW台湾 0.5x', type: ss, server: taiwan.example, port: 443, password: node-private-value}
  - {name: 'SG新加坡 1x', type: ss, server: singapore.example, port: 443, password: node-private-value}
`

func profileTestApp(t *testing.T) *App {
	t.Helper()
	a := mockTransactionApp(t)
	s := a.snapshot()
	s.URL = "https://original.example/sub?token=original-private-value"
	s.Quota = &Quota{Available: true, Full: true, Total: 1000, Remaining: 700, Updated: time.Now().Format(time.RFC3339)}
	if e := a.save(s); e != nil {
		t.Fatal(e)
	}
	a.restartHook = func(bool) error { return nil }
	if e := a.InitProfiles(); e != nil {
		t.Fatal(e)
	}
	return a
}
func profileAction(t *testing.T, a *App, r Request) (string, any) {
	t.Helper()
	message, result, e, handled := a.performProfiles(r)
	if !handled || e != nil {
		t.Fatalf("%s: handled=%v error=%v", r.Action, handled, e)
	}
	return message, result
}
func TestProfileMigrationPreservesCredentialsConfigAndBilling(t *testing.T) {
	a := profileTestApp(t)
	got, _ := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
	if string(got) != original {
		t.Fatal("migration changed core config")
	}
	p := a.profileSnapshot()
	if p.ActiveSub != "existing" || p.ActiveRule != "current" || len(p.Rules) != 2 || len(p.Rules[1].Rules) != 220 {
		t.Fatal("migration lost existing or common rule profile")
	}
	if a.snapshot().Token != strings.Repeat("a", 64) || a.snapshot().Quota.Remaining != 700 || p.Subscriptions[0].Quota.Remaining != 700 {
		t.Fatal("credentials or airport billing lost")
	}
	view, _ := json.Marshal(a.profilesView())
	if strings.Contains(string(view), "private-value") || strings.Contains(string(view), "?token=") {
		t.Fatal("raw subscription URL exposed")
	}
	if !strings.Contains(string(view), "https://original.example/…") {
		t.Fatal("masked airport URL missing")
	}
	if _, e := os.Stat(a.profileCache("existing")); e != nil {
		t.Fatal("existing nodes were not cached", e)
	}
	other := &App{dir: a.dir, store: a.store, state: a.snapshot()}
	if e := other.InitProfiles(); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(p, other.profileSnapshot()) {
		t.Fatal("reopening profiles changed records")
	}
}
func TestInactiveSubscriptionQuotaDoesNotSwitchCoreOrActiveBalance(t *testing.T) {
	a := profileTestApp(t)
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.UserAgent() != "Clash.Meta/1.19" {
			t.Error("wrong subscription UA")
		}
		w.Header().Set("Subscription-Userinfo", "upload=10; download=20; total=900; expire=1893456000")
		_, _ = w.Write([]byte(profileNodes))
	}))
	defer srv.Close()
	profileAction(t, a, Request{Action: "sub-save", Name: "备用机场", URL: srv.URL + "/sub?token=second-private-value"})
	p := a.profileSnapshot()
	if len(p.Subscriptions) != 2 || p.ActiveSub != "existing" || p.Subscriptions[1].Quota.Remaining != 870 {
		t.Fatal("separate airport quota was not cached")
	}
	profileAction(t, a, Request{Action: "sub-quota", ID: p.Subscriptions[1].ID})
	if requests != 2 || a.snapshot().Quota.Remaining != 700 {
		t.Fatal("inactive balance overwrote active airport")
	}
	got, _ := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
	if string(got) != original {
		t.Fatal("inactive refresh changed core config")
	}
	view, _ := json.Marshal(a.profilesView())
	if strings.Contains(string(view), "second-private-value") {
		t.Fatal("inactive token leaked")
	}
}
func TestSubscriptionSwitchKeepsSelectedRuleAndRouterSettings(t *testing.T) {
	a := profileTestApp(t)
	a.profileFetchHook = func(string) ([]byte, *Quota, error) {
		return []byte(profileNodes), &Quota{Available: true, Remaining: 555}, nil
	}
	profileAction(t, a, Request{Action: "rule-switch", ID: "common"})
	profileAction(t, a, Request{Action: "sub-save", Name: "备用", URL: "https://second.example/sub?token=secret"})
	p := a.profileSnapshot()
	profileAction(t, a, Request{Action: "sub-switch", ID: p.Subscriptions[1].ID})
	got, _ := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
	var c map[string]any
	if e := yaml.Unmarshal(got, &c); e != nil {
		t.Fatal(e)
	}
	if c["mixed-port"] != 7890 || c["external-controller"] != "127.0.0.1:9091" || c["secret"] != "test-secret" || c["dns"].(map[string]any)["listen"] != "192.168.31.1:1053" {
		t.Fatal("subscription overwrote local network settings")
	}
	rules, _ := configRules(got)
	common, _ := configRules(commonRules)
	if !reflect.DeepEqual(rules, common) || a.profileSnapshot().ActiveRule != "common" {
		t.Fatal("airport update replaced selected common rules")
	}
	if a.snapshot().URL != "https://second.example/sub?token=secret" || a.snapshot().Quota.Remaining != 555 {
		t.Fatal("active billing/URL did not follow airport")
	}
	if len(c["proxies"].([]any)) != 2 {
		t.Fatal("airport nodes not installed")
	}
}
func TestActiveURLFailureRetainsProfilesConfigAndOldCache(t *testing.T) {
	a := profileTestApp(t)
	old := a.profileSnapshot()
	cachePath := a.profileCache("existing")
	cache, _ := os.ReadFile(cachePath)
	a.profileFetchHook = func(string) ([]byte, *Quota, error) {
		return []byte(profileNodes), &Quota{Available: true, Remaining: 22}, nil
	}
	calls := 0
	a.restartHook = func(bool) error {
		calls++
		if calls == 1 {
			return errors.New("new start failed")
		}
		return nil
	}
	_, _, e, handled := a.performProfiles(Request{Action: "sub-save", ID: "existing", URL: "https://changed.example/sub?token=changed-secret"})
	if !handled || e == nil {
		t.Fatal("failed core start not returned")
	}
	got, _ := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
	newCache, _ := os.ReadFile(cachePath)
	if string(got) != original || !reflect.DeepEqual(old, a.profileSnapshot()) || string(cache) != string(newCache) || a.snapshot().URL != old.Subscriptions[0].URL {
		t.Fatal("active URL failure did not preserve working configuration and cache")
	}
	if exists(filepath.Join(a.store, "profiles-pending.json")) || exists(filepath.Join(a.store, "pending.json")) {
		t.Fatal("completed rollback left journal")
	}
	if e = a.InitProfiles(); e != nil {
		t.Fatal("reopening after rollback", e)
	}
	if !reflect.DeepEqual(old, a.profileSnapshot()) {
		t.Fatal("failed selection persisted after reopen")
	}
}
func TestRuleImportsCannotReplaceNetworkingOrReferenceMissingGroups(t *testing.T) {
	a := profileTestApp(t)
	for _, content := range []string{
		"rules: ['MATCH,DIRECT']\ndns: {listen: '0.0.0.0:53'}",
		"rules: ['MATCH,不存在的分组']",
		"rules: ['MATCH,DIRECT','DOMAIN,example.com,DIRECT']",
		"rules: ['RULE-SET,missing,DIRECT','MATCH,节点选择']",
		"rules: ['SCRIPT,something,DIRECT','MATCH,节点选择']",
		"rules: ['MATCH,DIRECT']\n---\nrules: ['MATCH,REJECT']",
	} {
		_, _, e, handled := a.performProfiles(Request{Action: "rule-save", Name: "导入", Content: content})
		if !handled || e == nil {
			t.Fatalf("unsafe rule import accepted: %s", content)
		}
	}
	if len(a.profileSnapshot().Rules) != 2 {
		t.Fatal("rejected imports changed stored profiles")
	}
	profileAction(t, a, Request{Action: "rule-save", Name: "我的第二套", Content: "DOMAIN-SUFFIX,example.com,DIRECT\nMATCH,节点选择"})
	p := a.profileSnapshot()
	profileAction(t, a, Request{Action: "rule-switch", ID: p.Rules[2].ID})
	got, _ := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
	if !sameProfileConfigExceptRules([]byte(original), got) {
		t.Fatal("switching rules changed non-rule configuration")
	}
	if a.snapshot().Quota.Remaining != 700 {
		t.Fatal("rule switch changed airport quota")
	}
}
func TestProfileRestoreRestoresRuleAndSubscriptionIdentity(t *testing.T) {
	a := profileTestApp(t)
	profileAction(t, a, Request{Action: "rule-switch", ID: "common"})
	if a.profileSnapshot().ActiveRule != "common" {
		t.Fatal("rule switch failed")
	}
	profileAction(t, a, Request{Action: "restore"})
	got, _ := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
	if string(got) != original || a.profileSnapshot().ActiveRule != "current" || a.profileSnapshot().ActiveSub != "existing" {
		t.Fatal("restore config and profile identity diverged")
	}
}
func TestUpgradeKeepsExistingLegacyRestoreAvailable(t *testing.T) {
	a := mockTransactionApp(t)
	s := a.snapshot()
	s.URL = "https://new.example/sub?token=current-secret"
	s.PreviousURL = "https://old.example/sub?token=previous-secret"
	s.PreviousQuota = &Quota{Available: true, Remaining: 77}
	if e := a.save(s); e != nil {
		t.Fatal(e)
	}
	previous := []byte(strings.Replace(original, "name: old,", "name: previous-node,", 1))
	if e := atomicWrite(filepath.Join(a.store, "previous.yaml"), previous); e != nil {
		t.Fatal(e)
	}
	a.restartHook = func(bool) error { return nil }
	if e := a.InitProfiles(); e != nil {
		t.Fatal(e)
	}
	profileAction(t, a, Request{Action: "restore"})
	got, _ := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
	if string(got) != string(previous) || a.snapshot().URL != s.PreviousURL || a.snapshot().Quota.Remaining != 77 {
		t.Fatal("upgrade broke existing restore backup")
	}
	p := a.profileSnapshot()
	i := subIndex(p, p.ActiveSub)
	if i < 0 || p.Subscriptions[i].URL != s.PreviousURL {
		t.Fatal("restored legacy subscription has wrong profile identity")
	}
}
func TestInterruptedProfileTransactionUsesCommittedConfigAndState(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "interrupted", true: "committed"}[committed], func(t *testing.T) {
			a := profileTestApp(t)
			before := a.profileSnapshot()
			after := cloneProfiles(before)
			after.ActiveRule = "common"
			after.Revision++
			candidate, e := applyRulesToConfig([]byte(original), after.Rules[1].Rules)
			if e != nil {
				t.Fatal(e)
			}
			next := a.snapshot()
			next.Updated = "2001-10-23T08:00:00.000001+08:00"
			j := profileJournal{Before: *before, After: *after, CandidateHash: profileHash(candidate), StateURL: next.URL, StateUpdated: next.Updated}
			b, _ := json.Marshal(j)
			if e = atomicWrite(filepath.Join(a.store, "profiles-pending.json"), b); e != nil {
				t.Fatal(e)
			}
			_ = a.writeProfiles(after)
			_ = atomicWrite(filepath.Join(a.dir, "config.yaml"), candidate)
			if committed {
				_ = a.save(next)
			} // Without the State commit, candidate alone is insufficient.
			if e = a.InitProfiles(); e != nil {
				t.Fatal(e)
			}
			want := "current"
			if committed {
				want = "common"
			}
			if a.profileSnapshot().ActiveRule != want {
				t.Fatal("incorrect recovery choice", a.profileSnapshot().ActiveRule, want)
			}
			if exists(filepath.Join(a.store, "profiles-pending.json")) {
				t.Fatal("recovery did not close journal")
			}
		})
	}
}
func TestMissingAirportQuotaKeepsPreviousReadingAndMarksStale(t *testing.T) {
	a := profileTestApp(t)
	a.profileFetchHook = func(string) ([]byte, *Quota, error) {
		return []byte(profileNodes), &Quota{Available: false, Updated: time.Now().Format(time.RFC3339)}, nil
	}
	_, _, e, _ := a.performProfiles(Request{Action: "sub-quota", ID: "existing"})
	if e == nil || a.profileSnapshot().Subscriptions[0].Quota.Remaining != 700 || a.snapshot().Quota.Remaining != 700 {
		t.Fatal("missing provider information replaced previous quota")
	}
	view := a.profilesView().(ProfilesView)
	if !view.Items[0].Stale || view.Items[0].Error == "" {
		t.Fatal("failed balance must show stale status")
	}
}
