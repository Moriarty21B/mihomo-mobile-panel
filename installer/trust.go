package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type installerOptions struct {
	Host        string
	SSHPort     int
	Fingerprint string
	Reset       bool
	NoOpen      bool
	InstallBoot bool
}

func validateHost(host string) error {
	if host == "" || strings.TrimSpace(host) != host || len(host) > 253 {
		return errors.New("请提供有效的路由器 IP 地址或主机名")
	}
	if net.ParseIP(host) != nil {
		return nil
	}
	// A host is a network address, never a URL, shell command, or host:port pair.
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return errors.New("路由器主机名无效；端口请使用 --ssh-port 指定")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return errors.New("路由器地址只支持 IP 或 ASCII 主机名")
			}
		}
	}
	return nil
}

func validFingerprint(value string) bool {
	if !strings.HasPrefix(value, "SHA256:") {
		return false
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(value, "SHA256:"))
	return err == nil && len(raw) == sha256.Size
}

func (o installerOptions) address() (string, error) {
	if err := validateHost(o.Host); err != nil {
		return "", err
	}
	if o.SSHPort < 1 || o.SSHPort > 65535 {
		return "", errors.New("SSH 端口必须在 1 到 65535 之间")
	}
	if o.Fingerprint != "" && !validFingerprint(o.Fingerprint) {
		return "", errors.New("--fingerprint 必须是完整的 SHA256 SSH 主机密钥指纹")
	}
	return net.JoinHostPort(strings.ToLower(o.Host), strconv.Itoa(o.SSHPort)), nil
}

func publicConfigDir() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		var err error
		base, err = os.UserConfigDir()
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(base, "MihomoMobilePanelOpenSource"), nil
}

func routerStateDir(base, address string) string {
	sum := sha256.Sum256([]byte(address))
	return filepath.Join(base, "routers", hex.EncodeToString(sum[:16]))
}

type hostTrust struct {
	Address     string `json:"address"`
	Fingerprint string `json:"fingerprint"`
}

// verifyHostTrust pins one verified key per SSH endpoint. A saved key never
// changes automatically, even if a different --fingerprint is supplied later.
func verifyHostTrust(base, address, expected, actual string, confirm func(string, string) bool) error {
	if !validFingerprint(actual) || expected != "" && !validFingerprint(expected) {
		return errors.New("SSH 主机指纹格式无效，已停止连接")
	}
	if expected != "" && expected != actual {
		return errors.New("路由器主机指纹与 --fingerprint 不符，已停止连接")
	}
	path := filepath.Join(routerStateDir(base, address), "host-key.json")
	b, err := os.ReadFile(path)
	if err == nil {
		var known hostTrust
		if json.Unmarshal(b, &known) != nil || known.Address != address || !validFingerprint(known.Fingerprint) {
			return errors.New("本机保存的路由器身份记录无效，请检查 host-key.json")
		}
		if known.Fingerprint != actual {
			return errors.New("路由器 SSH 身份与已确认记录不同，已停止连接；请独立核实后再手动移除对应 host-key.json")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("无法读取本机路由器身份记录：%w", err)
	}
	if expected == "" && (confirm == nil || !confirm(address, actual)) {
		return errors.New("尚未确认路由器 SSH 身份，未发送登录密码")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(hostTrust{address, actual}, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "host-key-*.new")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(encoded, '\n'))
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
