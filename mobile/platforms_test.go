package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPlatformChecksDoNotConfuseChallengesWithUnlockedServices(t *testing.T) {
	for _, r := range []struct {
		id             string
		code           int
		ct, body, want string
	}{
		{"chatgpt", 200, "application/json", smartValidProviders, "pass"},
		{"chatgpt", 403, "text/html", "<html>Just a moment cf-chl-test</html>", "unknown"},
		{"youtube", 200, "text/html", "<html><script>var ytInitialData = {};</script></html>", "pass"},
		{"youtube", 200, "text/html", "<html>Everything is OK</html>", "unknown"},
		{"twitter", 403, "text/html", "<html>Access Denied</html>", "unknown"},
		{"twitter", 200, "text/html", "<title>X</title><div>verify you are human captcha</div>", "unknown"},
		{"netflix", 200, "text/html", "<title>Netflix</title><div>You seem to be using an unblocker or proxy.</div>", "restricted"},
		{"tiktok", 451, "text/html", "<title>TikTok</title>", "restricted"},
		{"tiktok", 429, "text/html", "<title>TikTok</title>", "unknown"},
		{"netflix", 503, "text/html", "<title>Netflix</title>", "fail"},
	} {
		got, reason := classifyPlatformResponse(r.id, r.code, r.ct, []byte(r.body))
		if got != r.want {
			t.Fatalf("%s HTTP %d => %s (%s), want %s", r.id, r.code, got, reason, r.want)
		}
	}
	status, reason := classifyPlatformResponse("netflix", 200, "text/html", []byte("<title>Netflix</title>"))
	if status != "pass" || !strings.Contains(reason, "未检测") {
		t.Fatal("public page pass advertised playback", reason)
	}
}

func TestPlatformHTTPActuallyUsesPrivateProxyAndCachesOnlySameCatalog(t *testing.T) {
	requests := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Host != "platform.example" {
			t.Error("wrong proxy destination", r.URL)
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><script>ytcfg.set({});</script></html>"))
	}))
	defer proxy.Close()
	result := probePlatformHTTP(context.Background(), "test-node", "youtube", proxy.URL, "http://platform.example/")
	if result.Status != "pass" || result.Delay < 1 || requests != 1 {
		t.Fatal("platform request did not use private proxy", result, requests)
	}
	a := mockTransactionApp(t)
	a.recordPlatform(result)
	if _, ok := a.cachedPlatform("test-node", "youtube", 10*time.Minute); !ok {
		t.Fatal("fresh cache not reusable")
	}
	s := a.platformsView()
	if len(s.Items) != 1 {
		t.Fatal("cache result missing")
	}
	body := strings.Replace(original, "server: example.com", "server: changed.example", 1)
	_ = os.WriteFile(filepath.Join(a.dir, "config.yaml"), []byte(body), 0600)
	if _, ok := a.cachedPlatform("test-node", "youtube", 10*time.Minute); ok {
		t.Fatal("changed node credentials/server reused stale platform pass")
	}
}

func TestPlatformBatchChangesOnlyProbeGroupAndHonorsFreshCache(t *testing.T) {
	a := mockTransactionApp(t)
	body, _, e := addSmartProbeConfig([]byte(original))
	if e != nil {
		t.Fatal(e)
	}
	_ = os.WriteFile(filepath.Join(a.dir, "config.yaml"), body, 0600)
	h := attachServiceHarness(t, a)
	a.initPlatforms()
	count := 0
	a.platforms.probeHook = func(ctx context.Context, node, id string) PlatformResult {
		count++
		return PlatformResult{Node: node, Platform: id, Status: "unknown", Reason: "人机验证", Checked: time.Now().Format(time.RFC3339)}
	}
	r := Request{Action: "platform-check", Platform: &PlatformEdit{Node: "old", Platforms: []string{"chatgpt", "youtube", "chatgpt"}}}
	if _, _, e, ok := a.performPlatforms(r); e != nil || !ok {
		t.Fatal(e)
	}
	if count != 2 {
		t.Fatal("duplicate platform checked repeatedly", count)
	}
	if _, _, e, _ = a.performPlatforms(r); e != nil {
		t.Fatal(e)
	}
	if count != 2 {
		t.Fatal("fresh cache was ignored")
	}
	r.Platform.Force = true
	if _, _, e, _ = a.performPlatforms(r); e != nil {
		t.Fatal(e)
	}
	if count != 4 {
		t.Fatal("forced check did not refresh")
	}
	for _, put := range h.puts {
		if !strings.HasPrefix(put, smartGroup+"=") {
			t.Fatal("platform batch modified household/service selector", h.puts)
		}
	}
	if a.platformsView().Running {
		t.Fatal("running flag stuck after completed batch")
	}
}
