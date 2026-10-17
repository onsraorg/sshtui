package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"gopkg.in/yaml.v3"
)

// ServerConfig 定义单台服务器配置
type ServerConfig struct {
	Name     string   `yaml:"name"`
	Group    string   `yaml:"group"`
	Host     string   `yaml:"host"`
	Port     int      `yaml:"port"`
	User     string   `yaml:"user"`
	Auth     string   `yaml:"auth"` // "key" 或 "password"
	Key      string   `yaml:"key"`
	Password string   `yaml:"password"`
	Args     []string `yaml:"args"` // 额外 ssh 参数
}

// Config 全局配置结构
type Config struct {
	Default struct {
		User                string `yaml:"user"`
		Port                int    `yaml:"port"`
		Key                 string `yaml:"key"`
		ServerAliveInterval int    `yaml:"server_alive_interval"` // 心跳间隔秒数，默认 30
		ServerAliveCountMax int    `yaml:"server_alive_count_max"` // 探测失败重试次数，默认 3
	} `yaml:"default"`
	Servers []ServerConfig `yaml:"servers"`
}

// 展开波浪号 ~ 为用户家目录
func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// 获取配置文件路径
func getConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	p1 := filepath.Join(home, ".config", "sshtui", "servers.yaml")
	if _, err := os.Stat(p1); err == nil {
		return p1, nil
	}

	p2 := filepath.Join(home, ".ssh", "servers.yaml")
	if _, err := os.Stat(p2); err == nil {
		return p2, nil
	}

	return p1, nil
}

// 创建示例配置文件
func createDefaultConfig(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	sample := `# ==============================================================================
# sshtui 服务器列表配置文件
# 文件位置: ~/.config/sshtui/servers.yaml
# 文件权限会自动保持 600，保证密码安全性
# ==============================================================================

default:
  user: root
  port: 22
  key: ~/.ssh/id_ed25519

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
	return os.WriteFile(path, []byte(sample), 0600)
}

// 加载配置
func loadConfig() (*Config, string, error) {
	cfgPath, err := getConfigPath()
	if err != nil {
		return nil, "", err
	}

	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		if err := createDefaultConfig(cfgPath); err != nil {
			return nil, "", fmt.Errorf("创建默认配置文件失败: %w", err)
		}
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, "", fmt.Errorf("读取配置文件失败: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, "", fmt.Errorf("解析 YAML 配置文件失败: %w", err)
	}

	// 补全全局保活默认值 (30秒心跳，3次失败超时断开，彻底解决死锁)
	if cfg.Default.ServerAliveInterval <= 0 {
		cfg.Default.ServerAliveInterval = 30
	}
	if cfg.Default.ServerAliveCountMax <= 0 {
		cfg.Default.ServerAliveCountMax = 3
	}

	for i := range cfg.Servers {
		s := &cfg.Servers[i]
		if s.User == "" {
			if cfg.Default.User != "" {
				s.User = cfg.Default.User
			} else {
				s.User = "root"
			}
		}
		if s.Port == 0 {
			if cfg.Default.Port != 0 {
				s.Port = cfg.Default.Port
			} else {
				s.Port = 22
			}
		}
		if s.Auth == "" {
			if s.Password != "" {
				s.Auth = "password"
			} else {
				s.Auth = "key"
			}
		}
		if s.Auth == "key" && s.Key == "" && cfg.Default.Key != "" {
			s.Key = cfg.Default.Key
		}
		if s.Group == "" {
			s.Group = "Default"
		}
	}

	return &cfg, cfgPath, nil
}

// Model for Bubble Tea
type model struct {
	config      *Config
	configPath  string
	filter      string
	filtered    []ServerConfig
	cursor      int
	viewportTop int
	selected    *ServerConfig
	width       int
	height      int
	quitting    bool
}

func initialModel(cfg *Config, cfgPath string, initialFilter string) model {
	m := model{
		config:      cfg,
		configPath:  cfgPath,
		filter:      initialFilter,
		viewportTop: 0,
		height:      24,
	}
	m.applyFilter()
	return m
}

func (m *model) applyFilter() {
	if m.filter == "" {
		m.filtered = make([]ServerConfig, len(m.config.Servers))
		copy(m.filtered, m.config.Servers)
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

	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.adjustViewport()
}

func (m *model) maxVisibleItems() int {
	avail := m.height - 9
	if avail < 5 {
		return 5
	}
	if avail > 18 {
		return 18
	}
	return avail
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
			if len(m.filtered) > 0 && m.cursor < len(m.filtered) {
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
	if m.quitting || m.selected != nil {
		return ""
	}

	// 现代色彩风格 (与 Pi 极度契合的深灰/亮青/琥珀风格)
	primaryColor := lipgloss.Color("#38bdf8")   // 亮天青
	accentColor := lipgloss.Color("#a855f7")    // 紫色
	dimColor := lipgloss.Color("#64748b")       // 中性灰
	textColor := lipgloss.Color("#f8fafc")      // 亮文本
	selectedBg := lipgloss.Color("#1e293b")     // 选中行高亮背景
	keyBadgeColor := lipgloss.Color("#34d399")  // 绿色
	passBadgeColor := lipgloss.Color("#fbbf24") // 琥珀橙

	headerBox := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#475569")).
		Padding(0, 1).
		Width(72)

	searchPrompt := lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("❯ Filter: ")
	searchText := lipgloss.NewStyle().Foreground(textColor).Render(m.filter)
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
		end := m.viewportTop + visible
		if end > len(m.filtered) {
			end = len(m.filtered)
		}

		for i := m.viewportTop; i < end; i++ {
			s := m.filtered[i]
			isSelected := (i == m.cursor)

			// 分组
			groupStr := fmt.Sprintf("[%s]", s.Group)
			groupWidth := 9
			gw := runewidth.StringWidth(groupStr)
			if gw < groupWidth {
				groupStr += strings.Repeat(" ", groupWidth-gw)
			}
			groupStyle := lipgloss.NewStyle().Foreground(accentColor)
			if isSelected {
				groupStyle = groupStyle.Bold(true)
			}
			renderedGroup := groupStyle.Render(groupStr)

			// 认证徽章
			var authBadge string
			if s.Auth == "password" {
				authBadge = lipgloss.NewStyle().Foreground(passBadgeColor).Render("🔒 密码")
			} else {
				authBadge = lipgloss.NewStyle().Foreground(keyBadgeColor).Render("🔑 密钥")
			}

			// 名称 (自适应宽度，支持中文完整对齐)
			nameWidth := 20
			name := s.Name
			nw := runewidth.StringWidth(name)
			if nw > nameWidth {
				name = runewidth.Truncate(name, nameWidth-2, "..")
				nw = runewidth.StringWidth(name)
			}
			namePad := nameWidth - nw
			if namePad > 0 {
				name += strings.Repeat(" ", namePad)
			}

			// 连接目标
			connStr := fmt.Sprintf("%s@%s:%d", s.User, s.Host, s.Port)
			connWidth := 23
			cw := runewidth.StringWidth(connStr)
			if cw > connWidth {
				connStr = runewidth.Truncate(connStr, connWidth-2, "..")
				cw = runewidth.StringWidth(connStr)
			}
			connPad := connWidth - cw
			if connPad > 0 {
				connStr += strings.Repeat(" ", connPad)
			}

			prefix := "   "
			if isSelected {
				prefix = lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render(" ❯ ")
			}

			var line string
			if isSelected {
				nameStyled := lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render(name)
				connStyled := lipgloss.NewStyle().Foreground(textColor).Render(connStr)
				content := fmt.Sprintf("%s %s %s %s", renderedGroup, nameStyled, connStyled, authBadge)
				line = prefix + lipgloss.NewStyle().Background(selectedBg).Padding(0, 1).Render(content)
			} else {
				nameStyled := lipgloss.NewStyle().Foreground(textColor).Render(name)
				connStyled := lipgloss.NewStyle().Foreground(dimColor).Render(connStr)
				content := fmt.Sprintf("%s %s %s %s", renderedGroup, nameStyled, connStyled, authBadge)
				line = prefix + " " + content
			}

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

// 执行连接
func connectToServer(s *ServerConfig, cfg *Config) error {
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("未找到 ssh 命令: %w", err)
	}

	portStr := strconv.Itoa(s.Port)
	target := fmt.Sprintf("%s@%s", s.User, s.Host)

	// 重置终端所有颜色和光标状态，防止上一帧控制字符残留
	fmt.Print("\033[0m\033[?25h")
	_ = os.Stdout.Sync()

	// 解决 Ghostty 核心乱码问题：
	// Ghostty 默认设置 TERM=xterm-ghostty，远程 Linux 服务器通常没有该 terminfo，
	// 导致 oh-my-zsh / bash 无法解析光标与符号，出现 '?➜' 乱码与光标重叠。
	// 这里将本地进程的 TERM 强制替换为工业标准通用的 xterm-256color，SSH 协议会自动以 xterm-256color 协商 PTY。
	env := os.Environ()
	newEnv := make([]string, 0, len(env)+2)
	for _, e := range env {
		if strings.HasPrefix(e, "TERM=") {
			continue
		}
		newEnv = append(newEnv, e)
	}
	newEnv = append(newEnv, "TERM=xterm-256color")

	// 核心修复：防止长时间不操作导致连接挂起/死锁卡死
	// 1. ServerAliveInterval: 客户端定时发送心跳保活数据包，防止中间 NAT/防火墙静默丢弃空闲 TCP 连接
	// 2. ServerAliveCountMax: 连续多次心跳无响应时，客户端自动断开连接退回本地 Shell，绝不死锁
	// 3. TCPKeepAlive: 开启底层 TCP KeepAlive 探测
	intervalStr := fmt.Sprintf("ServerAliveInterval=%d", cfg.Default.ServerAliveInterval)
	countMaxStr := fmt.Sprintf("ServerAliveCountMax=%d", cfg.Default.ServerAliveCountMax)
	keepAliveArgs := []string{
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", intervalStr,
		"-o", countMaxStr,
		"-o", "TCPKeepAlive=yes",
	}

	if s.Auth == "password" {
		sshpassPath, err := exec.LookPath("sshpass")
		if err == nil {
			args := []string{
				"sshpass",
				"-p", s.Password,
				sshPath,
			}
			args = append(args, keepAliveArgs...)
			args = append(args, "-p", portStr)
			if len(s.Args) > 0 {
				args = append(args, s.Args...)
			}
			args = append(args, target)

			return syscall.Exec(sshpassPath, args, newEnv)
		}

		// Fallback: expect
		expectPath, err := exec.LookPath("expect")
		if err != nil {
			return fmt.Errorf("密码登录需要 sshpass 或系统自带 expect。建议执行: brew install hudochenkov/sshpass/sshpass")
		}

		expectScript := fmt.Sprintf(`
set timeout 30
spawn %s -o StrictHostKeyChecking=accept-new -o %s -o %s -o TCPKeepAlive=yes -p %s %s
expect {
    "*password:*" { send "%s\r"; exp_continue }
    "*yes/no*" { send "yes\r"; exp_continue }
    timeout { puts "Connection timed out"; exit 1 }
}
interact
`, sshPath, intervalStr, countMaxStr, portStr, target, s.Password)

		args := []string{"expect", "-c", expectScript}
		return syscall.Exec(expectPath, args, newEnv)
	}

	// 密钥登录
	args := []string{"ssh"}
	args = append(args, keepAliveArgs...)
	args = append(args, "-p", portStr)
	if s.Key != "" {
		expandedKey := expandPath(s.Key)
		args = append(args, "-i", expandedKey)
	}
	if len(s.Args) > 0 {
		args = append(args, s.Args...)
	}
	args = append(args, target)

	return syscall.Exec(sshPath, args, newEnv)
}

func main() {
	editFlag := flag.Bool("e", false, "用系统编辑器直接打开配置文件编辑")
	listFlag := flag.Bool("l", false, "纯文本列出所有配置的服务器")
	pathFlag := flag.Bool("p", false, "打印配置文件所在绝对路径")
	flag.Parse()

	cfg, cfgPath, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}

	if *pathFlag {
		fmt.Println(cfgPath)
		return
	}

	if *editFlag {
		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "vim"
		}
		cmd := exec.Command(editor, cfgPath)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "打开编辑器失败: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *listFlag {
		fmt.Printf("配置文件: %s (共 %d 台)\n\n", cfgPath, len(cfg.Servers))
		for i, s := range cfg.Servers {
			authStr := "🔑 密钥"
			if s.Auth == "password" {
				authStr = "🔒 密码"
			}
			fmt.Printf("%2d. [%s] %-20s %s@%s:%d (%s)\n",
				i+1, s.Group, s.Name, s.User, s.Host, s.Port, authStr)
		}
		return
	}

	initialFilter := ""
	if flag.NArg() > 0 {
		initialFilter = flag.Arg(0)
	}

	// 使用 WithAltScreen() 确保退出时终端恢复为干净的原生屏幕，不留残影
	p := tea.NewProgram(initialModel(cfg, cfgPath, initialFilter), tea.WithAltScreen())
	finalModel, err := p.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "运行错误: %v\n", err)
		os.Exit(1)
	}

	m, ok := finalModel.(model)
	if ok && m.selected != nil {
		if err := connectToServer(m.selected, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
			os.Exit(1)
		}
	}
}
