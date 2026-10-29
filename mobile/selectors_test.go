package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupRestoreAndRollbackKeepTheirSavedDefaultSelectors(t *testing.T) {
	a := newTestApp(t)
	if err := os.WriteFile(filepath.Join(a.store, "backup-restore-pending.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"台湾 · 来自备份", "新加坡 · 回滚前选择"} {
		if err := a.rememberSelectors(map[string]string{selectGroup: want}); err != nil {
			t.Fatal(err)
		}
		selected, err := a.selectorsForRestart(func() (map[string]string, error) {
			t.Fatal("backup transaction sampled live pre-restore selector")
			return map[string]string{selectGroup: "old live choice"}, nil
		})
		if err != nil || selected[selectGroup] != want || a.savedSelectors()[selectGroup] != want {
			t.Fatalf("restored choice changed: %+v %v", selected, err)
		}
	}
}

func TestNormalRestartRemembersCurrentSelectorAndColdBootUsesSaved(t *testing.T) {
	a := newTestApp(t)
	selected, err := a.selectorsForRestart(func() (map[string]string, error) { return map[string]string{selectGroup: "new live choice"}, nil })
	if err != nil || selected[selectGroup] != "new live choice" || a.savedSelectors()[selectGroup] != "new live choice" {
		t.Fatal("normal restart did not persist current choice", err)
	}
	selected, err = a.selectorsForRestart(func() (map[string]string, error) { return nil, errors.New("core absent after boot") })
	if err != nil || selected[selectGroup] != "new live choice" {
		t.Fatal("cold boot lost persisted choice", err)
	}
	if err = os.WriteFile(filepath.Join(a.store, "selectors.json"), []byte("null"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.rememberDefaultSelector("recovered choice"); err != nil {
		t.Fatal(err)
	}
	if a.savedSelectors()[selectGroup] != "recovered choice" {
		t.Fatal("null selector store did not recover safely")
	}
}

func TestColdBootDoesNotOverwriteSavedChoiceWithEngineDefault(t *testing.T) {
	a := newTestApp(t)
	a.startupPreferences.Store(true)
	if err := a.rememberDefaultSelector("我的已保存节点"); err != nil {
		t.Fatal(err)
	}
	selected, err := a.selectorsForRestart(func() (map[string]string, error) {
		t.Fatal("boot sampled a temporary engine default")
		return map[string]string{selectGroup: "启动默认节点"}, nil
	})
	if err != nil || selected[selectGroup] != "我的已保存节点" || a.savedSelectors()[selectGroup] != "我的已保存节点" {
		t.Fatal("saved boot preference overwritten", err)
	}
}
