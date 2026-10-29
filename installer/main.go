package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

//go:embed payload/mihomo-mobile
var binary []byte

func protect(b []byte, decrypt bool) ([]byte, error) {
	if len(b) == 0 {
		return nil, errors.New("没有保存的密码")
	}
	in := windows.DataBlob{Size: uint32(len(b)), Data: &b[0]}
	var out windows.DataBlob
	var e error
	if decrypt {
		e = windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	} else {
		e = windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	}
	if e != nil {
		return nil, e
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, int(out.Size))...), nil
}
func getPassword(reset bool, savedPath string) (string, error) {
	if !reset {
		if b, e := os.ReadFile(savedPath); e == nil {
			if p, e := protect(b, true); e == nil {
				return string(p), nil
			}
		}
	}
	fmt.Print("请输入路由器 root 密码（输入不显示，按回车完成）：")
	b, e := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if e != nil {
		return "", errors.New("请双击运行这个程序，在弹出的窗口中输入密码")
	}
	if len(b) == 0 {
		return "", errors.New("密码不能为空")
	}
	return string(b), nil
}
func run(c *ssh.Client, command string, input []byte) (string, error) {
	s, e := c.NewSession()
	if e != nil {
		return "", e
	}
	defer s.Close()
	if input != nil {
		s.Stdin = bytes.NewReader(input)
	}
	var out, errout bytes.Buffer
	s.Stdout = &out
	// SSH copies stdout/stderr concurrently; bytes.Buffer is not concurrency safe.
	s.Stderr = &errout
	done := make(chan error, 1)
	go func() { done <- s.Run(command) }()
	select {
	case e = <-done:
		if e != nil {
			return out.String(), fmt.Errorf("%s", strings.TrimSpace(out.String()+errout.String()))
		}
		return out.String(), nil
	case <-time.After(65 * time.Second):
		s.Close()
		return "", errors.New("路由器响应超时，请稍后重试")
	}
}
func install(options installerOptions) (result error) {
	address, e := options.address()
	if e != nil {
		return e
	}
	configDir, e := publicConfigDir()
	if e != nil {
		return e
	}
	savedPath := filepath.Join(routerStateDir(configDir, address), "password.dpapi")
	password, e := getPassword(options.Reset, savedPath)
	if e != nil {
		return e
	}
	config := &ssh.ClientConfig{User: "root", Auth: []ssh.AuthMethod{ssh.Password(password)}, Timeout: 10 * time.Second,
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA},
		HostKeyCallback: func(host string, remote net.Addr, key ssh.PublicKey) error {
			return verifyHostTrust(configDir, address, options.Fingerprint, ssh.FingerprintSHA256(key), func(endpoint, fingerprint string) bool {
				fmt.Printf("首次连接 %s\nSSH 主机指纹：%s\n请先通过路由器本机或可信记录核对，确认后输入 yes：", endpoint, fingerprint)
				answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
				return err == nil && strings.EqualFold(strings.TrimSpace(answer), "yes")
			})
		}}
	fmt.Println("正在连接路由器…")
	client, e := ssh.Dial("tcp", address, config)
	if e != nil {
		if strings.Contains(e.Error(), "authenticate") {
			return errors.New("密码不正确。请运行“重新输入密码.bat”再试")
		}
		return fmt.Errorf("无法连接，请确认电脑连着家里的网络、路由器 SSH 已开启。详细信息：%s", e)
	}
	defer client.Close()
	if b, e := protect([]byte(password), false); e == nil {
		os.MkdirAll(filepath.Dir(savedPath), 0700)
		os.WriteFile(savedPath, b, 0600)
	}
	preflight := `set -eu
test "$(uname -m)" = aarch64 || { echo '路由器架构不匹配'; exit 1; }
test -x /data/other/mihomo/mihomo || { echo '请先恢复路由器上的 Mihomo 程序'; exit 1; }
test -f /data/other/mihomo/config.yaml || { echo '请先恢复 Mihomo 配置'; exit 1; }
test ! -d /tmp/mihomo-manager.lock || { echo '旧管理工具仍在操作，请完成后重试'; exit 1; }
command -v flock >/dev/null || { echo '当前固件缺少恢复所需的进程锁工具'; exit 1; }
mkdir -p /data/other/mihomo/mobile-panel
available=$(df -k /tmp | awk 'END {print $4}')
test "$available" -gt 20000 || { echo '路由器临时空间不足'; exit 1; }
`
	if _, e = run(client, preflight, nil); e != nil {
		return e
	}
	fmt.Println("正在准备手机管理页面，保留现有订阅与登录设置…")
	if _, e = run(client, "umask 077; cat > /tmp/mihomo-mobile.new", binary); e != nil {
		return e
	}
	sum := sha256.Sum256(binary)
	expected := hex.EncodeToString(sum[:])
	out, e := run(client, "sha256sum /tmp/mihomo-mobile.new", nil)
	if e != nil || !strings.HasPrefix(strings.TrimSpace(out), expected) {
		return fmt.Errorf("文件传输检查失败，未启动新版本（校验响应：%q；%v）", out, e)
	}
	stop := `set -eu
exec 9>/tmp/mihomo-manager.flock
flock -w 45 9 || { echo '管理操作尚未结束，请稍后重试'; exit 1; }
mkdir /tmp/mihomo-manager.lock || { echo '有管理操作正在进行，请稍后重试'; exit 1; }
trap 'rmdir /tmp/mihomo-manager.lock 2>/dev/null || true' EXIT
date +%s > /data/other/mihomo/mobile-panel/installing
for f in /proc/[0-9]*/exe; do
 e=$(readlink "$f" 2>/dev/null || true)
 case "$e" in /tmp/mihomo-mobile|'/tmp/mihomo-mobile (deleted)') p=${f#/proc/}; p=${p%/exe}; kill "$p";; esac
done
for n in 1 2 3 4 5 6 7 8 9 10; do
 if ! netstat -lnt | awk '$4 ~ /:9080$/ {found=1} END {exit !found}'; then break; fi
 sleep 1
done
if netstat -lnt | awk '$4 ~ /:9080$/ {found=1} END {exit !found}'; then echo '9080 端口被其他程序占用，已停止安装'; exit 1; fi
if [ -f /tmp/mihomo-mobile ]; then cp /tmp/mihomo-mobile /tmp/mihomo-mobile.previous; fi
mv /tmp/mihomo-mobile.new /tmp/mihomo-mobile
chmod 700 /tmp/mihomo-mobile
`
	if _, e = run(client, stop, nil); e != nil {
		_, _ = run(client, "rm -f /data/other/mihomo/mobile-panel/installing", nil)
		return e
	}
	defer func() { _, _ = run(client, "rm -f /data/other/mihomo/mobile-panel/installing", nil) }()
	defer func() {
		if result != nil {
			_, rollbackErr := run(client, `set -eu
if [ -f /tmp/mihomo-mobile.previous ]; then
 for f in /proc/[0-9]*/exe; do e=$(readlink "$f" 2>/dev/null || true); case "$e" in /tmp/mihomo-mobile|'/tmp/mihomo-mobile (deleted)') p=${f#/proc/}; kill "${p%/exe}";; esac; done
 sleep 1
 mv /tmp/mihomo-mobile.previous /tmp/mihomo-mobile
 (trap '' HUP; /tmp/mihomo-mobile </dev/null >/tmp/mihomo-mobile.log 2>&1 &)
fi`, nil)
			if rollbackErr != nil {
				result = fmt.Errorf("%w；旧管理程序恢复未完成，请检查 /tmp/mihomo-mobile.previous", result)
			}
		}
	}()
	credentials, _ := json.Marshal(map[string]string{"password": password})
	if _, e = run(client, "if [ ! -f /data/other/mihomo/mobile-panel/settings.json ]; then /tmp/mihomo-mobile -init; fi", credentials); e != nil {
		return e
	}
	start := `set -eu
(trap '' HUP; /tmp/mihomo-mobile </dev/null >/tmp/mihomo-mobile.log 2>&1 &)
status=0
for n in 1 2 3 4 5 6 7 8 9 10; do
 status=$(curl --noproxy '*' -s -o /dev/null -w '%{http_code}' --max-time 3 http://192.168.31.1:9080/ || true)
 [ "$status" = 200 ] && break
 sleep 2
done
[ "$status" = 200 ] || { echo '新版本未启动成功，将恢复原管理服务'; exit 1; }
`
	if _, e = run(client, start, nil); e != nil {
		return e
	}
	if options.InstallBoot {
		verified := `set -eu
mount | awk '$3=="/etc/crontabs" && $5 ~ /^ext4/ {found=1} END {exit !found}' || { echo '页面已更新，但此固件的持久启动位置尚未验证'; exit 1; }
/tmp/mihomo-mobile -install-boot /etc/crontabs/root
`
		if _, e = run(client, verified, nil); e != nil {
			return e
		}
	}
	panelURL := "http://" + net.JoinHostPort(options.Host, "9080") + "/"
	fmt.Printf("完成！手机连上路由器 Wi-Fi 后，打开：\n%s\n日常使用无需保持电脑开机。\n", panelURL)
	if !options.InstallBoot {
		fmt.Println("本次未更改开机恢复设置；需要时请使用 --install-boot。")
	}
	if !options.NoOpen {
		_ = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", panelURL).Start()
	}
	return nil
}
func main() {
	check := flag.Bool("check-only", false, "Check embedded payload without connecting")
	reset := flag.Bool("reset-password", false, "Ask for password again")
	noOpen := flag.Bool("no-open", false, "Do not open browser")
	noPause := flag.Bool("no-pause", false, "Do not pause")
	host := flag.String("host", "192.168.31.1", "SSH host (panel LAN preset remains 192.168.31.0/24)")
	port := flag.Int("ssh-port", 22, "SSH port")
	fingerprint := flag.String("fingerprint", "", "Expected SHA256 SSH host fingerprint; first use otherwise asks for confirmation")
	boot := flag.Bool("install-boot", false, "Opt in to persistent panel/proxy recovery on supported firmware; does not enable SSH recovery")
	flag.Parse()
	if *check {
		sum := sha256.Sum256(binary)
		fmt.Printf("Embedded ARM64 service: %d bytes; SHA256 %x\n", len(binary), sum)
		return
	}
	fmt.Println("小米网络 4.0 · 安装或更新管理页面")
	fmt.Println()
	e := install(installerOptions{Host: *host, SSHPort: *port, Fingerprint: *fingerprint, Reset: *reset, NoOpen: *noOpen, InstallBoot: *boot})
	if e != nil {
		fmt.Println("\n未完成：", e)
	}
	if !*noPause {
		fmt.Print("\n按回车关闭窗口…")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
	if e != nil {
		os.Exit(1)
	}
}
