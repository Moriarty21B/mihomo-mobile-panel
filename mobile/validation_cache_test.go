package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidationCacheTracksAllLocalInputsAndRefusesProviders(t *testing.T) {
	a := newTestApp(t)
	path := filepath.Join(a.dir, "config.yaml")
	os.WriteFile(path, []byte("rules: [MATCH,DIRECT]\n"), 0600)
	os.WriteFile(filepath.Join(a.dir, "mihomo"), []byte("core1"), 0700)
	os.WriteFile(filepath.Join(a.dir, "GeoSite.dat"), []byte("geo1"), 0600)
	first := a.validationKey(path)
	if first == "" || a.validationKey(path) != first {
		t.Fatal("unstable cache")
	}
	os.WriteFile(filepath.Join(a.dir, "GeoSite.dat"), []byte("geo2"), 0600)
	if a.validationKey(path) == first {
		t.Fatal("geodata modification reused stale validation")
	}
	second := a.validationKey(path)
	os.WriteFile(filepath.Join(a.dir, "mihomo"), []byte("core2"), 0700)
	if a.validationKey(path) == second {
		t.Fatal("core modification reused stale validation")
	}
	os.WriteFile(path, []byte("rule-providers:\n  external: {type: http, url: 'https://example.invalid/rules'}\n"), 0600)
	if a.validationKey(path) != "" {
		t.Fatal("external dependency was treated as fingerprintable")
	}
}
