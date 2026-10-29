package main

import (
	"os"
	"strings"
	"testing"
)

func TestColdBootAtomicallyCreatesMissingTCPFoundation(t *testing.T) {
	target := desiredOwned(testDevices())
	batch := restoreBatchWithTCP(OwnedRules{}, target, false)
	if path := os.Getenv("MOBILE_V4_COLD_BOOT_BATCH"); path != "" {
		if err := os.WriteFile(path, []byte(batch), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".repeat", []byte(restoreBatchWithTCP(target, target, true)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	declaration := strings.Index(batch, ":MIHOMO_TCP - [0:0]")
	redirect := strings.Index(batch, "-A MIHOMO_TCP -p tcp -j REDIRECT --to-ports 7892")
	entry := strings.Index(batch, "-A MIHOMO_DEVICE -p tcp -j MIHOMO_TCP")
	if declaration < 0 || redirect < declaration || entry < redirect {
		t.Fatal("fresh NAT cannot resolve TCP chain", batch)
	}
	if !strings.Contains(batch, "-A MIHOMO_TCP -d 192.168.0.0/16 -j RETURN") || strings.Contains(batch, "-F PREROUTING") {
		t.Fatal("cold boot changed LAN access or other chains")
	}
	if preserved := restoreBatchWithTCP(OwnedRules{}, target, true); strings.Contains(preserved, ":MIHOMO_TCP") || strings.Contains(preserved, "-F MIHOMO_TCP") {
		t.Fatal("existing TCP chain must retain rules and counters")
	}
	off := testDevices()
	off.Enabled = false
	if strings.Contains(restoreBatchWithTCP(OwnedRules{}, desiredOwned(off), false), ":MIHOMO_TCP") {
		t.Fatal("off state need not install proxy foundation")
	}
}
