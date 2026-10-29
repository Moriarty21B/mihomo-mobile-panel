package main

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type BootSettings struct {
	Version     int    `json:"version"`
	Enabled     bool   `json:"enabled"`
	Supported   bool   `json:"supported"`
	Mechanism   string `json:"mechanism"`
	CronPath    string `json:"cronPath"`
	PanelSHA256 string `json:"panelSHA256"`
	SSH         bool   `json:"ssh"`
}

func (a *App) bootStatus() map[string]any {
	var b BootSettings
	p, e := os.ReadFile(filepath.Join(a.store, "boot.json"))
	if e == nil {
		_ = json.Unmarshal(p, &b)
	}
	msg := "尚未安装开机恢复"
	if b.Enabled && b.Supported {
		msg = "开机后约 1 分钟内恢复，保留原来的代理开关"
	}
	return map[string]any{"enabled": b.Enabled, "supported": b.Supported, "mechanism": b.Mechanism, "message": msg, "ssh": b.SSH}
}

func shellLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func bootScript(dir, host, sha string, preserveSSH bool) string {
	sshRecovery := ""
	if preserveSSH {
		sshRecovery = `# SSH recovery was explicitly requested with -boot-ssh.
# Reuse the router's existing host key; never change its account password.
if [ -s "$STORE/boot/ssh-host.key" ] && ! netstat -lnt 2>/dev/null | awk '$4 ~ /:22$/ {found=1} END {exit !found}'; then
  mkdir -p /etc/dropbear
  cp "$STORE/boot/ssh-host.key" /etc/dropbear/dropbear_rsa_host_key
  chmod 600 /etc/dropbear/dropbear_rsa_host_key
  /usr/sbin/dropbear -p 192.168.31.1:22 9>&- >>/tmp/mihomo-mobile-boot.log 2>&1
fi
`
	}
	return `#!/bin/sh
# Xiaomi panel: minute cron is stored in the verified persistent crontab.
# Only this program is supervised. Mihomo recovery belongs to the panel and
# honours the user's saved enabled switch. No global firewall flush or reboot.
umask 077
DIR=` + shellLiteral(dir) + `
STORE="$DIR/mobile-panel"
HOST=` + shellLiteral(host) + `
EXPECTED=` + shellLiteral(sha) + `
exec 9>/tmp/mihomo-mobile-boot.flock
flock -n 9 || exit 0
# Installer holds this short-lived marker while replacing a running panel.
if [ -f "$STORE/installing" ]; then
  now=$(date +%s)
  started=$(cat "$STORE/installing" 2>/dev/null)
  case "$started" in ''|*[!0-9]*) started=0 ;; esac
  [ "$started" -gt 0 ] && [ "$now" -ge "$started" ] && [ $((now-started)) -lt 600 ] && exit 0
fi
` + sshRecovery + `for f in /proc/[0-9]*/exe; do
  target=$(readlink "$f" 2>/dev/null) || continue
  case "$target" in /tmp/mihomo-mobile|'/tmp/mihomo-mobile (deleted)')
    echo 0 >/tmp/mihomo-mobile-boot-failures
    exit 0 ;;
  esac
done
# A different program owns the port: do not compete or kill it.
netstat -lnt 2>/dev/null | awk '$4 ~ /:9080$/ {found=1} END {exit !found}' && exit 0
tries=$(cat /tmp/mihomo-mobile-boot-failures 2>/dev/null)
case "$tries" in ''|*[!0-9]*) tries=0 ;; esac
[ "$tries" -lt 3 ] || exit 0
tries=$((tries+1))
echo "$tries" >/tmp/mihomo-mobile-boot-failures
candidate=/tmp/mihomo-mobile.boot.$$
trap 'rm -f "$candidate"' EXIT HUP INT TERM
gzip -dc "$STORE/boot/panel.gz" >"$candidate" || exit 1
actual=$(sha256sum "$candidate" 2>/dev/null | awk '{print $1}')
[ "$actual" = "$EXPECTED" ] || { echo 'Panel payload checksum failed; existing files preserved.' >>/tmp/mihomo-mobile-boot.log; exit 1; }
chmod 700 "$candidate" || exit 1
mv -f "$candidate" /tmp/mihomo-mobile || exit 1
(trap '' HUP; /tmp/mihomo-mobile -dir "$DIR" -listen "$HOST" 9>&- >>/tmp/mihomo-mobile-panel.log 2>&1 </dev/null &)
exit 0
`
}

// Preparation is explicit, invoked by the verified router installer, never by
// the ordinary monitoring loop. Original firmware startup files are not edited.
func (a *App) configureBootRecovery(panelExecutable, verifiedCronPath string, preserveSSH bool) (result error) {
	if verifiedCronPath != "/etc/crontabs/root" && verifiedCronPath != "/data/etc/crontabs/root" {
		return errors.New("当前固件的持久启动入口尚未验证，未修改启动设置")
	}
	f, e := os.Open(panelExecutable)
	if e != nil {
		return e
	}
	defer f.Close()
	s, e := f.Stat()
	if e != nil || !s.Mode().IsRegular() || s.Size() > 32<<20 {
		return errors.New("面板程序文件无法使用")
	}
	space := storageInfo(a.store)
	if space.Available && space.Free < uint64(s.Size()+4<<20) {
		return errors.New("持久存储空间不足，尚未安装开机恢复")
	}
	dir := filepath.Join(a.store, "boot")
	// Preparation is a small transaction as well: a later cron/status failure
	// must not leave an old script pointing at a new, differently hashed payload.
	paths := []string{filepath.Join(dir, "panel.gz"), filepath.Join(dir, "panel.sha256"), filepath.Join(a.store, "boot.sh"), filepath.Join(a.store, "boot.json"), verifiedCronPath, verifiedCronPath + ".mobile-panel-before", filepath.Join(filepath.Dir(verifiedCronPath), "cron.update")}
	if preserveSSH {
		paths = append(paths, filepath.Join(dir, "ssh-host.key"))
	}
	before, e := snapshotBootFiles(paths)
	if e != nil {
		return e
	}
	defer func() {
		if result != nil {
			if e := restoreBootFiles(before); e != nil {
				result = errors.New("开机恢复安装未完成，旧启动文件需要检查；当前网络保持原状")
			}
		}
	}()
	if e = os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	if preserveSSH {
		sshKey, sshError := readBackupSource("/etc/dropbear/dropbear_rsa_host_key", 8192)
		if sshError != nil || len(sshKey) == 0 {
			return errors.New("无法读取已有 SSH 主机密钥；未安装开机恢复")
		}
		if e = atomicWrite(filepath.Join(dir, "ssh-host.key"), sshKey); e != nil {
			return e
		}
	}
	tmp := filepath.Join(dir, "panel.gz.new")
	out, e := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	h := sha256.New()
	gz := gzip.NewWriter(out)
	_, e = io.Copy(gz, io.TeeReader(f, h))
	if ce := gz.Close(); e == nil {
		e = ce
	}
	if e == nil {
		e = out.Sync()
	}
	if ce := out.Close(); e == nil {
		e = ce
	}
	if e != nil {
		os.Remove(tmp)
		return e
	}
	sha := hex.EncodeToString(h.Sum(nil))
	if e = os.Rename(tmp, filepath.Join(dir, "panel.gz")); e != nil {
		return e
	}
	if e = atomicWrite(filepath.Join(dir, "panel.sha256"), []byte(sha+"\n")); e != nil {
		return e
	}
	p := filepath.Join(a.store, "boot.sh")
	if e = atomicWrite(p, []byte(bootScript(a.dir, a.host, sha, preserveSSH))); e != nil {
		return e
	}
	if e = os.Chmod(p, 0700); e != nil {
		return e
	}
	if e = installPersistentCron(verifiedCronPath, p); e != nil {
		return e
	}
	return writeJSON(filepath.Join(a.store, "boot.json"), BootSettings{Version: 1, Enabled: true, Supported: true, Mechanism: "persistent-cron", CronPath: verifiedCronPath, PanelSHA256: sha, SSH: preserveSSH})
}

func snapshotBootFiles(paths []string) (map[string][]byte, error) {
	before := map[string][]byte{}
	for _, p := range paths {
		b, e := readBackupSource(p, 32<<20)
		if os.IsNotExist(e) {
			before[p] = nil
			continue
		}
		if e != nil {
			return nil, errors.New("无法备份原启动设置，未修改开机恢复")
		}
		before[p] = b
	}
	return before, nil
}

func restoreBootFiles(before map[string][]byte) error {
	var failed bool
	for p, b := range before {
		if b == nil {
			if e := os.Remove(p); e != nil && !os.IsNotExist(e) {
				failed = true
			}
		} else {
			if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
				failed = true
				continue
			}
			if e := atomicWrite(p, b); e != nil {
				failed = true
			} else if strings.HasSuffix(p, "boot.sh") {
				_ = os.Chmod(p, 0700)
			}
		}
	}
	if failed {
		return errors.New("恢复启动设置失败")
	}
	return nil
}

const bootCronMarker = " # mihomo-mobile-v4"

func mergeBootCron(old []byte, script string) ([]byte, error) {
	if !path.IsAbs(script) || strings.ContainsAny(script, "\r\n\x00") {
		return nil, errors.New("启动脚本路径无效")
	}
	lines := []string{}
	for _, line := range strings.Split(strings.TrimRight(string(old), "\n"), "\n") {
		if strings.Contains(line, bootCronMarker) {
			continue
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	lines = append(lines, "* * * * * /bin/sh "+shellLiteral(script)+bootCronMarker)
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

func installPersistentCron(cronPath, script string) error {
	s, e := os.Lstat(cronPath)
	if e != nil || !s.Mode().IsRegular() {
		return errors.New("已验证的定时任务文件无法读取，未修改启动设置")
	}
	old, e := os.ReadFile(cronPath)
	if e != nil {
		return e
	}
	next, e := mergeBootCron(old, script)
	if e != nil {
		return e
	}
	if string(next) == string(old) {
		return nil
	}
	if e = atomicWrite(cronPath+".mobile-panel-before", old); e != nil {
		return e
	}
	if e = atomicWrite(cronPath, next); e != nil {
		return e
	}
	// BusyBox cron notices crontab directory changes; cron.update also makes the
	// intended user explicit on firmwares whose directory watch is conservative.
	return atomicWrite(filepath.Join(filepath.Dir(cronPath), "cron.update"), []byte("root\n"))
}
