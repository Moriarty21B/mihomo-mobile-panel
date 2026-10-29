package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// The default choice is saved independently of Mihomo's volatile runtime. Service
// choices have their own structured store, while this file also remembers selectors
// which existed before the panel installed service groups.
func (a *App) rememberSelectors(selected map[string]string) error {
	if len(selected) == 0 {
		return nil
	}
	b, err := json.Marshal(selected)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(a.store, "selectors.json"), b)
}
func (a *App) savedSelectors() map[string]string {
	b, err := os.ReadFile(filepath.Join(a.store, "selectors.json"))
	if err != nil || len(b) > 256<<10 {
		return map[string]string{}
	}
	var selected map[string]string
	if json.Unmarshal(b, &selected) != nil || selected == nil || len(selected) > 128 {
		return map[string]string{}
	}
	return selected
}

// During a backup restore, the saved file has just been imported (or rolled
// back). Capturing the still-running core would overwrite the restored choice.
func (a *App) selectorsForRestart(capture func() (map[string]string, error)) (map[string]string, error) {
	selected := a.savedSelectors()
	if a.startupPreferences.Load() || exists(filepath.Join(a.store, "backup-restore-pending.json")) {
		return selected, nil
	}
	if current, err := capture(); err == nil {
		if err = a.rememberSelectors(current); err != nil {
			return nil, errors.New("当前节点选择备份失败，未重启代理")
		}
		return current, nil
	}
	return selected, nil
}
func (a *App) rememberDefaultSelector(name string) error {
	s := a.savedSelectors()
	s[selectGroup] = name
	return a.rememberSelectors(s)
}
