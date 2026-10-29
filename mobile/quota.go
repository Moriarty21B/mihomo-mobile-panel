package main

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Provider billing is independent of LAN usage and Mihomo sampled connections.
type Quota struct {
	Upload    uint64 `json:"upload"`
	Download  uint64 `json:"download"`
	Total     uint64 `json:"total"`
	Remaining uint64 `json:"remaining"`
	Full      bool   `json:"full"`
	Available bool   `json:"available"`
	Unlimited bool   `json:"unlimited"`
	Expire    string `json:"expire,omitempty"`
	ResetDays int    `json:"resetDays,omitempty"`
	Updated   string `json:"updated"`
	Source    string `json:"source"`
}

var remainingPattern = regexp.MustCompile(`剩余流量[：:\s]*([0-9]+(?:\.[0-9]+)?)\s*([KMGT]?B)`)
var expiryPattern = regexp.MustCompile(`(?:套餐到期|到期时间)[：:\s]*(\d{4}-\d{2}-\d{2})`)
var resetPattern = regexp.MustCompile(`重置[^0-9]*([0-9]+)\s*天`)

func quotaNode(name string) bool {
	return remainingPattern.MatchString(name) || expiryPattern.MatchString(name) || resetPattern.MatchString(name)
}
func parseQuota(header string, body []byte, now time.Time) *Quota {
	q := &Quota{Updated: now.Format(time.RFC3339), Source: "订阅信息"}
	values := map[string]uint64{}
	for _, item := range strings.Split(header, ";") {
		kv := strings.SplitN(strings.TrimSpace(item), "=", 2)
		if len(kv) != 2 {
			continue
		}
		if v, e := strconv.ParseUint(strings.TrimSpace(kv[1]), 10, 64); e == nil {
			values[strings.ToLower(kv[0])] = v
		}
	}
	_, up := values["upload"]
	_, down := values["download"]
	_, total := values["total"]
	if up && down && total {
		q.Upload, q.Download, q.Total = values["upload"], values["download"], values["total"]
		q.Full, q.Available = true, true
		q.Unlimited = q.Total == 0
		q.Source = "机场返回的套餐数据"
		// Subtraction avoids overflow and clamps exhausted packages to zero.
		if q.Upload < q.Total && q.Download < q.Total-q.Upload {
			q.Remaining = q.Total - q.Upload - q.Download
		}
	}
	if t := values["expire"]; t > 0 && t < 253402300800 {
		q.Expire = time.Unix(int64(t), 0).Format(time.RFC3339)
	}
	var data struct {
		Proxies []struct {
			Name string `yaml:"name"`
		} `yaml:"proxies"`
	}
	yaml.Unmarshal(body, &data)
	for _, p := range data.Proxies {
		if !q.Available {
			if m := remainingPattern.FindStringSubmatch(p.Name); len(m) > 0 {
				v, e := strconv.ParseFloat(m[1], 64)
				factor := float64(1)
				for _, unit := range []string{"KB", "MB", "GB", "TB"} {
					factor *= 1024
					if m[2] == unit {
						break
					}
				}
				if m[2] == "B" {
					factor = 1
				}
				if e == nil && v*factor < 1<<63 {
					q.Remaining = uint64(v * factor)
					q.Available = true
				}
			}
		}
		if q.Expire == "" {
			if m := expiryPattern.FindStringSubmatch(p.Name); len(m) > 0 {
				q.Expire = m[1]
			}
		}
		if m := resetPattern.FindStringSubmatch(p.Name); len(m) > 0 {
			q.ResetDays, _ = strconv.Atoi(m[1])
		}
	}
	return q
}
func (a *App) refreshQuota() error {
	if p := a.profileSnapshot(); p != nil {
		return a.refreshProfileQuota(p.ActiveSub)
	}
	s := a.snapshot()
	if s.URL == "" {
		return errors.New("请先在订阅页保存链接")
	}
	_, q, e := downloadSubscription(s.URL)
	if e != nil {
		return errors.New("暂时无法刷新套餐流量，已保留上次数据；代理配置没有改动")
	}
	if !q.Available {
		return errors.New("订阅未提供套餐流量信息，已保留上次数据")
	}
	s.Quota = q
	if e = a.save(s); e != nil {
		return errors.New("套餐数据已读取，但保存失败；请检查存储空间")
	}
	return nil
}
func (a *App) quotaMonitor() {
	// Cache is always displayed immediately; refresh does not restart the core.
	for {
		if p := a.profileSnapshot(); p != nil {
			// At most one saved airport is fetched in a minute. Failed responses
			// also have a checked timestamp, avoiding repeated requests every tick.
			for _, sub := range p.Subscriptions {
				checked, _ := time.Parse(time.RFC3339, sub.Checked)
				if !checked.IsZero() && time.Since(checked) < 30*time.Minute {
					continue
				}
				if len(a.gate) == 0 {
					_, q, e := a.fetchProfile(sub.URL)
					a.saveBackgroundProfileQuota(p, sub.ID, q, e)
				}
				break
			}
			time.Sleep(time.Minute)
			continue
		}
		s := a.snapshot()
		due := s.Quota == nil
		if s.Quota != nil {
			t, _ := time.Parse(time.RFC3339, s.Quota.Updated)
			due = time.Since(t) > 30*time.Minute
		}
		if due && s.URL != "" {
			// A slow quota server must not prevent turning the proxy off or repairing rules.
			if len(a.gate) == 0 {
				_, q, e := downloadSubscription(s.URL)
				if e == nil && q.Available {
					a.saveBackgroundQuota(s, q)
				}
			}
		}
		time.Sleep(time.Minute)
	}
}

func (a *App) saveBackgroundQuota(before State, q *Quota) {
	select {
	case a.gate <- struct{}{}:
		defer func() { <-a.gate }()
		release, err := acquireManagerLock()
		if err != nil {
			return
		}
		defer release()
		current := a.snapshot()
		if current.URL != before.URL || current.Updated != before.Updated {
			return
		}
		current.Quota = q
		_ = a.save(current)
	default:
	}
}
