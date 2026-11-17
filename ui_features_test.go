package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
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
