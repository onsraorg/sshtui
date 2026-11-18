package main

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

const remoteSystemProbeCmd = `sh -c 'uname -s -m; if [ -r /etc/os-release ]; then . /etc/os-release 2>/dev/null; printf "%s\n%s\n%s\n" "$ID" "$VERSION_ID" "$PRETTY_NAME"; elif command -v sw_vers >/dev/null 2>&1; then printf "macos\n%s\n\n" "$(sw_vers -productVersion 2>/dev/null)"; fi'`

type systemProbeState struct {
	Probing bool
	Err     string
}

func sanitizeSystemField(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) > 64 {
		s = string(runes[:64])
	}
	return strings.TrimSpace(s)
}

func normalizeArch(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "x86_64", "amd64":
		return "AMD64"
	case "aarch64", "arm64", "arm64e":
		return "ARM64"
	case "armv7l", "armv7", "armhf":
		return "ARMv7"
	case "armv6l", "armv6":
		return "ARMv6"
	case "i386", "i686", "x86":
		return "x86"
	case "riscv64":
		return "RISC-V"
	default:
		return sanitizeSystemField(raw)
	}
}

func formatDistroName(id, versionID, prettyName, kernel string) string {
	idLower := strings.ToLower(strings.TrimSpace(id))
	ver := strings.TrimSpace(versionID)
	pretty := strings.TrimSpace(prettyName)

	var distro string
	switch idLower {
	case "ubuntu":
		distro = "Ubuntu"
	case "debian":
		distro = "Debian"
	case "alpine":
		distro = "Alpine"
		if parts := strings.Split(ver, "."); len(parts) > 2 {
			ver = parts[0] + "." + parts[1]
		}
	case "rocky":
		distro = "Rocky"
	case "almalinux":
		distro = "AlmaLinux"
	case "centos":
		distro = "CentOS"
	case "ol":
		distro = "Oracle Linux"
	case "fedora":
		distro = "Fedora"
	case "arch":
		return "Arch Linux"
	case "kali":
		distro = "Kali"
	case "amzn":
		distro = "Amazon Linux"
	case "opensuse-leap", "opensuse":
		distro = "openSUSE"
	case "opensuse-tumbleweed":
		return "openSUSE Tumbleweed"
	case "sles":
		distro = "SLES"
	case "macos":
		distro = "macOS"
	case "freebsd":
		distro = "FreeBSD"
	}

	if distro != "" {
		if ver != "" {
			return distro + " " + ver
		}
		return distro
	}
	if pretty != "" {
		pretty = strings.ReplaceAll(pretty, " GNU/Linux", "")
		pretty = strings.TrimSuffix(pretty, " LTS")
		if idx := strings.Index(pretty, " ("); idx > 0 {
			pretty = pretty[:idx]
		}
		return sanitizeSystemField(pretty)
	}
	if idLower != "" {
		title := strings.ToUpper(idLower[:1]) + idLower[1:]
		if ver != "" {
			return title + " " + ver
		}
		return title
	}
	switch strings.ToLower(kernel) {
	case "darwin":
		return "macOS"
	default:
		return sanitizeSystemField(kernel)
	}
}

func parseRemoteSystemInfo(raw string) string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return ""
	}
	unameFields := strings.Fields(lines[0])
	var kernel, arch string
	if len(unameFields) >= 1 {
		kernel = unameFields[0]
	}
	if len(unameFields) >= 2 {
		arch = normalizeArch(unameFields[len(unameFields)-1])
	}
	var id, ver, pretty string
	if len(lines) > 1 {
		id = lines[1]
	}
	if len(lines) > 2 {
		ver = lines[2]
	}
	if len(lines) > 3 {
		pretty = lines[3]
	}
	osPart := sanitizeSystemField(formatDistroName(id, ver, pretty, kernel))
	arch = sanitizeSystemField(arch)
	switch {
	case osPart != "" && arch != "":
		return sanitizeSystemField(osPart + " · " + arch)
	case osPart != "":
		return osPart
	default:
		return arch
	}
}

func saveServerSystem(target ServerConfig, sysInfo string) (*Config, error) {
	sysInfo = sanitizeSystemField(sysInfo)
	if sysInfo == "" {
		return nil, fmt.Errorf("未获取到有效系统信息")
	}
	return editServersConfig(func(cfg *Config, servers *yaml.Node) error {
		index := -1
		for i := range cfg.Servers {
			c := cfg.Servers[i]
			if c.sourceIndex == target.sourceIndex && c.Name == target.Name && c.Host == target.Host && c.Port == target.Port && c.User == target.User {
				index = c.sourceIndex
				break
			}
		}
		if index < 0 {
			for i := range cfg.Servers {
				c := cfg.Servers[i]
				if c.Name == target.Name && c.Group == target.Group && c.Host == target.Host && c.Port == target.Port && c.User == target.User {
					index = c.sourceIndex
					break
				}
			}
		}
		if index < 0 || index >= len(servers.Content) {
			return fmt.Errorf("服务器配置已变化，请重新打开后再试")
		}
		entry := servers.Content[index]
		if entry.Kind == yaml.AliasNode || entry.Anchor != "" {
			entry = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Tag: "!!merge", Value: "<<"}, entry,
			}}
			servers.Content[index] = entry
		}
		if entry.Kind != yaml.MappingNode {
			return fmt.Errorf("服务器配置必须为 YAML 映射")
		}
		for i := 0; i < len(entry.Content); i += 2 {
			if entry.Content[i].Value == "system" {
				value := *entry.Content[i+1]
				value.Kind, value.Tag, value.Value = yaml.ScalarNode, "!!str", sysInfo
				value.Style, value.Alias, value.Content, value.Anchor = 0, nil, nil, ""
				entry.Content[i+1] = &value
				return nil
			}
		}
		entry.Content = append(entry.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "system"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: sysInfo},
		)
		return nil
	})
}

func fetchServerSystemInfo(parent context.Context, s ServerConfig, cfg *Config) (string, error) {
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return "", fmt.Errorf("未找到 ssh 命令")
	}
	ctx, cancel := context.WithTimeout(parent, 12*time.Second)
	defer cancel()

	args := []string{"-p", strconv.Itoa(s.Port)}
	if s.Auth == "key" && s.Key != "" {
		args = append(args, "-i", expandPath(s.Key))
	}
	args = append(args, sshOptions(&s, cfg)...)
	args = append(args, "-o", "PermitLocalCommand=no", "-T")
	if s.Auth != "password" {
		args = append(args, "-o", "BatchMode=yes")
	}
	args = append(args, "--", fmt.Sprintf("%s@%s", s.User, s.Host), remoteSystemProbeCmd)

	var out []byte
	if s.Auth == "password" {
		out, err = runAskpassCapture(ctx, sshPath, args, s.Password)
	} else {
		cmd := exec.CommandContext(ctx, sshPath, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		out, err = cmd.Output()
	}
	if err != nil {
		return "", err
	}
	info := parseRemoteSystemInfo(string(out))
	if info == "" {
		return "", fmt.Errorf("未能解析远端系统信息")
	}
	return info, nil
}

func runAskpassCapture(ctx context.Context, sshPath string, args []string, password string) ([]byte, error) {
	if err := validatePassword(password); err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	expectedInfo, _ := os.Stat(self)

	baseDir := os.TempDir()
	if len(filepath.Join(baseDir, "sshtui-askpass-0000000000", "password.sock")) >= 100 {
		baseDir = "/tmp"
	}
	dir, err := os.MkdirTemp(baseDir, "sshtui-askpass-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	sockPath := filepath.Join(dir, "password.sock")
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, err
	}

	pidChan := make(chan int, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	defer func() {
		close(pidChan)
		_ = listener.Close()
		wg.Wait()
	}()

	go func() {
		defer wg.Done()
		expectedPID, ok := <-pidChan
		if !ok || expectedPID <= 0 {
			return
		}
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if isAuthorizedPeer(conn, expectedPID, self, expectedInfo) {
				_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
				_, _ = io.WriteString(conn, password+"\n")
			}
			_ = conn.Close()
		}
	}()

	cmd := exec.CommandContext(ctx, sshPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "SSH_ASKPASS", "SSH_ASKPASS_REQUIRE", "SSH_ASKPASS_PROMPT", "SSHTUI_ASKPASS_SOCKET", "SSHTUI_ASKPASS_HELPER":
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env,
		"SSH_ASKPASS="+self,
		"SSH_ASKPASS_REQUIRE=force",
		"SSHTUI_ASKPASS_SOCKET="+sockPath,
		"SSHTUI_ASKPASS_HELPER=1",
	)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pidChan <- cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

func startMuxSystemCollector(s *ServerConfig, sshPath string) ([]string, func()) {
	if s == nil || s.Archived || strings.TrimSpace(s.System) != "" {
		return nil, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) || os.Getenv("TERM") == "dumb" {
		return nil, nil
	}
	baseDir := os.TempDir()
	if len(filepath.Join(baseDir, "sshtui-mux-0000000000", "m.sock")) >= 100 {
		baseDir = "/tmp"
	}
	muxDir, err := os.MkdirTemp(baseDir, "sshtui-mux-")
	if err != nil {
		return nil, nil
	}
	muxSock := filepath.Join(muxDir, "m.sock")
	target := *s
	endpoint := fmt.Sprintf("%s@%s", cmp.Or(s.User, "root"), s.Host)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(150 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				info, err := os.Stat(muxSock)
				if err != nil || info.Mode()&os.ModeSocket == 0 {
					continue
				}
				probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Second)
				cmd := exec.CommandContext(probeCtx, sshPath,
					"-F", "/dev/null",
					"-o", "ControlPath="+muxSock,
					"-o", "ProxyCommand=false",
					"-o", "BatchMode=yes",
					"-o", "PermitLocalCommand=no",
					"-p", strconv.Itoa(target.Port),
					"-T", "--", endpoint, remoteSystemProbeCmd,
				)
				cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
				out, err := cmd.Output()
				probeCancel()
				if err == nil {
					if sysInfo := parseRemoteSystemInfo(string(out)); sysInfo != "" {
						_, _ = saveServerSystem(target, sysInfo)
					}
				}
				return
			}
		}
	}()

	extraArgs := []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + muxSock,
		"-o", "ControlPersist=no",
	}
	cleanup := func() {
		cancel()
		wg.Wait()
		_ = os.RemoveAll(muxDir)
	}
	return extraArgs, cleanup
}
