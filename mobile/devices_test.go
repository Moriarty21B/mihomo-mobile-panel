package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Synthetic locally administered addresses; never copied from a real household.
const testComputerMAC = "02:00:00:00:00:01"

func testDevices() *DeviceConfig {
	return &DeviceConfig{Version: 2, Enabled: true, Devices: map[string]DeviceRecord{testComputerMAC: {MAC: testComputerMAC, Name: "Example-Desktop", Kind: "computer", Policy: "direct"}, "02:00:00:00:00:02": {MAC: "02:00:00:00:00:02", Name: "Example-Phone", Kind: "phone", Policy: "rule"}}}
}
func TestExplicitlyRetiredDeviceNotRediscovered(t *testing.T) {
	c := testDevices()
	c.Ignored = map[string]bool{testComputerMAC: true}
	views := buildInventory(c, nil, []byte("9999999999 02:00:00:00:00:01 192.168.31.10 Example-Desktop *"), nil, nil)
	for _, v := range views {
		if v.MAC == testComputerMAC {
			t.Fatal("retired device rediscovered")
		}
	}
	for _, rule := range desiredOwned(c).Chain {
		if strings.Contains(rule, testComputerMAC) {
			t.Fatal("retired device retained rules")
		}
	}
	if len(views) != 1 {
		t.Fatal("unrelated saved device lost")
	}
}
func TestDeviceInventoryMergesAndTracksIPChanges(t *testing.T) {
	cfg := testDevices()
	firmware := []byte(`[{"mac":"02:00:00:00:00:02","ip":"192.168.31.20","online":1,"hostname":"Example-Phone"}]`)
	leases := []byte("9999999999 02:00:00:00:00:02 192.168.31.21 Example-Phone *\n9999999999 02:00:00:00:00:01 192.168.31.10 Example-Desktop *")
	neighbors := []byte("192.168.31.21 lladdr 02:00:00:00:00:02 REACHABLE\n192.168.31.10 lladdr 02:00:00:00:00:01 REACHABLE")
	views := buildInventory(cfg, firmware, leases, neighbors, nil)
	for _, v := range views {
		if v.MAC == "02:00:00:00:00:02" {
			if v.IP != "192.168.31.21" || v.Policy != "rule" || !v.Online || !v.RandomMAC || len(v.Sources) != 3 {
				t.Fatal(v)
			}
		} else if v.MAC == testComputerMAC && v.Policy != "direct" {
			t.Fatal(v)
		}
	}
	before := desiredOwned(cfg)
	learned := learnedConfig(cfg, views)
	after := desiredOwned(learned)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("MAC-based rules changed on DHCP address change")
	}
	offline := buildInventory(learned, nil, nil, nil, nil)
	if len(offline) != 2 {
		t.Fatal("Lost offline policies")
	}
	for _, v := range offline {
		if v.Online || v.IP != "" {
			t.Fatal(v)
		}
	}
}

func TestDHCPNewestLeaseWinsOverOldNeighborCache(t *testing.T) {
	leases := []byte("9999999999 02:00:00:00:00:02 192.168.31.21 Phone *\n8888888888 02:00:00:00:00:02 192.168.31.20 Phone *")
	neighbors := []byte("192.168.31.21 lladdr 02:00:00:00:00:02 REACHABLE\n192.168.31.20 lladdr 02:00:00:00:00:02 REACHABLE")
	views := buildInventory(testDevices(), nil, leases, neighbors, nil)
	for _, v := range views {
		if v.MAC == "02:00:00:00:00:02" && v.IP != "192.168.31.21" {
			t.Fatal(v)
		}
	}
}
func TestSmartDefaultsAndPhoneException(t *testing.T) {
	for _, c := range []struct{ name, kind, policy string }{{"Example-Speaker", "speaker", "direct"}, {"yeelink-light-lamp", "iot", "direct"}, {"Example-Camera", "iot", "direct"}, {"MiTV", "tv", "direct"}, {"Example-Phone", "phone", "rule"}, {"Example-Pad", "tablet", "rule"}, {"iPhone", "phone", "rule"}, {"unknown", "unknown", "rule"}} {
		kind := deviceKind(c.name, "")
		if kind != c.kind || defaultPolicy(kind) != c.policy {
			t.Fatal(c, kind)
		}
	}
}
func TestNewDeviceHasNoBuiltInHouseholdIdentity(t *testing.T) {
	views := buildInventory(nil, nil, []byte("9999999999 02:00:00:00:00:01 192.168.31.10 Example-Device *"), nil, nil)
	if len(views) != 1 || views[0].Kind != "unknown" || views[0].Policy != "rule" || views[0].Note != "" {
		t.Fatal("a newly discovered device received a built-in identity or household-specific policy")
	}
}
func TestDeviceChainBypassBeforeDNSAndTCP(t *testing.T) {
	r := desiredOwned(testDevices())
	if len(r.Roots) != 1 || !r.Exists {
		t.Fatal(r)
	}
	if !strings.Contains(r.Chain[0], testComputerMAC+" -j RETURN") {
		t.Fatal("Direct device must return before both DNS and TCP rules")
	}
	if len(r.Chain) != 4 {
		t.Fatal(r)
	}
	all := strings.Join(r.Chain, "\n")
	if strings.Contains(all, "-s 192.") {
		t.Fatal("Policy pinned to an IP")
	}
	off := testDevices()
	off.Enabled = false
	if len(desiredOwned(off).Chain) != 0 {
		t.Fatal("Master off must disable all redirection")
	}
}
func TestOwnedSnapshotPreservesSystemRulesAndRootPositions(t *testing.T) {
	before := parseOwned("-A PREROUTING -i br-lan -p udp --dport 53 -j REDIRECT --to-ports 1053\n-A PREROUTING -j plugin_PREROUTING\n-A PREROUTING -j zone_lan_prerouting\n-A PREROUTING -i br-lan -p tcp -j MIHOMO_TCP\n")
	if len(before.Roots) != 2 || !reflect.DeepEqual(before.Positions, []int{1, 4}) {
		t.Fatal(before)
	}
	batch := restoreBatch(before, desiredOwned(testDevices()))
	if strings.Contains(batch, "-F MIHOMO_TCP") || strings.Contains(batch, "-D PREROUTING -j plugin") || strings.Contains(batch, "-F PREROUTING") {
		t.Fatal("Touched non-device chain")
	}
	rollback := restoreBatch(desiredOwned(testDevices()), before)
	if !strings.Contains(rollback, "-I PREROUTING 4 -i br-lan -p tcp -j MIHOMO_TCP") {
		t.Fatal("Root position not preserved")
	}
}
func mockDeviceApp(t *testing.T) (*App, *OwnedRules) {
	a := newTestApp(t)
	cfg := testDevices()
	a.setDeviceConfig(cfg)
	writeJSON(filepath.Join(a.store, "devices.json"), cfg)
	owned := desiredOwned(cfg)
	a.ownedHook = func() (OwnedRules, error) { return owned, nil }
	a.commitHook = func(r OwnedRules) error { owned = r; return nil }
	a.deviceHealthHook = func(bool) error { return nil }
	a.inventoryHook = func() []DeviceView { return nil }
	return a, &owned
}
func TestDeviceSaveIdempotenceAndMACValidation(t *testing.T) {
	a, rules := mockDeviceApp(t)
	want := *rules
	for i := 0; i < 4; i++ {
		if e := a.applyDevices(a.deviceSnapshot()); e != nil {
			t.Fatal(e)
		}
		if !reflect.DeepEqual(want, *rules) {
			t.Fatal("Repeated save multiplied NAT rules")
		}
	}
	bad := a.deviceSnapshot()
	bad.Devices["02:00:00:00:00:01; reboot"] = DeviceRecord{MAC: "bad", Policy: "direct"}
	if e := a.applyDevices(bad); e == nil {
		t.Fatal("Accepted injected MAC")
	}
	if !reflect.DeepEqual(want, *rules) {
		t.Fatal("Changed NAT on invalid input")
	}
}
func TestDeviceHealthFailureRollsBackFileAndRules(t *testing.T) {
	a, rules := mockDeviceApp(t)
	before := *rules
	old := a.deviceSnapshot()
	a.deviceHealthHook = func(bool) error { return errors.New("simulated health failure") }
	next := a.deviceSnapshot()
	r := next.Devices[testComputerMAC]
	r.Policy = "force"
	next.Devices[testComputerMAC] = r
	e := a.applyDevices(next)
	if e == nil || !strings.Contains(e.Error(), "已自动恢复上一版本") {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(before, *rules) || !reflect.DeepEqual(old, a.deviceSnapshot()) {
		t.Fatal("Rollback changed working network or state")
	}
	var disk DeviceConfig
	b, _ := os.ReadFile(filepath.Join(a.store, "devices.json"))
	json.Unmarshal(b, &disk)
	if disk.Devices[testComputerMAC].Policy != "direct" {
		t.Fatal("Policy file not restored")
	}
}
func TestDeviceInterruptedApplyRecovery(t *testing.T) {
	a, rules := mockDeviceApp(t)
	old := a.deviceSnapshot()
	oldRules := *rules
	writeJSON(filepath.Join(a.store, "devices-pending.json"), DeviceJournal{Previous: old, Rules: oldRules})
	changed := a.deviceSnapshot()
	changed.Enabled = false
	*rules = desiredOwned(changed)
	if e := a.recoverDevicePending(); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(*rules, oldRules) || exists(filepath.Join(a.store, "devices-pending.json")) {
		t.Fatal("Recovery incomplete")
	}
}
func TestDeviceNormalizationMatchesKernelOrdering(t *testing.T) {
	a := "-A MIHOMO_DEVICE -m mac --mac-source 02:00:00:00:00:01 -p tcp --dport 53 -j REDIRECT --to-ports 1053"
	b := "-A MIHOMO_DEVICE -p tcp -m mac --mac-source 02:00:00:00:00:01 -m tcp --dport 53 -j REDIRECT --to-ports 1053"
	if normalizeRule(a) != normalizeRule(b) {
		t.Fatal("Kernel normalization would cause constant reapply")
	}
}
func TestSyntheticDeviceDryRun(t *testing.T) {
	before := parseOwned("-A PREROUTING -i br-lan -p udp --dport 53 -j REDIRECT --to-ports 1053\n-A PREROUTING -j vendor_chain\n-A PREROUTING -i br-lan -p tcp -j MIHOMO_TCP\n")
	if len(before.Roots) != 2 {
		t.Fatal("synthetic roots not detected")
	}
	batch := restoreBatch(before, desiredOwned(testDevices()))
	if !strings.Contains(batch, "-D PREROUTING -i br-lan -p tcp -j MIHOMO_TCP") || !strings.Contains(batch, "-I PREROUTING 1 -i br-lan -j MIHOMO_DEVICE") {
		t.Fatal("migration plan missing old cleanup or new root")
	}
	if strings.Contains(batch, "-D PREROUTING -j vendor_chain") {
		t.Fatal("migration touched vendor chain")
	}
}
