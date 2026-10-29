package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type DeviceLayoutEntry struct {
	Pinned   bool `json:"pinned"`
	Archived bool `json:"archived"`
}
type DeviceLayout struct {
	Version int                          `json:"version"`
	Devices map[string]DeviceLayoutEntry `json:"devices"`
}

func (a *App) readDeviceLayout() (DeviceLayout, error) {
	l := DeviceLayout{Version: 1, Devices: map[string]DeviceLayoutEntry{}}
	b, err := os.ReadFile(filepath.Join(a.store, "device-layout.json"))
	if os.IsNotExist(err) {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	if len(b) > 128<<10 || json.Unmarshal(b, &l) != nil || l.Version != 1 || l.Devices == nil || len(l.Devices) > 512 {
		return l, errors.New("设备整理设置无法读取，原有策略与流量未修改")
	}
	for mac := range l.Devices {
		if canonical, err := canonicalMAC(mac); err != nil || canonical != mac {
			return l, errors.New("设备整理设置中的 MAC 无效")
		}
	}
	return l, nil
}

func (a *App) decorateDeviceLayout(views []DeviceView) {
	a.layoutMu.Lock()
	defer a.layoutMu.Unlock()
	l, err := a.readDeviceLayout()
	if err != nil {
		return
	}
	for i := range views {
		entry := l.Devices[views[i].MAC]
		views[i].Pinned = entry.Pinned
		views[i].Archived = entry.Archived
	}
}

func (a *App) saveDeviceLayout(mac string, pinned, archived *bool) error {
	canonical, err := canonicalMAC(mac)
	if err != nil {
		return err
	}
	if pinned == nil && archived == nil {
		return errors.New("请选择置顶或归档")
	}
	c := a.deviceSnapshot()
	if c == nil {
		return errors.New("设备列表尚未准备好")
	}
	if _, ok := c.Devices[canonical]; !ok {
		return errors.New("设备不存在，请刷新列表")
	}
	a.layoutMu.Lock()
	defer a.layoutMu.Unlock()
	l, err := a.readDeviceLayout()
	if err != nil {
		return err
	}
	e := l.Devices[canonical]
	if pinned != nil {
		e.Pinned = *pinned
	}
	if archived != nil {
		e.Archived = *archived
	}
	if e.Pinned || e.Archived {
		l.Devices[canonical] = e
	} else {
		delete(l.Devices, canonical)
	}
	if len(l.Devices) > 512 {
		return errors.New("整理记录过多，请先取消部分归档")
	}
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(a.store, "device-layout.json"), b)
}
