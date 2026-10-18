package main

import (
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

//go:embed VERSION
var version string

// ServerConfig describes one SSH target.
type ServerConfig struct {
	Name     string   `yaml:"name"`
	Group    string   `yaml:"group"`
	Host     string   `yaml:"host"`
	Port     int      `yaml:"port"`
	User     string   `yaml:"user"`
	Auth     string   `yaml:"auth"` // "key" or "password"
	Key      string   `yaml:"key"`
	Password string   `yaml:"password"`
	Args     []string `yaml:"args"` // Additional SSH arguments.
}

// Config holds defaults and SSH targets.
type Config struct {
	Default struct {
		User                string `yaml:"user"`
		Port                int    `yaml:"port"`
		Key                 string `yaml:"key"`
		ServerAliveInterval int    `yaml:"server_alive_interval"`  // Keepalive interval in seconds; defaults to 30.
		ServerAliveCountMax int    `yaml:"server_alive_count_max"` // Failed keepalive limit; defaults to 3.
	} `yaml:"default"`
	Servers []ServerConfig `yaml:"servers"`
}

func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func getConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	paths := []string{
		filepath.Join(home, ".config", "sshtui", "servers.yaml"),
		filepath.Join(home, ".ssh", "servers.yaml"),
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
	}
	return paths[0], nil
}

func createDefaultConfig(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	sample := `# ==============================================================================
# sshtui server configuration
# Location: ~/.config/sshtui/servers.yaml
# Permissions are enforced as 0600 to protect passwords.
# ==============================================================================

default:
  user: root
  port: 22
  key: ~/.ssh/id_ed25519
  server_alive_interval: 30
  server_alive_count_max: 3

servers:
  - name: "开发环境-应用服务"
    group: "Dev"
    host: "192.168.1.101"
    port: 22
    user: "ubuntu"
    auth: "key"
    key: "~/.ssh/id_ed25519"

  - name: "测试集群-API网关"
    group: "Test"
    host: "192.168.1.102"
    port: 22
    user: "root"
    auth: "password"
    password: "your_password_here"

  - name: "生产主库 (PostgreSQL)"
    group: "Prod"
    host: "10.0.0.5"
    port: 2222
    user: "admin"
    auth: "key"
    key: "~/.ssh/id_ed25519"
`
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		return nil // Another process created it; never overwrite an existing config.
	}
	if err != nil {
		return err
	}
	_, err = io.WriteString(f, sample)
	return errors.Join(err, f.Close())
}

// Check permissions on the descriptor that will be read to avoid path races.
func openConfig() (*os.File, string, error) {
	cfgPath, err := getConfigPath()
	if err != nil {
		return nil, "", err
	}

	f, err := os.Open(cfgPath)
	if os.IsNotExist(err) {
		if err := createDefaultConfig(cfgPath); err != nil {
			return nil, "", fmt.Errorf("创建默认配置文件失败: %w", err)
		}
		f, err = os.Open(cfgPath)
	}
	if err != nil {
		return nil, "", fmt.Errorf("读取配置文件失败: %w", err)
	}
	if err := secureConfig(f); err != nil {
		f.Close()
		return nil, "", fmt.Errorf("检查配置文件及权限失败: %w", err)
	}
	return f, cfgPath, nil
}

func secureConfig(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("配置文件必须是普通文件")
	}
	if info.Mode().Perm() != 0600 {
		return f.Chmod(0600)
	}
	return nil
}

func loadConfig() (*Config, string, error) {
	f, cfgPath, err := openConfig()
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, "", fmt.Errorf("读取配置文件失败: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, "", fmt.Errorf("解析 YAML 配置文件失败: %w", err)
	}

	if cfg.Default.ServerAliveInterval <= 0 {
		cfg.Default.ServerAliveInterval = 30
	}
	if cfg.Default.ServerAliveCountMax <= 0 {
		cfg.Default.ServerAliveCountMax = 3
	}

	for i := range cfg.Servers {
		s := &cfg.Servers[i]
		s.User = cmp.Or(s.User, cfg.Default.User, "root")
		s.Port = cmp.Or(s.Port, cfg.Default.Port, 22)
		s.Group = cmp.Or(s.Group, "Default")
		if s.Auth == "" {
			if s.Password != "" {
				s.Auth = "password"
			} else {
				s.Auth = "key"
			}
		}
		if s.Auth == "key" {
			s.Key = cmp.Or(s.Key, cfg.Default.Key)
		}
		if s.Host == "" || s.Port < 1 || s.Port > 65535 {
			return nil, "", fmt.Errorf("服务器 %q 的 host 不能为空，port 必须在 1–65535 之间", s.Name)
		}
		if s.Auth != "key" && s.Auth != "password" {
			return nil, "", fmt.Errorf("服务器 %q 的 auth 必须是 key 或 password", s.Name)
		}
		fields := append([]string{s.Name, s.Group, s.Host, s.User, s.Key}, s.Args...)
		for _, field := range fields {
			if strings.ContainsRune(field, 0) {
				return nil, "", fmt.Errorf("服务器 %q 的配置不能包含 NUL 字符", s.Name)
			}
		}
		if err := validatePassword(s.Password); err != nil {
			return nil, "", fmt.Errorf("服务器 %q: %w", s.Name, err)
		}
	}

	slices.SortStableFunc(cfg.Servers, func(a, b ServerConfig) int {
		return cmp.Or(strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), strings.Compare(a.Name, b.Name))
	})

	return &cfg, cfgPath, nil
}

type model struct {
	config      *Config
	filter      string
	filtered    []ServerConfig
	cursor      int
	viewportTop int
	selected    *ServerConfig
	width       int
	height      int
	quitting    bool
}

func initialModel(cfg *Config, initialFilter string) model {
	m := model{
		config:      cfg,
		filter:      initialFilter,
		viewportTop: 0,
		height:      24,
		width:       80,
	}
	m.applyFilter()
	return m
}

func (m *model) applyFilter() {
	if m.filter == "" {
		m.filtered = m.config.Servers
	} else {
		lowerFilter := strings.ToLower(m.filter)
		var res []ServerConfig
		for _, s := range m.config.Servers {
			target := strings.ToLower(fmt.Sprintf("%s %s %s %s", s.Name, s.Group, s.Host, s.User))
			if strings.Contains(target, lowerFilter) {
				res = append(res, s)
			}
		}
		m.filtered = res
	}

	m.cursor = max(0, min(m.cursor, len(m.filtered)-1))
	m.adjustViewport()
}

func (m *model) maxVisibleItems() int {
	avail := m.height - 9
	if m.width < 74 || m.height < 14 {
		avail = m.height - 3
	}
	return min(18, max(1, avail))
}

func (m *model) adjustViewport() {
	visible := m.maxVisibleItems()
	if len(m.filtered) <= visible {
		m.viewportTop = 0
		return
	}

	if m.cursor < m.viewportTop {
		m.viewportTop = m.cursor
	} else if m.cursor >= m.viewportTop+visible {
		m.viewportTop = m.cursor - visible + 1
	}
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.adjustViewport()
		return m, nil

	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC:
			m.quitting = true
			return m, tea.Quit

		case tea.KeyEsc:
			if m.filter != "" {
				m.filter = ""
				m.applyFilter()
				return m, nil
			}
			m.quitting = true
			return m, tea.Quit

		case tea.KeyUp, tea.KeyCtrlP:
			if m.cursor > 0 {
				m.cursor--
			} else if len(m.filtered) > 0 {
				m.cursor = len(m.filtered) - 1
			}
			m.adjustViewport()
			return m, nil

		case tea.KeyDown, tea.KeyCtrlN:
			if m.cursor < len(m.filtered)-1 {
				m.cursor++
			} else {
				m.cursor = 0
			}
			m.adjustViewport()
			return m, nil

		case tea.KeyEnter:
			if m.width >= 16 && m.height >= 4 && len(m.filtered) > 0 && m.cursor < len(m.filtered) {
				chosen := m.filtered[m.cursor]
				m.selected = &chosen
				return m, tea.Quit
			}

		case tea.KeyBackspace:
			if len(m.filter) > 0 {
				r := []rune(m.filter)
				m.filter = string(r[:len(r)-1])
				m.applyFilter()
			}
			return m, nil

		case tea.KeyRunes:
			m.filter += string(msg.Runes)
			m.applyFilter()
			return m, nil
		}
	}

	return m, nil
}

func (m model) View() string {
	if m.quitting || m.selected != nil || m.width <= 0 || m.height <= 0 {
		return ""
	}
	if m.width < 16 || m.height < 4 {
		return runewidth.Truncate("窗口过小，请放大", max(0, m.width), "")
	}
	if m.width < 74 || m.height < 14 {
		return m.compactView()
	}

	primaryColor := lipgloss.Color("#38bdf8")
	accentColor := lipgloss.Color("#a855f7")
	dimColor := lipgloss.Color("#64748b")
	textColor := lipgloss.Color("#f8fafc")
	selectedBg := lipgloss.Color("#1e293b")
	keyBadgeColor := lipgloss.Color("#34d399")
	passBadgeColor := lipgloss.Color("#fbbf24")

	headerBox := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#475569")).
		Padding(0, 1).
		Width(72)

	searchPrompt := lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("❯ Filter: ")
	searchText := lipgloss.NewStyle().Foreground(textColor).Render(runewidth.TruncateLeft(m.filter, 56, "…"))
	cursorBlock := lipgloss.NewStyle().Foreground(primaryColor).Render("▋")
	searchBar := headerBox.Render(searchPrompt + searchText + cursorBlock)

	title := lipgloss.NewStyle().Bold(true).Foreground(primaryColor).Render("⚡ GHOSTTY SSH PICKER")
	subTitle := lipgloss.NewStyle().Foreground(dimColor).Render("  上下移动选择，回车直接连接")
	head := title + subTitle

	var b strings.Builder
	b.WriteString("\n" + head + "\n" + searchBar + "\n\n")

	if len(m.filtered) == 0 {
		emptyMsg := lipgloss.NewStyle().
			Foreground(dimColor).
			Italic(true).
			PaddingLeft(3).
			Render("未找到匹配的服务器 (按 Esc 清空输入)")
		b.WriteString(emptyMsg + "\n\n")
	} else {
		visible := m.maxVisibleItems()
		end := min(len(m.filtered), m.viewportTop+visible)

		for i := m.viewportTop; i < end; i++ {
			s := m.filtered[i]
			isSelected := (i == m.cursor)

			nameColor, connColor := textColor, dimColor
			prefix := "   "
			rowStyle := lipgloss.NewStyle().Padding(0, 1)
			if isSelected {
				nameColor, connColor = primaryColor, textColor
				prefix = lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render(" ❯ ")
				rowStyle = rowStyle.Background(selectedBg)
			}
			authBadge := lipgloss.NewStyle().Foreground(keyBadgeColor).Render("🔑 密钥")
			if s.Auth == "password" {
				authBadge = lipgloss.NewStyle().Foreground(passBadgeColor).Render("🔒 密码")
			}
			content := fmt.Sprintf("%s %s %s %s",
				lipgloss.NewStyle().Foreground(accentColor).Bold(isSelected).Render(fitColumn("["+s.Group+"]", 9)),
				lipgloss.NewStyle().Foreground(nameColor).Bold(isSelected).Render(fitColumn(s.Name, 20)),
				lipgloss.NewStyle().Foreground(connColor).Render(fitColumn(fmt.Sprintf("%s@%s:%d", s.User, s.Host, s.Port), 23)),
				authBadge)
			line := prefix + rowStyle.Render(content)
			b.WriteString(line + "\n")
		}
	}

	b.WriteString("\n")
	helpKey := lipgloss.NewStyle().Foreground(primaryColor).Bold(true)
	helpDesc := lipgloss.NewStyle().Foreground(dimColor)
	countInfo := lipgloss.NewStyle().Foreground(dimColor).Render(fmt.Sprintf("(%d/%d)", len(m.filtered), len(m.config.Servers)))

	footer := fmt.Sprintf("  %s %s  %s %s  %s %s  %s %s  %s",
		helpKey.Render("↑/↓"), helpDesc.Render("选择"),
		helpKey.Render("Enter"), helpDesc.Render("连接"),
		helpKey.Render("输入"), helpDesc.Render("过滤"),
		helpKey.Render("Esc"), helpDesc.Render("清除/退出"),
		countInfo,
	)
	b.WriteString(footer + "\n")

	return b.String()
}

func fitColumn(s string, width int) string {
	return runewidth.FillRight(runewidth.Truncate(s, width, "…"), width)
}

// Keep target details visible in narrow terminals.
func (m model) compactView() string {
	fit := func(s string) string { return runewidth.Truncate(s, m.width, "…") }
	lines := []string{fit("⚡ SSH PICKER"), fit("❯ " + runewidth.TruncateLeft(m.filter, m.width-2, "…"))}
	end := min(len(m.filtered), m.viewportTop+m.maxVisibleItems())
	for i := m.viewportTop; i < end; i++ {
		s := m.filtered[i]
		address := fmt.Sprintf("%s@%s:%d", s.User, s.Host, s.Port)
		addressWidth := min(runewidth.StringWidth(address), (m.width-3)*2/3)
		nameWidth := m.width - addressWidth - 3
		line := "  " + fitColumn(s.Name, nameWidth) + " " + runewidth.Truncate(address, addressWidth, "…")
		if i == m.cursor {
			line = "❯ " + strings.TrimPrefix(line, "  ")
			line = lipgloss.NewStyle().Foreground(lipgloss.Color("#38bdf8")).Bold(true).Render(line)
		}
		lines = append(lines, line)
	}
	if len(m.filtered) == 0 {
		lines = append(lines, fit("未找到服务器，Esc 清空"))
	}
	lines = append(lines, fit(fmt.Sprintf("↑↓ 选择 Enter 连接 Esc 返回 (%d/%d)", len(m.filtered), len(m.config.Servers))))
	return strings.Join(lines, "\n")
}

// OpenSSH uses the first value, so explicit options must precede defaults.
func sshOptions(s *ServerConfig, cfg *Config) []string {
	args := append([]string(nil), s.Args...)
	return append(args,
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=10",
		"-o", "ConnectionAttempts=1",
		"-o", fmt.Sprintf("ServerAliveInterval=%d", cfg.Default.ServerAliveInterval),
		"-o", fmt.Sprintf("ServerAliveCountMax=%d", cfg.Default.ServerAliveCountMax),
		"-o", "TCPKeepAlive=yes")
}

func validatePassword(password string) error {
	// OpenSSH reserves one byte in its 1024-byte prompt buffer for NUL.
	if len(password) > 1023 {
		return fmt.Errorf("密码不能超过 1023 个 UTF-8 字节（OpenSSH 限制）")
	}
	if strings.ContainsAny(password, "\x00\r\n") {
		return fmt.Errorf("密码不能包含 NUL 或换行符")
	}
	return nil
}

func passwordPipe(password string) (*os.File, error) {
	if err := validatePassword(password); err != nil {
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	// At most 1024 bytes fit in a macOS/Linux pipe before Exec starts the reader.
	_, err = io.WriteString(w, password+"\n")
	err = errors.Join(err, w.Close())
	if err == nil {
		_, err = unix.FcntlInt(r.Fd(), unix.F_SETFD, 0)
	}
	if err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

// Only local SSH authentication can request the password through ASKPASS.
// Keep the socket private and secrets out of argv, environment variables and scripts.
func runWithAskpass(sshPath string, args []string, password string, env []string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	// Keep Unix socket paths below the platform's roughly 104-byte limit.
	dir, err := os.MkdirTemp("/tmp", "sshtui-askpass-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "password.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_, _ = io.WriteString(conn, password+"\n")
			conn.Close()
		}
	}()
	cmd := exec.Command(sshPath, args...)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if key != "SSH_ASKPASS" && key != "SSH_ASKPASS_REQUIRE" && key != "SSHTUI_ASKPASS_SOCKET" && key != "SSHTUI_ASKPASS_HELPER" {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "SSH_ASKPASS="+self, "SSH_ASKPASS_REQUIRE=force", "SSHTUI_ASKPASS_SOCKET="+path, "SSHTUI_ASKPASS_HELPER=1")
	return runSSH(cmd)
}

func runSSH(cmd *exec.Cmd) error {
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if progress := startConnectionProgress(); progress != nil {
		defer progress.stop(true)
		cmd.Stdout = connectionOutput{progress, os.Stdout}
		cmd.Stderr = connectionOutput{progress, os.Stderr}
	}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGWINCH)
	defer signal.Stop(signals)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case sig := <-signals:
				_ = cmd.Process.Signal(sig)
			case <-done:
				return
			}
		}
	}()
	return cmd.Wait()
}

type connectionProgress struct {
	mu     sync.Mutex
	active bool
	done   chan struct{}
}

func startConnectionProgress() *connectionProgress {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stderr.Fd())) || os.Getenv("TERM") == "dumb" {
		return nil
	}
	if width, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && width > 0 && width < 40 {
		return nil
	}
	state, err := term.GetState(int(os.Stdin.Fd()))
	if err != nil {
		return nil
	}
	p := &connectionProgress{active: true, done: make(chan struct{})}
	start := time.Now()
	style := lipgloss.NewStyle().Foreground(lipgloss.Color("#38bdf8"))
	frames := []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")
	fmt.Fprintln(os.Stderr, style.Render("  ⠋ 正在连接 · 已等待 0.0s"))
	go func() {
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		for frame := 1; ; frame++ {
			select {
			case <-p.done:
				return
			case <-ticker.C:
				current, err := term.GetState(int(os.Stdin.Fd()))
				if err != nil || *current != *state {
					// Password prompts use /dev/tty directly; leave their cursor untouched.
					p.stop(false)
					return
				}
				p.mu.Lock()
				if p.active {
					line := fmt.Sprintf("  %c 正在连接 · 已等待 %.1fs", frames[frame%len(frames)], time.Since(start).Seconds())
					fmt.Fprintf(os.Stderr, "\x1b7\x1b[1A\r\x1b[2K%s\x1b8", style.Render(line))
				}
				p.mu.Unlock()
			}
		}
	}()
	return p
}

func (p *connectionProgress) stop(clear bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active {
		p.active = false
		close(p.done)
		if clear {
			fmt.Fprint(os.Stderr, "\x1b[1A\r\x1b[2K")
		}
	}
}

type connectionOutput struct {
	progress *connectionProgress
	output   io.Writer
}

func (w connectionOutput) Write(data []byte) (int, error) {
	w.progress.stop(true)
	return w.output.Write(data)
}

func ghosttyScript(script string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "osascript", append([]string{"-e", script, "--"}, args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func pinGhosttyTabTitle(s *ServerConfig) func() {
	if runtime.GOOS != "darwin" || os.Getenv("TERM_PROGRAM") != "ghostty" || os.Getenv("TMUX") != "" {
		return nil
	}
	tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return nil
	}
	title := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, inventoryName(*s))
	// Identify this surface by a unique OSC title, even if the user switches tabs.
	marker := fmt.Sprintf("sshtui-%d-%d", os.Getpid(), time.Now().UnixNano())
	_, err = fmt.Fprintf(tty, "\x1b]2;%s\x1b\\", marker)
	if err != nil {
		tty.Close()
		return nil
	}
	id, err := ghosttyScript(`on run argv
	tell application "Ghostty"
		repeat 50 times
			set matches to every terminal whose name is (item 1 of argv)
			if (count of matches) > 0 then
				set targetTerminal to item 1 of matches
				if not (perform action ("set_tab_title:" & item 2 of argv) on targetTerminal) then error "Cannot set tab title"
				return id of targetTerminal
			end if
			delay 0.02
		end repeat
		error "Cannot find current Ghostty terminal"
	end tell
end run`, marker, title)
	_, _ = fmt.Fprintf(tty, "\x1b]2;%s\x1b\\", title)
	if err != nil {
		tty.Close()
		fmt.Fprintf(os.Stderr, "设置 Ghostty 标签标题失败，继续连接: %v\n", err)
		return nil
	}
	return func() {
		defer tty.Close()
		_, err := ghosttyScript(`on run argv
	tell application "Ghostty"
		if not (perform action "set_tab_title:" on terminal id (item 1 of argv)) then error "Cannot reset tab title"
	end tell
end run`, id)
		_, _ = fmt.Fprint(tty, "\x1b]2;\x1b\\")
		if err != nil {
			fmt.Fprintf(os.Stderr, "恢复 Ghostty 标签标题失败: %v\n", err)
		}
	}
}

func answerAskpass() error {
	// Never answer host confirmation or key passphrase prompts with a server password.
	if os.Getenv("SSH_ASKPASS_PROMPT") != "" || len(os.Args) != 2 || !strings.Contains(strings.ToLower(os.Args[1]), "password") {
		return fmt.Errorf("此 SSH 提示不能使用服务器密码自动回答")
	}
	conn, err := net.Dial("unix", os.Getenv("SSHTUI_ASKPASS_SOCKET"))
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = io.Copy(os.Stdout, conn)
	return err
}

func connectToServer(s *ServerConfig, cfg *Config) error {
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("未找到 ssh 命令: %w", err)
	}
	args := []string{"-p", strconv.Itoa(s.Port)}
	if s.Auth == "key" && s.Key != "" {
		args = append(args, "-i", expandPath(s.Key))
	}
	args = append(args, sshOptions(s, cfg)...)
	args = append(args, "--", fmt.Sprintf("%s@%s", s.User, s.Host))
	env := make([]string, 0, len(os.Environ())+2)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "TERM=") {
			env = append(env, e)
		}
	}
	env = append(env, "TERM=xterm-256color")
	if term.IsTerminal(int(os.Stderr.Fd())) {
		name := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#38bdf8")).Render("❯ " + inventoryName(*s))
		detail := lipgloss.NewStyle().Foreground(lipgloss.Color("#64748b")).Render(fmt.Sprintf("  [%s] %s@%s:%d", s.Group, s.User, s.Host, s.Port))
		fmt.Fprintf(os.Stderr, "\n%s\n%s\n", name, detail)
	} else {
		fmt.Fprintf(os.Stderr, "连接 [%s] %s (%s@%s:%d)\n", s.Group, s.Name, s.User, s.Host, s.Port)
	}
	restoreTitle := pinGhosttyTabTitle(s)
	if restoreTitle != nil {
		defer restoreTitle()
	}
	if s.Auth != "password" {
		if restoreTitle != nil || (term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))) {
			cmd := exec.Command(sshPath, args...)
			cmd.Env = env
			return runSSH(cmd)
		}
		return syscall.Exec(sshPath, append([]string{"ssh"}, args...), env)
	}

	helper, err := exec.LookPath("sshpass")
	if err != nil {
		return runWithAskpass(sshPath, args, s.Password, env)
	}
	input, err := passwordPipe(s.Password)
	if err != nil {
		return fmt.Errorf("准备密码描述符失败: %w", err)
	}
	defer input.Close()
	if restoreTitle != nil || (term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))) {
		cmd := exec.Command(helper, append([]string{"-d", "3", sshPath}, args...)...)
		cmd.Env, cmd.ExtraFiles = env, []*os.File{input}
		return runSSH(cmd)
	}
	fd := strconv.FormatUint(uint64(input.Fd()), 10)
	return syscall.Exec(helper, append([]string{"sshpass", "-d", fd, sshPath}, args...), env)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func inventoryName(s ServerConfig) string {
	if name := strings.TrimSpace(s.Name); name != "" {
		return name
	}
	return s.Host
}

func inventoryGroup(s ServerConfig) string {
	if group := strings.TrimSpace(s.Group); group != "" {
		return group
	}
	return "ungrouped"
}

func validateInventory(cfg *Config) error {
	seen := make(map[string]bool)
	for _, s := range cfg.Servers {
		name := inventoryName(s)
		if seen[name] {
			return fmt.Errorf("Ansible 主机名称重复: %q，请为服务器设置不同的 name", name)
		}
		seen[name] = true
		g := inventoryGroup(s)
		if g == "all" || g == "_meta" || strings.ContainsAny(g, "[]:\r\n") {
			return fmt.Errorf("Ansible 分组名称无效或保留: %q", g)
		}
	}
	return nil
}

// Keep JSON and INI connection settings in sync.
func inventoryVars(s ServerConfig, cfg *Config) map[string]interface{} {
	options := sshOptions(&s, cfg)
	for i := range options {
		options[i] = shellQuote(options[i])
	}
	vars := map[string]interface{}{
		"ansible_host":            s.Host,
		"ansible_port":            s.Port,
		"ansible_user":            s.User,
		"ansible_ssh_common_args": strings.Join(options, " "),
	}
	if s.Auth == "password" {
		vars["ansible_password"] = s.Password
		vars["ansible_ssh_pass"] = s.Password
	} else if s.Key != "" {
		vars["ansible_ssh_private_key_file"] = expandPath(s.Key)
	}
	return vars
}

func generateAnsibleJSON(cfg *Config) ([]byte, error) {
	if err := validateInventory(cfg); err != nil {
		return nil, err
	}
	hostvars := make(map[string]interface{})
	groups := make(map[string][]string)
	for _, s := range cfg.Servers {
		name, group := inventoryName(s), inventoryGroup(s)
		groups[group] = append(groups[group], name)
		hostvars[name] = inventoryVars(s, cfg)
	}
	result := map[string]interface{}{"_meta": map[string]interface{}{"hostvars": hostvars}}
	children := make([]string, 0, len(groups))
	for g, hosts := range groups {
		result[g] = map[string]interface{}{"hosts": hosts}
		children = append(children, g)
	}
	slices.Sort(children)
	result["all"] = map[string]interface{}{"children": children}
	return json.MarshalIndent(result, "", "  ")
}

func generateAnsibleINI(cfg *Config) (string, error) {
	if err := validateInventory(cfg); err != nil {
		return "", err
	}
	var b strings.Builder
	groups := make(map[string][]ServerConfig)
	for _, s := range cfg.Servers {
		groups[inventoryGroup(s)] = append(groups[inventoryGroup(s)], s)
	}
	for _, g := range slices.Sorted(maps.Keys(groups)) {
		fmt.Fprintf(&b, "[%s]\n", g)
		for _, s := range groups[g] {
			name := inventoryName(s)
			if strings.ContainsAny(name, "[]:\r\n") {
				return "", fmt.Errorf("Ansible INI 主机名称不能含 []、冒号或换行: %q，请改用 --ansible", name)
			}
			b.WriteString(shellQuote(name))
			vars := inventoryVars(s, cfg)
			for _, key := range slices.Sorted(maps.Keys(vars)) {
				value := fmt.Sprint(vars[key])
				if str, ok := vars[key].(string); ok {
					// INI uses shlex followed by Python literal parsing; preserve both quoting layers.
					value = shellQuote(strconv.Quote(str))
				}
				fmt.Fprintf(&b, " %s=%s", key, value)
			}
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func runAnsibleCommand(targetGroup string, cmdStr string) error {
	ansiblePath, err := exec.LookPath("ansible")
	if err != nil {
		return fmt.Errorf("未找到 ansible 命令，请确保已安装: brew install ansible")
	}

	selfBin, err := os.Executable()
	if err != nil {
		selfBin = "s"
	}

	if targetGroup == "" {
		targetGroup = "all"
	}

	fmt.Printf("🚀 正在通过 Ansible 批量执行 (目标: %s)...\n", targetGroup)
	fmt.Printf("💻 命令: %s\n\n", cmdStr)

	cmd := exec.Command(ansiblePath, targetGroup, "-i", selfBin, "-m", "shell", "-a", cmdStr)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "OBJC_DISABLE_INITIALIZE_FORK_SAFETY=YES")

	return cmd.Run()
}

// Parse quoting and escapes without evaluating shell expressions.
func splitEditor(command string) ([]string, error) {
	var args []string
	var word strings.Builder
	var quote rune
	escaped, started := false, false
	for _, r := range command {
		if escaped {
			if quote == '"' && !strings.ContainsRune("$`\"\\\n", r) {
				word.WriteRune('\\')
			}
			if r != '\n' {
				word.WriteRune(r)
			}
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped, started = true, true
		} else if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
		} else if r == '\'' || r == '"' {
			quote, started = r, true
		} else if unicode.IsSpace(r) {
			if started {
				args = append(args, word.String())
				word.Reset()
				started = false
			}
		} else {
			word.WriteRune(r)
			started = true
		}
	}
	if escaped || quote != 0 {
		return nil, fmt.Errorf("EDITOR 的引号或反斜线不完整")
	}
	if started {
		args = append(args, word.String())
	}
	if len(args) == 0 || args[0] == "" {
		return nil, fmt.Errorf("EDITOR 不能为空")
	}
	args[0] = expandPath(args[0])
	return args, nil
}

func editConfig() error {
	f, path, err := openConfig()
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	editor := os.Getenv("EDITOR")
	if strings.TrimSpace(editor) == "" {
		editor = "vim"
	}
	args, err := splitEditor(editor)
	if err != nil {
		return err
	}
	cmd := exec.Command(args[0], append(args[1:], path)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	runErr := cmd.Run()
	// Editors may replace the inode; recheck the saved file's permissions.
	f, err = os.Open(path)
	if err == nil {
		err = secureConfig(f)
		f.Close()
	}
	if err != nil {
		return fmt.Errorf("编辑后检查配置文件失败: %w", err)
	}
	return runErr
}

func directMatch(servers []ServerConfig, term string) *ServerConfig {
	var exact, partial *ServerConfig
	exactCount, partialCount := 0, 0
	lower := strings.ToLower(term)
	for i := range servers {
		if strings.EqualFold(servers[i].Name, term) {
			exact, exactCount = &servers[i], exactCount+1
		}
		if strings.Contains(strings.ToLower(servers[i].Name), lower) {
			partial, partialCount = &servers[i], partialCount+1
		}
	}
	if exactCount == 1 {
		return exact
	}
	if exactCount == 0 && partialCount == 1 {
		return partial
	}
	return nil
}

func exitWithError(prefix string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", prefix, err)
	if child, ok := err.(*exec.ExitError); ok {
		if status, ok := child.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			os.Exit(128 + int(status.Signal()))
		}
		os.Exit(child.ExitCode())
	}
	os.Exit(1)
}

func run() error {
	if len(os.Args) > 1 && (os.Args[1] == "run" || os.Args[1] == "exec") {
		runCmd := flag.NewFlagSet("run", flag.ExitOnError)
		group := runCmd.String("g", "all", "指定目标主机组 (默认 all)")
		_ = runCmd.Parse(os.Args[2:])
		if runCmd.NArg() == 0 {
			return fmt.Errorf("用法: s run [-g 分组] \"要执行的命令\"\n示例: s run -g Prod \"df -h\"")
		}
		return runAnsibleCommand(*group, strings.Join(runCmd.Args(), " "))
	}

	edit := flag.Bool("e", false, "用系统编辑器直接打开配置文件编辑")
	var showVersion bool
	flag.BoolVar(&showVersion, "version", false, "显示版本号")
	flag.BoolVar(&showVersion, "v", false, "显示版本号")
	list := flag.Bool("l", false, "纯文本表格列出所有配置的服务器")
	path := flag.Bool("p", false, "打印配置文件所在绝对路径")
	ansible := flag.Bool("ansible", false, "输出 Ansible Dynamic Inventory JSON 格式")
	ini := flag.Bool("ansible-ini", false, "输出 Ansible INI 格式")
	ansibleList := flag.Bool("list", false, "Ansible 官方动态清单规范 Hook (等同于 --ansible)")
	ansibleHost := flag.String("host", "", "Ansible 官方单机查询 Hook")
	flag.Parse()

	if showVersion {
		fmt.Printf("sshtui %s\n", strings.TrimSpace(version))
		return nil
	}
	if *path {
		cfgPath, err := getConfigPath()
		if err == nil {
			fmt.Println(cfgPath)
		}
		return err
	}
	if *edit {
		return editConfig()
	}
	cfg, cfgPath, err := loadConfig()
	if err != nil {
		return err
	}
	switch {
	case *ansible || *ansibleList:
		data, err := generateAnsibleJSON(cfg)
		if err == nil {
			fmt.Println(string(data))
		}
		return err
	case *ansibleHost != "":
		fmt.Println("{}")
		return nil
	case *ini:
		data, err := generateAnsibleINI(cfg)
		if err == nil {
			fmt.Print(data)
		}
		return err
	case *list:
		fmt.Printf("配置文件: %s (共 %d 台)\n\n", cfgPath, len(cfg.Servers))
		for i, s := range cfg.Servers {
			auth := "🔑 密钥"
			if s.Auth == "password" {
				auth = "🔒 密码"
			}
			fmt.Printf("%2d. [%s] %-20s %s@%s:%d (%s)\n", i+1, s.Group, s.Name, s.User, s.Host, s.Port, auth)
		}
		return nil
	}

	filter := strings.TrimSpace(flag.Arg(0))
	if filter != "" {
		if target := directMatch(cfg.Servers, filter); target != nil {
			return connectToServer(target, cfg)
		}
	}
	p := tea.NewProgram(initialModel(cfg, filter), tea.WithAltScreen())
	result, err := p.Run()
	if err != nil {
		return err
	}
	if m, ok := result.(model); ok && m.selected != nil {
		return connectToServer(m.selected, cfg)
	}
	return nil
}

func main() {
	if os.Getenv("SSHTUI_ASKPASS_HELPER") == "1" {
		if err := answerAskpass(); err != nil {
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		exitWithError("错误", err)
	}
}
