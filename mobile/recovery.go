package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const managerLockPath = "/tmp/mihomo-manager.flock"

func acquireManagerLock() (func(), error) {
	// Old external menu tools use a directory lock. Never silently remove a lock
	// whose owner cannot be established; the installer migrates it only when idle.
	if exists("/tmp/mihomo-manager.lock") {
		return nil, errors.New("旧管理工具仍持有操作锁，请完成该操作后重试")
	}
	return acquireFileLock(managerLockPath)
}

type RecoverySettings struct {
	Version   int  `json:"version"`
	Enabled   bool `json:"enabled"`
	DesiredOn bool `json:"desiredOn"`
}

type RecoveryManager struct {
	mu            sync.Mutex
	saved         RecoverySettings
	state         string
	message       string
	attempts      int
	apiFailures   int
	degraded      bool
	next          time.Time
	lastCheck     time.Time
	lastRecovered time.Time
	healthySince  time.Time
	// These hooks run only in tests; the public handler never accepts them.
	healthHook  func() error
	restartHook func(bool) error
	directHook  func() error
	lockHook    func() (func(), error)
}

type recoveryTransientError struct{ error }

func (a *App) initRecovery() {
	a.recoveryOnce.Do(func() {
		m := &RecoveryManager{saved: RecoverySettings{Version: 1, Enabled: true}, state: "healthy", message: "自动恢复已就绪"}
		if c := a.deviceSnapshot(); c != nil {
			m.saved.DesiredOn = c.Enabled
		} else if r, e := a.rules(); e == nil {
			m.saved.DesiredOn = r.Enabled
		}
		b, err := os.ReadFile(filepath.Join(a.store, "recovery.json"))
		if err == nil {
			var s RecoverySettings
			if json.Unmarshal(b, &s) == nil && s.Version == 1 {
				m.saved = s
			} else {
				m.saved.Enabled = false
				m.state = "attention"
				m.message = "自动恢复设置无法读取，已保留当前网络"
			}
		}
		if c := a.deviceSnapshot(); c != nil {
			m.saved.DesiredOn = c.Enabled
		}
		a.recovery = m
	})
}

func (a *App) recoverySnapshot() map[string]any {
	a.initRecovery()
	m := a.recovery
	m.mu.Lock()
	v := map[string]any{"enabled": m.saved.Enabled, "desiredOn": m.saved.DesiredOn, "state": m.state, "message": m.message, "attempts": m.attempts, "nextRetry": timeLabel(m.next), "lastCheck": timeLabel(m.lastCheck), "lastRecovered": timeLabel(m.lastRecovered)}
	m.mu.Unlock()
	v["boot"] = a.bootStatus()
	v["backups"] = a.listBackups()
	return v
}

func timeLabel(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

func (a *App) saveRecovery(s RecoverySettings) error {
	return writeJSON(filepath.Join(a.store, "recovery.json"), s)
}

// The user's saved switch is distinct from temporary emergency bypass. In
// particular, an emergency bypass must never overwrite the desired enabled bit.
func (a *App) recordDesiredProxy(on bool) error {
	a.initRecovery()
	m := a.recovery
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.saved
	s.DesiredOn = on
	// devices.json has already committed the user's switch. Keep the watchdog's
	// in-memory intent aligned even if this secondary settings file cannot save.
	// initRecovery likewise treats DeviceConfig.Enabled as authoritative on boot.
	m.saved = s
	m.attempts = 0
	m.next = time.Time{}
	m.degraded = false
	m.state = "healthy"
	m.message = "已保存网络开关状态"
	if err := a.saveRecovery(s); err != nil {
		m.message = "网络开关已生效，但恢复设置未能保存，请检查存储空间"
		return errors.New(m.message)
	}
	return nil
}

func (a *App) recoveryDegraded() bool {
	a.initRecovery()
	m := a.recovery
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.degraded
}

func (a *App) ensureRecoveryDirect() error {
	a.initRecovery()
	m := a.recovery
	if m.directHook != nil {
		return m.directHook()
	}
	if err := a.suspendDevices(); err != nil {
		return err
	}
	// Existing NAT mappings outlive the rule that created them. Remove only our
	// LAN redirects so reused DNS sockets can reach the ordinary DNS service.
	for _, protocol := range []string{"udp", "tcp"} {
		_, _ = boundedCommand("conntrack", "-D", "-f", "ipv4", "-s", "192.168.31.0/24", "-p", protocol, "--dport", "53", "--reply-port-src", "1053")
	}
	_, _ = boundedCommand("conntrack", "-D", "-f", "ipv4", "-s", "192.168.31.0/24", "-p", "tcp", "--reply-port-src", "7892")
	return nil
}

func (a *App) recoveryHealth() error {
	m := a.recovery
	if m.healthHook != nil {
		return m.healthHook()
	}
	if len(a.enginePIDs()) != 1 {
		return errors.New("代理进程未正常运行")
	}
	// Redir/TPROXY ports are not normal HTTP services. Dialling them fabricates
	// loopback connections and noisy proxy-loop errors; inspect listeners instead.
	out, e := boundedCommand("netstat", "-ln")
	if e != nil {
		return errors.New("无法读取代理监听端口")
	}
	tcp := map[string]bool{"7890": false, "7892": false, "7893": false, "1053": false, "9091": false}
	udpDNS := false
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		for port := range tcp {
			if strings.HasPrefix(fields[0], "tcp") && strings.HasSuffix(fields[3], ":"+port) {
				tcp[port] = true
			}
		}
		if strings.HasPrefix(fields[0], "udp") && strings.HasSuffix(fields[3], ":1053") {
			udpDNS = true
		}
	}
	for _, ready := range tcp {
		if !ready {
			return errors.New("代理监听端口未准备好")
		}
	}
	if !udpDNS {
		return errors.New("代理 UDP DNS 监听尚未准备好")
	}
	if _, e := a.apiTimeout("GET", "/version", nil, 2*time.Second); e != nil {
		return recoveryTransientError{errors.New("代理控制接口暂时没有响应")}
	}
	return nil
}

func (a *App) transactionPending() bool {
	for _, n := range []string{"pending.json", "profiles-pending.json", "devices-pending.json", "smart-provision-pending.json", "services-provision-pending.json", "backup-restore-pending.json"} {
		if exists(filepath.Join(a.store, n)) {
			return true
		}
	}
	return false
}

// Called while holding both the in-process gate and the cross-process lock.
// Provider/network failures are not engine failures and never trigger this path.
func (a *App) recoveryStep(now time.Time) {
	a.initRecovery()
	m := a.recovery
	if a.transactionPending() {
		return
	}
	health := a.recoveryHealth()
	if health == nil && a.startupPreferences.Load() {
		if err := a.restoreAvailableSelectors(a.savedSelectors()); err != nil {
			a.setDeviceError(err)
			return
		}
		if err := a.restoreServiceSelections(); err != nil {
			a.setDeviceError(err)
			return
		}
		a.startupPreferences.Store(false)
		a.setDeviceError(nil)
	}
	m.mu.Lock()
	m.lastCheck = now
	if health == nil {
		m.apiFailures = 0
		degraded, on := m.degraded, m.saved.DesiredOn
		if m.healthySince.IsZero() {
			m.healthySince = now
		}
		if !degraded {
			if now.Sub(m.healthySince) >= time.Minute {
				m.attempts = 0
				m.next = time.Time{}
			}
			if m.saved.Enabled {
				m.state = "healthy"
				m.message = "代理服务运行正常"
			} else {
				m.state = "paused"
				m.message = "自动恢复已暂停"
			}
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()
		if e := a.resumeDevices(on); e != nil {
			m.mu.Lock()
			m.state = "attention"
			m.message = "代理已启动，网络规则恢复失败；当前保持普通直连"
			m.mu.Unlock()
			return
		}
		m.mu.Lock()
		m.degraded = false
		m.state = "healthy"
		m.message = "代理服务已恢复，设备策略已重新应用"
		m.lastRecovered = now
		m.next = time.Time{}
		m.mu.Unlock()
		return
	}
	var transient recoveryTransientError
	if errors.As(health, &transient) {
		m.apiFailures++
		if m.apiFailures < 2 {
			m.message = "控制接口暂时没有响应，正在复查"
			m.mu.Unlock()
			return
		}
	} else {
		m.apiFailures = 0
	}
	m.healthySince = time.Time{}
	// Remove broken redirections on the first confirmed local-service failure.
	m.degraded = true
	m.state = "direct"
	m.message = "代理暂不可用，正在恢复普通 DNS 和直连"
	enabled, on, attempts, next := m.saved.Enabled, m.saved.DesiredOn, m.attempts, m.next
	m.mu.Unlock()
	if err := a.ensureRecoveryDirect(); err != nil {
		m.mu.Lock()
		m.state = "attention"
		m.message = "无法撤销代理接管，请在更多里查看状态并修复规则"
		m.mu.Unlock()
		return
	}
	if !enabled {
		m.mu.Lock()
		m.state = "paused"
		m.message = "代理异常，已保持普通直连；自动恢复已暂停"
		m.mu.Unlock()
		return
	}
	if attempts >= 3 {
		m.mu.Lock()
		m.state = "attention"
		m.message = "自动恢复已尝试 3 次，当前普通直连；可手动重试或恢复备份"
		m.mu.Unlock()
		return
	}
	if !next.IsZero() && now.Before(next) {
		return
	}
	m.mu.Lock()
	m.attempts++
	m.state = "recovering"
	m.message = "正在尝试恢复代理服务"
	attempts = m.attempts
	m.mu.Unlock()
	restart := a.restart
	if m.restartHook != nil {
		restart = m.restartHook
	}
	err := restart(on)
	if err == nil {
		err = a.recoveryHealth()
	}
	if err == nil {
		m.mu.Lock()
		m.degraded = false
		m.state = "healthy"
		m.message = "代理服务已恢复，保留原来的开关和设备策略"
		m.lastRecovered = now
		m.healthySince = now
		m.next = time.Time{}
		m.mu.Unlock()
		return
	}
	_ = a.ensureRecoveryDirect()
	waits := []time.Duration{15 * time.Second, time.Minute, 5 * time.Minute}
	m.mu.Lock()
	m.state = "direct"
	m.message = "本次恢复未成功，当前保持普通直连"
	m.next = now.Add(waits[attempts-1])
	m.mu.Unlock()
}

func (a *App) recoveryMonitor() {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for now := range t.C {
		select {
		case a.gate <- struct{}{}:
		default:
			continue
		}
		a.initRecovery()
		lock := acquireManagerLock
		if a.recovery.lockHook != nil {
			lock = a.recovery.lockHook
		}
		unlock, e := lock()
		if e == nil {
			a.recoveryStep(now)
			unlock()
		}
		<-a.gate
	}
}

func (a *App) performRecovery(r Request) (string, any, error, bool) {
	switch r.Action {
	case "recovery-settings":
		a.initRecovery()
		m := a.recovery
		m.mu.Lock()
		s := m.saved
		if r.Enabled == nil {
			m.mu.Unlock()
			return "", nil, errors.New("请选择是否启用自动恢复"), true
		}
		s.Enabled = *r.Enabled
		if e := a.saveRecovery(s); e != nil {
			m.mu.Unlock()
			return "", nil, errors.New("保存自动恢复设置失败"), true
		}
		m.saved = s
		m.mu.Unlock()
		return "自动恢复设置已保存", a.recoverySnapshot(), nil, true
	case "recovery-retry":
		a.initRecovery()
		m := a.recovery
		m.mu.Lock()
		m.attempts = 0
		m.next = time.Time{}
		enabled := m.saved.Enabled
		m.saved.Enabled = true
		m.mu.Unlock()
		a.recoveryStep(time.Now())
		m.mu.Lock()
		m.saved.Enabled = enabled
		bad := m.degraded
		m.mu.Unlock()
		if bad {
			return "", a.recoverySnapshot(), errors.New("仍未恢复，已保持普通直连，请检查详细状态"), true
		}
		return "代理服务检查完成", a.recoverySnapshot(), nil, true
	case "backup-create":
		v, e := a.createBackup(false)
		return "完整备份已保存，可下载到手机或电脑", v, e, true
	case "backup-remove":
		if !backupIDPattern.MatchString(r.ID) || a.transactionPending() {
			return "", nil, errors.New("备份暂时不能移除，请等当前操作完成"), true
		}
		if err := os.Remove(filepath.Join(a.backupDir(), r.ID)); err != nil {
			return "", nil, errors.New("备份不存在或暂时无法移除"), true
		}
		return "已移除路由器上的这份备份，当前设置不受影响", nil, nil, true
	}
	return "", nil, nil, false
}
