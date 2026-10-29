package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"

	"gopkg.in/yaml.v3"
)

// All configuration supplied by the airport remains separate from the saved
// local rules. A boot/recovery recompile is validated before its atomic write.
// Active transactions compile their own candidate using their intended profile.
func (a *App) reapplySavedConfig() error {
	if a.transactionPending() || a.profileSnapshot() == nil {
		return nil
	}
	original, err := os.ReadFile(filepath.Join(a.dir, "config.yaml"))
	if err != nil {
		return err
	}
	candidate, err := a.savedRulesAndServices(original)
	if err != nil {
		return err
	}
	var old, new map[string]any
	if yaml.Unmarshal(original, &old) != nil || yaml.Unmarshal(candidate, &new) != nil {
		return errors.New("已保存规则无法重建，现有配置保留")
	}
	if reflect.DeepEqual(old, new) {
		return nil
	}
	staged := filepath.Join(a.dir, "mobile-startup-candidate.yaml")
	if err = atomicWrite(staged, candidate); err != nil {
		return err
	}
	defer os.Remove(staged)
	if err = a.validate(staged); err != nil {
		return err
	}
	return atomicWrite(filepath.Join(a.dir, "config.yaml"), candidate)
}

func (a *App) recoverStartupTransactions() error {
	if err := a.recoverBackupRestore(); err != nil {
		return err
	}
	if exists(filepath.Join(a.store, "pending.json")) {
		if err := a.recoverPending(); err != nil {
			return err
		}
	}
	if exists(filepath.Join(a.store, "smart-provision-pending.json")) {
		if err := a.recoverSmartProvision(); err != nil {
			return err
		}
	}
	if err := a.recoverServicesProvision(); err != nil {
		return err
	}
	if err := a.recoverDevicePending(); err != nil {
		return err
	}
	return nil
}
