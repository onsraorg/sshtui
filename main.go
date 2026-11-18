package main

import (
	"bytes"
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

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
	Name        string   `yaml:"name"`
	Group       string   `yaml:"group"`
	Host        string   `yaml:"host"`
	Port        int      `yaml:"port,omitempty"`
	User        string   `yaml:"user"`
	Auth        string   `yaml:"auth"` // "key" or "password"
	Key         string   `yaml:"key"`
	Password    string   `yaml:"password"`
	Args        []string `yaml:"args"` // Additional SSH arguments.
	System      string   `yaml:"system,omitempty"`
	Spec        string   `yaml:"spec,omitempty"`
	Notes       string   `yaml:"notes,omitempty"`
	Archived    bool     `yaml:"archived,omitempty"`
	sourceIndex int
}

// Config holds defaults and SSH targets.
type Config struct {
	Default struct {
		User                string `yaml:"user"`
		Port                int    `yaml:"port,omitempty"`
		Key                 string `yaml:"key"`
		ServerAliveInterval int    `yaml:"server_alive_interval"`  // Keepalive interval in seconds; defaults to 15.
		ServerAliveCountMax int    `yaml:"server_alive_count_max"` // Failed keepalive limit; defaults to 30.
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
  server_alive_interval: 15
  server_alive_count_max: 30

servers:
  - name: "dev-app"
    group: "development"
    host: "192.0.2.10"
    user: "deploy"
    auth: "key"
    key: "~/.ssh/id_ed25519"

  - name: "staging-api"
    group: "staging"
    host: "198.51.100.20"
    user: "deploy"
    auth: "password"
    password: "your_password_here"

  - name: "prod-db"
    group: "production"
    host: "203.0.113.30"
    port: 2222
    user: "dbadmin"
    auth: "key"
    key: "~/.ssh/id_ed25519"
`
	f, err := os.CreateTemp(dir, ".servers.yaml-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := io.WriteString(f, sample); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Link(f.Name(), path); err != nil && !os.IsExist(err) {
		return err
	}
	return nil
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
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, "", fmt.Errorf("读取配置文件失败: %w", err)
	}
	cfg, err := decodeConfig(raw)
	if err != nil {
		return nil, "", err
	}
	return cfg, cfgPath, nil
}

func unwrapAlias(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

func mappingLookup(m *yaml.Node, key string) *yaml.Node {
	m = unwrapAlias(m)
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].ShortTag() != "!!merge" && m.Content[i].Value != "<<" {
			continue
		}
		merged := unwrapAlias(m.Content[i+1])
		if merged == nil {
			continue
		}
		if merged.Kind == yaml.MappingNode {
			if found := mappingLookup(merged, key); found != nil {
				return found
			}
		} else if merged.Kind == yaml.SequenceNode {
			for _, elem := range merged.Content {
				if found := mappingLookup(elem, key); found != nil {
					return found
				}
			}
		}
	}
	return nil
}

func isExplicitZeroPort(m *yaml.Node) bool {
	val := unwrapAlias(mappingLookup(m, "port"))
	if val == nil || val.Kind != yaml.ScalarNode || val.ShortTag() != "!!int" {
		return false
	}
	var port int
	return val.Decode(&port) == nil && port == 0
}

func decodeConfig(raw []byte) (*Config, error) {
	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("解析 YAML 配置文件失败: %w", err)
	}
	if err := decoder.Decode(new(yaml.Node)); err != io.EOF {
		return nil, fmt.Errorf("配置只允许一个 YAML 文档")
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("解析 YAML 配置文件失败: %w", err)
	}
	var defaultNode, serversNode *yaml.Node
	if len(doc.Content) > 0 {
		root := unwrapAlias(doc.Content[0])
		defaultNode = unwrapAlias(mappingLookup(root, "default"))
		serversNode = unwrapAlias(mappingLookup(root, "servers"))
	}

	if isExplicitZeroPort(defaultNode) || (cfg.Default.Port != 0 && (cfg.Default.Port < 1 || cfg.Default.Port > 65535)) {
		return nil, fmt.Errorf("default.port 必须在 1–65535 之间")
	}
	if strings.ContainsRune(cfg.Default.User, 0) || strings.ContainsRune(cfg.Default.Key, 0) {
		return nil, fmt.Errorf("default 配置不能包含 NUL 字符")
	}
	if strings.IndexFunc(cfg.Default.User, unicode.IsControl) >= 0 {
		return nil, fmt.Errorf("default.user 不能包含控制字符")
	}

	if cfg.Default.ServerAliveInterval <= 0 {
		cfg.Default.ServerAliveInterval = 15
	}
	if cfg.Default.ServerAliveCountMax <= 0 {
		cfg.Default.ServerAliveCountMax = 30
	}

	for i := range cfg.Servers {
		s := &cfg.Servers[i]
		if serversNode != nil && serversNode.Kind == yaml.SequenceNode && i < len(serversNode.Content) && isExplicitZeroPort(serversNode.Content[i]) {
			return nil, fmt.Errorf("服务器 %q 的 port 必须在 1–65535 之间", s.Name)
		}
		s.sourceIndex = i
		s.User = cmp.Or(s.User, cfg.Default.User, "root")
		s.Port = cmp.Or(s.Port, cfg.Default.Port, 22)
		s.Group = cmp.Or(strings.TrimSpace(s.Group), "Default")
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
			return nil, fmt.Errorf("服务器 %q 的 host 不能为空，port 必须在 1–65535 之间", s.Name)
		}
		if s.Auth != "key" && s.Auth != "password" {
			return nil, fmt.Errorf("服务器 %q 的 auth 必须是 key 或 password", s.Name)
		}
		fields := append([]string{s.Name, s.Group, s.Host, s.User, s.Key, s.System, s.Spec}, s.Args...)
		for _, field := range fields {
			if strings.ContainsRune(field, 0) {
				return nil, fmt.Errorf("服务器 %q 的配置不能包含 NUL 字符", s.Name)
			}
		}
		for _, field := range []string{s.Name, s.Group, s.Host, s.User, s.System, s.Spec} {
			if strings.IndexFunc(field, unicode.IsControl) >= 0 {
				return nil, fmt.Errorf("服务器 %q 的显示字段不能包含控制字符", s.Name)
			}
		}
		s.Notes = strings.ReplaceAll(s.Notes, "\r\n", "\n")
		for _, r := range s.Notes {
			if r == 0 {
				return nil, fmt.Errorf("服务器 %q 的配置不能包含 NUL 字符", s.Name)
			}
			if unicode.IsControl(r) && r != '\n' && r != '\t' {
				return nil, fmt.Errorf("服务器 %q 的备注不能包含控制字符", s.Name)
			}
		}
		if err := validatePassword(s.Password); err != nil {
			return nil, fmt.Errorf("服务器 %q: %w", s.Name, err)
		}
	}

	slices.SortStableFunc(cfg.Servers, func(a, b ServerConfig) int {
		return cmp.Or(strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), strings.Compare(a.Name, b.Name))
	})

	return &cfg, nil
}

type probeDebounceMsg struct {
	seq  int
	key  string
	host string
	port int
}

type latencyResultMsg struct {
	key    string
	result latencyResult
}

type systemInfoResultMsg struct {
	key     string
	target  ServerConfig
	profile serverProfile
	err     error
}

type model struct {
	config       *Config
	filter       string
	filtered     []ServerConfig
	cursor       int
	viewportTop  int
	splitLineTop int
	selected     *ServerConfig
	width        int
	height       int
	quitting     bool
	showArchived bool
	status       string
	latency      map[string]latencyState
	systemProbes map[string]systemProbeState
	probeSeq     int
	probeFn      func(context.Context, string, int) latencyResult
	sysProbeFn   func(context.Context, ServerConfig, *Config) (serverProfile, error)
	editingNotes bool
	noteBuf      []rune
	notePos      int
}

func sanitizeFilter(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

func initialModel(cfg *Config, initialFilter string) model {
	m := model{
		config:       cfg,
		filter:       sanitizeFilter(initialFilter),
		viewportTop:  0,
		height:       24,
		width:        80,
		latency:      make(map[string]latencyState),
		systemProbes: make(map[string]systemProbeState),
	}
	m.applyFilter()
	return m
}

func groupSortRank(group string) string {
	if strings.EqualFold(group, "Default") {
		return "\xff"
	}
	return strings.ToLower(group)
}

func (m *model) applyFilter() {
	lowerFilter := strings.ToLower(m.filter)
	m.filtered = nil
	for _, s := range m.config.Servers {
		if s.Archived && !m.showArchived {
			continue
		}
		target := strings.ToLower(fmt.Sprintf("%s %s %s %s %s %s %s", s.Name, s.Group, s.Host, s.User, s.System, s.Spec, s.Notes))
		if strings.Contains(target, lowerFilter) {
			m.filtered = append(m.filtered, s)
		}
	}
	slices.SortStableFunc(m.filtered, func(a, b ServerConfig) int {
		if a.Group == b.Group {
			return 0
		}
		return cmp.Or(
			strings.Compare(groupSortRank(a.Group), groupSortRank(b.Group)),
			strings.Compare(a.Group, b.Group),
		)
	})

	m.cursor = max(0, min(m.cursor, len(m.filtered)-1))
	m.adjustViewport()
}

func (m model) isSplitView() bool {
	return m.width >= 80 && m.height >= 16
}

func (m model) splitDimensions() (leftW, rightW, bodyH, listH int) {
	footerLines := 1
	if m.height >= 22 {
		footerLines = 2
	}
	bodyH = max(1, m.height-footerLines)
	availW := max(2, m.width-3)
	rightW = min(46, max(29, availW*37/100))
	leftW = max(1, availW-rightW)
	topChrome := 4
	if bodyH >= 19 {
		topChrome = 5
	}
	listH = max(1, bodyH-topChrome)
	return leftW, rightW, bodyH, listH
}

type leftItemKind uint8

const (
	leftItemServer leftItemKind = iota
	leftItemGroup
	leftItemSpacer
)

type leftVisualItem struct {
	kind      leftItemKind
	group     string
	count     int
	serverIdx int
}

func (m model) buildLeftItems() []leftVisualItem {
	if len(m.filtered) == 0 {
		return nil
	}
	counts := make(map[string]int)
	for _, s := range m.filtered {
		counts[s.Group]++
	}
	items := make([]leftVisualItem, 0, len(m.filtered)+len(counts)*2)
	for i, s := range m.filtered {
		if i == 0 || s.Group != m.filtered[i-1].Group {
			if i > 0 {
				items = append(items, leftVisualItem{kind: leftItemSpacer})
			}
			items = append(items, leftVisualItem{kind: leftItemGroup, group: s.Group, count: counts[s.Group]})
		}
		items = append(items, leftVisualItem{kind: leftItemServer, serverIdx: i})
	}
	return items
}

func (m *model) listLayout() (rows, columns int) {
	if m.isSplitView() {
		_, _, _, listH := m.splitDimensions()
		return listH, 1
	}
	rows = max(1, m.height-3)
	// Keep each column wide enough for a name and connection details.
	columns = min(max(1, (len(m.filtered)+rows-1)/rows), max(1, (m.width+2)/34))
	return rows, columns
}

func (m *model) maxVisibleItems() int {
	rows, columns := m.listLayout()
	return rows * columns
}

func (m *model) adjustViewport() {
	visible := m.maxVisibleItems()
	if len(m.filtered) <= visible {
		m.viewportTop = 0
	} else {
		if m.cursor < m.viewportTop {
			m.viewportTop = m.cursor
		} else if m.cursor >= m.viewportTop+visible {
			m.viewportTop = m.cursor - visible + 1
		}
		if maxTop := len(m.filtered) - visible; m.viewportTop > maxTop {
			m.viewportTop = maxTop
		}
		if m.viewportTop < 0 {
			m.viewportTop = 0
		}
	}

	_, _, _, listH := m.splitDimensions()
	items := m.buildLeftItems()
	if len(items) <= listH {
		m.splitLineTop = 0
		return
	}
	curLine := 0
	for i, it := range items {
		if it.kind == leftItemServer && it.serverIdx == m.cursor {
			curLine = i
			break
		}
	}
	wantTop := curLine
	if curLine > 0 && items[curLine-1].kind == leftItemGroup && listH >= 2 {
		wantTop = curLine - 1
	}
	if wantTop < m.splitLineTop {
		m.splitLineTop = wantTop
	} else if curLine >= m.splitLineTop+listH {
		m.splitLineTop = curLine - listH + 1
	}
	m.splitLineTop = max(0, min(m.splitLineTop, len(items)-listH))
}

const latencyCacheTTL = 20 * time.Second

func serverProbeKey(s ServerConfig) string {
	return fmt.Sprintf("%d:%s:%d", s.sourceIndex, s.Host, s.Port)
}

func (m *model) scheduleProbe(debounce bool) tea.Cmd {
	if len(m.filtered) == 0 || m.cursor < 0 || m.cursor >= len(m.filtered) {
		return nil
	}
	s := m.filtered[m.cursor]
	if s.Archived {
		return nil
	}
	if m.latency == nil {
		m.latency = make(map[string]latencyState)
	}
	key := serverProbeKey(s)
	st := m.latency[key]
	if st.HasData && time.Since(st.Result.At) < latencyCacheTTL {
		return nil
	}
	st.Probing = true
	m.latency[key] = st
	m.probeSeq++
	seq := m.probeSeq
	if debounce {
		return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg {
			return probeDebounceMsg{seq: seq, key: key, host: s.Host, port: s.Port}
		})
	}
	return m.runProbeCmd(key, s.Host, s.Port)
}

func (m model) runProbeCmd(key, host string, port int) tea.Cmd {
	fn := m.probeFn
	if fn == nil {
		fn = probeServerLatency
	}
	return func() tea.Msg {
		return latencyResultMsg{key: key, result: fn(context.Background(), host, port)}
	}
}

func (m *model) selectBySourceIndex(sourceIdx int) {
	for i, s := range m.filtered {
		if s.sourceIndex == sourceIdx {
			m.cursor = i
			m.adjustViewport()
			return
		}
	}
}

func (m model) Init() tea.Cmd {
	return m.scheduleProbe(false)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.adjustViewport()
		return m, nil

	case probeDebounceMsg:
		if msg.seq != m.probeSeq {
			return m, nil
		}
		return m, m.runProbeCmd(msg.key, msg.host, msg.port)

	case latencyResultMsg:
		if m.latency == nil {
			m.latency = make(map[string]latencyState)
		}
		m.latency[msg.key] = latencyState{
			Probing: false,
			HasData: true,
			Result:  msg.result,
		}
		return m, nil

	case systemInfoResultMsg:
		if m.systemProbes == nil {
			m.systemProbes = make(map[string]systemProbeState)
		}
		if msg.err != nil {
			m.systemProbes[msg.key] = systemProbeState{Err: msg.err.Error()}
			m.status = strings.Join(strings.Fields(sanitizeFilter("系统信息采集失败: "+msg.err.Error())), " ")
			return m, nil
		}
		cfg, err := saveServerProfile(msg.target, msg.profile)
		if err != nil {
			m.systemProbes[msg.key] = systemProbeState{Err: err.Error()}
			m.status = strings.Join(strings.Fields(sanitizeFilter("保存系统信息失败: "+err.Error())), " ")
			return m, nil
		}
		delete(m.systemProbes, msg.key)
		curIdx := -1
		if len(m.filtered) > 0 && m.cursor < len(m.filtered) {
			curIdx = m.filtered[m.cursor].sourceIndex
		}
		m.config = cfg
		m.applyFilter()
		if curIdx >= 0 {
			m.selectBySourceIndex(curIdx)
		}
		summary := cmp.Or(msg.profile.System, msg.profile.Spec)
		if msg.profile.System != "" && msg.profile.Spec != "" {
			summary = msg.profile.System + " / " + msg.profile.Spec
		}
		m.status = "已更新: " + summary
		return m, nil

	case tea.KeyMsg:
		if m.editingNotes {
			return m.updateNoteEditor(msg)
		}
		m.status = ""
		switch msg.Type {
		case tea.KeyF2:
			if len(m.filtered) == 0 {
				return m, nil
			}
			target := m.filtered[m.cursor]
			cfg, err := setServerArchived(target, !target.Archived)
			if err != nil {
				m.status = strings.Join(strings.Fields(sanitizeFilter("保存失败: "+err.Error())), " ")
				return m, nil
			}
			m.config = cfg
			m.status = "已归档；F3 查看并恢复"
			if target.Archived {
				m.status = "已恢复"
			}
			m.applyFilter()
			if target.Archived {
				m.selectBySourceIndex(target.sourceIndex)
			}
			return m, m.scheduleProbe(true)

		case tea.KeyF3:
			m.showArchived = !m.showArchived
			m.status = ""
			m.applyFilter()
			return m, m.scheduleProbe(true)

		case tea.KeyF4, tea.KeyCtrlE, tea.KeyTab:
			if len(m.filtered) == 0 {
				return m, nil
			}
			s := m.filtered[m.cursor]
			m.editingNotes = true
			m.noteBuf = []rune(s.Notes)
			m.notePos = len(m.noteBuf)
			return m, nil

		case tea.KeyF5, tea.KeyCtrlR:
			if len(m.filtered) == 0 {
				return m, nil
			}
			s := m.filtered[m.cursor]
			if !s.Archived && m.latency != nil {
				delete(m.latency, serverProbeKey(s))
			}
			return m, m.scheduleProbe(false)

		case tea.KeyF6, tea.KeyCtrlO:
			if len(m.filtered) == 0 {
				return m, nil
			}
			s := m.filtered[m.cursor]
			if s.Archived {
				m.status = "已归档，请先按 F2 恢复"
				return m, nil
			}
			if m.systemProbes == nil {
				m.systemProbes = make(map[string]systemProbeState)
			}
			key := serverProbeKey(s)
			if m.systemProbes[key].Probing {
				return m, nil
			}
			m.systemProbes[key] = systemProbeState{Probing: true}
			fn := m.sysProbeFn
			if fn == nil {
				fn = fetchServerProfile
			}
			cfgCopy := m.config
			return m, func() tea.Msg {
				profile, err := fn(context.Background(), s, cfgCopy)
				return systemInfoResultMsg{key: key, target: s, profile: profile, err: err}
			}

		case tea.KeyCtrlC:
			m.quitting = true
			return m, tea.Quit

		case tea.KeyEsc:
			if m.filter != "" {
				m.filter = ""
				m.applyFilter()
				return m, m.scheduleProbe(true)
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
			return m, m.scheduleProbe(true)

		case tea.KeyDown, tea.KeyCtrlN:
			if m.cursor < len(m.filtered)-1 {
				m.cursor++
			} else {
				m.cursor = 0
			}
			m.adjustViewport()
			return m, m.scheduleProbe(true)

		case tea.KeyPgUp:
			if len(m.filtered) > 0 {
				step := max(1, m.maxVisibleItems()-1)
				m.cursor = max(0, m.cursor-step)
				m.adjustViewport()
			}
			return m, m.scheduleProbe(true)

		case tea.KeyPgDown:
			if len(m.filtered) > 0 {
				step := max(1, m.maxVisibleItems()-1)
				m.cursor = min(len(m.filtered)-1, m.cursor+step)
				m.adjustViewport()
			}
			return m, m.scheduleProbe(true)

		case tea.KeyLeft, tea.KeyRight:
			rows, columns := m.listLayout()
			moved := false
			if columns > 1 && msg.Type == tea.KeyLeft && m.cursor >= rows {
				m.cursor -= rows
				moved = true
			} else if columns > 1 && msg.Type == tea.KeyRight && m.cursor+rows < len(m.filtered) {
				m.cursor += rows
				moved = true
			}
			m.adjustViewport()
			if moved {
				return m, m.scheduleProbe(true)
			}
			return m, nil

		case tea.KeyEnter:
			if m.width >= 16 && m.height >= 4 && len(m.filtered) > 0 && m.cursor < len(m.filtered) {
				chosen := m.filtered[m.cursor]
				if chosen.Archived {
					m.status = "已归档，请先按 F2 恢复"
					return m, nil
				}
				m.selected = &chosen
				return m, tea.Quit
			}

		case tea.KeyBackspace:
			if len(m.filter) > 0 {
				r := []rune(m.filter)
				m.filter = string(r[:len(r)-1])
				m.applyFilter()
				return m, m.scheduleProbe(true)
			}
			return m, nil

		case tea.KeySpace:
			if !msg.Alt {
				m.filter += " "
				m.applyFilter()
				return m, m.scheduleProbe(true)
			}
			return m, nil

		case tea.KeyRunes:
			if !msg.Alt {
				m.filter += sanitizeFilter(string(msg.Runes))
				m.applyFilter()
				return m, m.scheduleProbe(true)
			}
			return m, nil
		}
	}

	return m, nil
}

func (m model) updateNoteEditor(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC:
		m.quitting = true
		return m, tea.Quit

	case tea.KeyEsc:
		m.editingNotes = false
		m.noteBuf = nil
		m.notePos = 0
		m.status = ""
		return m, nil

	case tea.KeyCtrlS, tea.KeyF4:
		if len(m.filtered) == 0 || m.cursor >= len(m.filtered) {
			m.editingNotes = false
			return m, nil
		}
		target := m.filtered[m.cursor]
		cfg, err := setServerNotes(target, string(m.noteBuf))
		if err != nil {
			m.status = strings.Join(strings.Fields(sanitizeFilter("保存失败: "+err.Error())), " ")
			return m, nil
		}
		m.config = cfg
		m.editingNotes = false
		m.noteBuf = nil
		m.notePos = 0
		m.status = "备注已保存"
		m.applyFilter()
		m.selectBySourceIndex(target.sourceIndex)
		return m, nil

	case tea.KeyEnter, tea.KeyCtrlJ:
		m.insertNoteRunes([]rune{'\n'})
		return m, nil

	case tea.KeyBackspace:
		if m.notePos > 0 {
			m.noteBuf = append(m.noteBuf[:m.notePos-1], m.noteBuf[m.notePos:]...)
			m.notePos--
		}
		return m, nil

	case tea.KeyDelete, tea.KeyCtrlD:
		if m.notePos < len(m.noteBuf) {
			m.noteBuf = append(m.noteBuf[:m.notePos], m.noteBuf[m.notePos+1:]...)
		}
		return m, nil

	case tea.KeyLeft, tea.KeyCtrlB:
		if m.notePos > 0 {
			m.notePos--
		}
		return m, nil

	case tea.KeyRight, tea.KeyCtrlF:
		if m.notePos < len(m.noteBuf) {
			m.notePos++
		}
		return m, nil

	case tea.KeyUp, tea.KeyCtrlP:
		m.notePos = moveNoteCursorVertical(m.noteBuf, m.notePos, -1)
		return m, nil

	case tea.KeyDown, tea.KeyCtrlN:
		m.notePos = moveNoteCursorVertical(m.noteBuf, m.notePos, 1)
		return m, nil

	case tea.KeyHome, tea.KeyCtrlA:
		start, _, _ := noteLineBounds(m.noteBuf, m.notePos)
		m.notePos = start
		return m, nil

	case tea.KeyEnd, tea.KeyCtrlE:
		_, end, _ := noteLineBounds(m.noteBuf, m.notePos)
		m.notePos = end
		return m, nil

	case tea.KeyCtrlU:
		start, _, _ := noteLineBounds(m.noteBuf, m.notePos)
		if m.notePos > start {
			m.noteBuf = append(m.noteBuf[:start], m.noteBuf[m.notePos:]...)
			m.notePos = start
		}
		return m, nil

	case tea.KeyCtrlK:
		_, end, _ := noteLineBounds(m.noteBuf, m.notePos)
		if end > m.notePos {
			m.noteBuf = append(m.noteBuf[:m.notePos], m.noteBuf[end:]...)
		}
		return m, nil

	case tea.KeySpace:
		if !msg.Alt {
			m.insertNoteRunes([]rune{' '})
		}
		return m, nil

	case tea.KeyTab:
		m.insertNoteRunes([]rune{' ', ' '})
		return m, nil

	case tea.KeyRunes:
		if !msg.Alt {
			if cleaned := sanitizeNoteInput(string(msg.Runes)); len(cleaned) > 0 {
				m.insertNoteRunes(cleaned)
			}
		}
		return m, nil
	}
	return m, nil
}

func (m *model) insertNoteRunes(ins []rune) {
	if len(ins) == 0 {
		return
	}
	next := make([]rune, 0, len(m.noteBuf)+len(ins))
	next = append(next, m.noteBuf[:m.notePos]...)
	next = append(next, ins...)
	next = append(next, m.noteBuf[m.notePos:]...)
	m.noteBuf = next
	m.notePos += len(ins)
}

const (
	uiCyan       = lipgloss.Color("#22d3ee")
	uiPurple     = lipgloss.Color("#a855f7")
	uiPurpleSoft = lipgloss.Color("#c084fc")
	uiPurpleBg   = lipgloss.Color("#2a1b4e")
	uiSelectedBg = lipgloss.Color("#2e1d59")
	uiGreen      = lipgloss.Color("#34d399")
	uiGreenBg    = lipgloss.Color("#0a362b")
	uiAmber      = lipgloss.Color("#fbbf24")
	uiAmberBg    = lipgloss.Color("#3b2a0e")
	uiRed        = lipgloss.Color("#f87171")
	uiRedBg      = lipgloss.Color("#3b1520")
	uiText       = lipgloss.Color("#f8fafc")
	uiSubText    = lipgloss.Color("#cbd5e1")
	uiDim        = lipgloss.Color("#64748b")
	uiBorderDim  = lipgloss.Color("#2d3748")
	uiNoteBorder = lipgloss.Color("#4c3575")
	uiDivider    = lipgloss.Color("#1e293b")
)

func padStyledRight(s string, width int) string {
	w := lipgloss.Width(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

func renderRoundedBox(contentLines []string, outerWidth int, borderColor lipgloss.Color) []string {
	if outerWidth < 4 {
		return nil
	}
	innerW := outerWidth - 4
	bStyle := lipgloss.NewStyle().Foreground(borderColor)
	top := bStyle.Render("╭" + strings.Repeat("─", outerWidth-2) + "╮")
	bottom := bStyle.Render("╰" + strings.Repeat("─", outerWidth-2) + "╯")
	leftBar := bStyle.Render("│ ")
	rightBar := bStyle.Render(" │")

	out := make([]string, 0, len(contentLines)+2)
	out = append(out, top)
	for _, cl := range contentLines {
		out = append(out, leftBar+padStyledRight(cl, innerW)+rightBar)
	}
	out = append(out, bottom)
	return out
}

func (m model) View() string {
	if m.quitting || m.selected != nil || m.width <= 0 || m.height <= 0 {
		return ""
	}
	if m.width < 16 || m.height < 4 {
		return runewidth.Truncate("窗口过小，请放大", max(0, m.width), "")
	}
	if !m.isSplitView() {
		return m.compactView()
	}

	leftW, rightW, bodyH, listH := m.splitDimensions()
	leftLines := m.renderLeftPane(leftW, bodyH, listH)
	rightLines := m.renderRightPane(rightW, bodyH)

	sep := lipgloss.NewStyle().Foreground(uiDivider).Render(" │ ")
	lines := make([]string, 0, m.height)
	for i := range bodyH {
		var l, r string
		if i < len(leftLines) {
			l = leftLines[i]
		}
		if i < len(rightLines) {
			r = rightLines[i]
		}
		lines = append(lines, padStyledRight(l, leftW)+sep+r)
	}
	if m.height >= 22 {
		lines = append(lines, "")
	}
	lines = append(lines, m.renderFooterLine(m.width))
	return strings.Join(lines, "\n")
}

func (m model) renderLeftPane(leftW, bodyH, listH int) []string {
	lines := make([]string, 0, bodyH)

	// 1. Header bar
	titleText := "⚡ GHOSTTY SSH PICKER"
	archiveHint := "F3 显示归档"
	if m.showArchived {
		archiveHint = "F3 隐藏归档"
	}
	titleStyled := lipgloss.NewStyle().Bold(true).Foreground(uiCyan).Render(runewidth.Truncate(titleText, leftW, "…"))
	titleW := lipgloss.Width(titleStyled)
	remHead := leftW - titleW
	if remHead >= 16 {
		keyPill := lipgloss.NewStyle().Foreground(uiSubText).Background(uiDivider)
		dimSt := lipgloss.NewStyle().Foreground(uiDim)
		fullSub := "  │  " + keyPill.Render(" F2 ") + dimSt.Render(" 归档/恢复 · ") + keyPill.Render(" F3 ") + dimSt.Render(" "+strings.TrimPrefix(archiveHint, "F3 "))
		if lipgloss.Width(fullSub) <= remHead {
			titleStyled += fullSub
		} else {
			shortPlain := runewidth.Truncate(" │ F2 归档 · "+archiveHint, remHead, "…")
			titleStyled += dimSt.Render(shortPlain)
		}
	}
	lines = append(lines, titleStyled)

	// 2. Search bar (3 lines)
	searchBorder := uiCyan
	if m.editingNotes {
		searchBorder = uiBorderDim
	}
	innerSearchW := max(1, leftW-4)
	icon := lipgloss.NewStyle().Foreground(uiCyan).Render("⌕ ")
	var searchContent string
	if m.filter == "" {
		placeholder := runewidth.Truncate("Filter hosts...", max(0, innerSearchW-2), "")
		if m.editingNotes {
			searchContent = lipgloss.NewStyle().Foreground(uiDim).Render("⌕ " + placeholder)
		} else {
			searchContent = icon + lipgloss.NewStyle().Foreground(uiDim).Render(placeholder)
		}
	} else {
		textMax := max(0, innerSearchW-3)
		shown := truncateLeft(m.filter, textMax)
		cur := ""
		if !m.editingNotes && innerSearchW >= 3 {
			cur = lipgloss.NewStyle().Foreground(uiCyan).Render("▋")
		}
		searchContent = icon + lipgloss.NewStyle().Foreground(uiText).Render(shown) + cur
	}
	lines = append(lines, renderRoundedBox([]string{searchContent}, leftW, searchBorder)...)

	if bodyH >= 19 {
		lines = append(lines, "")
	}

	// 3. Grouped server list
	if len(m.filtered) == 0 {
		msg := runewidth.Truncate("  未找到匹配的服务器 (按 Esc 清空输入)", leftW, "…")
		lines = append(lines, lipgloss.NewStyle().Foreground(uiDim).Italic(true).Render(msg))
		return lines
	}

	items := m.buildLeftItems()
	end := min(len(items), m.splitLineTop+listH)
	for _, it := range items[m.splitLineTop:end] {
		switch it.kind {
		case leftItemSpacer:
			lines = append(lines, "")
		case leftItemGroup:
			lines = append(lines, m.renderGroupHeader(it.group, it.count, leftW))
		case leftItemServer:
			s := m.filtered[it.serverIdx]
			lines = append(lines, m.renderServerRow(s, it.serverIdx == m.cursor, leftW))
		}
	}
	return lines
}

func (m model) renderGroupHeader(group string, count, leftW int) string {
	chev := lipgloss.NewStyle().Foreground(uiDim).Render("▾ ")
	maxGroupW := max(4, leftW-12)
	gName := runewidth.Truncate(group, maxGroupW, "…")
	gBadge := lipgloss.NewStyle().Foreground(uiPurpleSoft).Background(uiPurpleBg).Bold(true).Render(" " + gName + " ")
	cBadge := lipgloss.NewStyle().Foreground(uiSubText).Background(uiDivider).Render(fmt.Sprintf(" %d ", count))
	head := chev + gBadge + " " + cBadge
	rem := leftW - lipgloss.Width(head) - 1
	if rem >= 3 {
		head += " " + lipgloss.NewStyle().Foreground(uiDivider).Render(strings.Repeat("─", rem))
	}
	return head
}

func (m model) renderServerRow(s ServerConfig, isSelected bool, leftW int) string {
	wide := leftW >= 58
	prefixPlain := " ▫ "
	if wide {
		prefixPlain = "  ▫ "
	}
	if isSelected {
		if wide {
			prefixPlain = "  ▪ "
		} else {
			prefixPlain = " ▪ "
		}
	}
	prefixW := runewidth.StringWidth(prefixPlain)

	var badgePlain string
	badgeFg, badgeBg := uiGreen, uiGreenBg
	if s.Auth == "password" {
		badgeFg, badgeBg = uiAmber, uiAmberBg
		if wide {
			badgePlain = " 🔒 密码 "
		} else {
			badgePlain = "🔒 密码"
		}
	} else {
		if wide {
			badgePlain = " 🔑 密钥 "
		} else {
			badgePlain = "🔑 密钥"
		}
	}
	if s.Archived {
		badgeFg, badgeBg = uiDim, uiDivider
		if wide {
			badgePlain = " [已归档] "
		} else {
			badgePlain = "[已归档]"
		}
	}
	badgeW := runewidth.StringWidth(badgePlain)

	gap := " "
	if wide {
		gap = "  "
	}
	gapW := len(gap)

	rem := max(4, leftW-prefixW-badgeW-gapW*2-1)
	endpointFull := formatEndpoint(s.User, s.Host, s.Port)
	endpointW := min(max(20, rem*56/100), max(2, rem-10))
	nameW := max(2, rem-endpointW)

	nameCell := fitColumn(inventoryName(s), nameW)
	endpointCell := fitColumn(endpointFull, endpointW)
	trailSpaces := max(0, leftW-prefixW-nameW-gapW-endpointW-gapW-badgeW)

	if isSelected {
		rowBg := uiSelectedBg
		iconColor := uiPurpleSoft
		nameColor := uiText
		connColor := uiSubText
		if s.Archived {
			iconColor, nameColor, connColor = uiDim, uiDim, uiDim
		}
		pSt := lipgloss.NewStyle().Foreground(iconColor).Background(rowBg).Bold(true)
		nSt := lipgloss.NewStyle().Foreground(nameColor).Background(rowBg).Bold(true)
		cSt := lipgloss.NewStyle().Foreground(connColor).Background(rowBg)
		gSt := lipgloss.NewStyle().Background(rowBg)
		bSt := lipgloss.NewStyle().Foreground(badgeFg).Background(rowBg).Bold(!s.Archived)
		return pSt.Render(prefixPlain) +
			nSt.Render(nameCell) +
			gSt.Render(gap) +
			cSt.Render(endpointCell) +
			gSt.Render(gap) +
			bSt.Render(badgePlain) +
			gSt.Render(strings.Repeat(" ", trailSpaces))
	}

	iconColor, nameColor, connColor := uiDim, uiText, uiDim
	if s.Archived {
		nameColor = uiDim
	}
	pSt := lipgloss.NewStyle().Foreground(iconColor)
	nSt := lipgloss.NewStyle().Foreground(nameColor)
	cSt := lipgloss.NewStyle().Foreground(connColor)
	bSt := lipgloss.NewStyle().Foreground(badgeFg)
	if wide {
		bSt = bSt.Background(badgeBg)
	}
	return pSt.Render(prefixPlain) +
		nSt.Render(nameCell) +
		gap +
		cSt.Render(endpointCell) +
		gap +
		bSt.Render(badgePlain)
}

func (m model) renderRightPane(rightW, bodyH int) []string {
	lines := make([]string, 0, bodyH)
	if len(m.filtered) == 0 || m.cursor >= len(m.filtered) {
		msg := runewidth.Truncate(" 暂无选中服务器", rightW, "…")
		lines = append(lines, lipgloss.NewStyle().Foreground(uiDim).Render(msg))
		return lines
	}

	s := m.filtered[m.cursor]
	divLine := lipgloss.NewStyle().Foreground(uiDivider).Render(strings.Repeat("─", rightW))
	spacious := bodyH >= 27

	// 1. Server Title Header
	iconBadge := lipgloss.NewStyle().Foreground(uiPurpleSoft).Background(uiPurpleBg).Bold(true).Render(" ⊡ ")
	titleName := runewidth.Truncate(inventoryName(s), max(1, rightW-4), "…")
	nameStyle := lipgloss.NewStyle().Bold(true).Foreground(uiText)
	if s.Archived {
		nameStyle = nameStyle.Foreground(uiDim)
	}
	lines = append(lines, iconBadge+" "+nameStyle.Render(titleName))
	if bodyH >= 20 {
		lines = append(lines, "")
	}

	// 2. Detail fields: Host, Username, Auth, Group, OS, Spec
	lblSt := lipgloss.NewStyle().Foreground(uiDim)
	valSt := lipgloss.NewStyle().Foreground(uiText)
	subValSt := lipgloss.NewStyle().Foreground(uiSubText)
	valMax := max(1, rightW-9)

	hostEndpoint := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	lines = append(lines, lblSt.Render(" ⊡ Host  ")+valSt.Render(runewidth.Truncate(hostEndpoint, valMax, "…")))
	lines = append(lines, lblSt.Render(" @ User  ")+subValSt.Render(runewidth.Truncate(s.User, valMax, "…")))

	var authPill string
	if s.Archived {
		authPill = lipgloss.NewStyle().Foreground(uiDim).Background(uiDivider).Render(" 已归档 ")
	} else if s.Auth == "password" {
		authPill = lipgloss.NewStyle().Foreground(uiAmber).Background(uiAmberBg).Render(" 🔒 密码 ")
	} else {
		authPill = lipgloss.NewStyle().Foreground(uiGreen).Background(uiGreenBg).Render(" 🔑 密钥 ")
	}
	lines = append(lines, lblSt.Render(" ⚷ Auth  ")+authPill)

	grpText := runewidth.Truncate(s.Group, max(1, valMax-2), "…")
	grpPill := lipgloss.NewStyle().Foreground(uiPurpleSoft).Background(uiPurpleBg).Render(" " + grpText + " ")
	lines = append(lines, lblSt.Render(" # Group ")+grpPill)
	lines = append(lines, m.renderSystemLine(s, rightW))
	lines = append(lines, m.renderSpecLine(s, rightW))

	// 3. Latency Section
	if spacious {
		lines = append(lines, "")
	}
	lines = append(lines, divLine)
	if spacious {
		lines = append(lines, "")
	}

	latHeader := lipgloss.NewStyle().Foreground(uiPurple).Render(" ∿ ") +
		lipgloss.NewStyle().Foreground(uiSubText).Render(runewidth.Truncate("延迟 (ICMP/TCP)", max(1, rightW-3), "…"))
	lines = append(lines, latHeader)
	lines = append(lines, m.renderLatencyLine(s, rightW))

	// 4. Notes Section
	if spacious {
		lines = append(lines, "")
	}
	lines = append(lines, divLine)
	if spacious && bodyH >= 28 {
		lines = append(lines, "")
	}

	lines = append(lines, m.renderNotesHeader(rightW))

	remForBox := bodyH - len(lines)
	if remForBox >= 3 {
		noteInnerH := min(8, remForBox-2)
		if m.editingNotes {
			noteInnerH = min(10, remForBox-2)
		}
		lines = append(lines, m.renderNotesCard(s, rightW, noteInnerH)...)
	}
	return lines
}

func (m model) renderSystemLine(s ServerConfig, rightW int) string {
	lbl := lipgloss.NewStyle().Foreground(uiDim).Render(" ⊞ OS    ")
	valMax := max(1, rightW-9)
	sysSt := m.systemProbes[serverProbeKey(s)]

	if sysSt.Probing {
		return lbl + lipgloss.NewStyle().Foreground(uiCyan).Render(runewidth.Truncate("采集中...", valMax, "…"))
	}
	if strings.TrimSpace(s.System) != "" {
		osPart, archPart, hasSep := strings.Cut(s.System, " · ")
		if hasSep && archPart != "" {
			archPlain := " " + archPart + " "
			archW := runewidth.StringWidth(archPlain)
			if archW+2 <= valMax {
				archFg, archBg := uiCyan, lipgloss.Color("#0c3142")
				if strings.HasPrefix(strings.ToUpper(archPart), "ARM") {
					archFg, archBg = uiGreen, uiGreenBg
				}
				osShown := runewidth.Truncate(osPart, max(1, valMax-archW-1), "…")
				osStyled := lipgloss.NewStyle().Foreground(uiText).Render(osShown)
				archPill := lipgloss.NewStyle().Foreground(archFg).Background(archBg).Bold(true).Render(archPlain)
				return lbl + osStyled + " " + archPill
			}
		}
		return lbl + lipgloss.NewStyle().Foreground(uiText).Render(runewidth.Truncate(s.System, valMax, "…"))
	}
	if s.Archived {
		return lbl + lipgloss.NewStyle().Foreground(uiDim).Render(runewidth.Truncate("已归档", valMax, "…"))
	}
	if sysSt.Err != "" {
		return lbl + lipgloss.NewStyle().Foreground(uiAmber).Render(runewidth.Truncate("采集失败 · Ctrl+O 重试", valMax, "…"))
	}
	hint := "待采集 · Ctrl+O"
	if valMax >= 24 {
		hint = "待采集 (连接或 Ctrl+O)"
	}
	return lbl + lipgloss.NewStyle().Foreground(uiDim).Render(runewidth.Truncate(hint, valMax, "…"))
}

func (m model) renderSpecLine(s ServerConfig, rightW int) string {
	lbl := lipgloss.NewStyle().Foreground(uiDim).Render(" ⊟ Spec  ")
	valMax := max(1, rightW-9)
	sysSt := m.systemProbes[serverProbeKey(s)]

	if sysSt.Probing {
		return lbl + lipgloss.NewStyle().Foreground(uiCyan).Render(runewidth.Truncate("采集中...", valMax, "…"))
	}
	if strings.TrimSpace(s.Spec) != "" {
		shown := runewidth.Truncate(s.Spec, valMax, "…")
		parts := strings.Split(shown, " · ")
		if len(parts) > 1 {
			valSt := lipgloss.NewStyle().Foreground(uiSubText)
			sepSt := lipgloss.NewStyle().Foreground(uiDim)
			var rendered []string
			for _, p := range parts {
				rendered = append(rendered, valSt.Render(p))
			}
			return lbl + strings.Join(rendered, sepSt.Render(" · "))
		}
		return lbl + lipgloss.NewStyle().Foreground(uiSubText).Render(shown)
	}
	if s.Archived {
		return lbl + lipgloss.NewStyle().Foreground(uiDim).Render("—")
	}
	if strings.TrimSpace(s.System) != "" {
		return lbl + lipgloss.NewStyle().Foreground(uiDim).Render(runewidth.Truncate("待采集 · Ctrl+O", valMax, "…"))
	}
	return lbl + lipgloss.NewStyle().Foreground(uiDim).Render("—")
}

func (m model) renderLatencyLine(s ServerConfig, rightW int) string {
	dimBarSt := lipgloss.NewStyle().Foreground(uiBorderDim)
	if s.Archived {
		return lipgloss.NewStyle().Foreground(uiDim).Render(runewidth.Truncate("   已归档 · 不探测", rightW, "…"))
	}

	st := m.latency[serverProbeKey(s)]
	var leftPart string
	var bars string

	switch {
	case !st.HasData:
		leftPart = "   " + lipgloss.NewStyle().Foreground(uiCyan).Render("检测中...")
		bars = dimBarSt.Render("▁▂▃▄▅▆")
	case st.Result.Err != nil:
		leftPart = "   " + lipgloss.NewStyle().Foreground(uiRed).Bold(true).Render("超时 / 不可达")
		bars = dimBarSt.Render("▁▁▁▁▁▁")
	default:
		rtt := st.Result.RTT
		ms := rtt.Milliseconds()
		color, bg := uiGreen, uiGreenBg
		lit := 6
		switch {
		case ms < 50:
			color, bg, lit = uiGreen, uiGreenBg, 6
		case ms < 100:
			color, bg, lit = uiGreen, uiGreenBg, 5
		case ms < 180:
			color, bg, lit = uiAmber, uiAmberBg, 4
		case ms <= 260:
			color, bg, lit = uiAmber, uiAmberBg, 3
		case ms <= 400:
			color, bg, lit = uiRed, uiRedBg, 2
		default:
			color, bg, lit = uiRed, uiRedBg, 1
		}
		msText := lipgloss.NewStyle().Bold(true).Foreground(color).Render(formatLatencyMs(rtt))
		methodPill := lipgloss.NewStyle().Foreground(color).Background(bg).Render(" " + st.Result.Method + " ")
		leftPart = "   " + msText + " " + methodPill
		glyphs := []rune("▁▂▃▄▅▆")
		activeSt := lipgloss.NewStyle().Foreground(color)
		bars = activeSt.Render(string(glyphs[:lit])) + dimBarSt.Render(string(glyphs[lit:]))
	}

	leftW := lipgloss.Width(leftPart)
	barsW := lipgloss.Width(bars)
	if leftW+2+barsW <= rightW {
		return leftPart + strings.Repeat(" ", rightW-leftW-barsW-1) + bars + " "
	}
	return leftPart
}

func (m model) renderNotesHeader(rightW int) string {
	if m.editingNotes {
		icon := lipgloss.NewStyle().Foreground(uiCyan).Bold(true).Render(" ✎ ")
		lbl := lipgloss.NewStyle().Foreground(uiCyan).Bold(true).Render(runewidth.Truncate("备注 · 编辑中", max(1, rightW-3), "…"))
		return icon + lbl
	}
	icon := lipgloss.NewStyle().Foreground(uiPurple).Render(" ✎ ")
	lbl := lipgloss.NewStyle().Foreground(uiSubText).Render(runewidth.Truncate("备注 (Notes)", max(1, rightW-3), "…"))
	head := icon + lbl
	hint := "Ctrl+E 编辑"
	if lipgloss.Width(head)+2+runewidth.StringWidth(hint) <= rightW {
		gap := rightW - lipgloss.Width(head) - runewidth.StringWidth(hint)
		head += strings.Repeat(" ", gap) + lipgloss.NewStyle().Foreground(uiDim).Render(hint)
	}
	return head
}

func (m model) renderNotesCard(s ServerConfig, rightW, innerH int) []string {
	innerW := max(1, rightW-4)
	if m.editingNotes {
		textSt := lipgloss.NewStyle().Foreground(uiText)
		curSt := lipgloss.NewStyle().Foreground(lipgloss.Color("#0f172a")).Background(uiCyan).Bold(true)
		edLines := renderNoteEditorLines(m.noteBuf, m.notePos, innerW, innerH, textSt, curSt)
		for len(edLines) < innerH {
			edLines = append(edLines, "")
		}
		return renderRoundedBox(edLines, rightW, uiCyan)
	}

	borderCol := uiBorderDim
	content := make([]string, 0, innerH)
	if strings.TrimSpace(s.Notes) == "" {
		hint := runewidth.Truncate("按 Ctrl+E 或 F4 添加备注...", innerW, "…")
		content = append(content, lipgloss.NewStyle().Foreground(uiDim).Italic(true).Render(hint))
	} else {
		borderCol = uiNoteBorder
		wrapped := wrapNoteLines(s.Notes, innerW)
		noteSt := lipgloss.NewStyle().Foreground(uiSubText)
		for i := 0; i < min(innerH, len(wrapped)); i++ {
			line := wrapped[i]
			if i == innerH-1 && len(wrapped) > innerH {
				line = runewidth.Truncate(line, max(1, innerW-1), "") + "…"
			}
			content = append(content, noteSt.Render(line))
		}
	}
	for len(content) < innerH {
		content = append(content, "")
	}
	return renderRoundedBox(content, rightW, borderCol)
}

func (m model) renderFooterLine(maxW int) string {
	if m.status != "" {
		clean := strings.Join(strings.Fields(sanitizeFilter(m.status)), " ")
		shown := runewidth.Truncate(" "+clean, maxW, "…")
		col := uiCyan
		if strings.Contains(clean, "失败") || strings.Contains(clean, "请先") {
			col = uiAmber
		}
		return lipgloss.NewStyle().Foreground(col).Render(shown)
	}
	if m.editingNotes {
		plain := runewidth.Truncate(" Ctrl+S / F4 保存  ·  Enter 换行  ·  Esc 取消", maxW, "…")
		return lipgloss.NewStyle().Foreground(uiCyan).Render(plain)
	}
	plain := fmt.Sprintf(" ↑↓ 选择   Enter 连接   Ctrl+E 备注   Ctrl+O 系统   Esc 返回   (%d/%d)", len(m.filtered), len(m.config.Servers))
	if runewidth.StringWidth(plain) > maxW {
		plain = fmt.Sprintf(" ↑↓ 选择  Enter 连接  Ctrl+E 备注  Esc 返回  (%d/%d)", len(m.filtered), len(m.config.Servers))
	}
	if runewidth.StringWidth(plain) > maxW {
		plain = runewidth.Truncate(m.footer(), maxW, "…")
	}
	return lipgloss.NewStyle().Foreground(uiDim).Render(plain)
}

func formatEndpoint(user, host string, port int) string {
	return fmt.Sprintf("%s@%s", user, net.JoinHostPort(host, strconv.Itoa(port)))
}

func fitColumn(s string, width int) string {
	return runewidth.FillRight(runewidth.Truncate(s, width, "…"), width)
}

func truncateLeft(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= maxWidth {
		return s
	}
	if maxWidth == 1 {
		return "…"
	}
	limit := maxWidth - 1
	start := len(s)
	nonZero := 0
	for start > 0 && nonZero <= limit {
		r, size := utf8.DecodeLastRuneInString(s[:start])
		start -= size
		if r == '\u200d' {
			if start > 0 {
				_, prevSize := utf8.DecodeLastRuneInString(s[:start])
				start -= prevSize
			}
			continue
		}
		if runewidth.RuneWidth(r) > 0 {
			nonZero++
		}
	}
	for start > 0 && runewidth.StringWidth(s[start:]) <= limit {
		r, size := utf8.DecodeLastRuneInString(s[:start])
		start -= size
		if r == '\u200d' && start > 0 {
			_, prevSize := utf8.DecodeLastRuneInString(s[:start])
			start -= prevSize
		}
	}
	tail := s[start:]
	for len(tail) > 0 && runewidth.StringWidth(tail) > limit {
		_, size := utf8.DecodeRuneInString(tail)
		tail = tail[size:]
		for len(tail) > 0 {
			next, nextSize := utf8.DecodeRuneInString(tail)
			if next == '\u200d' {
				tail = tail[nextSize:]
				if len(tail) > 0 {
					_, afterSize := utf8.DecodeRuneInString(tail)
					tail = tail[afterSize:]
				}
				continue
			}
			if runewidth.RuneWidth(next) == 0 {
				tail = tail[nextSize:]
				continue
			}
			break
		}
	}
	return "…" + tail
}

// Use compact columns when the terminal is too narrow or short for the split layout.
func (m model) compactView() string {
	fit := func(s string) string { return runewidth.Truncate(s, m.width, "…") }
	if m.editingNotes && len(m.filtered) > 0 && m.cursor < len(m.filtered) {
		lines := []string{fit("✎ 备注编辑: " + inventoryName(m.filtered[m.cursor]))}
		edH := max(1, m.height-2)
		textSt := lipgloss.NewStyle().Foreground(uiText)
		curSt := lipgloss.NewStyle().Foreground(lipgloss.Color("#0f172a")).Background(uiCyan)
		edLines := renderNoteEditorLines(m.noteBuf, m.notePos, max(1, m.width), edH, textSt, curSt)
		lines = append(lines, edLines...)
		lines = append(lines, fit(m.footer()))
		return strings.Join(lines, "\n")
	}

	title := "SSH · F2 归档/恢复 F3 显示归档"
	if m.showArchived {
		title = "SSH · F2 归档/恢复 F3 隐藏归档"
	}
	lines := []string{fit(title), fit("❯ " + truncateLeft(m.filter, max(0, m.width-2)))}
	rows, columns := m.listLayout()
	columnWidth := (m.width - 2*(columns-1)) / columns
	end := min(len(m.filtered), m.viewportTop+rows*columns)
	for row := range min(rows, end-m.viewportTop) {
		var line strings.Builder
		for column := range columns {
			i := m.viewportTop + column*rows + row
			if i >= end {
				break
			}
			if column > 0 {
				line.WriteString("  ")
			}
			s := m.filtered[i]
			address := formatEndpoint(s.User, s.Host, s.Port)
			if s.Archived {
				address = "[已归档]"
			}
			addressWidth := min(runewidth.StringWidth(address), (columnWidth-3)*2/3)
			nameWidth := columnWidth - addressWidth - 3
			cell := "  " + fitColumn(s.Name, nameWidth) + " " + fitColumn(address, addressWidth)
			color := uiCyan
			if s.Archived {
				color = uiDim
			}
			if i == m.cursor {
				cell = "❯ " + strings.TrimPrefix(cell, "  ")
			}
			if s.Archived || i == m.cursor {
				cell = lipgloss.NewStyle().Foreground(color).Bold(i == m.cursor).Render(cell)
			}
			line.WriteString(cell)
		}
		lines = append(lines, line.String())
	}
	if len(m.filtered) == 0 {
		lines = append(lines, fit("未找到服务器，Esc 清空"))
	}
	lines = append(lines, fit(m.footer()))
	return strings.Join(lines, "\n")
}

func (m model) footer() string {
	if m.status != "" {
		return strings.Join(strings.Fields(sanitizeFilter(m.status)), " ")
	}
	if m.editingNotes {
		return "Ctrl+S/F4 保存 Enter 换行 Esc 取消"
	}
	return fmt.Sprintf("↑↓←→ 选择 Enter 连接 Ctrl+E 备注 Esc 返回 (%d/%d)", len(m.filtered), len(m.config.Servers))
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
		"-o", "TCPKeepAlive=no",
		"-o", "IPQoS=none")
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

// Only local SSH authentication can request the password through ASKPASS.
// Keep the socket private and secrets out of argv, environment variables and scripts.
func runWithAskpass(sshPath string, args []string, password string, env []string) error {
	if err := validatePassword(password); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	expectedInfo, _ := os.Stat(self)
	// Keep Unix socket paths below the platform's roughly 104-byte limit.
	baseDir := os.TempDir()
	if len(filepath.Join(baseDir, "sshtui-askpass-0000000000", "password.sock")) >= 100 {
		baseDir = "/tmp"
	}
	dir, err := os.MkdirTemp(baseDir, "sshtui-askpass-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "password.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
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
	cmd := exec.Command(sshPath, args...)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "SSH_ASKPASS", "SSH_ASKPASS_REQUIRE", "SSH_ASKPASS_PROMPT", "SSHTUI_ASKPASS_SOCKET", "SSHTUI_ASKPASS_HELPER":
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, "SSH_ASKPASS="+self, "SSH_ASKPASS_REQUIRE=force", "SSHTUI_ASKPASS_SOCKET="+path, "SSHTUI_ASKPASS_HELPER=1")
	return runSSH(cmd, func(pid int) {
		pidChan <- pid
	})
}

func isAuthorizedPeer(conn net.Conn, expectedPID int, expectedExe string, expectedInfo os.FileInfo) bool {
	if expectedPID <= 0 {
		return false
	}
	peerPID, err := getPeerPID(conn)
	if err != nil || peerPID <= 0 {
		return false
	}
	ppid, err := getProcessPPID(peerPID)
	if err != nil || ppid != expectedPID {
		return false
	}
	exePath, err := getProcessPath(peerPID)
	if err != nil {
		return false
	}
	if expectedInfo != nil {
		if infoPeer, err := os.Stat(exePath); err == nil && os.SameFile(infoPeer, expectedInfo) {
			return true
		}
	}
	realPeer, err := filepath.EvalSymlinks(exePath)
	if err != nil {
		realPeer = exePath
	}
	return realPeer == expectedExe
}

func runSSH(cmd *exec.Cmd, onStart ...func(pid int)) error {
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	var terminalState *term.State
	if term.IsTerminal(int(os.Stdin.Fd())) {
		terminalState, _ = term.GetState(int(os.Stdin.Fd()))
	}
	defer restoreTerminal(terminalState)
	if progress := startConnectionProgress(); progress != nil {
		defer progress.finish()
	}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGWINCH)
	if err := cmd.Start(); err != nil {
		signal.Stop(signals)
		return err
	}
	if len(onStart) > 0 && onStart[0] != nil && cmd.Process != nil {
		onStart[0](cmd.Process.Pid)
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	defer func() {
		signal.Stop(signals)
		close(done)
		wg.Wait()
	}()
	go func() {
		defer wg.Done()
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

var ttyMu sync.Mutex

type connectionProgress struct {
	done  chan struct{}
	wg    sync.WaitGroup
	drawn atomic.Bool
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
	p := &connectionProgress{done: make(chan struct{})}
	start := time.Now()
	style := lipgloss.NewStyle().Foreground(lipgloss.Color("#38bdf8"))
	frames := []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")
	var lastMtime time.Time
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		for frame := 0; ; frame++ {
			select {
			case <-p.done:
				return
			case <-ticker.C:
				current, err := term.GetState(int(os.Stdin.Fd()))
				if err != nil || *current != *state {
					ttyMu.Lock()
					if p.drawn.Load() && !lastMtime.IsZero() {
						if info, statErr := os.Stderr.Stat(); statErr == nil && info.ModTime().Equal(lastMtime) {
							fmt.Fprint(os.Stderr, "\r\x1b[2K")
						}
					}
					p.drawn.Store(false)
					ttyMu.Unlock()
					return
				}
				line := fmt.Sprintf("  %c 正在连接 · 已等待 %.1fs", frames[frame%len(frames)], time.Since(start).Seconds())
				ttyMu.Lock()
				fmt.Fprintf(os.Stderr, "\r\x1b[2K%s\r", style.Render(line))
				p.drawn.Store(true)
				if info, statErr := os.Stderr.Stat(); statErr == nil {
					lastMtime = info.ModTime()
				}
				ttyMu.Unlock()
			}
		}
	}()
	return p
}

func (p *connectionProgress) finish() {
	close(p.done)
	p.wg.Wait()
	if p.drawn.Load() {
		ttyMu.Lock()
		fmt.Fprint(os.Stderr, "\r\x1b[2K")
		ttyMu.Unlock()
	}
}

// Clear nested Kitty modes on both screens; their keyboard stacks are independent.
const terminalCleanup = "\x1b[?2026l\x1b[?9l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1004l\x1b[?1005l\x1b[?1006l\x1b[?1015l\x1b[?1016l\x1b[?2004l\x1b[>4;0m\x1b[<999u\x1b[0m\x1b[?25h\x1b[?1049l\x1b[<999u"

func restoreTerminal(state *term.State) {
	if state == nil {
		return
	}
	_ = term.Restore(int(os.Stdin.Fd()), state)
	output := os.Stderr
	if !term.IsTerminal(int(output.Fd())) {
		var err error
		output, err = os.OpenFile("/dev/tty", os.O_WRONLY, 0)
		if err != nil {
			return
		}
		defer output.Close()
	}
	fmt.Fprint(output, terminalCleanup)
}

func ghosttyScript(script string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
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
	if _, err := fmt.Fprintf(tty, "\x1b]2;%s\x1b\\", marker); err != nil {
		tty.Close()
		return nil
	}

	type pinResult struct {
		id  string
		err error
	}
	var mu sync.Mutex
	active := true
	resCh := make(chan pinResult, 1)

	go func() {
		id, err := ghosttyScript(`on run argv
	if application "Ghostty" is running then
		tell application "Ghostty"
			repeat 15 times
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
	end if
end run`, marker, title)
		mu.Lock()
		if err == nil && id != "" && active {
			ttyMu.Lock()
			_, _ = fmt.Fprintf(tty, "\x1b]2;%s\x1b\\", title)
			ttyMu.Unlock()
		}
		mu.Unlock()
		resCh <- pinResult{id: id, err: err}
	}()

	return func() {
		defer tty.Close()
		mu.Lock()
		active = false
		mu.Unlock()
		res := <-resCh
		if res.err != nil {
			ttyMu.Lock()
			_, _ = fmt.Fprint(tty, "\x1b]2;\x1b\\")
			ttyMu.Unlock()
			fmt.Fprintf(os.Stderr, "设置 Ghostty 标签标题失败，继续连接: %v\n", res.err)
			return
		}
		if res.id == "" {
			ttyMu.Lock()
			_, _ = fmt.Fprint(tty, "\x1b]2;\x1b\\")
			ttyMu.Unlock()
			return
		}
		_, err := ghosttyScript(`on run argv
	if application "Ghostty" is running then
		tell application "Ghostty"
			if not (perform action "set_tab_title:" on terminal id (item 1 of argv)) then error "Cannot reset tab title"
		end tell
	end if
end run`, res.id)
		ttyMu.Lock()
		_, _ = fmt.Fprint(tty, "\x1b]2;\x1b\\")
		ttyMu.Unlock()
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
	n, err := io.Copy(os.Stdout, conn)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("此 SSH 提示不能使用服务器密码自动回答")
	}
	return nil
}

const loginClearCommand = `LocalCommand=printf '\033[H\033[2J\033[3J'`

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
	if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) && os.Getenv("TERM") != "dumb" {
		args = append(args, "-o", "PermitLocalCommand=yes", "-o", loginClearCommand)
	}
	muxArgs, stopMux := startMuxSystemCollector(s, sshPath)
	if stopMux != nil {
		defer stopMux()
		args = append(args, muxArgs...)
	}
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
		detail := lipgloss.NewStyle().Foreground(lipgloss.Color("#64748b")).Render(fmt.Sprintf("  [%s] %s", s.Group, formatEndpoint(s.User, s.Host, s.Port)))
		fmt.Fprintf(os.Stderr, "\n%s\n%s\n", name, detail)
	} else {
		fmt.Fprintf(os.Stderr, "连接 [%s] %s (%s)\n", s.Group, s.Name, formatEndpoint(s.User, s.Host, s.Port))
	}
	restoreTitle := pinGhosttyTabTitle(s)
	if restoreTitle != nil {
		defer restoreTitle()
	}
	if s.Auth != "password" {
		if restoreTitle != nil || term.IsTerminal(int(os.Stdin.Fd())) {
			cmd := exec.Command(sshPath, args...)
			cmd.Env = env
			return runSSH(cmd)
		}
		return syscall.Exec(sshPath, append([]string{"ssh"}, args...), env)
	}

	return runWithAskpass(sshPath, args, s.Password, env)
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
	return cmp.Or(strings.TrimSpace(s.Group), "Default")
}

func validateInventory(cfg *Config) error {
	seen := make(map[string]bool)
	for _, s := range cfg.Servers {
		if s.Archived {
			continue
		}
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
		if s.Archived {
			continue
		}
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
		if s.Archived {
			continue
		}
		groups[inventoryGroup(s)] = append(groups[inventoryGroup(s)], s)
	}
	for _, g := range slices.Sorted(maps.Keys(groups)) {
		if strings.IndexFunc(g, unicode.IsSpace) >= 0 {
			return "", fmt.Errorf("Ansible INI 分组名称不能含空白字符: %q，请改用 --ansible", g)
		}
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
func isKeyPath(s string) bool {
	if strings.HasPrefix(s, "~/") || strings.HasPrefix(s, "/") || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") {
		return true
	}
	lower := strings.ToLower(s)
	for _, ext := range []string{".pem", ".key", ".pub", ".id_rsa", ".id_ed25519", ".id_ecdsa", ".id_dsa"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	expanded := expandPath(s)
	if info, err := os.Stat(expanded); err == nil && !info.IsDir() {
		return true
	}
	return false
}

func parseServerArgs(args []string, defaultUser string, defaultPort int) (*ServerConfig, error) {
	return parseServerArgsWithUsage("s -e", args, defaultUser, defaultPort)
}

func parseServerArgsWithUsage(usageCmd string, args []string, defaultUser string, defaultPort int) (*ServerConfig, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("缺少服务器参数。用法: %s [user@]host [port] [password|key] [name] [group]", usageCmd)
	}

	s := &ServerConfig{
		Group: "Default",
		User:  cmp.Or(defaultUser, "root"),
	}

	target := strings.TrimSpace(args[0])
	if target == "" {
		return nil, fmt.Errorf("主机地址不能为空")
	}

	if u, rem, ok := strings.Cut(target, "@"); ok {
		if u != "" {
			s.User = u
		}
		target = rem
	}

	if ip, err := netip.ParseAddr(target); err == nil && ip.IsValid() {
		s.Host = target
	} else if h, p, err := net.SplitHostPort(target); err == nil {
		s.Host = h
		port, err := strconv.Atoi(p)
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("端口必须在 1–65535 之间")
		}
		s.Port = port
	} else if strings.HasPrefix(target, "[") && strings.HasSuffix(target, "]") {
		inner := target[1 : len(target)-1]
		if ip, err := netip.ParseAddr(inner); err == nil && ip.IsValid() {
			s.Host = inner
		} else {
			return nil, fmt.Errorf("无效的主机或端口: %s", target)
		}
	} else if strings.ContainsAny(target, ":[]") {
		return nil, fmt.Errorf("无效的主机或端口: %s", target)
	} else {
		s.Host = target
	}

	if s.Host == "" {
		return nil, fmt.Errorf("未能解析出有效的主机名或 IP")
	}

	portSet := s.Port > 0
	var nameSet, groupSet bool
	for _, arg := range args[1:] {
		if arg == "" {
			continue
		}
		if !portSet && s.Auth == "" {
			if n, err := strconv.Atoi(arg); err == nil && n > 0 && n <= 65535 {
				s.Port = n
				portSet = true
				continue
			}
		}
		if s.Auth == "" {
			if isKeyPath(arg) {
				s.Auth = "key"
				s.Key = arg
			} else {
				s.Auth = "password"
				s.Password = arg
			}
			continue
		}
		if !nameSet {
			s.Name = arg
			nameSet = true
			continue
		}
		if !groupSet {
			s.Group = arg
			groupSet = true
			continue
		}
		return nil, fmt.Errorf("参数过多: %q。用法: %s [user@]host [port] [password|key] [name] [group]", arg, usageCmd)
	}

	s.Group = cmp.Or(strings.TrimSpace(s.Group), "Default")
	if s.Port <= 0 {
		s.Port = cmp.Or(defaultPort, 22)
	}
	if s.Port < 1 || s.Port > 65535 {
		return nil, fmt.Errorf("端口必须在 1–65535 之间")
	}
	if strings.ContainsRune(s.Key, 0) {
		return nil, fmt.Errorf("服务器字段不能包含 NUL 字符")
	}
	for _, field := range []string{s.Host, s.User, s.Name, s.Group} {
		if strings.IndexFunc(field, unicode.IsControl) >= 0 {
			return nil, fmt.Errorf("服务器字段不能包含控制字符")
		}
	}
	if s.Auth == "" {
		s.Auth = "key"
	}
	if s.Name == "" {
		if s.Port != 22 {
			s.Name = net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
		} else {
			s.Name = s.Host
		}
	}

	return s, nil
}

func quickAddServer(args []string) error {
	return quickAddServerWithUsage("s -e", args)
}

func quickAddServerWithUsage(usageCmd string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("缺少服务器参数。用法: %s [user@]host [port] [password|key] [name] [group]", usageCmd)
	}
	var server *ServerConfig
	var defaultKey string
	_, err := editServersConfig(func(cfg *Config, servers *yaml.Node) error {
		var err error
		server, err = parseServerArgsWithUsage(usageCmd, args, cfg.Default.User, cfg.Default.Port)
		if err != nil {
			return err
		}
		if err := validatePassword(server.Password); err != nil {
			return err
		}
		defaultKey = cfg.Default.Key
		var entry yaml.Node
		if err := entry.Encode(server); err != nil {
			return err
		}
		servers.Content = append(servers.Content, &entry)
		return nil
	})
	if err != nil {
		return err
	}
	authDesc := "密码认证"
	if server.Auth == "key" {
		if key := cmp.Or(server.Key, defaultKey); key != "" {
			authDesc = "密钥认证: " + key
		} else {
			authDesc = "密钥认证"
		}
	}
	fmt.Println("✓ 已成功将服务器添加到配置文件")
	fmt.Printf("  • 名称: %s\n", server.Name)
	fmt.Printf("  • 分组: %s\n", server.Group)
	fmt.Printf("  • 目标: %s\n", formatEndpoint(server.User, server.Host, server.Port))
	fmt.Printf("  • 认证: %s\n", authDesc)
	return nil
}

func editServersConfig(edit func(*Config, *yaml.Node) error) (*Config, error) {
	cfgPath, err := getConfigPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(cfgPath+".lock", os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("打开配置锁失败: %w", err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return nil, fmt.Errorf("锁定配置失败: %w", err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	f, cfgPath, err := openConfig()
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(f)
	closeErr := f.Close()
	if readErr != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", readErr)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	cfg, err := decodeConfig(raw)
	if err != nil {
		return nil, err
	}

	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("解析 YAML 配置失败: %w", err)
	}
	if len(document.Content) == 0 {
		return nil, fmt.Errorf("配置必须为 YAML 映射")
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("配置必须为 YAML 映射")
	}
	var servers *yaml.Node
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == "servers" {
			servers = root.Content[i+1]
			break
		}
	}
	if servers == nil {
		servers = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "servers"}, servers)
	}
	if servers.Kind == yaml.AliasNode {
		servers = servers.Alias
	}
	if servers.Tag == "!!null" {
		servers.Kind, servers.Tag, servers.Value = yaml.SequenceNode, "!!seq", ""
	}
	if servers.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("servers 必须为 YAML 序列")
	}
	if err := edit(cfg, servers); err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return nil, fmt.Errorf("编码 YAML 配置失败: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	updated := buffer.Bytes()
	updatedCfg, err := decodeConfig(updated)
	if err != nil {
		return nil, err
	}

	dir := filepath.Dir(cfgPath)
	temp, err := os.CreateTemp(dir, "sshtui-servers-*.yaml")
	if err != nil {
		return nil, fmt.Errorf("创建临时文件失败: %w", err)
	}
	defer os.Remove(temp.Name())

	if _, err := temp.Write(updated); err != nil {
		temp.Close()
		return nil, fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := temp.Chmod(0600); err != nil {
		temp.Close()
		return nil, fmt.Errorf("设置文件权限失败: %w", err)
	}
	if err := temp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(temp.Name(), cfgPath); err != nil {
		return nil, fmt.Errorf("保存配置文件失败: %w", err)
	}

	return updatedCfg, nil
}

func directMatch(servers []ServerConfig, term string) *ServerConfig {
	var exact, partial *ServerConfig
	exactCount, partialCount := 0, 0
	lower := strings.ToLower(term)
	for i := range servers {
		if servers[i].Archived {
			continue
		}
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
	if len(os.Args) > 1 && os.Args[1] == "add" {
		addCmd := flag.NewFlagSet("add", flag.ContinueOnError)
		addCmd.Usage = func() {
			fmt.Fprintf(addCmd.Output(), "用法: s add [user@]host [port] [password|key] [name] [group]\n")
		}
		if err := addCmd.Parse(os.Args[2:]); err != nil {
			if err == flag.ErrHelp {
				return nil
			}
			return err
		}
		return quickAddServerWithUsage("s add", addCmd.Args())
	}

	edit := flag.Bool("e", false, "打开配置文件编辑，或带参数快速添加服务器: s -e [user@]host [port] [password|key] [name] [group]")
	add := flag.Bool("a", false, "快速添加服务器到配置文件: s -a [user@]host [port] [password|key] [name] [group]")
	var showVersion bool
	flag.BoolVar(&showVersion, "version", false, "显示版本号")
	flag.BoolVar(&showVersion, "v", false, "显示版本号")
	list := flag.Bool("l", false, "纯文本表格列出所有配置的服务器")
	path := flag.Bool("p", false, "打印配置文件所在绝对路径")
	ansible := flag.Bool("ansible", false, "输出 Ansible Dynamic Inventory JSON 格式")
	ini := flag.Bool("ansible-ini", false, "输出 Ansible INI 格式")
	ansibleList := flag.Bool("list", false, "Ansible 官方动态清单规范 Hook (等同于 --ansible)")
	ansibleHost := flag.String("host", "", "Ansible 官方单机查询 Hook")
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		cmdName := filepath.Base(os.Args[0])
		fmt.Fprintf(out, "用法: %s [选项] [筛选词]\n", cmdName)
		fmt.Fprintf(out, "      %s add [user@]host [port] [password|key] [name] [group]\n", cmdName)
		fmt.Fprintf(out, "      %s run [-g 分组] \"要执行的命令\"\n\n选项:\n", cmdName)
		flag.PrintDefaults()
	}
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
		if flag.NArg() > 0 {
			return quickAddServerWithUsage("s -e", flag.Args())
		}
		return editConfig()
	}
	if *add {
		return quickAddServerWithUsage("s -a", flag.Args())
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
		count := 0
		for _, s := range cfg.Servers {
			if !s.Archived {
				count++
			}
		}
		fmt.Printf("配置文件: %s (共 %d 台)\n\n", cfgPath, count)
		i := 0
		for _, s := range cfg.Servers {
			if s.Archived {
				continue
			}
			auth := "🔑 密钥"
			if s.Auth == "password" {
				auth = "🔒 密码"
			}
			fmt.Printf("%2d. [%s] %-20s %s (%s)\n", i+1, s.Group, s.Name, formatEndpoint(s.User, s.Host, s.Port), auth)
			i++
		}
		return nil
	}

	if flag.NArg() > 1 {
		return fmt.Errorf("服务器筛选词只能是一个参数；名称含空格时请使用引号")
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
