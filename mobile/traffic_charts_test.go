package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func chartTestApp(t *testing.T, now time.Time) *App {
	t.Helper()
	a := newTestApp(t)
	a.initUsage()
	a.usage.Started = trafficTimestamp(now)
	a.usageSessionStarted = now
	return a
}

func assertChartConservation(t *testing.T, v *TrafficChartView) {
	t.Helper()
	used := uint64(0)
	for _, d := range v.Devices {
		used += d.Used
	}
	if used != v.Total.Used {
		t.Fatalf("total used %d != device sum %d", v.Total.Used, used)
	}
	for i := range v.Points {
		download, upload, known := uint64(0), uint64(0), false
		for _, d := range v.Devices {
			if d.Values[i] == nil {
				continue
			}
			known = true
			download += *d.Download[i]
			upload += *d.Upload[i]
			if *d.Values[i] != *d.Download[i]+*d.Upload[i] {
				t.Fatal("device value differs from its two byte directions")
			}
		}
		if !known {
			if v.Total.Values[i] != nil {
				t.Fatal("unknown interval became zero traffic")
			}
		} else if v.Total.Values[i] == nil || *v.Total.Values[i] != download+upload || *v.Total.Download[i] != download || *v.Total.Upload[i] != upload {
			t.Fatalf("point %d total differs from device sum", i)
		}
	}
}

func TestTrafficChartsRealtimeProxyOnlyAndConservation(t *testing.T) {
	now := time.Date(2001, 10, 23, 10, 3, 0, 0, trafficZone)
	a := chartTestApp(t, now)
	computer, phone := "02:00:00:00:00:01", "02:00:00:00:00:02"
	views := []DeviceView{
		{DeviceRecord: DeviceRecord{MAC: computer, Name: "电脑"}, Online: true, IP: "192.168.31.10", Traffic: &DeviceTraffic{Download: 1 << 30}},
		{DeviceRecord: DeviceRecord{MAC: phone, Name: "手机"}, Online: true, IP: "192.168.31.20"},
	}
	direct := Connection{ID: "steam", Start: now, Download: 10 << 30, Chains: []string{"DIRECT", selectGroup}}
	direct.Metadata.SourceIP = views[0].IP
	c1 := Connection{ID: "gpt", Start: now, Download: 200, Upload: 40, Chains: []string{"台湾04", selectGroup}}
	c1.Metadata.SourceIP = views[0].IP
	c2 := Connection{ID: "tiktok", Start: now, Download: 500, Upload: 100, Chains: []string{"新加坡01", selectGroup}}
	c2.Metadata.SourceIP = views[1].IP
	a.recordUsageSample(views, []Connection{direct, c1, c2}, true, now)
	c1.Download += 1000
	c2.Download += 2000
	direct.Download += 10 << 30
	views[0].Traffic.Download += 10 << 30
	a.recordUsageSample(views, []Connection{direct, c1, c2}, true, now.Add(5*time.Second))
	v, err := a.trafficViewAt("today", now.Add(5*time.Second))
	if err != nil || !v.Available || !v.Cumulative || len(v.Points) != 2 || v.Total.Used != 3840 {
		t.Fatalf("wrong actual observations, source, or refresh: %+v %v", v, err)
	}
	last := len(v.Points) - 1
	if *v.Total.Values[last] != v.Total.Used || v.Total.DownSpeed == nil || *v.Total.DownSpeed != 600 {
		t.Fatal("head is not current cumulative proxy use/rate")
	}
	assertChartConservation(t, v)
	if v.Points[0].Label != "10:03" || v.CoverageStarted != trafficTimestamp(now) {
		t.Fatal("intraday observations invented an earlier start")
	}
}

func TestTrafficChartsMigrationPreservesDailyCountersWithoutInventedCurve(t *testing.T) {
	now := time.Date(2001, 10, 23, 10, 3, 0, 0, trafficZone)
	a := chartTestApp(t, now)
	mac := "02:00:00:00:00:01"
	old := []byte(`{"version":1,"started":"2001-10-06T08:00:00+08:00","devices":{"02:00:00:00:00:01":{"proxy":{"download":700,"upload":20},"proxyDays":{"2001-10-21":{"download":100,"upload":5},"2001-10-23":{"download":200,"upload":10}},"total":{"download":9000000,"upload":90}}}}`)
	if err := os.WriteFile(filepath.Join(a.store, "traffic.json"), old, 0600); err != nil {
		t.Fatal(err)
	}
	a.initUsage()
	before, _ := a.trafficViewAt("today", now)
	if len(before.Points) != 0 || before.Total.Used != 210 {
		t.Fatal("daily history was fabricated into an intraday line", before)
	}
	views := []DeviceView{{DeviceRecord: DeviceRecord{MAC: mac}, Online: true, IP: "192.168.31.10"}}
	a.recordUsageSample(views, nil, true, now)
	v, _ := a.trafficViewAt("today", now)
	if len(v.Points) != 1 || *v.Total.Values[0] != 210 || v.Points[0].At != trafficTimestamp(now) || a.usage.Devices[mac].Proxy.Download != 700 {
		t.Fatal("migration lost totals or added a fake baseline", v)
	}
	days, _ := a.trafficViewAt("7d", now)
	if len(days.Points) != 7 || days.Total.Values[0] != nil || *days.Total.Values[4] != 105 || *days.Total.Values[6] != 210 {
		t.Fatal("known daily history or missing-day semantics changed", days)
	}
	assertChartConservation(t, days)
	a.saveUsage(now)
	b := newTestApp(t)
	b.store = a.store
	b.initUsage()
	if b.usage.Version != 1 || b.usage.Charts == nil || b.usage.Devices[mac].Proxy != a.usage.Devices[mac].Proxy || b.usage.Devices[mac].Total.Download != 9000000 {
		t.Fatal("optional chart persistence reset the old ledger")
	}
}

func TestTrafficChartsFailureGapIdleZeroAndRestartGap(t *testing.T) {
	now := time.Date(2001, 10, 23, 10, 3, 0, 0, trafficZone)
	a := chartTestApp(t, now)
	mac := "02:00:00:00:00:01"
	views := []DeviceView{{DeviceRecord: DeviceRecord{MAC: mac}, Online: true, IP: "192.168.31.10"}}
	a.recordUsageSample(views, nil, true, now)
	a.recordUsageSample(views, nil, true, now.Add(5*time.Second))
	idle, _ := a.trafficViewAt("today", now.Add(5*time.Second))
	if !idle.Available || idle.Total.Values[1] == nil || *idle.Total.Values[1] != 0 {
		t.Fatal("observed idle zero is not a known zero")
	}
	a.recordUsageSample(views, nil, false, now.Add(10*time.Second))
	failed, _ := a.trafficViewAt("today", now.Add(10*time.Second))
	if failed.Available || failed.Error == "" || failed.Total.Values[len(failed.Points)-1] != nil || failed.Total.DownSpeed != nil {
		t.Fatal("failed sample became fresh zero usage or rate", failed)
	}
	a.recordUsageSample(views, nil, true, now.Add(15*time.Second))
	recovered, _ := a.trafficViewAt("today", now.Add(15*time.Second))
	if !recovered.Available || len(recovered.Points) != 3 || !recovered.Points[1].Gap || recovered.Total.Values[1] != nil || recovered.Total.Values[2] == nil {
		t.Fatal("recovery erased the sampling gap", recovered)
	}
	a.saveUsage(now.Add(15 * time.Second))
	b := newTestApp(t)
	b.store = a.store
	b.initUsage()
	b.recordUsageSample(views, nil, true, now.Add(time.Hour))
	restarted, _ := b.trafficViewAt("today", now.Add(time.Hour))
	gap := false
	for _, p := range restarted.Points {
		if p.At == trafficTimestamp(now.Add(20*time.Second)) && p.Gap {
			gap = true
		}
	}
	if !gap {
		t.Fatal("panel downtime was silently connected as a known interval")
	}
	stale, _ := b.trafficViewAt("today", now.Add(time.Hour+30*time.Second))
	if stale.Available || stale.Total.DownSpeed != nil {
		t.Fatal("stopped sampler presented live measurements")
	}
	assertChartConservation(t, restarted)
}

func TestTrafficChartsBeijingMidnightOfflineHistoryAndMACNames(t *testing.T) {
	now := time.Date(2001, 10, 23, 15, 59, 55, 0, time.UTC) // 23:59:55 Beijing.
	a := chartTestApp(t, now)
	mac := "02:00:00:00:00:01"
	views := []DeviceView{{DeviceRecord: DeviceRecord{MAC: mac, Name: "旧主机名"}, Online: true, IP: "192.168.31.10"}}
	c := Connection{ID: "stable-owner", Start: now, Download: 100, Chains: []string{"香港01", selectGroup}}
	c.Metadata.SourceIP = views[0].IP
	a.recordUsageSample(views, []Connection{c}, true, now)
	c.Download = 170
	views[0].Online = false
	views[0].IP = ""
	a.recordUsageSample(views, []Connection{c}, true, now.Add(10*time.Second))
	a.deviceConfig = &DeviceConfig{Devices: map[string]DeviceRecord{mac: {MAC: mac, Note: "我的电脑", Policy: "rule"}}}
	v, _ := a.trafficViewAt("today", now.Add(10*time.Second))
	if len(v.Points) != 1 || v.Total.Used != 70 || v.Points[0].Label != "00:00" || v.Devices[0].Name != "我的电脑" || v.Devices[0].Online {
		t.Fatal("midnight, offline ownership, or saved MAC name is wrong", v)
	}
	days, _ := a.trafficViewAt("7d", now.Add(10*time.Second))
	if *days.Total.Values[5] != 100 || *days.Total.Values[6] != 70 {
		t.Fatal("daily bytes used UTC day or lost offline history", days)
	}
	assertChartConservation(t, days)
}

func TestTrafficChartsBoundedStorageAndBeforeCoverageNull(t *testing.T) {
	now := time.Date(2001, 10, 23, 12, 0, 0, 0, trafficZone)
	a := chartTestApp(t, now.AddDate(0, 0, -9))
	views := []DeviceView{{DeviceRecord: DeviceRecord{MAC: "02:00:00:00:00:01"}, Online: true, IP: "192.168.31.10"}}
	for day := 9; day >= 0; day-- {
		start := now.AddDate(0, 0, -day)
		for tick := 0; tick < 1000; tick++ {
			a.recordUsageSample(views, nil, tick%2 == 0, start.Add(time.Duration(tick)*5*time.Second))
		}
	}
	if len(a.usage.Charts.Days) > 7 {
		t.Fatal("intraday days are unbounded")
	}
	for _, day := range a.usage.Charts.Days {
		if len(day.Buckets) > 288 || len(day.Gaps) > 288 {
			t.Fatal("rapid failures create an unbounded journal")
		}
	}
	v, _ := a.trafficViewAt("30d", now)
	if len(v.Points) != 30 || v.Total.Values[0] != nil || v.Devices[0].Values[0] != nil {
		t.Fatal("pre-install days became zero traffic")
	}
	assertChartConservation(t, v)
	if _, err := a.trafficViewAt("all", now); err == nil {
		t.Fatal("invalid period silently selected a different one")
	}
	b, err := json.Marshal(v)
	if err != nil || len(b) == 0 {
		t.Fatal("chart JSON could not be rendered")
	}
}

func TestTrafficChartsCounterResetAndReloadDoNotRecount(t *testing.T) {
	now := time.Date(2001, 10, 23, 12, 0, 0, 0, trafficZone)
	a := chartTestApp(t, now)
	views := []DeviceView{{DeviceRecord: DeviceRecord{MAC: "02:00:00:00:00:01"}, Online: true, IP: "192.168.31.10"}}
	c := Connection{ID: "resettable", Start: now, Download: 200, Upload: 10, Chains: []string{"台湾01", selectGroup}}
	c.Metadata.SourceIP = views[0].IP
	a.recordUsageSample(views, []Connection{c}, true, now)
	c.Download, c.Upload = 30, 2
	a.recordUsageSample(views, []Connection{c}, true, now.Add(5*time.Second))
	v, _ := a.trafficViewAt("today", now.Add(5*time.Second))
	if v.Total.Used != 242 || *v.Total.Values[len(v.Points)-1] != 242 {
		t.Fatal("reset wrapped or discarded real post-reset counters")
	}
	a.saveUsage(now.Add(5 * time.Second))
	b := newTestApp(t)
	b.store = a.store
	b.initUsage()
	b.usageSessionStarted = now.Add(10 * time.Second)
	c.Download, c.Upload = 60, 4
	b.recordUsageSample(views, []Connection{c}, true, now.Add(15*time.Second))
	first, _ := b.trafficViewAt("today", now.Add(15*time.Second))
	if first.Total.Used != 242 {
		t.Fatal("panel restart reimported existing connection bytes")
	}
	c.Download, c.Upload = 70, 6
	b.recordUsageSample(views, []Connection{c}, true, now.Add(20*time.Second))
	second, _ := b.trafficViewAt("today", now.Add(20*time.Second))
	if second.Total.Used != 254 || *second.Total.Values[len(second.Points)-1] != 254 {
		t.Fatal("post-restart chart lost subsequent deltas")
	}
	assertChartConservation(t, second)
}
