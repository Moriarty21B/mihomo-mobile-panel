package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQuotaHeaderFallbackAndExhaustion(t *testing.T) {
	now := time.Date(2001, 10, 6, 12, 0, 0, 0, time.UTC)
	body := []byte("proxies:\n - name: '剩余流量：12.5 GB'\n - name: '距离下次重置剩余：30 天'\n - name: '套餐到期：2030-01-01'\n")
	q := parseQuota("upload=1024; download=2048; total=1073741824; expire=1893456000", body, now)
	if !q.Full || q.Remaining != 1073738752 || q.ResetDays != 30 || q.Expire == "" {
		t.Fatalf("header must override rounded pseudo-node: %+v", q)
	}
	q = parseQuota("", body, now)
	if q.Full || !q.Available || q.Remaining != 13421772800 || q.Expire != "2030-01-01" {
		t.Fatalf("fallback: %+v", q)
	}
	for _, h := range []string{"upload=110; download=3; total=100", "upload=80; download=30; total=100"} {
		q = parseQuota(h, nil, now)
		if q.Remaining != 0 || !q.Available {
			t.Fatal("exhaustion underflow", q)
		}
	}
	q = parseQuota("upload=3; download=4; total=0", nil, now)
	if !q.Unlimited {
		t.Fatal("unlimited package not recognized")
	}
	q = parseQuota("", []byte("proxies: []"), now)
	if q.Available {
		t.Fatal("missing quota must not become zero")
	}
	if quotaNode("🇹🇼台湾04") || !quotaNode("套餐到期：2030-01-01") {
		t.Fatal("pseudo-node detection")
	}
}
func TestDownloadReturnsProviderQuota(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Subscription-Userinfo", "upload=10; download=20; total=100")
		w.Write([]byte("proxies: []"))
	}))
	defer s.Close()
	_, q, e := downloadSubscription(s.URL)
	if e != nil || q.Remaining != 70 {
		t.Fatal(q, e)
	}
}

func TestBackgroundQuotaDoesNotBlockActions(t *testing.T) {
	a := newTestApp(t)
	a.gate <- struct{}{}
	done := make(chan struct{})
	go func() { a.saveBackgroundQuota(a.snapshot(), &Quota{Available: true}); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("background quota blocked behind a network operation")
	}
	if a.snapshot().Quota != nil {
		t.Fatal("background quota must defer while an action is running")
	}
	<-a.gate
}
func TestMACUsageResetIPChangePeriodAndReload(t *testing.T) {
	a := newTestApp(t)
	a.initUsage()
	now := time.Date(2001, 10, 31, 15, 59, 55, 0, time.UTC)
	mac := "02:00:00:00:00:01"
	view := []DeviceView{{DeviceRecord: DeviceRecord{MAC: mac}, IP: "192.168.31.11", Online: true, Traffic: &DeviceTraffic{Download: 1000, Upload: 500}}}
	l := a.usage
	l.sampleNative(view, "boot1", now)
	if l.entry(mac).Total.Download != 0 {
		t.Fatal("pre-install traffic was included")
	}
	view[0].IP = "192.168.31.10"
	view[0].Traffic.Download = 1100
	view[0].Traffic.Upload = 550
	l.sampleNative(view, "boot1", now.Add(time.Second))
	view[0].Traffic.Download = 30
	view[0].Traffic.Upload = 20
	l.sampleNative(view, "boot1", now.Add(10*time.Second))
	e := l.entry(mac)
	if e.Total.Download != 130 || e.Total.Upload != 70 || e.Months["2001-10"].Download != 100 || e.Months["2001-11"].Download != 30 {
		t.Fatalf("counter reset or midnight attribution %+v", e)
	}
	view[0].Online = false
	view[0].Traffic.Download = 0
	l.sampleNative(view, "boot1", now.Add(11*time.Second))
	if e.Baseline.Download != 30 {
		t.Fatal("offline zero damaged baseline")
	}
	a.saveUsage(now)
	b := newTestApp(t)
	b.store = a.store
	b.initUsage()
	if b.usage.entry(mac).Total != e.Total {
		t.Fatal("persisted totals lost")
	}
	view[0].Online = true
	view[0].Traffic.Download = 10
	l.sampleNative(view, "boot2", now.Add(15*time.Second))
	if e.Total.Download != 140 {
		t.Fatal("post-router-reboot counters not added")
	}
}
func TestProxySamplingExcludesDirectAndRestartBaseline(t *testing.T) {
	a := newTestApp(t)
	a.initUsage()
	now := time.Now()
	a.usageSessionStarted = now
	mac := "02:00:00:00:00:01"
	views := []DeviceView{{DeviceRecord: DeviceRecord{MAC: mac}, Online: true, IP: "192.168.31.10"}}
	c := Connection{ID: "old", Start: now.Add(-time.Hour), Download: 1000, Chains: []string{"🇹🇼台湾04", selectGroup}}
	c.Metadata.SourceIP = views[0].IP
	a.sampleConnections([]Connection{c}, views, now)
	if a.usage.entry(mac).Proxy.Download != 0 {
		t.Fatal("old open connection counted on panel restart")
	}
	c.Download = 1040
	a.sampleConnections([]Connection{c}, views, now.Add(time.Second))
	if a.usage.entry(mac).Proxy.Download != 40 {
		t.Fatal("connection delta lost")
	}
	c.ID = "direct"
	c.Start = now
	c.Chains = []string{"DIRECT"}
	a.sampleConnections([]Connection{c}, views, now)
	if a.usage.entry(mac).Proxy.Download != 40 {
		t.Fatal("DIRECT counted as airport traffic")
	}
	c.ID = "new"
	c.Start = now
	c.Download = 20
	c.Chains = []string{"🇹🇼台湾04"}
	a.sampleConnections([]Connection{c}, views, now)
	if a.usage.entry(mac).Proxy.Download != 60 {
		t.Fatal("new connection initial sample missing")
	}
}

func TestProxyUsageAndSpeedExcludeHeavyDirectTraffic(t *testing.T) {
	a := newTestApp(t)
	a.initUsage()
	now := time.Now()
	a.usageSessionStarted = now
	mac := "02:00:00:00:00:01"
	views := []DeviceView{{DeviceRecord: DeviceRecord{MAC: mac}, Online: true, IP: "192.168.31.10", Traffic: &DeviceTraffic{Download: 1000}}}
	direct := Connection{ID: "bilibili", Start: now, Download: 50 << 20, Chains: []string{"DIRECT", selectGroup}}
	direct.Metadata.SourceIP = views[0].IP
	proxy := Connection{ID: "tiktok", Start: now, Download: 200, Upload: 40, Chains: []string{"🇹🇼台湾04", selectGroup}}
	proxy.Metadata.SourceIP = views[0].IP
	a.recordUsageSample(views, []Connection{direct, proxy}, true, now)
	first := a.usageViews[0].Usage
	if !first.ProxyAvailable || first.ProxySpeedAvailable || first.Proxy != (BytePair{Download: 200, Upload: 40}) {
		t.Fatalf("first proxy sample availability or baseline: %+v", first)
	}
	views[0].Traffic.Download += 100 << 20
	direct.Download += 100 << 20
	proxy.Download += 1000
	proxy.Upload += 250
	a.recordUsageSample(views, []Connection{direct, proxy}, true, now.Add(5*time.Second))
	u := a.usageViews[0].Usage
	if u.Total.Download != 100<<20 || u.Proxy != (BytePair{Download: 1200, Upload: 290}) || !u.ProxySpeedAvailable || u.ProxySpeed != (BytePair{Download: 200, Upload: 50}) {
		t.Fatalf("direct traffic entered proxy total or proxy speed: %+v", u)
	}
	day, month := usageKeys(now)
	e := a.usage.entry(mac)
	if e.ProxyDays[day] != u.Proxy || e.ProxyMonths[month] != u.Proxy {
		t.Fatal("proxy periods use a different accounting source")
	}
}

func TestProxySamplingBaselinesAllNonNodeExits(t *testing.T) {
	for _, chains := range [][]string{{"DIRECT"}, {"REJECT"}, {"REJECT-DROP"}, {"PASS"}, {"COMPATIBLE"}, {selectGroup, "DIRECT"}, nil} {
		t.Run(strings.Join(chains, "/"), func(t *testing.T) {
			a := newTestApp(t)
			a.initUsage()
			now := time.Now()
			a.usageSessionStarted = now
			mac := "02:00:00:00:00:01"
			views := []DeviceView{{DeviceRecord: DeviceRecord{MAC: mac}, Online: true, IP: "192.168.31.10"}}
			c := Connection{ID: "exit-changes", Start: now, Download: 1 << 20, Chains: chains}
			c.Metadata.SourceIP = views[0].IP
			if d := a.sampleConnections([]Connection{c}, views, now); len(d) != 0 {
				t.Fatal("non-node exit produced proxy deltas", d)
			}
			c.Chains = []string{"🇹🇼台湾04", selectGroup}
			c.Download += 20
			a.sampleConnections([]Connection{c}, views, now.Add(time.Second))
			if a.usage.entry(mac).Proxy.Download != 0 {
				t.Fatal("previous non-node bytes imported after exit changed")
			}
			c.Download += 10
			d := a.sampleConnections([]Connection{c}, views, now.Add(2*time.Second))
			if d[mac].Download != 10 || a.usage.entry(mac).Proxy.Download != 10 {
				t.Fatal("confirmed proxy increment not recorded", d)
			}
		})
	}
}

func TestProxyConnectionOwnerSurvivesOfflineAndIPReuse(t *testing.T) {
	a := newTestApp(t)
	a.initUsage()
	now := time.Now()
	a.usageSessionStarted = now
	oldMAC, newMAC := "02:00:00:00:00:01", "02:00:00:00:00:02"
	views := []DeviceView{{DeviceRecord: DeviceRecord{MAC: oldMAC}, Online: true, IP: "192.168.31.10"}}
	c := Connection{ID: "original-computer-connection", Start: now, Download: 100, Chains: []string{"🇹🇼台湾04"}}
	c.Metadata.SourceIP = views[0].IP
	a.sampleConnections([]Connection{c}, views, now)
	// An old connection remains in Mihomo while its source IP is assigned to a phone.
	views = []DeviceView{{DeviceRecord: DeviceRecord{MAC: oldMAC}, Online: false}, {DeviceRecord: DeviceRecord{MAC: newMAC}, Online: true, IP: c.Metadata.SourceIP}}
	c.Download += 40
	d := a.sampleConnections([]Connection{c}, views, now.Add(time.Second))
	if d[oldMAC].Download != 40 || a.usage.entry(oldMAC).Proxy.Download != 140 || a.usage.entry(newMAC).Proxy.Download != 0 {
		t.Fatal("existing connection changed MAC owner after IP reuse", d)
	}
	phone := c
	phone.ID, phone.Start, phone.Download = "new-phone-connection", now.Add(time.Second), 20
	a.sampleConnections([]Connection{c, phone}, views, now.Add(2*time.Second))
	if a.usage.entry(newMAC).Proxy.Download != 20 {
		t.Fatal("new source-IP connection not assigned to new device")
	}
	// A temporary inventory outage must keep updating the connection cursor.
	c.Download += 10
	a.sampleConnections([]Connection{c}, nil, now.Add(3*time.Minute))
	c.Download += 10
	a.sampleConnections([]Connection{c}, views, now.Add(4*time.Minute))
	if a.usage.entry(oldMAC).Proxy.Download != 160 || a.usage.entry(newMAC).Proxy.Download != 20 {
		t.Fatal("offline inventory gap lost, moved, or recounted bytes")
	}
}

func TestProxySamplingDoesNotGuessConflictingIPOwner(t *testing.T) {
	a := newTestApp(t)
	a.initUsage()
	now := time.Now()
	a.usageSessionStarted = now
	mac1, mac2 := "02:00:00:00:00:01", "02:00:00:00:00:02"
	views := []DeviceView{{DeviceRecord: DeviceRecord{MAC: mac1}, Online: true, IP: "192.168.31.10"}, {DeviceRecord: DeviceRecord{MAC: mac2}, Online: true, IP: "192.168.31.10"}}
	c := Connection{ID: "ambiguous", Start: now, Download: 100, Chains: []string{"🇹🇼台湾04"}}
	c.Metadata.SourceIP = views[0].IP
	a.sampleConnections([]Connection{c}, views, now)
	if a.usageConnections[c.ID].MAC != "" || a.usage.entry(mac1).Proxy.Download != 0 || a.usage.entry(mac2).Proxy.Download != 0 {
		t.Fatal("conflicting inventory arbitrarily selected a device")
	}
	views[1].Online = false
	c.Download += 50
	a.sampleConnections([]Connection{c}, views, now.Add(time.Second))
	if a.usage.entry(mac1).Proxy.Download != 0 {
		t.Fatal("previously unattributed traffic imported at first safe association")
	}
	c.Download += 10
	a.sampleConnections([]Connection{c}, views, now.Add(2*time.Second))
	if a.usage.entry(mac1).Proxy.Download != 10 {
		t.Fatal("subsequent safely attributed delta not recorded")
	}
}

func TestProxyAvailabilityWithoutNativeCountersAndAfterFailureReload(t *testing.T) {
	a := newTestApp(t)
	a.initUsage()
	now := time.Now()
	a.usageSessionStarted = now
	mac := "02:00:00:00:00:01"
	views := []DeviceView{{DeviceRecord: DeviceRecord{MAC: mac}, Online: true, IP: "192.168.31.10"}}
	c := Connection{ID: "proxy-only", Start: now, Download: 100, Chains: []string{"🇹🇼台湾04"}}
	c.Metadata.SourceIP = views[0].IP
	a.recordUsageSample(views, []Connection{c}, true, now)
	if u := a.usageViews[0].Usage; u.Available || !u.ProxyAvailable || u.Proxy.Download != 100 || u.ProxySpeedAvailable {
		t.Fatalf("proxy availability depends on native counter: %+v", u)
	}
	c.Download = 150
	a.recordUsageSample(views, []Connection{c}, true, now.Add(5*time.Second))
	if u := a.usageViews[0].Usage; !u.ProxySpeedAvailable || u.ProxySpeed.Download != 10 {
		t.Fatalf("proxy-only rate unavailable: %+v", u)
	}
	a.recordUsageSample(views, nil, false, now.Add(10*time.Second))
	if u := a.usageViews[0].Usage; !u.ProxyAvailable || u.Proxy.Download != 150 || u.ProxySpeedAvailable || u.ProxySpeed.Download != 0 {
		t.Fatalf("failed API sample fabricated a fresh speed or lost totals: %+v", u)
	}
	c.Download = 350
	a.recordUsageSample(views, []Connection{c}, true, now.Add(25*time.Second))
	if u := a.usageViews[0].Usage; u.ProxySpeed.Download != 10 || !u.ProxySpeedAvailable || u.Proxy.Download != 350 {
		t.Fatalf("rate did not use actual elapsed successful-sample interval: %+v", u)
	}
	a.saveUsage(now)
	b := newTestApp(t)
	b.store = a.store
	b.initUsage()
	b.usageSessionStarted = now.Add(30 * time.Second)
	c.Download = 400
	b.recordUsageSample(views, []Connection{c}, true, now.Add(31*time.Second))
	if u := b.usageViews[0].Usage; !u.ProxyAvailable || u.Proxy.Download != 350 || u.ProxySpeedAvailable {
		t.Fatalf("restart lost persisted proxy data or imported existing connection: %+v", u)
	}
	c.Download = 430
	b.recordUsageSample(views, []Connection{c}, true, now.Add(36*time.Second))
	if u := b.usageViews[0].Usage; u.Proxy.Download != 380 || u.ProxySpeed.Download != 6 {
		t.Fatalf("post-restart increment or speed wrong: %+v", u)
	}
}

func TestProxyZeroAndLegacyLedgerRemainUsableWithoutNativeCounters(t *testing.T) {
	a := newTestApp(t)
	mac := "02:00:00:00:00:01"
	old := []byte(`{"version":1,"started":"2001-10-06T08:00:00+08:00","devices":{"02:00:00:00:00:01":{"proxy":{"download":700,"upload":20},"total":{"download":9000,"upload":90}}}}`)
	if e := os.WriteFile(filepath.Join(a.store, "traffic.json"), old, 0600); e != nil {
		t.Fatal(e)
	}
	a.initUsage()
	now := time.Now()
	views := []DeviceView{{DeviceRecord: DeviceRecord{MAC: mac}, Online: true, IP: "192.168.31.10"}, {DeviceRecord: DeviceRecord{MAC: "02:00:00:00:00:02"}, Online: true, IP: "192.168.31.20"}}
	a.recordUsageSample(views, nil, false, now)
	if u := a.usageViews[0].Usage; !u.ProxyAvailable || u.Proxy != (BytePair{Download: 700, Upload: 20}) || u.Total.Download != 9000 {
		t.Fatalf("old version-1 ledger needs a destructive migration: %+v", u)
	}
	a.recordUsageSample(views, nil, true, now.Add(time.Second))
	a.recordUsageSample(views, nil, true, now.Add(6*time.Second))
	if u := a.usageViews[1].Usage; !u.ProxyAvailable || !u.ProxySpeedAvailable || u.Available || u.Proxy != (BytePair{}) || u.ProxySpeed != (BytePair{}) {
		t.Fatalf("valid idle proxy sample misrepresented as unavailable: %+v", u)
	}
}
func TestLogCleanupRetainsTailOpenFileAndUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "engine.log")
	old := filepath.Join(dir, "old.log")
	config := filepath.Join(dir, "config.yaml")
	archive := filepath.Join(dir, "archive")
	content := bytes.Repeat([]byte("old log line\n"), 1200)
	content = append(content, []byte("KEEP THIS LAST LINE\n")...)
	os.WriteFile(active, content, 0600)
	os.WriteFile(old, []byte("expired"), 0600)
	os.Chtimes(old, time.Now().AddDate(0, 0, -8), time.Now().AddDate(0, 0, -8))
	os.WriteFile(config, []byte("rules unchanged"), 0600)
	writer, e := os.OpenFile(active, os.O_WRONLY|os.O_APPEND, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer writer.Close()
	r := maintainLogs([]LogTarget{{active, "engine", 4096, 1024, false}, {old, "legacy", 4096, 1024, true}}, archive, time.Now(), func(p string) (bool, bool) { return p == active, true })
	writer.Write([]byte("STILL WRITING\n"))
	b, _ := os.ReadFile(active)
	c, _ := os.ReadFile(config)
	if !r.Changed || r.Freed == 0 || exists(old) || len(b) > 1100 || !bytes.Contains(b, []byte("KEEP THIS LAST LINE")) || !bytes.Contains(b, []byte("STILL WRITING")) || string(c) != "rules unchanged" || len(managedArchives(archive)) != 1 {
		t.Fatal("unsafe cleanup", r, len(b))
	}
	r = maintainLogs([]LogTarget{{active, "engine", 5, 2, false}}, archive, time.Now(), func(string) (bool, bool) { return true, false })
	if r.Changed || r.Error == "" {
		t.Fatal("non-append writer must be preserved")
	}
}
func TestArchiveCapAndCorruptLedger(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t)
	os.WriteFile(filepath.Join(a.store, "traffic.json"), []byte("broken"), 0600)
	a.initUsage()
	if a.usage.Devices == nil || a.usage.Started == "" {
		t.Fatal("bad ledger prevented recovery")
	}
	for _, name := range []string{"engine-1.log.gz", "engine-2.log.gz", "unrelated.txt"} {
		os.WriteFile(filepath.Join(dir, name), bytes.Repeat([]byte("x"), 6<<20), 0600)
	}
	r := maintainLogs(nil, dir, time.Now(), func(string) (bool, bool) { return false, true })
	if r.Total > 10<<20 || !exists(filepath.Join(dir, "unrelated.txt")) {
		t.Fatal("archive cap or ownership", r)
	}
	data, _ := json.Marshal(a.usage)
	if !strings.Contains(string(data), "started") {
		t.Fatal("missing persistence metadata")
	}
}
