package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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

	// 补全全局保活默认值
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

	// 按服务器名称以 A-z 自然字母序排序 (忽略大小写)
	sort.SliceStable(cfg.Servers, func(i, j int) bool {
		nameI := strings.ToLower(cfg.Servers[i].Name)
		nameJ := strings.ToLower(cfg.Servers[j].Name)
		if nameI == nameJ {
			return cfg.Servers[i].Name < cfg.Servers[j].Name
		}
		return nameI < nameJ
	})

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
			// 回车立即选中并退出 TUI，零中间等待直接发起连接
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

	// 重置终端状态
	fmt.Print("\033[0m\033[?25h")

	// 打印即时连接反馈，避免纯黑屏等待
	connBadge := "🔑 密钥"
	if s.Auth == "password" {
		connBadge = "🔒 密码"
	}
	fmt.Printf("\n🚀 正在连接 [%s] %s (%s@%s:%d) [%s]...\n\n",
		s.Group, s.Name, s.User, s.Host, s.Port, connBadge)
	_ = os.Stdout.Sync()

	env := os.Environ()
	newEnv := make([]string, 0, len(env)+2)
	for _, e := range env {
		if strings.HasPrefix(e, "TERM=") {
			continue
		}
		newEnv = append(newEnv, e)
	}
	newEnv = append(newEnv, "TERM=xterm-256color")

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

// 生成 Ansible Dynamic Inventory 标准 JSON
func generateAnsibleJSON(cfg *Config) ([]byte, error) {
	type HostVars map[string]interface{}

	meta := map[string]interface{}{
		"hostvars": make(map[string]HostVars),
	}

	groups := make(map[string][]string)
	allChildren := make(map[string]bool)

	commonArgs := fmt.Sprintf("-o StrictHostKeyChecking=accept-new -o ServerAliveInterval=%d -o ServerAliveCountMax=%d -o TCPKeepAlive=yes",
		cfg.Default.ServerAliveInterval, cfg.Default.ServerAliveCountMax)

	for _, s := range cfg.Servers {
		hostName := strings.TrimSpace(s.Name)
		if hostName == "" {
			hostName = s.Host
		}

		groupName := strings.TrimSpace(s.Group)
		if groupName == "" {
			groupName = "ungrouped"
		}

		groups[groupName] = append(groups[groupName], hostName)
		allChildren[groupName] = true

		vars := HostVars{
			"ansible_host":            s.Host,
			"ansible_port":            s.Port,
			"ansible_user":            s.User,
			"ansible_ssh_common_args": commonArgs,
		}

		if s.Auth == "password" && s.Password != "" {
			vars["ansible_password"] = s.Password
			vars["ansible_ssh_pass"] = s.Password
		} else if s.Key != "" {
			vars["ansible_ssh_private_key_file"] = expandPath(s.Key)
		}

		meta["hostvars"].(map[string]HostVars)[hostName] = vars
	}

	result := make(map[string]interface{})
	result["_meta"] = meta

	var childrenList []string
	for g, hosts := range groups {
		result[g] = map[string]interface{}{
			"hosts": hosts,
		}
		childrenList = append(childrenList, g)
	}

	result["all"] = map[string]interface{}{
		"children": childrenList,
	}

	return json.MarshalIndent(result, "", "  ")
}

// 生成 Ansible INI 格式清单
func generateAnsibleINI(cfg *Config) string {
	var b strings.Builder
	commonArgs := fmt.Sprintf("-o StrictHostKeyChecking=accept-new -o ServerAliveInterval=%d -o ServerAliveCountMax=%d -o TCPKeepAlive=yes",
		cfg.Default.ServerAliveInterval, cfg.Default.ServerAliveCountMax)

	groups := make(map[string][]ServerConfig)
	for _, s := range cfg.Servers {
		g := strings.TrimSpace(s.Group)
		if g == "" {
			g = "ungrouped"
		}
		groups[g] = append(groups[g], s)
	}

	for g, list := range groups {
		b.WriteString(fmt.Sprintf("[%s]\n", g))
		for _, s := range list {
			hostName := strings.TrimSpace(s.Name)
			if hostName == "" {
				hostName = s.Host
			}
			line := fmt.Sprintf("%-20s ansible_host=%s ansible_port=%d ansible_user=%s",
				hostName, s.Host, s.Port, s.User)
			if s.Auth == "password" && s.Password != "" {
				line += fmt.Sprintf(" ansible_password=\"%s\" ansible_ssh_pass=\"%s\"", s.Password, s.Password)
			} else if s.Key != "" {
				line += fmt.Sprintf(" ansible_ssh_private_key_file=\"%s\"", expandPath(s.Key))
			}
			line += fmt.Sprintf(" ansible_ssh_common_args=\"%s\"\n", commonArgs)
			b.WriteString(line)
		}
		b.WriteString("\n")
	}

	return b.String()
}

// 封装运行 Ansible 命令
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
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "OBJC_DISABLE_INITIALIZE_FORK_SAFETY=YES")

	return cmd.Run()
}

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "run" || os.Args[1] == "exec") {
		runCmd := flag.NewFlagSet("run", flag.ExitOnError)
		groupFlag := runCmd.String("g", "all", "指定目标主机组 (默认 all)")
		_ = runCmd.Parse(os.Args[2:])

		args := runCmd.Args()
		if len(args) == 0 {
			fmt.Println("用法: s run [-g 分组] \"要执行的命令\"")
			fmt.Println("示例: s run \"uptime\"")
			fmt.Println("示例: s run -g Prod \"df -h\"")
			os.Exit(1)
		}

		cmdToRun := strings.Join(args, " ")
		if err := runAnsibleCommand(*groupFlag, cmdToRun); err != nil {
			os.Exit(1)
		}
		return
	}

	editFlag := flag.Bool("e", false, "用系统编辑器直接打开配置文件编辑")
	listTableFlag := flag.Bool("l", false, "纯文本表格列出所有配置的服务器")
	pathFlag := flag.Bool("p", false, "打印配置文件所在绝对路径")
	ansibleFlag := flag.Bool("ansible", false, "输出 Ansible Dynamic Inventory JSON 格式")
	ansibleIniFlag := flag.Bool("ansible-ini", false, "输出 Ansible INI 格式")
	ansibleListHook := flag.Bool("list", false, "Ansible 官方动态清单规范 Hook (等同于 --ansible)")
	ansibleHostHook := flag.String("host", "", "Ansible 官方单机查询 Hook")
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

	if *ansibleFlag || *ansibleListHook {
		jsonBytes, err := generateAnsibleJSON(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "生成 Ansible Inventory 失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(jsonBytes))
		return
	}

	if *ansibleHostHook != "" {
		fmt.Println("{}")
		return
	}

	if *ansibleIniFlag {
		fmt.Print(generateAnsibleINI(cfg))
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

	if *listTableFlag {
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
