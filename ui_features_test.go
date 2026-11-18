package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestSplitViewRendersDetailsGroupsLatencyAndNotes(t *testing.T) {
	cfg, err := decodeConfig([]byte(`
servers:
  - name: "0.10"
    group: "work"
    host: "192.168.40.10"
    user: "fcb"
    auth: "password"
    password: "secret"
  - name: "Oracle-伦敦-AMD-1"
    group: "Default"
    host: "132.145.52.157"
    port: 22
    user: "root"
    auth: "key"
    notes: |
      OCI 伦敦 AMD
      主力落地机：Xray / WireGuard
      备用代理，稳定性较好
`))
	if err != nil {
		t.Fatal(err)
	}

	m := initialModel(cfg, "")
	// Custom group "work" comes before "Default".
	if len(m.filtered) != 2 || m.filtered[0].Group != "work" || m.filtered[1].Name != "Oracle-伦敦-AMD-1" {
		t.Fatalf("unexpected group ordering: %+v", m.filtered)
	}

	next, _ := m.Update(tea.WindowSizeMsg{Width: 116, Height: 30})
	m = next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(model)

	// Before async probe finishes, UI renders non-blocking probing state.
	viewBefore := m.View()
	if !strings.Contains(viewBefore, "检测中...") {
		t.Fatalf("expected non-blocking probing state before result arrives: %s", viewBefore)
	}

	key := serverProbeKey(m.filtered[m.cursor])
	next, _ = m.Update(latencyResultMsg{
		key:    key,
		result: latencyResult{RTT: 86 * time.Millisecond, Method: "ICMP", At: time.Now()},
	})
	m = next.(model)

	view := m.View()
	if lipgloss.Width(view) > 116 || lipgloss.Height(view) > 30 {
		t.Fatalf("split view %dx%d exceeded 116x30", lipgloss.Width(view), lipgloss.Height(view))
	}
	for _, want := range []string{
		"GHOSTTY SSH PICKER",
		"Filter hosts...",
		"work",
		"Default",
		"Oracle-伦敦-AMD-1",
		"132.145.52.157:22",
		"root",
		"密钥",
		"延迟 (ICMP/TCP)",
		"86 ms",
		"ICMP",
		"备注 (Notes)",
		"OCI 伦敦 AMD",
		"主力落地机：Xray / WireGuard",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("split view missing %q\n%s", want, view)
		}
	}
}

func TestLatencyAsyncNonBlockingDebounceAndStates(t *testing.T) {
	cfg := &Config{Servers: []ServerConfig{
		{Name: "alpha", Group: "Default", Host: "192.0.2.1", Port: 22, User: "root", Auth: "key", sourceIndex: 0},
		{Name: "beta", Group: "Default", Host: "192.0.2.2", Port: 22, User: "root", Auth: "key", sourceIndex: 1},
		{Name: "retired", Group: "Default", Host: "192.0.2.3", Port: 22, User: "root", Auth: "key", Archived: true, sourceIndex: 2},
	}}

	var calls []string
	m := initialModel(cfg, "")
	m.showArchived = true
	m.applyFilter()
	m.probeFn = func(_ context.Context, host string, port int) latencyResult {
		calls = append(calls, fmt.Sprintf("%s:%d", host, port))
		if host == "192.0.2.2" {
			return latencyResult{RTT: 175 * time.Millisecond, Method: "TCP", At: time.Now()}
		}
		return latencyResult{Err: errors.New("i/o timeout"), At: time.Now()}
	}

	// Init schedules immediate async Cmd without blocking Init() itself.
	initCmd := m.Init()
	if initCmd == nil || len(calls) != 0 {
		t.Fatalf("Init should return lazy Cmd without synchronous network call, calls=%v", calls)
	}

	// Rapidly move alpha -> beta; alpha's stale debounce seq must be ignored.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(model)
	staleSeq := m.probeSeq - 1
	next, staleCmd := m.Update(probeDebounceMsg{seq: staleSeq, key: serverProbeKey(cfg.Servers[0]), host: "192.0.2.1", port: 22})
	m = next.(model)
	if staleCmd != nil {
		t.Fatal("stale debounce message should not trigger network probe")
	}

	next, activeCmd := m.Update(probeDebounceMsg{seq: m.probeSeq, key: serverProbeKey(m.filtered[m.cursor]), host: "192.0.2.2", port: 22})
	m = next.(model)
	if activeCmd == nil {
		t.Fatal("current debounce message must return probe Cmd")
	}
	msg := activeCmd()
	next, _ = m.Update(msg)
	m = next.(model)
	next, _ = m.Update(tea.WindowSizeMsg{Width: 96, Height: 24})
	m = next.(model)

	view := m.View()
	if !strings.Contains(view, "175 ms") || !strings.Contains(view, "TCP") {
		t.Fatalf("expected 175 ms TCP result in view: %s", view)
	}

	// Error state renders timeout clearly.
	alphaKey := serverProbeKey(cfg.Servers[0])
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = next.(model)
	next, _ = m.Update(latencyResultMsg{key: alphaKey, result: latencyResult{Err: errors.New("timeout"), At: time.Now()}})
	m = next.(model)
	if !strings.Contains(m.View(), "超时 / 不可达") {
		t.Fatalf("expected unreachable status: %s", m.View())
	}

	// Archived server must not be probed.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = next.(model)
	if !m.filtered[m.cursor].Archived {
		t.Fatalf("expected archived server selected, got %+v", m.filtered[m.cursor])
	}
	if cmd := m.scheduleProbe(false); cmd != nil {
		t.Fatal("archived server should never schedule latency probe")
	}
	if !strings.Contains(m.View(), "已归档 · 不探测") {
		t.Fatalf("archived latency placeholder missing: %s", m.View())
	}
}

func TestProbeTCPAndLoopbackLatency(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	addr := netip.MustParseAddr("127.0.0.1")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rtt, err := probeTCPAddr(ctx, addr, port)
	if err != nil || rtt <= 0 {
		t.Fatalf("probeTCPAddr failed: rtt=%v err=%v", rtt, err)
	}

	res := probeServerLatency(ctx, "127.0.0.1", port)
	if res.Err != nil || (res.Method != "ICMP" && res.Method != "TCP") {
		t.Fatalf("probeServerLatency loopback failed: %+v", res)
	}

	// Closed port when calling probeTCPAddr directly returns error promptly.
	closedLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := closedLn.Addr().(*net.TCPAddr).Port
	_ = closedLn.Close()
	if _, err := probeTCPAddr(ctx, addr, closedPort); err == nil {
		t.Fatal("expected error probing closed TCP port")
	}
}

func TestNotesEditSaveCancelAndPersistence(t *testing.T) {
	raw := "# top comment\ndefault:\n  user: root\nservers:\n  - name: alpha\n    host: 192.0.2.10\n    password: keep-secret # inline comment\n  - name: beta\n    host: 192.0.2.20\n"
	home, path := configFixture(t, raw)
	t.Setenv("HOME", home)

	cfg, _, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}

	next, _ := initialModel(cfg, "").Update(tea.WindowSizeMsg{Width: 100, Height: 26})
	m := next.(model)

	// Enter Notes edit mode with Ctrl+E, type multi-line note, and save with Ctrl+S.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
	m = next.(model)
	if !m.editingNotes {
		t.Fatal("Ctrl+E did not enter notes edit mode")
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("第一行备注")})
	m = next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Line 2")})
	m = next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
	m = next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("备注")})
	m = next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = next.(model)

	if m.editingNotes || !strings.Contains(m.View(), "备注已保存") {
		t.Fatalf("notes did not save cleanly: editing=%v status=%q", m.editingNotes, m.status)
	}

	// Reload from disk to verify persistence across restarts, permissions, and untouched fields.
	reloaded, _, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Servers[0].Notes != "第一行备注\nLine 2 备注" || reloaded.Servers[0].Password != "keep-secret" || reloaded.Servers[1].Notes != "" {
		t.Fatalf("reloaded servers mismatch: %+v", reloaded.Servers)
	}
	diskBytes, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(diskBytes), "# top comment") || !strings.Contains(string(diskBytes), "# inline comment") {
		t.Fatalf("YAML comments lost after saving notes: %s (%v)", diskBytes, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("config permissions not 0600: %v (%v)", info, err)
	}

	// Esc during edit cancels without overwriting saved notes or quitting TUI.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyF4})
	m = next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("discard-me")})
	m = next.(model)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(model)
	if m.quitting || cmd != nil || m.editingNotes || m.filtered[0].Notes != "第一行备注\nLine 2 备注" {
		t.Fatalf("Esc in note editor did not cancel cleanly: quitting=%v editing=%v notes=%q", m.quitting, m.editingNotes, m.filtered[0].Notes)
	}

	// Clearing notes removes notes cleanly and persists empty state.
	m.editingNotes = true
	m.noteBuf = nil
	m.notePos = 0
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = next.(model)
	reloaded, _, err = loadConfig()
	if err != nil || reloaded.Servers[0].Notes != "" {
		t.Fatalf("clearing notes failed: %+v (%v)", reloaded, err)
	}
}

func TestNotesPreservesAliasesAndRejectsStaleOrControlConfig(t *testing.T) {
	raw := "servers:\n  - &base {name: node, host: 192.0.2.1, notes: shared}\n  - *base\n  - {<<: *base, name: merged}\n"
	home, path := configFixture(t, raw)
	t.Setenv("HOME", home)

	cfg, _, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	var aliasTarget ServerConfig
	for _, s := range cfg.Servers {
		if s.sourceIndex == 1 {
			aliasTarget = s
		}
	}
	cfg, err = setServerNotes(aliasTarget, "local override")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range cfg.Servers {
		want := "shared"
		if s.sourceIndex == 1 {
			want = "local override"
		}
		if s.Notes != want {
			t.Fatalf("alias isolation broken at sourceIndex=%d: got %q, want %q", s.sourceIndex, s.Notes, want)
		}
	}

	// Clearing an entry that inherits notes via << merge overrides locally to empty string.
	var mergedTarget ServerConfig
	for _, s := range cfg.Servers {
		if s.sourceIndex == 2 {
			mergedTarget = s
		}
	}
	cfg, err = setServerNotes(mergedTarget, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range cfg.Servers {
		if s.sourceIndex == 2 && s.Notes != "" {
			t.Fatalf("merged note clear failed: %+v", s)
		}
		if s.sourceIndex == 0 && s.Notes != "shared" {
			t.Fatalf("base anchor note mutated: %+v", s)
		}
	}

	// External concurrent edit must be rejected rather than overwritten.
	external := "servers: [{name: node, host: 192.0.2.99}]\n"
	if err := os.WriteFile(path, []byte(external), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := setServerNotes(cfg.Servers[0], "stale"); err == nil {
		t.Fatal("stale note save should be rejected")
	}
	if after, _ := os.ReadFile(path); string(after) != external {
		t.Fatalf("stale note save modified file: %s", after)
	}

	// Control characters (except newline/tab) in YAML notes must be rejected.
	if _, err := decodeConfig([]byte("servers:\n  - {name: bad, host: 192.0.2.1, notes: \"bad\\e[31m\"}\n")); err == nil {
		t.Fatal("ANSI escape in notes was accepted")
	}
}

func TestNotesEditorBoundsAcrossWindowSizes(t *testing.T) {
	cfg := &Config{Servers: []ServerConfig{
		{Name: "Oracle-伦敦", Group: "Default", Host: "192.0.2.1", Port: 22, User: "root", Auth: "key", Notes: "初始备注"},
	}}
	for _, size := range []tea.WindowSizeMsg{
		{Width: 120, Height: 32},
		{Width: 80, Height: 24},
		{Width: 80, Height: 16},
		{Width: 60, Height: 14},
		{Width: 40, Height: 10},
		{Width: 16, Height: 4},
	} {
		next, _ := initialModel(cfg, "").Update(size)
		m := next.(model)
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
		m = next.(model)
		for range 12 {
			next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("超长中文备注内容测试WidthBounds")})
			m = next.(model)
			next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
			m = next.(model)
		}
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
		m = next.(model)
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyHome})
		m = next.(model)
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnd})
		m = next.(model)

		view := m.View()
		if lipgloss.Width(view) > size.Width || lipgloss.Height(view) > size.Height {
			t.Fatalf("editor view %dx%d overflowed %dx%d", lipgloss.Width(view), lipgloss.Height(view), size.Width, size.Height)
		}
	}
}

func TestParseRemoteSystemInfo(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"Linux aarch64\nubuntu\n24.04\nUbuntu 24.04.1 LTS\n", "Ubuntu 24.04 · ARM64"},
		{"Linux x86_64\nubuntu\n22.04\nUbuntu 22.04.4 LTS\n", "Ubuntu 22.04 · AMD64"},
		{"Linux x86_64\ndebian\n12\nDebian GNU/Linux 12 (bookworm)\n", "Debian 12 · AMD64"},
		{"Linux aarch64\nalpine\n3.20.3\nAlpine Linux v3.20\n", "Alpine 3.20 · ARM64"},
		{"Linux aarch64\nol\n9.4\nOracle Linux Server 9.4\n", "Oracle Linux 9.4 · ARM64"},
		{"Linux x86_64\narch\n\nArch Linux\n", "Arch Linux · AMD64"},
		{"Darwin arm64\nmacos\n15.1\n\n", "macOS 15.1 · ARM64"},
		{"Linux x86_64\n\n\nCustomOS\x1b[31m 1.0 (codename)\n", "CustomOS[31m 1.0 · AMD64"},
	}
	for _, tc := range cases {
		if got := parseRemoteSystemInfo(tc.raw); got != tc.want {
			t.Errorf("parseRemoteSystemInfo(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestSystemInfoTUIProbePersistenceAndFilter(t *testing.T) {
	raw := "# keep header\nservers:\n  - name: arm-box\n    host: 192.0.2.10\n    password: keep-secret\n  - name: amd-box\n    host: 192.0.2.20\n    system: \"Debian 12 · AMD64\"\n"
	home, path := configFixture(t, raw)
	t.Setenv("HOME", home)

	cfg, _, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	next, _ := initialModel(cfg, "").Update(tea.WindowSizeMsg{Width: 108, Height: 26})
	m := next.(model)

	// amd-box comes first alphabetically ("amd-box" < "arm-box") and shows pre-saved OS + Arch.
	if !strings.Contains(m.View(), "Debian 12") || !strings.Contains(m.View(), "AMD64") {
		t.Fatalf("saved system field not rendered: %s", m.View())
	}

	// Select arm-box (uncollected) -> shows placeholder, then press Ctrl+O to probe asynchronously.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(model)
	if !strings.Contains(m.View(), "待采集") {
		t.Fatalf("expected uncollected placeholder on arm-box: %s", m.View())
	}

	m.sysProbeFn = func(_ context.Context, s ServerConfig, _ *Config) (string, error) {
		return "Ubuntu 24.04 · ARM64", nil
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	m = next.(model)
	if cmd == nil || !strings.Contains(m.View(), "采集中...") {
		t.Fatalf("Ctrl+O did not start non-blocking system probe: %s", m.View())
	}

	// Simulate user editing notes BEFORE async system probe finishes: saveServerSystem must not clobber notes!
	targetBefore := m.filtered[m.cursor]
	cfgWithNote, err := setServerNotes(targetBefore, "保留此备注")
	if err != nil {
		t.Fatal(err)
	}
	m.config = cfgWithNote
	m.applyFilter()

	resMsg := cmd()
	next, _ = m.Update(resMsg)
	m = next.(model)

	view := m.View()
	if !strings.Contains(view, "Ubuntu 24.04") || !strings.Contains(view, "ARM64") {
		t.Fatalf("probed system info missing from view: %s", view)
	}

	// Verify disk persistence, preserved notes, comments, and permissions.
	reloaded, _, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	armSrv := directMatch(reloaded.Servers, "arm-box")
	if armSrv == nil || armSrv.System != "Ubuntu 24.04 · ARM64" || armSrv.Notes != "保留此备注" || armSrv.Password != "keep-secret" {
		t.Fatalf("persisted server mismatch: %+v", armSrv)
	}
	diskBytes, _ := os.ReadFile(path)
	if !strings.Contains(string(diskBytes), "# keep header") {
		t.Fatalf("comment lost after system save: %s", diskBytes)
	}

	// Verify filtering by OS / architecture keyword.
	for _, r := range "arm64" {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(model)
	}
	if len(m.filtered) != 1 || m.filtered[0].Name != "arm-box" {
		t.Fatalf("system/arch filter failed: %+v", m.filtered)
	}
}

func TestFetchServerSystemInfoKeyAndAskpass(t *testing.T) {
	bin := t.TempDir()
	fakeSSH := filepath.Join(bin, "ssh")
	script := `#!/bin/sh
if [ "$SSH_ASKPASS_REQUIRE" = "force" ]; then
	pw=$("$SSH_ASKPASS" "root@host's password:") || exit 21
	[ "$pw" = "probe-pass-123" ] || exit 22
fi
printf 'Linux aarch64\nubuntu\n24.04\nUbuntu 24.04.1 LTS\n'
`
	if err := os.WriteFile(fakeSSH, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SSHTUI_TEST_PROCESS", "1")

	cfg := &Config{}
	cfg.Default.ServerAliveInterval = 15
	cfg.Default.ServerAliveCountMax = 30

	for _, s := range []ServerConfig{
		{Name: "key-node", Host: "192.0.2.1", Port: 22, User: "root", Auth: "key"},
		{Name: "pass-node", Host: "192.0.2.2", Port: 22, User: "root", Auth: "password", Password: "probe-pass-123"},
	} {
		got, err := fetchServerSystemInfo(context.Background(), s, cfg)
		if err != nil || got != "Ubuntu 24.04 · ARM64" {
			t.Fatalf("fetchServerSystemInfo(%s): got %q, err=%v", s.Auth, got, err)
		}
	}
}
