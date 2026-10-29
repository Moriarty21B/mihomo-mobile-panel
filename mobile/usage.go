package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type BytePair struct {
	Download uint64 `json:"download"`
	Upload   uint64 `json:"upload"`
}

func (p *BytePair) add(d, u uint64) { p.Download += d; p.Upload += u }

type UsageEntry struct {
	Baseline          BytePair            `json:"baseline"`
	HasBaseline       bool                `json:"hasBaseline"`
	HasProxyBaseline  bool                `json:"hasProxyBaseline,omitempty"`
	Boot              string              `json:"boot"`
	Total             BytePair            `json:"total"`
	Proxy             BytePair            `json:"proxy"`
	Days              map[string]BytePair `json:"days"`
	Months            map[string]BytePair `json:"months"`
	ProxyDays         map[string]BytePair `json:"proxyDays"`
	ProxyMonths       map[string]BytePair `json:"proxyMonths"`
	ProxyObservedDays map[string]bool     `json:"proxyObservedDays,omitempty"`
	LastSeen          string              `json:"lastSeen"`
}
type UsageLedger struct {
	Version int                    `json:"version"`
	Started string                 `json:"started"`
	Saved   string                 `json:"saved"`
	Devices map[string]*UsageEntry `json:"devices"`
	Charts  *TrafficChartLedger    `json:"charts,omitempty"`
}
type DeviceUsage struct {
	Total               BytePair `json:"total"`
	Today               BytePair `json:"today"`
	Month               BytePair `json:"month"`
	Proxy               BytePair `json:"proxy"`
	ProxyToday          BytePair `json:"proxyToday"`
	ProxyMonth          BytePair `json:"proxyMonth"`
	Available           bool     `json:"available"`
	ProxyAvailable      bool     `json:"proxyAvailable"`
	ProxySpeed          BytePair `json:"proxySpeed"`
	ProxySpeedAvailable bool     `json:"proxySpeedAvailable"`
}
type ConnectionCursor struct {
	Download, Upload uint64
	Seen             time.Time
	MAC              string
	Proxy            bool
}
type Connection struct {
	ID       string    `json:"id"`
	Download uint64    `json:"download"`
	Upload   uint64    `json:"upload"`
	Start    time.Time `json:"start"`
	Chains   []string  `json:"chains"`
	Metadata struct {
		SourceIP string `json:"sourceIP"`
	} `json:"metadata"`
}

func newUsageEntry() *UsageEntry {
	return &UsageEntry{Days: map[string]BytePair{}, Months: map[string]BytePair{}, ProxyDays: map[string]BytePair{}, ProxyMonths: map[string]BytePair{}}
}
func (l *UsageLedger) entry(mac string) *UsageEntry {
	if l.Devices[mac] == nil {
		l.Devices[mac] = newUsageEntry()
	}
	e := l.Devices[mac]
	if e.Days == nil {
		e.Days = map[string]BytePair{}
	}
	if e.Months == nil {
		e.Months = map[string]BytePair{}
	}
	if e.ProxyDays == nil {
		e.ProxyDays = map[string]BytePair{}
	}
	if e.ProxyMonths == nil {
		e.ProxyMonths = map[string]BytePair{}
	}
	if e.ProxyObservedDays == nil {
		e.ProxyObservedDays = map[string]bool{}
	}
	return e
}
func usageKeys(t time.Time) (string, string) {
	t = t.In(time.FixedZone("Asia/Shanghai", 8*3600))
	return t.Format("2006-01-02"), t.Format("2006-01")
}
func addPeriod(m map[string]BytePair, k string, d, u uint64) { v := m[k]; v.add(d, u); m[k] = v }
func counterDelta(current, last uint64) uint64 {
	if current >= last {
		return current - last
	}
	return current
}
func (l *UsageLedger) sampleNative(views []DeviceView, boot string, now time.Time) {
	day, month := usageKeys(now)
	for _, v := range views {
		e := l.entry(v.MAC)
		if !v.Online || v.Traffic == nil {
			continue
		}
		current := BytePair{v.Traffic.Download, v.Traffic.Upload}
		if e.HasBaseline {
			d, u := counterDelta(current.Download, e.Baseline.Download), counterDelta(current.Upload, e.Baseline.Upload)
			e.Total.add(d, u)
			addPeriod(e.Days, day, d, u)
			addPeriod(e.Months, month, d, u)
		}
		e.Baseline = current
		e.Boot = boot
		e.HasBaseline = true
		e.LastSeen = now.Format(time.RFC3339)
	}
}
func onlineIPMAC(views []DeviceView) map[string]string {
	ipMAC := map[string]string{}
	for _, v := range views {
		if v.Online && validLANIP(v.IP) {
			old, seen := ipMAC[v.IP]
			if seen && old != v.MAC {
				ipMAC[v.IP] = ""
			} else if !seen {
				ipMAC[v.IP] = v.MAC
			}
		}
	}
	return ipMAC
}

func proxyConnection(chains []string) bool {
	if len(chains) == 0 {
		return false
	}
	for _, chain := range chains {
		switch strings.ToUpper(strings.TrimSpace(chain)) {
		case "DIRECT", "REJECT", "REJECT-DROP", "PASS", "COMPATIBLE":
			return false
		}
	}
	return true
}

func (a *App) sampleConnections(conns []Connection, views []DeviceView, now time.Time) map[string]BytePair {
	if a.usageConnections == nil {
		a.usageConnections = map[string]ConnectionCursor{}
	}
	ipMAC := onlineIPMAC(views)
	deltas := map[string]BytePair{}
	day, month := usageKeys(now)
	started := a.usageSessionStarted
	for _, c := range conns {
		if c.ID == "" {
			continue
		}
		old, seen := a.usageConnections[c.ID]
		mac := old.MAC
		if mac == "" {
			mac = ipMAC[c.Metadata.SourceIP]
		}
		proxy := proxyConnection(c.Chains)
		// Keep the connection's original owner even when its old IP is reused.
		// Baseline every exit, including DIRECT, so a later classification change
		// cannot import earlier direct traffic into the proxy totals.
		a.usageConnections[c.ID] = ConnectionCursor{Download: c.Download, Upload: c.Upload, Seen: now, MAC: mac, Proxy: proxy}
		if mac == "" || !proxy {
			continue
		}
		d, u := uint64(0), uint64(0)
		if seen && old.Proxy && old.MAC != "" {
			d, u = counterDelta(c.Download, old.Download), counterDelta(c.Upload, old.Upload)
		} else if !seen && !c.Start.IsZero() && !c.Start.Before(started) {
			d, u = c.Download, c.Upload
		}
		e := a.usage.entry(mac)
		e.HasProxyBaseline = true
		e.Proxy.add(d, u)
		addPeriod(e.ProxyDays, day, d, u)
		addPeriod(e.ProxyMonths, month, d, u)
		addPeriod(deltas, mac, d, u)
	}
	for id, c := range a.usageConnections {
		if now.Sub(c.Seen) > 2*time.Minute {
			delete(a.usageConnections, id)
		}
	}
	return deltas
}
func (a *App) initUsage() {
	b, _ := os.ReadFile(filepath.Join(a.store, "traffic.json"))
	var l UsageLedger
	if json.Unmarshal(b, &l) != nil || l.Version != 1 || l.Devices == nil {
		l = UsageLedger{Version: 1, Started: time.Now().Format(time.RFC3339), Devices: map[string]*UsageEntry{}}
	}
	a.usage = &l
	a.usageSessionStarted = time.Now()
	b, _ = os.ReadFile("/proc/sys/kernel/random/boot_id")
	a.usageBoot = strings.TrimSpace(string(b))
}
func (a *App) usagePoll() {
	views := a.inventory()
	var conns struct {
		Connections *[]Connection `json:"connections"`
	}
	b, e := a.apiTimeout("GET", "/connections", nil, 2*time.Second)
	a.recordRuntimeConnections(b, e, views, time.Now())
	proxyOK := e == nil && json.Unmarshal(b, &conns) == nil && conns.Connections != nil
	var connections []Connection
	if proxyOK {
		connections = *conns.Connections
	}
	a.recordUsageSample(views, connections, proxyOK, time.Now())
}

func (a *App) recordUsageSample(views []DeviceView, conns []Connection, proxyOK bool, now time.Time) {
	a.usageMu.Lock()
	defer a.usageMu.Unlock()
	a.usageProxyOK = proxyOK
	a.usage.sampleNative(views, a.usageBoot, now)
	deltas := map[string]BytePair{}
	elapsed := now.Sub(a.usageProxyLastSample).Seconds()
	a.usageProxySpeedReady = proxyOK && !a.usageProxyLastSample.IsZero() && elapsed > 0
	if proxyOK {
		deltas = a.sampleConnections(conns, views, now)
		a.usageProxyLastSample = now
	}
	ipMAC := onlineIPMAC(views)
	day, month := usageKeys(now)
	for i := range views {
		e := a.usage.entry(views[i].MAC)
		mapped := views[i].Online && validLANIP(views[i].IP) && ipMAC[views[i].IP] == views[i].MAC
		if proxyOK && mapped {
			e.HasProxyBaseline = true
		}
		usage := &DeviceUsage{Total: e.Total, Today: e.Days[day], Month: e.Months[month], Proxy: e.Proxy, ProxyToday: e.ProxyDays[day], ProxyMonth: e.ProxyMonths[month], Available: e.HasBaseline, ProxyAvailable: e.HasProxyBaseline || e.Proxy.Download > 0 || e.Proxy.Upload > 0, ProxySpeedAvailable: a.usageProxySpeedReady && mapped}
		if usage.ProxySpeedAvailable {
			delta := deltas[views[i].MAC]
			usage.ProxySpeed = BytePair{Download: uint64(float64(delta.Download) / elapsed), Upload: uint64(float64(delta.Upload) / elapsed)}
		}
		views[i].Usage = usage
	}
	a.recordTrafficChartSample(proxyOK, now)
	a.usageViews = views
	a.usageUpdated = now.Format(time.RFC3339)
}
func (a *App) saveUsage(now time.Time) {
	a.usageFileMu.Lock()
	defer a.usageFileMu.Unlock()
	a.usageMu.Lock()
	defer a.usageMu.Unlock()
	if a.usage == nil {
		return
	}
	dayLimit, _ := usageKeys(now.AddDate(0, 0, -90))
	_, monthLimit := usageKeys(now.AddDate(-1, 0, 0))
	for _, e := range a.usage.Devices {
		for k := range e.ProxyObservedDays {
			if k < dayLimit {
				delete(e.ProxyObservedDays, k)
			}
		}
		for _, m := range []map[string]BytePair{e.Days, e.ProxyDays} {
			for k := range m {
				if k < dayLimit {
					delete(m, k)
				}
			}
		}
		for _, m := range []map[string]BytePair{e.Months, e.ProxyMonths} {
			for k := range m {
				if k < monthLimit {
					delete(m, k)
				}
			}
		}
	}
	a.usage.pruneTrafficCharts(now)
	a.usage.Saved = now.Format(time.RFC3339)
	b, _ := json.Marshal(a.usage)
	a.usageError = ""
	if atomicWrite(filepath.Join(a.store, "traffic.json"), b) != nil {
		a.usageError = "流量统计暂时无法保存，请检查存储空间；网络规则未受影响"
	}
}
func (a *App) usageMonitor() {
	a.usagePoll()
	a.saveUsage(time.Now())
	checkpoint := time.Now()
	for range time.Tick(5 * time.Second) {
		a.usagePoll()
		if time.Since(checkpoint) >= 5*time.Minute {
			a.saveUsage(time.Now())
			checkpoint = time.Now()
		}
	}
}
func (a *App) usageResponse() ([]DeviceView, map[string]any) {
	a.usageMu.RLock()
	defer a.usageMu.RUnlock()
	views := append([]DeviceView(nil), a.usageViews...)
	started := ""
	if a.usage != nil {
		started = a.usage.Started
	}
	return views, map[string]any{"started": started, "updated": a.usageUpdated, "error": a.usageError, "interval": 5, "proxySampled": true, "proxyAvailable": a.usageProxyOK, "proxySpeedAvailable": a.usageProxySpeedReady}
}
