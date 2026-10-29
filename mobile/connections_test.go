package main

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConnectionSampleHonestSpeedsAndStickyMAC(t *testing.T) {
	a := newTestApp(t)
	phone := "02:00:00:00:00:02"
	devices := []DeviceView{{DeviceRecord: DeviceRecord{MAC: phone, Name: "手机", Policy: "rule"}, IP: "192.168.31.9", Online: true}}
	makeBody := func(n int) []byte {
		b, _ := json.Marshal(map[string]any{"connections": []any{map[string]any{"id": "one", "download": n, "upload": 20, "chains": []string{"台湾01", "ChatGPT"}, "rule": "DomainSuffix", "rulePayload": "openai.com", "metadata": map[string]any{"sourceIP": "192.168.31.9", "host": "chatgpt.com", "destinationPort": "443", "network": "tcp"}}, map[string]any{"id": "internal", "metadata": map[string]any{"sourceIP": "127.0.0.1"}}}})
		return b
	}
	now := time.Now()
	a.recordRuntimeConnections(makeBody(100), nil, devices, now)
	first := a.runtimeConnections.Rows
	if len(first) != 1 || first[0].DownloadSpeed != nil || first[0].MAC != phone || !first[0].Proxy {
		t.Fatalf("bad initial sample: %+v", first)
	}
	devices[0].MAC = testComputerMAC
	a.recordRuntimeConnections(makeBody(600), nil, devices, now.Add(5*time.Second))
	second := a.runtimeConnections.Rows[0]
	if second.MAC != phone || second.DownloadSpeed == nil || *second.DownloadSpeed != 100 || second.DestinationPort != "443" {
		t.Fatalf("owner or rate wrong: %+v", second)
	}
	a.recordRuntimeConnections(nil, errors.New("down"), devices, now.Add(10*time.Second))
	a.recordRuntimeConnections(makeBody(1600), nil, devices, now.Add(15*time.Second))
	if a.runtimeConnections.Rows[0].DownloadSpeed != nil {
		t.Fatal("outage must not fabricate an instantaneous rate")
	}
}

func TestConnectionsDirectVisibilityAndFiltering(t *testing.T) {
	a := newTestApp(t)
	a.setDeviceConfig(testDevices())
	a.runtimeConnections = runtimeConnectionSample{Updated: time.Now(), OK: true, Rows: []RuntimeConnection{{ID: "steam", MAC: testComputerMAC, Host: "steamcontent.com", Node: "DIRECT", Proxy: false}, {ID: "gpt", MAC: "02:00:00:00:00:02", Host: "chatgpt.com", Node: "台湾01", Proxy: true}}}
	v, err := a.connectionsView(url.Values{"mac": {testComputerMAC}})
	if err != nil || v["observed"] != false {
		t.Fatal("direct device must not pretend complete observation", v, err)
	}
	v, err = a.connectionsView(url.Values{"proxyOnly": {"1"}, "q": {"gpt"}})
	if err != nil || len(v["connections"].([]RuntimeConnection)) != 1 {
		t.Fatal(v, err)
	}
	if err = a.closeConnection("../../all"); err == nil {
		t.Fatal("invalid close target accepted")
	}
}

func TestDeviceArchiveDoesNotChangePoliciesOrTraffic(t *testing.T) {
	a := newTestApp(t)
	cfg := testDevices()
	a.setDeviceConfig(cfg)
	ledger := []byte(`{"version":1,"devices":{"keep":"unchanged"}}`)
	os.WriteFile(filepath.Join(a.store, "traffic.json"), ledger, 0600)
	yes := true
	if err := a.saveDeviceLayout(testComputerMAC, &yes, &yes); err != nil {
		t.Fatal(err)
	}
	if a.deviceSnapshot().Devices[testComputerMAC].Policy != "direct" {
		t.Fatal("archive changed policy")
	}
	b, _ := os.ReadFile(filepath.Join(a.store, "traffic.json"))
	if string(b) != string(ledger) {
		t.Fatal("archive altered traffic ledger")
	}
	views := []DeviceView{{DeviceRecord: cfg.Devices[testComputerMAC]}}
	a.decorateDeviceLayout(views)
	if !views[0].Pinned || !views[0].Archived {
		t.Fatal("layout not restored")
	}
	os.WriteFile(filepath.Join(a.store, "device-layout.json"), []byte("broken"), 0600)
	if err := a.saveDeviceLayout(testComputerMAC, nil, &yes); err == nil {
		t.Fatal("corrupt layout was silently overwritten")
	}
}
