package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func mockTransactionApp(t *testing.T) *App {
	a := newTestApp(t)
	os.WriteFile(filepath.Join(a.dir, "config.yaml"), []byte(original), 0600)
	a.validateHook = func(string) error { return nil }
	a.rulesHook = func() (Rules, error) { return Rules{Total: 3, Enabled: true}, nil }
	return a
}
func TestSubscriptionClientIdentificationAndTrailingPunctuation(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() != "Clash.Meta/1.19" {
			w.Write([]byte("<html>text toolbox</html>"))
			return
		}
		w.Write([]byte("proxies: []\n"))
	}))
	defer s.Close()
	b, e := download(s.URL + "。")
	if e != nil || string(b) != "proxies: []\n" {
		t.Fatal("subscription endpoint returned camouflage instead of YAML", e)
	}
}
func TestFailedStartRollsBackConfigAndSubscription(t *testing.T) {
	a := mockTransactionApp(t)
	before := a.snapshot()
	before.URL = "https://old.example/sub"
	a.save(before)
	calls := 0
	a.restartHook = func(on bool) error {
		calls++
		if !on {
			t.Fatal("Lost enabled state")
		}
		if calls == 1 {
			return errors.New("simulated start failure")
		}
		return nil
	}
	next := before
	next.URL = "https://new.example/sub"
	if e := a.applyConfig([]byte("new: candidate"), next); e == nil || !strings.Contains(e.Error(), "已恢复") {
		t.Fatalf("Rollback not reported: %v", e)
	}
	b, _ := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
	if string(b) != original || a.snapshot().URL != before.URL || calls != 2 {
		t.Fatal("Rollback did not preserve previous state")
	}
	if exists(filepath.Join(a.store, "pending.json")) {
		t.Fatal("Completed rollback left pending marker")
	}
}
func TestSuccessfulUpdateAndRestoreBackup(t *testing.T) {
	a := mockTransactionApp(t)
	a.restartHook = func(on bool) error { return nil }
	next := a.snapshot()
	next.URL = "https://new.example/sub"
	next.Updated = "2001-09-13T21:00:00+08:00"
	if e := a.applyConfig([]byte("new: candidate"), next); e != nil {
		t.Fatal(e)
	}
	backup, _ := os.ReadFile(filepath.Join(a.store, "previous.yaml"))
	if string(backup) != original {
		t.Fatal("Missing previous configuration")
	}
	if a.snapshot().URL != next.URL || exists(filepath.Join(a.store, "pending.json")) {
		t.Fatal("Commit incomplete")
	}
	next = a.snapshot()
	next.URL = next.PreviousURL
	next.Updated = next.PreviousUpdated
	if e := a.applyConfig(backup, next); e != nil {
		t.Fatal(e)
	}
	current, _ := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
	if string(current) != original {
		t.Fatal("Restore failed")
	}
}
func TestInterruptedUpdateRecovery(t *testing.T) {
	a := mockTransactionApp(t)
	before := a.snapshot()
	before.URL = "https://old.example/sub"
	a.restartHook = func(on bool) error {
		if !on {
			t.Fatal("Lost switch state")
		}
		return nil
	}
	os.WriteFile(filepath.Join(a.store, "rollback.yaml"), []byte(original), 0600)
	journal, _ := json.Marshal(map[string]any{"state": before, "on": true})
	os.WriteFile(filepath.Join(a.store, "pending.json"), journal, 0600)
	os.WriteFile(filepath.Join(a.dir, "config.yaml"), []byte("broken: candidate"), 0600)
	if e := a.recoverPending(); e != nil {
		t.Fatal(e)
	}
	current, _ := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
	if string(current) != original || a.snapshot().URL != before.URL {
		t.Fatal("Recovery failed")
	}
}
func TestSyntheticRouterConfiguration(t *testing.T) {
	input := []byte(original)
	merged, count, e := mergeNodes(input, input)
	if e != nil {
		t.Fatal(e)
	}
	if count != 1 {
		t.Fatalf("Unexpected synthetic node count %d", count)
	}
	if !sameProfileConfigExceptRules(input, merged) {
		t.Fatal("merging identical synthetic subscription changed router settings")
	}
}

const original = `mixed-port: 7890
redir-port: 7892
tproxy-port: 7893
external-controller: 127.0.0.1:9091
secret: test-secret
mode: rule
ipv6: false
dns:
  listen: 192.168.31.1:1053
  enhanced-mode: fake-ip
proxies:
  - {name: old, type: ss, server: example.com, port: 443}
proxy-groups:
  - {name: 节点选择, type: select, proxies: [自动选择, 故障转移, old]}
  - {name: 自动选择, type: url-test, proxies: [old], url: https://www.gstatic.com/generate_204, interval: 300}
  - {name: 故障转移, type: fallback, proxies: [old]}
rules:
  - DOMAIN-SUFFIX,cn,DIRECT
  - MATCH,节点选择
`

func TestMergePreservesRouterAndRuleSettings(t *testing.T) {
	incoming := []byte("mixed-port: 9999\nexternal-controller: 0.0.0.0:9999\ndns: {listen: 0.0.0.0:53}\nrules: [MATCH,DIRECT]\nproxies:\n  - {name: \"🇹🇼台湾04 O'Brien\", type: ss, server: new.example, port: 443}\n")
	got, count, err := mergeNodes([]byte(original), incoming)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal(count)
	}
	var before, after map[string]any
	yaml.Unmarshal([]byte(original), &before)
	yaml.Unmarshal(got, &after)
	for k, v := range before {
		if k != "proxies" && k != "proxy-groups" && !reflect.DeepEqual(v, after[k]) {
			t.Fatalf("Changed protected setting %s", k)
		}
	}
	if strings.Contains(string(got), "old") {
		t.Fatal("Old node reference retained")
	}
	groups := after["proxy-groups"].([]any)
	list := groups[0].(map[string]any)["proxies"].([]any)
	if !reflect.DeepEqual(list, []any{"自动选择", "故障转移", "🇹🇼台湾04 O'Brien"}) {
		t.Fatal(list)
	}
}
func TestInvalidSubscriptionsDoNotProduceConfig(t *testing.T) {
	for _, input := range []string{"", "<html>error</html>", "c3M6Ly9leGFtcGxl", "proxies: []", "proxy-providers: {foo: {type: http}}", "proxies: [{name: 自动选择, type: ss}]", "proxies: [{name: x, type: ss}, {name: x, type: ss}]"} {
		if _, _, err := mergeNodes([]byte(original), []byte(input)); err == nil {
			t.Errorf("Accepted invalid input: %s", input)
		}
	}
}
func TestRulesClassification(t *testing.T) {
	valid := "-A PREROUTING -i br-lan -p tcp -j MIHOMO_TCP\n-A PREROUTING -i br-lan -p tcp -m tcp --dport 53 -j REDIRECT --to-ports 1053\n-A PREROUTING -i br-lan -p udp -m udp --dport 53 -j REDIRECT --to-ports 1053\n"
	if r := parseRules(valid); !r.Enabled || r.Partial {
		t.Fatal(r)
	}
	if r := parseRules(valid + "-A PREROUTING -s 192.168.31.10/32 -p tcp -j MIHOMO_TCP\n"); r.Enabled || !r.Partial || r.Total != 4 {
		t.Fatal(r)
	}
	if r := parseRules("-A PREROUTING -j plugin_PREROUTING"); r.Enabled || r.Partial || r.Total != 0 {
		t.Fatal(r)
	}
}
func newTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	return &App{dir: dir, store: dir, host: "192.168.31.1:9080", gate: make(chan struct{}, 1), state: State{Salt: "salt", Hash: passwordHash("example-password", "salt"), Token: strings.Repeat("a", 64)}}
}
func request(a *App, method, path, host, origin, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://"+host+path, strings.NewReader(body))
	r.Host = host
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	a.handler().ServeHTTP(w, r)
	return w
}
func TestAuthenticationAndOrigin(t *testing.T) {
	a := newTestApp(t)
	for _, c := range []struct {
		host, origin, token string
		want                int
	}{{a.host, "", "", 401}, {"evil.example:9080", "", a.state.Token, 403}, {a.host, "http://evil.example", a.state.Token, 403}, {a.host, "http://" + a.host, a.state.Token, 200}} {
		w := request(a, "GET", "/api/subscription", c.host, c.origin, c.token, "")
		if w.Code != c.want {
			t.Fatalf("got %d want %d", w.Code, c.want)
		}
	}
	w := request(a, "POST", "/api/login", a.host, "", "", `{"password":"example-password"}`)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	var v map[string]string
	json.Unmarshal(w.Body.Bytes(), &v)
	if v["token"] != a.state.Token {
		t.Fatal("Wrong token")
	}
	w = request(a, "POST", "/api/login", a.host, "", "", `{"password":"bad"}`)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
}

func TestSubscriptionResponseHidesSensitiveLink(t *testing.T) {
	a := newTestApp(t)
	a.state.URL = "https://example.com/secret-token-path?token=private-value"
	w := request(a, "GET", "/api/subscription", a.host, "", a.state.Token, "")
	if strings.Contains(w.Body.String(), "secret-token") || strings.Contains(w.Body.String(), "private-value") {
		t.Fatal("Subscription credentials exposed")
	}
}
func TestJobConcurrency(t *testing.T) {
	a := newTestApp(t)
	a.gate <- struct{}{}
	w := request(a, "POST", "/api/action", a.host, "", a.state.Token, `{"action":"restart"}`)
	if w.Code != 409 {
		t.Fatal(w.Code)
	}
}
func TestValidateFailureKeepsConfig(t *testing.T) {
	a := newTestApp(t)
	config := filepath.Join(a.dir, "config.yaml")
	os.WriteFile(config, []byte(original), 0600)
	if err := a.applyConfig([]byte("invalid: candidate"), a.state); err == nil {
		t.Fatal("Expected failure for missing validator")
	}
	got, _ := os.ReadFile(config)
	if !bytes.Equal(got, []byte(original)) {
		t.Fatal("Changed live config before validation")
	}
	if exists(filepath.Join(a.store, "pending.json")) {
		t.Fatal("Started transaction before validation")
	}
}
func TestSubscriptionURLValidation(t *testing.T) {
	for _, raw := range []string{"file:///etc/passwd", "http://", "ftp://example.com/a", "https://user:password@example.com"} {
		if _, err := download(raw); err == nil {
			t.Fatalf("Accepted %s", raw)
		}
	}
}
func TestStaticAssets(t *testing.T) {
	a := newTestApp(t)
	for _, path := range []string{"/", "/app.js", "/style.css", "/manifest.webmanifest", "/icon.svg"} {
		w := request(a, http.MethodGet, path, a.host, "", "", "")
		if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
		if w.Header().Get("Content-Security-Policy") == "" {
			t.Fatal("Missing CSP")
		}
	}
}
