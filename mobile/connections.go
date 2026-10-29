package main

import (
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// These rows share the five-second /connections sample with traffic accounting.
// Direct LAN traffic which never enters Mihomo is intentionally not inferred.
type RuntimeConnection struct {
	ID              string   `json:"id"`
	MAC             string   `json:"mac"`
	Device          string   `json:"device"`
	SourceIP        string   `json:"sourceIP"`
	Host            string   `json:"host"`
	DestinationIP   string   `json:"destinationIP"`
	DestinationPort string   `json:"destinationPort"`
	Network         string   `json:"network"`
	Rule            string   `json:"rule"`
	RulePayload     string   `json:"rulePayload"`
	Chains          []string `json:"chains"`
	Node            string   `json:"node"`
	Proxy           bool     `json:"proxy"`
	Upload          uint64   `json:"upload"`
	Download        uint64   `json:"download"`
	UploadSpeed     *uint64  `json:"uploadSpeed"`
	DownloadSpeed   *uint64  `json:"downloadSpeed"`
	Start           string   `json:"start"`
}

type runtimeConnectionSample struct {
	Updated time.Time
	OK      bool
	Limited bool
	Rows    []RuntimeConnection
}

type mihomoConnection struct {
	ID          string   `json:"id"`
	Upload      uint64   `json:"upload"`
	Download    uint64   `json:"download"`
	Start       string   `json:"start"`
	Chains      []string `json:"chains"`
	Rule        string   `json:"rule"`
	RulePayload string   `json:"rulePayload"`
	Metadata    struct {
		SourceIP        string          `json:"sourceIP"`
		Host            string          `json:"host"`
		DestinationIP   string          `json:"destinationIP"`
		DestinationPort json.RawMessage `json:"destinationPort"`
		Network         string          `json:"network"`
	} `json:"metadata"`
}

func connectionSpeed(current, previous uint64, seconds float64) *uint64 {
	if seconds <= 0 || current < previous {
		return nil
	}
	v := uint64(float64(current-previous) / seconds)
	return &v
}

func (a *App) recordRuntimeConnections(body []byte, apiError error, devices []DeviceView, now time.Time) {
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	if apiError != nil {
		a.runtimeConnections.OK = false
		return
	}
	var data struct {
		Connections *[]mihomoConnection `json:"connections"`
	}
	if len(body) > 16<<20 || json.Unmarshal(body, &data) != nil || data.Connections == nil {
		a.runtimeConnections.OK = false
		return
	}
	previous := make(map[string]RuntimeConnection, len(a.runtimeConnections.Rows))
	for _, row := range a.runtimeConnections.Rows {
		previous[row.ID] = row
	}
	elapsed := now.Sub(a.runtimeConnections.Updated).Seconds()
	continuous := a.runtimeConnections.OK && elapsed > 0 && elapsed < 20
	ipMAC := onlineIPMAC(devices)
	byMAC := map[string]DeviceView{}
	for _, d := range devices {
		byMAC[d.MAC] = d
	}
	rows := make([]RuntimeConnection, 0, len(*data.Connections))
	limited := false
	for _, raw := range *data.Connections {
		if raw.ID == "" || !validLANIP(raw.Metadata.SourceIP) {
			continue
		}
		if len(rows) >= 10000 {
			limited = true
			break
		}
		old, seen := previous[raw.ID]
		mac := ipMAC[raw.Metadata.SourceIP]
		if seen && old.MAC != "" {
			mac = old.MAC
		} // Preserve owner when DHCP reuses an IP.
		device := byMAC[mac]
		name := strings.TrimSpace(device.Note)
		if name == "" {
			name = device.Name
		}
		if name == "" && seen {
			name = old.Device
		}
		if name == "" {
			name = raw.Metadata.SourceIP
		}
		port := strings.Trim(string(raw.Metadata.DestinationPort), "\"")
		if n, e := strconv.Atoi(port); e != nil || n < 0 || n > 65535 {
			port = ""
		}
		host := raw.Metadata.Host
		if host == "" {
			host = raw.Metadata.DestinationIP
		}
		proxy := proxyConnection(raw.Chains)
		node := "DIRECT"
		if len(raw.Chains) > 0 {
			node = raw.Chains[0]
		}
		row := RuntimeConnection{ID: raw.ID, MAC: mac, Device: name, SourceIP: raw.Metadata.SourceIP, Host: host, DestinationIP: raw.Metadata.DestinationIP, DestinationPort: port, Network: raw.Metadata.Network, Rule: raw.Rule, RulePayload: raw.RulePayload, Chains: append([]string{}, raw.Chains...), Node: node, Proxy: proxy, Upload: raw.Upload, Download: raw.Download, Start: raw.Start}
		if continuous && seen && old.MAC == mac {
			row.UploadSpeed = connectionSpeed(raw.Upload, old.Upload, elapsed)
			row.DownloadSpeed = connectionSpeed(raw.Download, old.Download, elapsed)
		}
		rows = append(rows, row)
	}
	a.runtimeConnections = runtimeConnectionSample{Updated: now, OK: true, Limited: limited, Rows: rows}
}

func (a *App) connectionsView(q url.Values) (map[string]any, error) {
	mac := strings.TrimSpace(q.Get("mac"))
	if mac != "" {
		var err error
		mac, err = canonicalMAC(mac)
		if err != nil {
			return nil, err
		}
	}
	a.runtimeMu.RLock()
	sample := a.runtimeConnections
	rows := append([]RuntimeConnection{}, sample.Rows...)
	a.runtimeMu.RUnlock()
	observed := true
	reason := "仅显示进入代理服务的连接；绕过代理的设备连接不在此列表。"
	cfg := a.deviceSnapshot()
	if cfg == nil || !cfg.Enabled || a.recoveryDegraded() {
		observed = false
		reason = "当前为普通直连，代理服务不能观察设备的完整联网活动。"
	} else if mac != "" {
		if d, ok := cfg.Devices[mac]; ok && d.Policy == "direct" {
			observed = false
			reason = "此设备完全直连；连接绕过代理服务，无法在此查看完整记录。"
		}
	}
	needle := strings.ToLower(strings.TrimSpace(q.Get("q")))
	filtered := make([]RuntimeConnection, 0, len(rows))
	for _, row := range rows {
		if mac != "" && row.MAC != mac {
			continue
		}
		if q.Get("proxyOnly") == "1" && !row.Proxy {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(row.Device+" "+row.Host+" "+row.SourceIP+" "+row.DestinationIP+" "+row.Node+" "+row.Rule+" "+row.RulePayload+" "+strings.Join(row.Chains, " ")), needle) {
			continue
		}
		filtered = append(filtered, row)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		a, b := filtered[i], filtered[j]
		switch q.Get("sort") {
		case "bytes":
			if a.Upload+a.Download != b.Upload+b.Download {
				return a.Upload+a.Download > b.Upload+b.Download
			}
		case "device":
			if a.Device != b.Device {
				return a.Device < b.Device
			}
		default:
			var av, bv uint64
			if a.UploadSpeed != nil {
				av += *a.UploadSpeed
			}
			if a.DownloadSpeed != nil {
				av += *a.DownloadSpeed
			}
			if b.UploadSpeed != nil {
				bv += *b.UploadSpeed
			}
			if b.DownloadSpeed != nil {
				bv += *b.DownloadSpeed
			}
			if av != bv {
				return av > bv
			}
		}
		return a.ID < b.ID
	})
	total := len(filtered)
	if len(filtered) > 2000 {
		filtered = filtered[:2000]
	}
	updated := ""
	if !sample.Updated.IsZero() {
		updated = sample.Updated.Format(time.RFC3339)
	}
	available := sample.OK && time.Since(sample.Updated) < 20*time.Second
	if !available && observed {
		reason = "连接采样暂不可用，显示最近一次记录；网速未知时显示 —。"
	}
	return map[string]any{"available": available, "updated": updated, "observed": observed, "reason": reason, "connections": filtered, "total": total, "limited": sample.Limited || total > len(filtered), "interval": 5}, nil
}

func (a *App) closeConnection(id string) error {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 128 || strings.ContainsAny(id, "/\\\r\n?#") {
		return errors.New("连接编号无效，请刷新列表")
	}
	a.runtimeMu.RLock()
	found := false
	for _, row := range a.runtimeConnections.Rows {
		if row.ID == id {
			found = true
			break
		}
	}
	a.runtimeMu.RUnlock()
	if !found {
		return errors.New("该连接已结束，请刷新列表")
	}
	_, err := a.apiTimeout("DELETE", "/connections/"+url.PathEscape(id), nil, 3*time.Second)
	return err
}
