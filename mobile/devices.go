package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

const deviceChain = "MIHOMO_DEVICE"

type DeviceRecord struct {
	MAC      string `json:"mac"`
	Name     string `json:"name"`
	Hostname string `json:"hostname"`
	Note     string `json:"note"`
	Kind     string `json:"kind"`
	Policy   string `json:"policy"`
	LastIP   string `json:"lastIP"`
	Manual   bool   `json:"manual"`
}
type DeviceConfig struct {
	Ignored  map[string]bool         `json:"ignored,omitempty"`
	Version  int                     `json:"version"`
	Enabled  bool                    `json:"enabled"`
	Revision int                     `json:"revision"`
	Devices  map[string]DeviceRecord `json:"devices"`
}
type DeviceView struct {
	DeviceRecord
	Traffic   *DeviceTraffic `json:"traffic,omitempty"`
	Usage     *DeviceUsage   `json:"usage,omitempty"`
	IP        string         `json:"ip"`
	Online    bool           `json:"online"`
	Presence  string         `json:"presence"`
	RandomMAC bool           `json:"randomMAC"`
	Sources   []string       `json:"sources"`
	Pinned    bool           `json:"pinned"`
	Archived  bool           `json:"archived"`
}
type DeviceTraffic struct {
	Download  uint64 `json:"download"`
	Upload    uint64 `json:"upload"`
	DownSpeed uint64 `json:"downSpeed"`
	UpSpeed   uint64 `json:"upSpeed"`
}

// Xiaomi firmware versions return decimal counters as either strings or numbers.
// Missing/invalid data stays unavailable instead of being presented as zero usage.
func parseDeviceTraffic(raw map[string]json.RawMessage) *DeviceTraffic {
	values := make([]uint64, 4)
	for i, key := range []string{"download", "upload", "downspeed", "upspeed"} {
		s := strings.Trim(string(raw[key]), "\"")
		v, e := strconv.ParseUint(s, 10, 64)
		if e != nil {
			return nil
		}
		values[i] = v
	}
	return &DeviceTraffic{values[0], values[1], values[2], values[3]}
}

type DeviceEdit struct {
	MAC    string `json:"mac"`
	Policy string `json:"policy"`
	Note   string `json:"note"`
	Kind   string `json:"kind"`
}
type OwnedRules struct {
	Roots     []string `json:"roots"`
	Positions []int    `json:"positions"`
	Chain     []string `json:"chain"`
	Exists    bool     `json:"exists"`
}
type DeviceJournal struct {
	Previous *DeviceConfig `json:"previous"`
	Rules    OwnedRules    `json:"rules"`
}

func canonicalMAC(s string) (string, error) {
	m, e := net.ParseMAC(strings.TrimSpace(s))
	if e != nil || len(m) != 6 || m[0]&1 != 0 || bytes.Equal(m, make([]byte, 6)) {
		return "", errors.New("设备 MAC 地址无效")
	}
	return strings.ToUpper(m.String()), nil
}
func validLANIP(s string) bool {
	p := net.ParseIP(s).To4()
	return p != nil && p[0] == 192 && p[1] == 168 && p[2] == 31 && p[3] > 1 && p[3] < 255
}
func deviceKind(name, _ string) string {
	n := strings.ToLower(name)
	for _, s := range []string{"phone", "xiaomi-phone", "redmi-note", "galaxy", "pixel", "手机"} {
		if strings.Contains(n, s) {
			return "phone"
		}
	}
	for _, s := range []string{"ipad", "pad", "平板"} {
		if strings.Contains(n, s) {
			return "tablet"
		}
	}
	for _, s := range []string{"miaisound", "soundbox", "speaker", "小爱", "音箱"} {
		if strings.Contains(n, s) {
			return "speaker"
		}
	}
	for _, s := range []string{"television", "mitv", "mibox", "电视", "tvbox", "android-tv"} {
		if strings.Contains(n, s) {
			return "tv"
		}
	}
	for _, s := range []string{"yeelink", "-light-", "lamp", "plug", "chuangmi", "camera", "vacuum", "roborock", "zhimi", "lumi.", "miair", "米家", "智能", "摄像", "扫地", "插座", "xiaomi-light", "_mibt", "_miap", "dmaker-", "-airp-", "cokit-"} {
		if strings.Contains(n, s) {
			return "iot"
		}
	}
	for _, s := range []string{"desktop-", "laptop-", "电脑"} {
		if strings.Contains(n, s) {
			return "computer"
		}
	}
	return "unknown"
}
func defaultPolicy(kind string) string {
	switch kind {
	case "computer", "speaker", "iot", "tv":
		return "direct"
	default:
		return "rule"
	}
}
func validKind(s string) bool {
	for _, v := range []string{"computer", "phone", "tablet", "speaker", "iot", "tv", "unknown"} {
		if s == v {
			return true
		}
	}
	return false
}
func (a *App) deviceSnapshot() *DeviceConfig {
	a.deviceMu.RLock()
	defer a.deviceMu.RUnlock()
	if a.deviceConfig == nil {
		return nil
	}
	b, _ := json.Marshal(a.deviceConfig)
	var c DeviceConfig
	json.Unmarshal(b, &c)
	return &c
}
func (a *App) setDeviceConfig(c *DeviceConfig) {
	a.deviceMu.Lock()
	a.deviceConfig = c
	a.deviceMu.Unlock()
}
func (a *App) setDeviceError(e error) {
	a.deviceMu.Lock()
	defer a.deviceMu.Unlock()
	a.deviceError = ""
	if e != nil {
		a.deviceError = e.Error()
	}
}
func boundedCommand(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

// Firmware names, current DHCP leases and neighbor reachability complement each other.
func buildInventory(saved *DeviceConfig, firmware, leases, neighbors, arp []byte) []DeviceView {
	found := map[string]*DeviceView{}
	leaseIP := map[string]string{}
	leaseExpiry := map[string]int64{}
	get := func(raw string) *DeviceView {
		mac, e := canonicalMAC(raw)
		if e != nil {
			return nil
		}
		if saved != nil && saved.Ignored[mac] {
			return nil
		}
		if found[mac] == nil {
			r := DeviceRecord{MAC: mac, Kind: "unknown", Policy: "rule"}
			if saved != nil {
				if old, ok := saved.Devices[mac]; ok {
					r = old
				}
			}
			parsed, _ := net.ParseMAC(mac)
			found[mac] = &DeviceView{DeviceRecord: r, Presence: "offline", RandomMAC: parsed[0]&2 != 0, Sources: []string{}}
		}
		return found[mac]
	}
	source := func(v *DeviceView, s string) {
		for _, old := range v.Sources {
			if old == s {
				return
			}
		}
		v.Sources = append(v.Sources, s)
	}
	if saved != nil {
		for mac := range saved.Devices {
			get(mac)
		}
	}
	var rows []struct {
		MAC        string                     `json:"mac"`
		IP         string                     `json:"ip"`
		Name       string                     `json:"name"`
		Hostname   string                     `json:"hostname"`
		Origin     string                     `json:"origin_name"`
		Online     int                        `json:"online"`
		Statistics map[string]json.RawMessage `json:"statistics"`
	}
	json.Unmarshal(firmware, &rows)
	for _, r := range rows {
		v := get(r.MAC)
		if v == nil {
			continue
		}
		source(v, "路由器")
		v.Traffic = parseDeviceTraffic(r.Statistics)
		if r.Hostname != "" && r.Hostname != "*" {
			v.Hostname = r.Hostname
		}
		name := r.Name
		if _, e := net.ParseMAC(name); e == nil {
			name = ""
		}
		if name == "" {
			name = r.Origin
		}
		if name != "" {
			v.Name = name
		}
		if r.Online == 1 && validLANIP(r.IP) {
			v.IP = r.IP
			v.LastIP = r.IP
			v.Online = true
			v.Presence = "online"
		}
	}
	for _, l := range strings.Split(string(leases), "\n") {
		f := strings.Fields(l)
		if len(f) < 4 || !validLANIP(f[2]) {
			continue
		}
		v := get(f[1])
		if v == nil {
			continue
		}
		source(v, "DHCP")
		expires, _ := strconv.ParseInt(f[0], 10, 64)
		if expires == 0 {
			expires = 1 << 62
		}
		if old, ok := leaseExpiry[v.MAC]; ok && old > expires {
			continue
		}
		leaseExpiry[v.MAC] = expires
		leaseIP[v.MAC] = f[2]
		if f[3] != "*" {
			v.Hostname = f[3]
			if v.Name == "" {
				v.Name = f[3]
			}
		}
		v.LastIP = f[2]
		if v.IP == "" {
			v.IP = f[2]
		}
	}
	for _, l := range strings.Split(string(arp), "\n") {
		f := strings.Fields(l)
		if len(f) < 6 || f[5] != "br-lan" || f[2] != "0x2" || !validLANIP(f[0]) {
			continue
		}
		v := get(f[3])
		if v == nil {
			continue
		}
		source(v, "ARP")
		if v.IP == "" {
			v.IP = f[0]
		}
		if v.Presence == "offline" && len(v.Sources) == 1 {
			v.Presence = "unknown"
		}
	}
	activeLease := map[string]bool{}
	for _, l := range strings.Split(string(neighbors), "\n") {
		f := strings.Fields(l)
		if len(f) < 4 {
			continue
		}
		state := f[len(f)-1]
		if state != "REACHABLE" && state != "DELAY" && state != "PROBE" {
			continue
		}
		for i := 0; i < len(f)-1; i++ {
			if f[i] == "lladdr" {
				mac, e := canonicalMAC(f[i+1])
				if e == nil && leaseIP[mac] == f[0] {
					activeLease[mac] = true
				}
			}
		}
	}
	for _, l := range strings.Split(string(neighbors), "\n") {
		f := strings.Fields(l)
		if len(f) < 4 || !validLANIP(f[0]) {
			continue
		}
		var mac string
		for i := 0; i < len(f)-1; i++ {
			if f[i] == "lladdr" {
				mac = f[i+1]
			}
		}
		if mac == "" {
			continue
		}
		v := get(mac)
		if v == nil {
			continue
		}
		source(v, "邻居表")
		state := f[len(f)-1]
		if state == "REACHABLE" || state == "DELAY" || state == "PROBE" {
			if activeLease[v.MAC] && f[0] != leaseIP[v.MAC] {
				continue
			}
			v.IP = f[0]
			v.LastIP = f[0]
			v.Online = true
			v.Presence = "online"
		} else if v.IP == "" {
			v.IP = f[0]
		}
	}
	result := []DeviceView{}
	for _, v := range found {
		if v.Name == "" {
			v.Name = v.Hostname
		}
		if v.Name == "" {
			v.Name = "未命名设备"
		}
		if saved == nil || saved.Devices[v.MAC].MAC == "" {
			v.Kind = deviceKind(v.Name+" "+v.Hostname, v.MAC)
			v.Policy = defaultPolicy(v.Kind)
		}
		if !v.Online {
			v.IP = ""
			if v.Traffic != nil {
				v.Traffic.DownSpeed = 0
				v.Traffic.UpSpeed = 0
			}
		}
		result = append(result, *v)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Online != result[j].Online {
			return result[i].Online
		}
		return result[i].Name < result[j].Name
	})
	return result
}
func (a *App) inventory() []DeviceView {
	if a.inventoryHook != nil {
		return a.inventoryHook()
	}
	// The display list omits some Ethernet clients. The traffic table includes them.
	firmware, _ := boundedCommand("lua", "-e", `local d=require("xiaoqiang.util.XQDeviceUtil"); local j=require("luci.json"); local rows=d.getDeviceList() or {};local found={};for _,v in ipairs(rows) do found[string.upper(v.mac or "")]=v end;local ok,stats=pcall(d.getDevNetStatisticsDict);if ok and type(stats)=="table" then for mac,s in pairs(stats) do local key=string.upper(mac);local v=found[key];if not v then v={mac=key,name=s.devname};table.insert(rows,v);found[key]=v end;v.statistics=s end end;print(j.encode(rows))`)
	leases, _ := os.ReadFile("/tmp/dhcp.leases")
	arp, _ := os.ReadFile("/proc/net/arp")
	neighbors, _ := boundedCommand("ip", "-4", "neigh", "show", "dev", "br-lan")
	return buildInventory(a.deviceSnapshot(), firmware, leases, neighbors, arp)
}
func learnedConfig(c *DeviceConfig, views []DeviceView) *DeviceConfig {
	if c == nil {
		c = &DeviceConfig{Version: 2, Devices: map[string]DeviceRecord{}}
	}
	for _, v := range views {
		r, ok := c.Devices[v.MAC]
		if !ok {
			r = v.DeviceRecord
		}
		r.Name = v.Name
		r.Hostname = v.Hostname
		if !r.Manual && r.Kind == "unknown" {
			kind := deviceKind(v.Name+" "+v.Hostname, v.MAC)
			if kind != "unknown" {
				r.Kind = kind
				r.Policy = defaultPolicy(kind)
			}
		}
		if v.IP != "" {
			r.LastIP = v.IP
		}
		c.Devices[v.MAC] = r
	}
	return c
}
func (a *App) devicesResponse() map[string]any {
	views, stats := a.usageResponse()
	if views == nil {
		views = a.inventory()
	}
	counts := map[string]int{"online": 0, "rule": 0, "direct": 0, "force": 0}
	c := a.deviceSnapshot()
	enabled := false
	revision := 0
	if c != nil {
		enabled = c.Enabled
		revision = c.Revision
	}
	for _, v := range views {
		if v.Online {
			counts["online"]++
			counts[v.Policy]++
		}
	}
	a.deviceMu.RLock()
	err := a.deviceError
	a.deviceMu.RUnlock()
	// Policy edits can complete between traffic samples; always return latest saved policy.
	if c != nil {
		for i := range views {
			if saved, ok := c.Devices[views[i].MAC]; ok {
				views[i].Policy, views[i].Note, views[i].Kind = saved.Policy, saved.Note, saved.Kind
			}
		}
	}
	a.decorateDeviceLayout(views)
	return map[string]any{"devices": views, "counts": counts, "enabled": enabled, "revision": revision, "ready": c != nil, "error": err, "statistics": stats}
}

func ownedRoot(l string) bool {
	if !strings.HasPrefix(l, "-A PREROUTING ") {
		return false
	}
	fields := strings.Fields(l)
	for i := 0; i < len(fields)-1; i++ {
		if fields[i] == "-j" && (fields[i+1] == deviceChain || fields[i+1] == "MIHOMO_TCP") {
			return true
		}
	}
	return strings.Contains(l, "--dport 53 ") && strings.Contains(l, "-j REDIRECT ") && strings.HasSuffix(l, "--to-ports 1053")
}
func parseOwned(all string) OwnedRules {
	r := OwnedRules{Roots: []string{}, Chain: []string{}}
	position := 0
	for _, l := range strings.Split(all, "\n") {
		if strings.HasPrefix(l, "-A PREROUTING ") {
			position++
		}
		if ownedRoot(l) {
			r.Roots = append(r.Roots, l)
			r.Positions = append(r.Positions, position)
		}
		if l == "-N "+deviceChain {
			r.Exists = true
		}
		if strings.HasPrefix(l, "-A "+deviceChain+" ") {
			r.Chain = append(r.Chain, l)
		}
	}
	return r
}
func (a *App) readOwned() (OwnedRules, error) {
	if a.ownedHook != nil {
		return a.ownedHook()
	}
	b, e := boundedCommand("iptables", "-t", "nat", "-S")
	return parseOwned(string(b)), e
}
func desiredOwned(c *DeviceConfig) OwnedRules {
	r := OwnedRules{Exists: true, Roots: []string{"-A PREROUTING -i br-lan -j " + deviceChain}, Positions: []int{1}, Chain: []string{}}
	if !c.Enabled {
		return r
	}
	macs := []string{}
	for mac := range c.Devices {
		if c.Ignored[mac] {
			continue
		}
		macs = append(macs, mac)
	}
	sort.Strings(macs)
	for _, mac := range macs {
		d := c.Devices[mac]
		prefix := "-A " + deviceChain + " -m mac --mac-source " + mac
		if d.Policy == "direct" {
			r.Chain = append(r.Chain, prefix+" -j RETURN")
		}
		if d.Policy == "force" {
			r.Chain = append(r.Chain, prefix+" -p udp --dport 53 -j REDIRECT --to-ports 1053", prefix+" -p tcp --dport 53 -j REDIRECT --to-ports 1053", prefix+" -p tcp -j MIHOMO_TCP")
		}
	}
	r.Chain = append(r.Chain, "-A "+deviceChain+" -p udp --dport 53 -j REDIRECT --to-ports 1053", "-A "+deviceChain+" -p tcp --dport 53 -j REDIRECT --to-ports 1053", "-A "+deviceChain+" -p tcp -j MIHOMO_TCP")
	return r
}

// No full-table flush. All owned changes commit together; Xiaomi/plugin chains stay untouched.
func restoreBatch(current, target OwnedRules) string {
	var lines = []string{"*nat"}
	if target.Exists {
		lines = append(lines, ":"+deviceChain+" - [0:0]")
	}
	for _, r := range current.Roots {
		lines = append(lines, strings.Replace(r, "-A PREROUTING ", "-D PREROUTING ", 1))
	}
	if current.Exists || target.Exists {
		lines = append(lines, "-F "+deviceChain)
	}
	lines = append(lines, target.Chain...)
	for i, r := range target.Roots {
		position := i + 1
		if i < len(target.Positions) {
			position = target.Positions[i]
		}
		lines = append(lines, strings.Replace(r, "-A PREROUTING ", fmt.Sprintf("-I PREROUTING %d ", position), 1))
	}
	if current.Exists && !target.Exists {
		lines = append(lines, "-X "+deviceChain)
	}
	lines = append(lines, "COMMIT", "")
	return strings.Join(lines, "\n")
}

func needsTCPChain(target OwnedRules) bool {
	for _, line := range append(append([]string{}, target.Roots...), target.Chain...) {
		if strings.Contains(line, "-j MIHOMO_TCP") {
			return true
		}
	}
	return false
}

// A reboot clears NAT chains. Recreate the missing TCP foundation in the same
// atomic transaction as device rules, before installing any redirecting entry.
func restoreBatchWithTCP(current, target OwnedRules, tcpExists bool) string {
	batch := restoreBatch(current, target)
	if tcpExists || !needsTCPChain(target) {
		return batch
	}
	lines := strings.Split(batch, "\n")
	insert := 1
	for insert < len(lines) && strings.HasPrefix(lines[insert], ":") {
		insert++
	}
	prefix := []string{"*nat", ":MIHOMO_TCP - [0:0]"}
	prefix = append(prefix, lines[1:insert]...)
	for _, cidr := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16", "224.0.0.0/4", "240.0.0.0/4"} {
		prefix = append(prefix, "-A MIHOMO_TCP -d "+cidr+" -j RETURN")
	}
	prefix = append(prefix, "-A MIHOMO_TCP -p tcp -j REDIRECT --to-ports 7892")
	return strings.Join(append(prefix, lines[insert:]...), "\n")
}

func (a *App) commitOwned(target OwnedRules) error {
	if a.commitHook != nil {
		return a.commitHook(target)
	}
	current, e := a.readOwned()
	if e != nil {
		return e
	}
	tcpExists := true
	if needsTCPChain(target) {
		_, err := boundedCommand("iptables", "-t", "nat", "-S", "MIHOMO_TCP")
		tcpExists = err == nil
	}
	batch := restoreBatchWithTCP(current, target, tcpExists)
	for _, test := range []bool{true, false} {
		args := []string{"--noflush", "--wait", "5"}
		if test {
			args = append(args, "--test")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, "iptables-restore", args...)
		cmd.Stdin = strings.NewReader(batch)
		out, e := cmd.CombinedOutput()
		cancel()
		if e != nil {
			return fmt.Errorf("网络规则未通过%s：%s", map[bool]string{true: "检查", false: "提交"}[test], strings.TrimSpace(string(out)))
		}
	}
	return nil
}
func (a *App) deviceHealth(enabled bool) error {
	if a.deviceHealthHook != nil {
		return a.deviceHealthHook(enabled)
	}
	if exists("/tmp/mihomo-mobile.fail-device-health-once") {
		os.Remove("/tmp/mihomo-mobile.fail-device-health-once")
		return errors.New("本次健康检查按测试要求模拟失败")
	}
	r, e := a.readOwned()
	if e != nil || !r.Exists || len(r.Roots) != 1 {
		return errors.New("设备策略链检查未通过")
	}
	iface, e := net.InterfaceByName("br-lan")
	if e != nil || iface.Flags&net.FlagUp == 0 {
		return errors.New("LAN 接口检查未通过")
	}
	conn, e := net.DialTimeout("tcp", "192.168.31.1:80", 2*time.Second)
	if e != nil {
		return errors.New("路由器 LAN 访问检查未通过")
	}
	conn.Close()
	// An off switch remains usable even when the engine itself is stopped.
	if enabled {
		core, e := boundedCommand("iptables", "-t", "nat", "-S", "MIHOMO_TCP")
		if e != nil || !bytes.Contains(core, []byte("-j REDIRECT --to-ports 7892")) {
			return errors.New("现有 TCP 代理链检查未通过")
		}
		if len(a.enginePIDs()) != 1 {
			return errors.New("Mihomo 进程检查未通过")
		}
		if _, e = a.sh("ready && has_port 9091 '^tcp'"); e != nil {
			return errors.New("7892、1053 或 9091 监听检查未通过")
		}
		if _, e = a.api("GET", "/version", nil); e != nil {
			return e
		}
	}
	return nil
}
func writeJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return atomicWrite(path, b)
}
func validateDeviceConfig(c *DeviceConfig) error {
	if c == nil || len(c.Devices) > 512 {
		return errors.New("设备数量或策略格式无效")
	}
	for mac, r := range c.Devices {
		m, e := canonicalMAC(mac)
		if e != nil || m != mac || r.MAC != mac {
			return errors.New("设备 MAC 地址无效")
		}
		if r.Policy != "rule" && r.Policy != "direct" && r.Policy != "force" {
			return errors.New("请选择有效的设备策略")
		}
		if !validKind(r.Kind) || len([]rune(r.Note)) > 48 {
			return errors.New("设备类型无效或备注超过 48 个字")
		}
	}
	return nil
}
func (a *App) applyDevices(next *DeviceConfig) error {
	if e := validateDeviceConfig(next); e != nil {
		return e
	}
	old := a.deviceSnapshot()
	before, e := a.readOwned()
	if e != nil {
		return e
	}
	// Port preflight occurs before touching any working NAT rule.
	if next.Enabled && a.deviceHealthHook == nil {
		if _, e = a.sh("ready && has_port 9091 '^tcp'"); e != nil {
			return errors.New("代理端口没有就绪，原策略已保留")
		}
	}
	if e = writeJSON(filepath.Join(a.store, "devices.previous.json"), old); e != nil {
		return fmt.Errorf("设备策略备份失败，未修改网络。请检查代理目录的实际分区：%w", e)
	}
	if e = writeJSON(filepath.Join(a.store, "devices-rules.previous.json"), before); e != nil {
		return errors.New("网络规则备份失败，未修改网络")
	}
	if !exists(filepath.Join(a.store, "devices-v1-rules.json")) {
		if e = writeJSON(filepath.Join(a.store, "devices-v1-rules.json"), before); e != nil {
			return e
		}
	}
	journal := DeviceJournal{Previous: old, Rules: before}
	pending := filepath.Join(a.store, "devices-pending.json")
	if e = writeJSON(pending, journal); e != nil {
		return e
	}
	rollback := func(cause error) error {
		re := a.commitOwned(before)
		var se error
		if old != nil {
			se = writeJSON(filepath.Join(a.store, "devices.json"), old)
		} else {
			se = os.Remove(filepath.Join(a.store, "devices.json"))
			if os.IsNotExist(se) {
				se = nil
			}
		}
		if re == nil && se == nil {
			a.setDeviceConfig(old)
			os.Remove(pending)
			return fmt.Errorf("应用失败，已自动恢复上一版本。%s", cause)
		}
		return errors.New("应用失败，恢复尚未完成；已保留备份，请使用恢复工具")
	}
	if e = a.commitOwned(desiredOwned(next)); e != nil {
		return rollback(e)
	}
	if e = a.deviceHealth(next.Enabled); e != nil {
		return rollback(e)
	}
	next.Version = 2
	next.Revision++
	if e = writeJSON(filepath.Join(a.store, "devices.json"), next); e != nil {
		return rollback(errors.New("策略保存失败"))
	}
	if e = os.Remove(pending); e != nil {
		return rollback(errors.New("保存确认失败"))
	}
	a.setDeviceConfig(next)
	a.setDeviceError(nil)
	a.clearChangedConnections(old, next)
	return nil
}
func (a *App) clearChangedConnections(old, next *DeviceConfig) {
	if a.inventoryHook != nil {
		return
	}
	for _, v := range a.inventory() {
		if !v.Online || !validLANIP(v.IP) {
			continue
		}
		before := "rule"
		wasOn := next.Enabled
		if old != nil {
			wasOn = old.Enabled
			if r, ok := old.Devices[v.MAC]; ok {
				before = r.Policy
			}
		}
		now := next.Devices[v.MAC].Policy
		if wasOn == next.Enabled && before == now {
			continue
		}
		// Delete only this device's previous transparent-proxy/DNS conntrack entries; no global flush.
		boundedCommand("conntrack", "-D", "-f", "ipv4", "-s", v.IP, "-p", "tcp", "--reply-port-src", "7892")
		for _, p := range []string{"udp", "tcp"} {
			boundedCommand("conntrack", "-D", "-f", "ipv4", "-s", v.IP, "-p", p, "--dport", "53")
		}
	}
}
func (a *App) recoverDevicePending() error {
	p := filepath.Join(a.store, "devices-pending.json")
	b, e := os.ReadFile(p)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	var j DeviceJournal
	if e = json.Unmarshal(b, &j); e != nil {
		return e
	}
	if e = a.commitOwned(j.Rules); e != nil {
		return e
	}
	if j.Previous != nil {
		if e = writeJSON(filepath.Join(a.store, "devices.json"), j.Previous); e != nil {
			return e
		}
	} else {
		os.Remove(filepath.Join(a.store, "devices.json"))
	}
	a.setDeviceConfig(j.Previous)
	return os.Remove(p)
}
func (a *App) initializeDevices() error {
	if e := a.recoverDevicePending(); e != nil {
		return e
	}
	var cfg *DeviceConfig
	if b, e := os.ReadFile(filepath.Join(a.store, "devices.json")); e == nil {
		if json.Unmarshal(b, &cfg) != nil || validateDeviceConfig(cfg) != nil {
			return errors.New("设备策略文件无法读取，保留当前网络")
		}
		a.setDeviceConfig(cfg)
	} else if !os.IsNotExist(e) {
		return e
	}
	views := a.inventory()
	if cfg == nil {
		r, e := a.rules()
		if e != nil {
			return e
		}
		cfg = &DeviceConfig{Version: 2, Enabled: r.Total > 0, Devices: map[string]DeviceRecord{}}
	}
	cfg = learnedConfig(cfg, views)
	return a.applyDevices(cfg)
}
func (a *App) deviceTick() {
	select {
	case a.gate <- struct{}{}:
	default:
		return
	}
	defer func() { <-a.gate }()
	release, e := acquireManagerLock()
	if e != nil {
		return
	}
	defer release()
	if a.recoveryDegraded() {
		_ = a.ensureRecoveryDirect()
		return
	}
	current := a.deviceSnapshot()
	if exists(filepath.Join(a.store, "devices-pending.json")) {
		if e := a.recoverDevicePending(); e != nil {
			a.setDeviceError(e)
			return
		}
		current = a.deviceSnapshot()
	}
	if current == nil {
		a.setDeviceError(a.initializeDevices())
		return
	}
	next := learnedConfig(a.deviceSnapshot(), a.inventory())
	owned, e := a.readOwned()
	if e != nil {
		a.setDeviceError(e)
		return
	}
	// iptables canonicalizes match order and adds '-m tcp/udp'; compare normalized forms.
	if !reflect.DeepEqual(next.Devices, current.Devices) || !sameOwned(owned, desiredOwned(current)) {
		a.setDeviceError(a.applyDevices(next))
	}
}
func normalizeRule(l string) string {
	l = strings.ReplaceAll(l, " -m tcp", "")
	l = strings.ReplaceAll(l, " -m udp", "")
	f := strings.Fields(l)
	parts := []string{}
	for i := 0; i < len(f); i += 2 {
		v := f[i]
		if i+1 < len(f) {
			v += " " + f[i+1]
		}
		parts = append(parts, v)
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}
func sameOwned(a, b OwnedRules) bool {
	if !reflect.DeepEqual(a.Positions, b.Positions) {
		return false
	}
	if a.Exists != b.Exists || len(a.Roots) != len(b.Roots) || len(a.Chain) != len(b.Chain) {
		return false
	}
	for i := range a.Roots {
		if normalizeRule(a.Roots[i]) != normalizeRule(b.Roots[i]) {
			return false
		}
	}
	for i := range a.Chain {
		if normalizeRule(a.Chain[i]) != normalizeRule(b.Chain[i]) {
			return false
		}
	}
	return true
}
func (a *App) saveDeviceEdits(edits []DeviceEdit, reset bool, revision int) error {
	c := a.deviceSnapshot()
	if c == nil {
		return errors.New("设备管理尚未准备好，请稍后重试")
	}
	if revision != c.Revision {
		return errors.New("设备列表已更新，请刷新后再保存")
	}
	c = learnedConfig(c, a.inventory())
	if reset {
		for mac, r := range c.Devices {
			r.Policy = defaultPolicy(r.Kind)
			r.Manual = false
			c.Devices[mac] = r
		}
	} else {
		seen := map[string]bool{}
		for _, edit := range edits {
			mac, e := canonicalMAC(edit.MAC)
			if e != nil {
				return e
			}
			r, ok := c.Devices[mac]
			if !ok || seen[mac] {
				return errors.New("设备不存在或重复，请刷新列表")
			}
			seen[mac] = true
			r.Policy = edit.Policy
			r.Note = strings.TrimSpace(edit.Note)
			r.Kind = edit.Kind
			r.Manual = true
			c.Devices[mac] = r
		}
	}
	return a.applyDevices(c)
}
func (a *App) setWholeHouse(enabled bool) error {
	c := a.deviceSnapshot()
	if c == nil {
		return errors.New("设备策略尚未准备好，保留当前网络")
	}
	c.Enabled = enabled
	return a.applyDevices(c)
}
func (a *App) suspendDevices() error {
	c := a.deviceSnapshot()
	if c == nil {
		_, e := a.sh("cleanup")
		return e
	}
	c.Enabled = false
	return a.commitOwned(desiredOwned(c))
}
func (a *App) resumeDevices(enabled bool) error {
	c := a.deviceSnapshot()
	if c == nil {
		if enabled {
			_, e := a.sh("safe_on")
			return e
		}
		return nil
	}
	c.Enabled = enabled
	return a.commitOwned(desiredOwned(c))
}

func (a *App) engineLogPath() string {
	for _, pid := range a.enginePIDs() {
		target, e := os.Readlink(fmt.Sprintf("/proc/%d/fd/1", pid))
		if e == nil && strings.HasPrefix(target, "/") {
			if st, e := os.Stat(target); e == nil && st.Mode().IsRegular() {
				return target
			}
		}
	}
	newest := ""
	var modified time.Time
	for _, p := range []string{"/tmp/mihomo-mobile-engine.log", filepath.Join(a.dir, "mihomo.log"), filepath.Join(a.dir, "manager.log")} {
		if st, e := os.Stat(p); e == nil && st.Mode().IsRegular() && st.ModTime().After(modified) {
			newest = p
			modified = st.ModTime()
		}
	}
	return newest
}
