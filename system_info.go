package main

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"math"
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

const remoteSystemProbeCmd = `sh -c 'uname -s -m; if [ -r /etc/os-release ]; then . /etc/os-release 2>/dev/null; printf "%s\n%s\n%s\n" "$ID" "$VERSION_ID" "$PRETTY_NAME"; elif command -v sw_vers >/dev/null 2>&1; then printf "macos\n%s\n\n" "$(sw_vers -productVersion 2>/dev/null)"; else printf "\n\n\n"; fi; cpus=$(getconf _NPROCESSORS_ONLN 2>/dev/null || nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || grep -c "^processor" /proc/cpuinfo 2>/dev/null); mem=""; if [ -r /proc/meminfo ]; then while read -r k v _; do case "$k" in MemTotal:) mem="K:$v"; break;; esac; done </proc/meminfo; fi; [ -n "$mem" ] || mem="B:$(sysctl -n hw.memsize 2>/dev/null || sysctl -n hw.physmem 2>/dev/null)"; disk=$(df -Pk / 2>/dev/null | { read -r _ && read -r _ d _ && printf "%s" "$d"; }); printf "SPEC\t%s\t%s\t%s\n" "$cpus" "$mem" "$disk"'`

type serverProfile struct {
	System string
	Spec   string
}

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

func formatMemoryBytes(totalBytes uint64) string {
	if totalBytes == 0 {
		return ""
	}
	mib := totalBytes / (1024 * 1024)
	if mib == 0 {
		return ""
	}
	if mib < 768 {
		if mib >= 128 {
			mib = ((mib + 32) / 64) * 64
		}
		return fmt.Sprintf("%dM", mib)
	}
	gib := float64(totalBytes) / (1024 * 1024 * 1024)
	ceil := math.Ceil(gib)
	if diff := ceil - gib; diff > 0 && diff <= max(0.25, ceil*0.05) {
		return fmt.Sprintf("%dG", max(1, int(ceil)))
	}
	return fmt.Sprintf("%dG", max(1, int(math.Round(gib))))
}

func formatDiskKiB(kib uint64) string {
	if kib == 0 {
		return ""
	}
	gib := float64(kib) / (1024 * 1024)
	if gib >= 1000 {
		tib := gib / 1024
		if math.Abs(tib-math.Round(tib)) < 0.05 {
			return fmt.Sprintf("%dT", max(1, int(math.Round(tib))))
		}
		return fmt.Sprintf("%.1fT", tib)
	}
	if gib >= 1 {
		return fmt.Sprintf("%dG", max(1, int(math.Round(gib))))
	}
	return fmt.Sprintf("%dM", max(1, kib/1024))
}

func parseSpecLine(line string) string {
	parts := strings.Split(strings.TrimPrefix(line, "SPEC\t"), "\t")
	var items []string
	if len(parts) >= 1 {
		if cpus, err := strconv.Atoi(strings.TrimSpace(parts[0])); err == nil && cpus >= 1 && cpus <= 4096 {
			items = append(items, fmt.Sprintf("%dC", cpus))
		}
	}
	if len(parts) >= 2 {
		memToken := strings.TrimSpace(parts[1])
		var memBytes uint64
		if after, ok := strings.CutPrefix(memToken, "K:"); ok {
			if kib, err := strconv.ParseUint(strings.TrimSpace(after), 10, 64); err == nil {
				memBytes = kib * 1024
			}
		} else if after, ok := strings.CutPrefix(memToken, "B:"); ok {
			if b, err := strconv.ParseUint(strings.TrimSpace(after), 10, 64); err == nil {
				memBytes = b
			}
		}
		if memStr := formatMemoryBytes(memBytes); memStr != "" {
			items = append(items, memStr)
		}
	}
	if len(parts) >= 3 {
		if diskKiB, err := strconv.ParseUint(strings.TrimSpace(parts[2]), 10, 64); err == nil {
			if diskStr := formatDiskKiB(diskKiB); diskStr != "" {
				items = append(items, diskStr)
			}
		}
	}
	return sanitizeSystemField(strings.Join(items, " · "))
}

func parseRemoteServerProfile(raw string) serverProfile {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	rawLines := strings.Split(strings.TrimRight(raw, "\n"), "\n")
	var osLines []string
	var spec string
	for _, line := range rawLines {
		if strings.HasPrefix(line, "SPEC\t") {
			spec = parseSpecLine(line)
			continue
		}
		osLines = append(osLines, line)
	}
	if len(osLines) == 0 || strings.TrimSpace(osLines[0]) == "" {
		return serverProfile{Spec: spec}
	}

	unameFields := strings.Fields(osLines[0])
	var kernel, arch string
	if len(unameFields) >= 1 {
		kernel = unameFields[0]
	}
	if len(unameFields) >= 2 {
		arch = normalizeArch(unameFields[len(unameFields)-1])
	}
	var id, ver, pretty string
	if len(osLines) > 1 {
		id = osLines[1]
	}
	if len(osLines) > 2 {
		ver = osLines[2]
	}
	if len(osLines) > 3 {
		pretty = osLines[3]
	}
	osPart := sanitizeSystemField(formatDistroName(id, ver, pretty, kernel))
	arch = sanitizeSystemField(arch)
	var sys string
	switch {
	case osPart != "" && arch != "":
		sys = sanitizeSystemField(osPart + " · " + arch)
	case osPart != "":
		sys = osPart
	default:
		sys = arch
	}
	return serverProfile{System: sys, Spec: spec}
}

func parseRemoteSystemInfo(raw string) string {
	return parseRemoteServerProfile(raw).System
}

func upsertMappingString(entry *yaml.Node, key, val string) {
	if val == "" {
		return
	}
	for i := 0; i < len(entry.Content); i += 2 {
		if entry.Content[i].Value == key {
			node := *entry.Content[i+1]
			node.Kind, node.Tag, node.Value = yaml.ScalarNode, "!!str", val
			node.Style, node.Alias, node.Content, node.Anchor = 0, nil, nil, ""
			entry.Content[i+1] = &node
			return
		}
	}
	entry.Content = append(entry.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: val},
	)
}

func saveServerProfile(target ServerConfig, profile serverProfile) (*Config, error) {
	profile.System = sanitizeSystemField(profile.System)
	profile.Spec = sanitizeSystemField(profile.Spec)
	if profile.System == "" && profile.Spec == "" {
		return nil, fmt.Errorf("未获取到有效服务器系统与硬件信息")
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
		upsertMappingString(entry, "system", profile.System)
		upsertMappingString(entry, "spec", profile.Spec)
		return nil
	})
}

func fetchServerProfile(parent context.Context, s ServerConfig, cfg *Config) (serverProfile, error) {
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return serverProfile{}, fmt.Errorf("未找到 ssh 命令")
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
		return serverProfile{}, err
	}
	profile := parseRemoteServerProfile(string(out))
	if profile.System == "" && profile.Spec == "" {
		return serverProfile{}, fmt.Errorf("未能解析远端系统与硬件信息")
	}
	return profile, nil
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
	if s == nil || s.Archived || (strings.TrimSpace(s.System) != "" && strings.TrimSpace(s.Spec) != "") {
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
					profile := parseRemoteServerProfile(string(out))
					if profile.System != "" || profile.Spec != "" {
						_, _ = saveServerProfile(target, profile)
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
