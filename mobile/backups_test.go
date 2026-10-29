package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testBackupFiles(t *testing.T, a *App) map[string][]byte {
	t.Helper()
	if e := os.WriteFile(filepath.Join(a.dir, "config.yaml"), []byte(original), 0600); e != nil {
		t.Fatal(e)
	}
	if e := a.save(a.snapshot()); e != nil {
		t.Fatal(e)
	}
	f, e := a.collectBackupFiles(false)
	if e != nil {
		t.Fatal(e)
	}
	return f
}
func backupBytes(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	if e := encodeBackup(&b, files, false, time.Now()); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}

func TestBackupRoundtripAndChecksums(t *testing.T) {
	a, _ := mockDeviceApp(t)
	files := testBackupFiles(t, a)
	files["state/services.json"], _ = json.Marshal(defaultServicesStore())
	b := backupBytes(t, files)
	got, m, e := decodeBackup(bytes.NewReader(b))
	if e != nil {
		t.Fatal(e)
	}
	if m.Format != 1 || m.Architecture != "linux-arm64" || !bytes.Equal(got["router/config.yaml"], files["router/config.yaml"]) {
		t.Fatal("backup changed private configuration")
	}
	b[len(b)-5] ^= 0xff
	if _, _, e = decodeBackup(bytes.NewReader(b)); e == nil {
		t.Fatal("damaged gzip checksum accepted")
	}
}

func maliciousTar(t *testing.T, headers []*tar.Header) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, h := range headers {
		if e := tw.WriteHeader(h); e != nil {
			t.Fatal(e)
		}
		if h.Size > 0 {
			tw.Write(make([]byte, h.Size))
		}
	}
	tw.Close()
	gz.Close()
	return b.Bytes()
}
func TestBackupRejectsTraversalLinksUnknownAndDuplicates(t *testing.T) {
	cases := [][]*tar.Header{
		{{Name: "../settings.json", Typeflag: tar.TypeReg}},
		{{Name: "/etc/crontabs/root", Typeflag: tar.TypeReg}},
		{{Name: "state/../boot.sh", Typeflag: tar.TypeReg}},
		{{Name: "state\\settings.json", Typeflag: tar.TypeReg}},
		{{Name: "state/settings.json", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}},
		{{Name: "state/settings.json", Typeflag: tar.TypeLink, Linkname: "router/config.yaml"}},
		{{Name: "state/settings.json", Typeflag: tar.TypeReg}, {Name: "state/settings.json", Typeflag: tar.TypeReg}},
		{{Name: "state/boot.sh", Typeflag: tar.TypeReg}},
	}
	for i, hs := range cases {
		if _, _, e := decodeBackup(bytes.NewReader(maliciousTar(t, hs))); e == nil {
			t.Fatalf("malicious archive %d accepted", i)
		}
	}
}

func TestBackupManifestDoesNotAllowOmittedOrAlteredFiles(t *testing.T) {
	a, _ := mockDeviceApp(t)
	files := testBackupFiles(t, a)
	good := backupBytes(t, files)
	got, m, e := decodeBackup(bytes.NewReader(good))
	if e != nil {
		t.Fatal(e)
	}
	got["router/config.yaml"] = []byte("mixed-port: 1\n")
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	mb, _ := json.Marshal(m)
	got["manifest.json"] = mb
	for n, b := range got {
		tw.WriteHeader(&tar.Header{Name: n, Size: int64(len(b)), Mode: 0600, Typeflag: tar.TypeReg})
		tw.Write(b)
	}
	tw.Close()
	gz.Close()
	if _, _, e = decodeBackup(bytes.NewReader(raw.Bytes())); e == nil {
		t.Fatal("forged file checksum accepted")
	}
}

func TestInvalidBackupCannotChangeNetworkOrFiles(t *testing.T) {
	a, _ := mockDeviceApp(t)
	files := testBackupFiles(t, a)
	files["state/devices.json"] = []byte(`{"version":99}`)
	a.restartHook = func(bool) error { t.Fatal("invalid backup reached restart"); return nil }
	a.validateHook = func(string) error { return nil }
	if e := a.restoreBackup(bytes.NewReader(backupBytes(t, files))); e == nil {
		t.Fatal("invalid device policy accepted")
	}
	b, _ := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
	if string(b) != original {
		t.Fatal("invalid restore changed config")
	}
	if exists(filepath.Join(a.store, "backup-restore-pending.json")) {
		t.Fatal("invalid restore began a transaction")
	}
}

func TestFailedBackupRestoreRollsBackPrivateDataAndSwitch(t *testing.T) {
	a, _ := mockDeviceApp(t)
	files := testBackupFiles(t, a)
	if e := os.WriteFile(filepath.Join(a.dir, "GeoSite.dat"), []byte("original geodata"), 0600); e != nil {
		t.Fatal(e)
	}
	files["geodata/GeoSite.dat"] = []byte("replacement geodata")
	old := a.snapshot()
	old.URL = "https://old.example/sub"
	if e := a.save(old); e != nil {
		t.Fatal(e)
	}
	var incoming State
	json.Unmarshal(files["state/settings.json"], &incoming)
	incoming.URL = "https://new.example/sub"
	incoming.Token = "stale-login"
	files["state/settings.json"], _ = json.Marshal(incoming)
	files["state/smart.json"] = []byte(`{"version":1,"enabled":false,"excludeHighMultiplier":true,"overrides":{}}`)
	a.validateHook = func(string) error { return nil }
	calls := 0
	a.restartHook = func(on bool) error {
		calls++
		if !on {
			t.Fatal("restore lost desired switch")
		}
		if calls == 1 {
			return errors.New("start failed")
		}
		return nil
	}
	e := a.restoreBackup(bytes.NewReader(backupBytes(t, files)))
	if e == nil || !strings.Contains(e.Error(), "已自动恢复") {
		t.Fatal("rollback not reported", e)
	}
	if calls != 2 || a.snapshot().URL != old.URL || a.snapshot().Token != old.Token || !a.deviceSnapshot().Enabled {
		t.Fatal("restore rollback lost prior state")
	}
	if exists(filepath.Join(a.store, "smart.json")) {
		t.Fatal("rollback retained newly introduced settings")
	}
	if exists(filepath.Join(a.store, "backup-restore-pending.json")) {
		t.Fatal("finished rollback left journal")
	}
	geo, _ := os.ReadFile(filepath.Join(a.dir, "GeoSite.dat"))
	if string(geo) != "original geodata" {
		t.Fatal("failed restore lost original geodata")
	}
}

func TestSuccessfulBackupRestorePreservesCurrentLogin(t *testing.T) {
	a, _ := mockDeviceApp(t)
	files := testBackupFiles(t, a)
	oldToken := a.snapshot().Token
	var incoming State
	json.Unmarshal(files["state/settings.json"], &incoming)
	incoming.URL = "https://restored.example/sub"
	incoming.Token = "old-backup-login"
	incoming.Hash = "old-password"
	files["state/settings.json"], _ = json.Marshal(incoming)
	a.validateHook = func(string) error { return nil }
	a.restartHook = func(bool) error { return nil }
	if e := a.restoreBackup(bytes.NewReader(backupBytes(t, files))); e != nil {
		t.Fatal(e)
	}
	if a.snapshot().Token != oldToken || a.snapshot().URL != incoming.URL || a.snapshot().Hash == "old-password" {
		t.Fatal("restored login invalidated current browser")
	}
}

func TestInterruptedBackupRestoreRecoversFromPersistentJournal(t *testing.T) {
	a, _ := mockDeviceApp(t)
	testBackupFiles(t, a)
	old := a.snapshot()
	old.URL = "https://before.example/sub"
	a.save(old)
	v, e := a.createBackup(true)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(filepath.Join(a.backupDir(), v.ID))
	if e != nil {
		t.Fatal(e)
	}
	j := backupRestoreJournal{Version: 1, Before: v.ID, BeforeSHA256: backupHash(raw), Targets: []string{"router/config.yaml", "state/settings.json", "state/new-unrecognized.json"}, PreviousOn: true}
	// The journal itself is also constrained by the same path allowlist.
	if e = writeJSON(filepath.Join(a.store, "backup-restore-pending.json"), j); e != nil {
		t.Fatal(e)
	}
	if e = a.recoverBackupRestore(); e == nil {
		t.Fatal("malformed interrupted restore target accepted")
	}
	j.Targets = []string{"router/config.yaml", "state/settings.json", "state/smart.json"}
	writeJSON(filepath.Join(a.store, "backup-restore-pending.json"), j)
	os.WriteFile(filepath.Join(a.dir, "config.yaml"), []byte("partial: update\n"), 0600)
	partial := old
	partial.URL = "https://partial.example/sub"
	a.save(partial)
	os.WriteFile(filepath.Join(a.store, "smart.json"), []byte(`{"version":1}`), 0600)
	a.restartHook = func(on bool) error {
		if !on {
			t.Fatal("crash recovery lost prior enabled state")
		}
		return nil
	}
	if e = a.recoverBackupRestore(); e != nil {
		t.Fatal(e)
	}
	config, _ := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
	if string(config) != original || a.snapshot().URL != old.URL || exists(filepath.Join(a.store, "smart.json")) || exists(filepath.Join(a.store, "backup-restore-pending.json")) {
		t.Fatal("crash recovery did not restore prior complete state")
	}
}

func TestBackupRejectsDifferentLANPortsBeforeRestart(t *testing.T) {
	a, _ := mockDeviceApp(t)
	files := testBackupFiles(t, a)
	files["router/config.yaml"] = bytes.ReplaceAll(files["router/config.yaml"], []byte("7890"), []byte("8888"))
	a.validateHook = func(string) error { return nil }
	if e := a.validateBackupState(files); e == nil {
		t.Fatal("restore accepted incompatible LAN ports")
	}
}

func TestAutomaticBackupsRetainThreeAndExcludePrograms(t *testing.T) {
	a, _ := mockDeviceApp(t)
	testBackupFiles(t, a)
	for i := 0; i < 5; i++ {
		v, e := a.createBackup(true)
		if e != nil {
			t.Fatal(e)
		}
		b, _ := os.ReadFile(filepath.Join(a.backupDir(), v.ID))
		files, m, e := decodeBackup(bytes.NewReader(b))
		if e != nil || m.Complete || files["programs/mihomo"] != nil {
			t.Fatal("automatic snapshot copied reinstall programs", e)
		}
	}
	if got := len(a.listBackups()); got != 3 {
		t.Fatal("snapshot retention did not bound storage", got)
	}
}
