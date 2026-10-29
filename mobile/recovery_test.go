package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestKernelLockCrashChild(t *testing.T) {
	if os.Getenv("MIHOMO_TEST_LOCK_CHILD") == "" {
		return
	}
	u, e := acquireFileLock(os.Getenv("MIHOMO_TEST_LOCK_PATH"))
	if e != nil {
		t.Fatal(e)
	}
	defer u()
	fmt.Println("LOCKED")
	<-time.After(time.Minute)
}

func TestKernelLockReleasedWhenProcessDies(t *testing.T) {
	p := filepath.Join(t.TempDir(), "process.lock")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestKernelLockCrashChild$")
	cmd.Env = append(os.Environ(), "MIHOMO_TEST_LOCK_CHILD=1", "MIHOMO_TEST_LOCK_PATH="+p)
	pipe, e := cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	line, e := bufio.NewReader(pipe).ReadString('\n')
	if e != nil || !strings.Contains(line, "LOCKED") {
		t.Fatal("child failed to acquire lock", line, e)
	}
	if release, e := acquireFileLock(p); e == nil {
		release()
		t.Fatal("parent bypassed child process lock")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	u, e := acquireFileLock(p)
	if e != nil {
		t.Fatal("crashed process left stale manager lock", e)
	}
	u()
}

func TestTransientAPIHangRequiresTwoChecks(t *testing.T) {
	a, _ := mockDeviceApp(t)
	a.initRecovery()
	a.recovery.healthHook = func() error { return recoveryTransientError{errors.New("API busy")} }
	calls := 0
	a.recovery.directHook = func() error { calls++; return nil }
	a.recovery.restartHook = func(bool) error { return errors.New("still busy") }
	a.recoveryStep(time.Now())
	if calls != 0 || a.recoveryDegraded() {
		t.Fatal("single transient API timeout interrupted network")
	}
	a.recoveryStep(time.Now().Add(10 * time.Second))
	if calls == 0 || !a.recoveryDegraded() {
		t.Fatal("persistent API hang not recovered")
	}
}

func TestKernelManagerLockExcludesAndReleases(t *testing.T) {
	p := filepath.Join(t.TempDir(), "manager.lock")
	u, e := acquireFileLock(p)
	if e != nil {
		t.Fatal(e)
	}
	if second, e := acquireFileLock(p); e == nil {
		second()
		t.Fatal("second owner acquired live manager lock")
	}
	u()
	u, e = acquireFileLock(p)
	if e != nil {
		t.Fatal("kernel did not release lock", e)
	}
	u()
	// The lock file intentionally survives; it must not become a stale lock.
	if _, e = os.Stat(p); e != nil {
		t.Fatal(e)
	}
}

func TestRecoveryBoundedAttemptsAndSafeDirect(t *testing.T) {
	a, owned := mockDeviceApp(t)
	a.initRecovery()
	m := a.recovery
	m.healthHook = func() error { return errors.New("engine absent") }
	calls, direct := 0, 0
	m.restartHook = func(on bool) error {
		calls++
		if !on {
			t.Fatal("desired enabled switch lost")
		}
		return errors.New("start failure")
	}
	m.directHook = func() error {
		direct++
		off := testDevices()
		off.Enabled = false
		*owned = desiredOwned(off)
		return nil
	}
	now := time.Now()
	a.recoveryStep(now)
	if calls != 1 || !a.recoveryDegraded() || len(owned.Chain) != 0 || !a.deviceSnapshot().Enabled {
		t.Fatal("failed engine left redirections or overwrote desired state")
	}
	a.recoveryStep(now.Add(time.Second))
	if calls != 1 {
		t.Fatal("backoff ignored")
	}
	a.recoveryStep(now.Add(20 * time.Second))
	a.recoveryStep(now.Add(2 * time.Minute))
	a.recoveryStep(now.Add(10 * time.Minute))
	if calls != 3 || direct < 3 || m.state != "attention" {
		t.Fatalf("retry budget not enforced: %d/%d/%s", calls, direct, m.state)
	}
}

func TestRecoveryHonorsExplicitOff(t *testing.T) {
	a, owned := mockDeviceApp(t)
	cfg := a.deviceSnapshot()
	cfg.Enabled = false
	a.setDeviceConfig(cfg)
	a.initRecovery()
	m := a.recovery
	healthy := false
	m.healthHook = func() error {
		if healthy {
			return nil
		}
		return errors.New("core absent")
	}
	m.directHook = func() error { *owned = desiredOwned(cfg); return nil }
	m.restartHook = func(on bool) error {
		if on {
			t.Fatal("watchdog enabled manually disabled proxy")
		}
		healthy = true
		return nil
	}
	a.recoveryStep(time.Now())
	if a.recoveryDegraded() || len(owned.Chain) != 0 || m.saved.DesiredOn {
		t.Fatal("proxy state changed during service recovery")
	}
}

func TestExplicitOffSurvivesSecondaryRecoveryWriteFailure(t *testing.T) {
	a, owned := mockDeviceApp(t)
	a.initRecovery()
	m := a.recovery
	if !m.saved.DesiredOn {
		t.Fatal("fixture must initially be enabled")
	}
	// This is the state immediately after the main off transaction committed.
	cfg := a.deviceSnapshot()
	cfg.Enabled = false
	a.setDeviceConfig(cfg)
	if err := os.Mkdir(filepath.Join(a.store, "recovery.json.new"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := a.recordDesiredProxy(false); err == nil {
		t.Fatal("secondary save failure was not surfaced")
	}
	if m.saved.DesiredOn {
		t.Fatal("watchdog kept stale on intent after successful main off transaction")
	}
	healthy := false
	m.healthHook = func() error {
		if healthy {
			return nil
		}
		return errors.New("core stopped later")
	}
	m.directHook = func() error { *owned = desiredOwned(cfg); return nil }
	calls := 0
	m.restartHook = func(on bool) error {
		calls++
		if on {
			t.Fatal("secondary file failure caused proxy to re-enable")
		}
		healthy = true
		return nil
	}
	a.recoveryStep(time.Now())
	if calls != 1 || len(owned.Chain) != 0 || a.deviceSnapshot().Enabled {
		t.Fatal("manual off did not survive later core recovery")
	}
}

func TestDisabledRecoveryOnlyBypasses(t *testing.T) {
	a, _ := mockDeviceApp(t)
	a.initRecovery()
	m := a.recovery
	m.saved.Enabled = false
	m.healthHook = func() error { return errors.New("absent") }
	m.directHook = func() error { return nil }
	m.restartHook = func(bool) error { t.Fatal("paused watchdog restarted core"); return nil }
	a.recoveryStep(time.Now())
	if !m.degraded || m.state != "paused" {
		t.Fatal("paused recovery did not expose safe fallback")
	}
}

func TestRecoveryDoesNotTouchPendingTransactions(t *testing.T) {
	a, _ := mockDeviceApp(t)
	a.initRecovery()
	a.recovery.healthHook = func() error { t.Fatal("health check raced configuration transaction"); return nil }
	os.WriteFile(filepath.Join(a.store, "services-provision-pending.json"), []byte("{}"), 0600)
	a.recoveryStep(time.Now())
}

func TestCronMergePreservesVendorJobsAndIsIdempotent(t *testing.T) {
	old := []byte("0 3 * * * /vendor/cleanup\n*/5 * * * * /vendor/check\n* * * * * /old/boot.sh # mihomo-mobile-v4\n")
	one, e := mergeBootCron(old, "/data/other/mihomo/mobile-panel/boot.sh")
	if e != nil {
		t.Fatal(e)
	}
	two, e := mergeBootCron(one, "/data/other/mihomo/mobile-panel/boot.sh")
	if e != nil || string(one) != string(two) {
		t.Fatal("duplicate boot job", e)
	}
	if strings.Count(string(one), bootCronMarker) != 1 || !strings.Contains(string(one), "/vendor/cleanup") || !strings.Contains(string(one), "/vendor/check") {
		t.Fatal("vendor cron changed")
	}
	if _, e = mergeBootCron(old, "/tmp/boot\nsh"); e == nil {
		t.Fatal("unsafe cron path accepted")
	}
}

func TestBootScriptHasChecksumSingletonAndInstallerGuard(t *testing.T) {
	s := bootScript("/data/other/mihomo", "192.168.31.1:9080", strings.Repeat("a", 64), false)
	for _, required := range []string{"flock -n 9", "sha256sum", `[ "$actual" = "$EXPECTED" ]`, "$STORE/installing", `[ "$tries" -lt 3 ]`, "/proc/[0-9]*/exe", "9>&-", `[ "$now" -ge "$started" ]`} {
		if !strings.Contains(s, required) {
			t.Fatal("missing boot safeguard", required)
		}
	}
	if strings.Contains(s, "iptables -F") || strings.Contains(s, "reboot") && !strings.Contains(s, "flush or reboot") {
		t.Fatal("unexpected router-wide boot mutation")
	}
	if strings.Contains(s, "nohup ") {
		t.Fatal("stock Xiaomi firmware does not provide nohup")
	}
	if strings.Contains(s, "dropbear") || strings.Contains(s, "ssh-host.key") {
		t.Fatal("ordinary panel boot recovery must not restore SSH, including any leftover key")
	}
}

func TestBootSSHRecoveryRequiresExplicitOptIn(t *testing.T) {
	s := bootScript("/data/other/mihomo", "192.168.31.1:9080", strings.Repeat("a", 64), true)
	if !strings.Contains(s, "/usr/sbin/dropbear -p 192.168.31.1:22 9>&-") || !strings.Contains(s, "$STORE/boot/ssh-host.key") {
		t.Fatal("explicit SSH opt-in did not include LAN-only key-preserving recovery")
	}
	if strings.Contains(s, "passwd") || strings.Contains(s, "shadow") {
		t.Fatal("SSH recovery must not change or copy account passwords")
	}
}

func TestBootPreparationFailureRestoresOldPayloadAndCron(t *testing.T) {
	dir := t.TempDir()
	payload := filepath.Join(dir, "panel.gz")
	cron := filepath.Join(dir, "root")
	newFile := filepath.Join(dir, "boot.json")
	os.WriteFile(payload, []byte("old payload"), 0600)
	os.WriteFile(cron, []byte("vendor jobs\n"), 0600)
	before, e := snapshotBootFiles([]string{payload, cron, newFile})
	if e != nil {
		t.Fatal(e)
	}
	os.WriteFile(payload, []byte("new payload"), 0600)
	os.WriteFile(cron, []byte("new cron"), 0600)
	os.WriteFile(newFile, []byte("partial settings"), 0600)
	if e = restoreBootFiles(before); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(payload)
	c, _ := os.ReadFile(cron)
	if string(b) != "old payload" || string(c) != "vendor jobs\n" || exists(newFile) {
		t.Fatal("partial boot installation was not rolled back")
	}
}
