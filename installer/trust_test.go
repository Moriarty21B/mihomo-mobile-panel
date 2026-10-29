package main

import (
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func testFingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

func TestInstallerAddressRejectsShellAndURLInputs(t *testing.T) {
	for _, host := range []string{"", " router.local", "router.local ", "https://router.local", "router.local:22", "router;reboot", "router\nreboot", "$(whoami)", "router.local/", "-router", "router..local"} {
		if _, err := (installerOptions{Host: host, SSHPort: 22}).address(); err == nil {
			t.Errorf("accepted invalid host %q", host)
		}
	}
	for _, host := range []string{"192.168.31.1", "router.local", "ROUTER", "::1"} {
		if _, err := (installerOptions{Host: host, SSHPort: 22}).address(); err != nil {
			t.Errorf("rejected valid host %q: %v", host, err)
		}
	}
	for _, port := range []int{0, -1, 65536} {
		if _, err := (installerOptions{Host: "router.local", SSHPort: port}).address(); err == nil {
			t.Fatalf("accepted invalid port %d", port)
		}
	}
	if _, err := (installerOptions{Host: "router.local", SSHPort: 22, Fingerprint: "SHA256:truncated"}).address(); err == nil {
		t.Fatal("accepted truncated fingerprint")
	}
}

func TestUnknownHostRequiresExplicitTrust(t *testing.T) {
	base, address := t.TempDir(), "router.local:22"
	pin := testFingerprint("synthetic-router-one")
	path := filepath.Join(routerStateDir(base, address), "host-key.json")
	if err := verifyHostTrust(base, address, "", pin, nil); err == nil {
		t.Fatal("trusted an unknown host without confirmation")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("unconfirmed host persisted")
	}
	if err := verifyHostTrust(base, address, "", pin, func(endpoint, actual string) bool {
		if endpoint != address || actual != pin {
			t.Fatal("confirmation does not identify the current endpoint/key")
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("confirmed host not persisted:", err)
	}
	if err := verifyHostTrust(base, address, "", pin, func(string, string) bool { t.Fatal("prompted for a pinned host"); return false }); err != nil {
		t.Fatal(err)
	}
}

func TestChangedHostKeyRejectedEvenWithNewExplicitPin(t *testing.T) {
	base, address := t.TempDir(), "router.local:22"
	old, next := testFingerprint("synthetic-old"), testFingerprint("synthetic-new")
	if err := verifyHostTrust(base, address, old, old, nil); err != nil {
		t.Fatal(err)
	}
	if err := verifyHostTrust(base, address, "", next, func(string, string) bool { t.Fatal("changed key must not prompt"); return true }); err == nil {
		t.Fatal("accepted changed key")
	}
	if err := verifyHostTrust(base, address, next, next, nil); err == nil {
		t.Fatal("explicit pin silently replaced existing trust")
	}
	if err := verifyHostTrust(base, address, "", old, nil); err != nil {
		t.Fatal("failed attempt changed stored identity:", err)
	}
}

func TestTrustAndPasswordNamespacesSeparateEndpoints(t *testing.T) {
	base, address := t.TempDir(), "router.local:22"
	pin := testFingerprint("synthetic-one")
	if err := verifyHostTrust(base, address, pin, pin, nil); err != nil {
		t.Fatal(err)
	}
	for _, other := range []string{"router.local:2222", "other.local:22"} {
		if routerStateDir(base, address) == routerStateDir(base, other) {
			t.Fatal("two endpoints share cached credentials")
		}
		if err := verifyHostTrust(base, other, "", pin, nil); err == nil {
			t.Fatal("another endpoint inherited trust")
		}
	}
}

func TestMismatchedExplicitFingerprintDoesNotPersist(t *testing.T) {
	base, address := t.TempDir(), "router.local:22"
	if err := verifyHostTrust(base, address, testFingerprint("expected"), testFingerprint("other"), nil); err == nil {
		t.Fatal("accepted unexpected key")
	}
	if _, err := os.Stat(routerStateDir(base, address)); !os.IsNotExist(err) {
		t.Fatal("mismatched explicit identity persisted")
	}
}

func TestCorruptIdentityRecordDoesNotBecomeFirstUse(t *testing.T) {
	base, address := t.TempDir(), "router.local:22"
	path := filepath.Join(routerStateDir(base, address), "host-key.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"address":"another.local:22","fingerprint":"invalid"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyHostTrust(base, address, "", testFingerprint("valid"), func(string, string) bool { t.Fatal("corrupt identity must not be silently replaced"); return true }); err == nil {
		t.Fatal("accepted corrupt stored identity")
	}
}
