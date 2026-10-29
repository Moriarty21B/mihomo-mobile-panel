package main

import (
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Charts use the very same MAC-owned proxy counters as the device cards. The
// native LAN counters never enter this ledger. Earlier daily totals are useful
// history, but they cannot reconstruct an intraday curve retroactively.
type TrafficChartSnapshot struct {
	At     string              `json:"at"`
	Values map[string]BytePair `json:"values,omitempty"`
	Gap    bool                `json:"gap,omitempty"`
}

type TrafficChartDay struct {
	Baseline *TrafficChartSnapshot           `json:"baseline,omitempty"`
	Buckets  map[string]TrafficChartSnapshot `json:"buckets"`
	Gaps     map[string]string               `json:"gaps,omitempty"`
}

type TrafficChartLedger struct {
	Version int                         `json:"version"`
	Started string                      `json:"started"`
	Last    string                      `json:"last"`
	LastOK  bool                        `json:"lastOK"`
	Days    map[string]*TrafficChartDay `json:"days"`
}

type TrafficChartPoint struct {
	At      string `json:"at"`
	Label   string `json:"label"`
	Partial bool   `json:"partial,omitempty"`
	Gap     bool   `json:"gap,omitempty"`
}

type TrafficChartSeries struct {
	MAC       string    `json:"mac,omitempty"`
	Name      string    `json:"name,omitempty"`
	Online    bool      `json:"online"`
	Policy    string    `json:"policy,omitempty"`
	Values    []*uint64 `json:"values"`
	Download  []*uint64 `json:"download"`
	Upload    []*uint64 `json:"upload"`
	Used      uint64    `json:"used"`
	DownSpeed *uint64   `json:"downSpeed"`
	UpSpeed   *uint64   `json:"upSpeed"`
}

type TrafficChartView struct {
	Range           string               `json:"range"`
	Unit            string               `json:"unit"`
	Cumulative      bool                 `json:"cumulative"`
	Interval        int                  `json:"interval"`
	BucketMinutes   int                  `json:"bucketMinutes"`
	Updated         string               `json:"updated"`
	Started         string               `json:"started"`
	CoverageStarted string               `json:"coverageStarted"`
	Available       bool                 `json:"available"`
	Sampled         bool                 `json:"sampled"`
	Error           string               `json:"error,omitempty"`
	Points          []TrafficChartPoint  `json:"points"`
	Total           TrafficChartSeries   `json:"total"`
	Devices         []TrafficChartSeries `json:"devices"`
}

var trafficZone = time.FixedZone("Asia/Shanghai", 8*3600)

func trafficTimestamp(t time.Time) string { return t.In(trafficZone).Format(time.RFC3339) }
func trafficBucket(t time.Time) string {
	t = t.In(trafficZone)
	return strconv.Itoa((t.Hour()*60 + t.Minute()) / 5)
}

func (l *UsageLedger) chartLedger() *TrafficChartLedger {
	if l.Charts == nil || l.Charts.Version != 1 || l.Charts.Days == nil {
		l.Charts = &TrafficChartLedger{Version: 1, Days: map[string]*TrafficChartDay{}}
	}
	return l.Charts
}

func (l *UsageLedger) chartDay(day string) *TrafficChartDay {
	c := l.chartLedger()
	if c.Days[day] == nil {
		c.Days[day] = &TrafficChartDay{}
	}
	d := c.Days[day]
	if d.Buckets == nil {
		d.Buckets = map[string]TrafficChartSnapshot{}
	}
	if d.Gaps == nil {
		d.Gaps = map[string]string{}
	}
	return d
}

func (l *UsageLedger) markChartGap(at time.Time) {
	day, _ := usageKeys(at)
	d := l.chartDay(day)
	bucket := trafficBucket(at)
	stamp := trafficTimestamp(at)
	if d.Gaps[bucket] == "" {
		d.Gaps[bucket] = stamp
	}
	d.Buckets[bucket] = TrafficChartSnapshot{At: stamp, Gap: true}
}

// Called with usageMu held, after sampleConnections and availability have been
// updated. Each bucket holds its most recent actual observation; the first
// observation is separately preserved so installation never invents midnight.
func (a *App) recordTrafficChartSample(ok bool, now time.Time) {
	if a.usage == nil {
		return
	}
	l := a.usage
	c := l.chartLedger()
	if previous, err := time.Parse(time.RFC3339, c.Last); err == nil && now.Sub(previous) > 15*time.Second {
		l.markChartGap(previous.Add(5 * time.Second))
	}
	day, _ := usageKeys(now)
	stamp := trafficTimestamp(now)
	if !ok {
		l.markChartGap(now)
		c.Last, c.LastOK = stamp, false
		l.pruneTrafficCharts(now)
		return
	}
	values := map[string]BytePair{}
	for mac, e := range l.Devices {
		if !e.HasProxyBaseline && e.Proxy == (BytePair{}) {
			if _, recorded := e.ProxyDays[day]; !recorded {
				continue
			}
		}
		// A successful complete /connections response also observes genuine
		// zero use by already known devices, including devices now offline.
		if e.ProxyObservedDays == nil {
			e.ProxyObservedDays = map[string]bool{}
		}
		e.ProxyObservedDays[day] = true
		values[mac] = e.ProxyDays[day]
	}
	s := TrafficChartSnapshot{At: stamp, Values: values}
	d := l.chartDay(day)
	if d.Baseline == nil {
		baseline := s
		d.Baseline = &baseline
	}
	d.Buckets[trafficBucket(now)] = s
	if c.Started == "" {
		c.Started = stamp
	}
	c.Last, c.LastOK = stamp, true
	l.pruneTrafficCharts(now)
}

func (l *UsageLedger) pruneTrafficCharts(now time.Time) {
	if l.Charts == nil {
		return
	}
	limit, _ := usageKeys(now.In(trafficZone).AddDate(0, 0, -6))
	for day, d := range l.Charts.Days {
		if day < limit || d == nil {
			delete(l.Charts.Days, day)
			continue
		}
		for key := range d.Buckets {
			n, err := strconv.Atoi(key)
			if err != nil || n < 0 || n >= 288 {
				delete(d.Buckets, key)
			}
		}
		for key := range d.Gaps {
			n, err := strconv.Atoi(key)
			if err != nil || n < 0 || n >= 288 {
				delete(d.Gaps, key)
			}
		}
	}
}

func uintPointer(v uint64) *uint64 { return &v }

func emptyChartSeries(mac, name string, n int) TrafficChartSeries {
	return TrafficChartSeries{MAC: mac, Name: name, Values: make([]*uint64, n), Download: make([]*uint64, n), Upload: make([]*uint64, n)}
}

func setChartValue(s *TrafficChartSeries, index int, pair BytePair) {
	s.Download[index] = uintPointer(pair.Download)
	s.Upload[index] = uintPointer(pair.Upload)
	s.Values[index] = uintPointer(pair.Download + pair.Upload)
}

func firstChartName(r DeviceRecord) string {
	for _, name := range []string{r.Note, r.Name, r.Hostname, r.MAC} {
		if name = strings.TrimSpace(name); name != "" {
			return name
		}
	}
	return "未知设备"
}

func (a *App) trafficView(r string) (any, error) { return a.trafficViewAt(r, time.Now()) }

func (a *App) trafficViewAt(r string, now time.Time) (*TrafficChartView, error) {
	if r == "" {
		r = "today"
	}
	if r != "today" && r != "7d" && r != "30d" {
		return nil, errors.New("请选择今日、近 7 天或近 30 天")
	}
	// Do not query Xiaomi firmware or Mihomo from a chart request. All chart
	// responses use the existing five-second sampler and saved MAC settings.
	config := a.deviceSnapshot()
	a.usageMu.RLock()
	defer a.usageMu.RUnlock()
	v := &TrafficChartView{Range: r, Unit: "bytes", Cumulative: r == "today", Interval: 5, BucketMinutes: 5, Updated: a.usageUpdated, Available: a.usageProxyOK, Sampled: true, Error: a.usageError, Points: []TrafficChartPoint{}, Devices: []TrafficChartSeries{}}
	l := a.usage
	if l == nil {
		v.Total = emptyChartSeries("", "全部设备", 0)
		v.Available = false
		v.Error = "流量统计正在准备，请稍后刷新"
		return v, nil
	}
	v.Started = l.Started
	if l.Charts != nil {
		v.CoverageStarted = l.Charts.Started
	}
	if sampledAt, err := time.Parse(time.RFC3339, a.usageUpdated); err != nil || now.Sub(sampledAt) > 20*time.Second {
		v.Available = false
	}
	if !v.Available && v.Error == "" {
		v.Error = "代理流量采样暂不可用，保留上次统计"
	}
	macs := make([]string, 0, len(l.Devices))
	for mac := range l.Devices {
		macs = append(macs, mac)
	}
	sort.Strings(macs)
	views := make(map[string]DeviceView, len(a.usageViews))
	for _, d := range a.usageViews {
		views[d.MAC] = d
	}
	var snapshots []TrafficChartSnapshot
	var dayKeys []string
	day, _ := usageKeys(now)
	if r == "today" {
		if l.Charts != nil {
			if d := l.Charts.Days[day]; d != nil {
				byTime := map[string]TrafficChartSnapshot{}
				if d.Baseline != nil {
					byTime[d.Baseline.At] = *d.Baseline
				}
				for _, s := range d.Buckets {
					byTime[s.At] = s
				}
				for _, at := range d.Gaps {
					// Preserve an actual baseline if the first failed sample had
					// exactly the same timestamp; otherwise show a real gap.
					if _, present := byTime[at]; !present {
						byTime[at] = TrafficChartSnapshot{At: at, Gap: true}
					}
				}
				for _, s := range byTime {
					snapshots = append(snapshots, s)
				}
				sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].At < snapshots[j].At })
			}
		}
		for _, s := range snapshots {
			label := ""
			if t, err := time.Parse(time.RFC3339, s.At); err == nil {
				label = t.In(trafficZone).Format("15:04")
			}
			v.Points = append(v.Points, TrafficChartPoint{At: s.At, Label: label, Gap: s.Gap, Partial: true})
		}
	} else {
		n := 7
		if r == "30d" {
			n = 30
		}
		today := now.In(trafficZone)
		today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, trafficZone)
		for i := n - 1; i >= 0; i-- {
			d := today.AddDate(0, 0, -i)
			key, _ := usageKeys(d)
			dayKeys = append(dayKeys, key)
			v.Points = append(v.Points, TrafficChartPoint{At: trafficTimestamp(d), Label: d.Format("01-02"), Partial: i == 0})
		}
	}
	v.Total = emptyChartSeries("", "全部设备", len(v.Points))
	ledgerStartDay := ""
	if t, err := time.Parse(time.RFC3339, l.Started); err == nil {
		ledgerStartDay, _ = usageKeys(t)
	}
	for _, mac := range macs {
		e := l.Devices[mac]
		r := DeviceRecord{MAC: mac}
		if d, found := views[mac]; found {
			r = d.DeviceRecord
		}
		if config != nil {
			if saved, found := config.Devices[mac]; found {
				r = saved
			}
		}
		s := emptyChartSeries(mac, firstChartName(r), len(v.Points))
		s.Policy = r.Policy
		if d, found := views[mac]; found {
			s.Online = d.Online
			if v.Available && d.Usage != nil && d.Usage.ProxySpeedAvailable {
				s.DownSpeed = uintPointer(d.Usage.ProxySpeed.Download)
				s.UpSpeed = uintPointer(d.Usage.ProxySpeed.Upload)
			}
		}
		if v.Cumulative {
			for index, p := range snapshots {
				if value, found := p.Values[mac]; found && !p.Gap {
					setChartValue(&s, index, value)
				}
			}
			todayPair := e.ProxyDays[day]
			s.Used = todayPair.Download + todayPair.Upload
		} else {
			for index, key := range dayKeys {
				if ledgerStartDay != "" && key < ledgerStartDay {
					continue
				}
				pair, present := e.ProxyDays[key]
				if present || e.ProxyObservedDays[key] {
					setChartValue(&s, index, pair)
					s.Used += pair.Download + pair.Upload
				} else {
					v.Points[index].Partial = true
				}
			}
		}
		v.Total.Used += s.Used
		v.Devices = append(v.Devices, s)
	}
	for index := range v.Points {
		pair, known := BytePair{}, false
		for _, s := range v.Devices {
			if s.Values[index] != nil {
				known = true
				pair.add(*s.Download[index], *s.Upload[index])
			}
		}
		if known {
			setChartValue(&v.Total, index, pair)
		}
	}
	if v.Available && a.usageProxySpeedReady {
		down, up := uint64(0), uint64(0)
		for _, s := range v.Devices {
			if s.DownSpeed != nil && s.UpSpeed != nil {
				down += *s.DownSpeed
				up += *s.UpSpeed
			}
		}
		v.Total.DownSpeed, v.Total.UpSpeed = uintPointer(down), uintPointer(up)
	}
	return v, nil
}
