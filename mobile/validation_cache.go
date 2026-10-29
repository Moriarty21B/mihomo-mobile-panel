package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// Reuse a successful check only when the exact config, core executable and every
// local geodata file are unchanged. Provider configurations always get a fresh
// validator run because their external dependencies cannot be fingerprinted here.
func (a *App) validationKey(configPath string) string {
	body, err := os.ReadFile(configPath)
	if err != nil || len(body) > maxConfig {
		return ""
	}
	var config map[string]any
	if yaml.Unmarshal(body, &config) != nil {
		return ""
	}
	for _, key := range []string{"rule-providers", "proxy-providers"} {
		if entries, ok := config[key].(map[string]any); ok && len(entries) > 0 {
			return ""
		}
	}
	h := sha256.New()
	_, _ = h.Write([]byte("mihomo-panel-validation-v1\x00"))
	_, _ = h.Write(body)
	names := []string{"mihomo"}
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if backupAssetPattern.MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(a.dir, name)
		st, err := os.Lstat(path)
		if err != nil || !st.Mode().IsRegular() {
			return ""
		}
		file, err := os.Open(path)
		if err != nil {
			return ""
		}
		_, _ = h.Write([]byte("\x00" + name + "\x00"))
		_, err = io.Copy(h, file)
		_ = file.Close()
		if err != nil {
			return ""
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}
