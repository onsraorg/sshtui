package main

import (
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestArchivePersistsOnlySelectedServer(t *testing.T) {
	raw := "# inventory comment\ndefault: {user: admin}\nservers:\n  - {name: z, host: 192.0.2.1, password: keep-secret} # keep comment\n  - {name: a, host: 192.0.2.2}\n  - {name: z, host: 192.0.2.3}\n"
	home, path := configFixture(t, raw)
	t.Setenv("HOME", home)
	cfg, _, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = setServerArchived(cfg.Servers[1], true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Servers[0].Archived || !cfg.Servers[1].Archived || cfg.Servers[2].Archived || cfg.Servers[1].Password != "keep-secret" {
		t.Fatalf("wrong server changed: %+v", cfg.Servers)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "# keep comment") || !strings.Contains(string(data), "# inventory comment") {
		t.Fatalf("comments lost: %s (%v)", data, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("unsafe config permissions: %v (%v)", info, err)
	}
	cfg, err = setServerArchived(cfg.Servers[1], false)
	if err != nil || cfg.Servers[1].Archived || cfg.Servers[1].Password != "keep-secret" {
		t.Fatalf("restore failed: %+v (%v)", cfg, err)
	}
}

func TestArchivePreservesAliasesAndMerges(t *testing.T) {
	for _, raw := range []string{
		"servers:\n  - &base {name: node, host: 192.0.2.1}\n  - *base\n  - {<<: *base, name: merged}\n",
		"servers:\n  - &base {name: node, host: 192.0.2.1, archived: false}\n  - *base\n  - {<<: *base, name: merged}\n",
	} {
		for _, index := range []int{0, 1, 2} {
			home, _ := configFixture(t, raw)
			t.Setenv("HOME", home)
			cfg, _, err := loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			var target ServerConfig
			for _, s := range cfg.Servers {
				if s.sourceIndex == index {
					target = s
				}
			}
			cfg, err = setServerArchived(target, true)
			if err != nil {
				t.Fatalf("archive alias index %d: %v", index, err)
			}
			for _, s := range cfg.Servers {
				if s.Archived != (s.sourceIndex == index) {
					t.Fatalf("archive affected another alias: index=%d server=%+v", index, s)
				}
				if s.sourceIndex == index {
					target = s
				}
			}
			cfg, err = setServerArchived(target, false)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range cfg.Servers {
				if s.Archived {
					t.Fatalf("alias did not restore: %+v", s)
				}
			}
		}
	}
}

func TestArchiveRejectsStaleSelection(t *testing.T) {
	home, path := configFixture(t, "servers: [{name: node, host: 192.0.2.1}]\n")
	t.Setenv("HOME", home)
	cfg, _, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	changed := "servers: [{name: node, host: 192.0.2.2}]\n"
	if err := os.WriteFile(path, []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	m := initialModel(cfg, "")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyF2})
	m = next.(model)
	if !strings.Contains(m.View(), "保存失败") || m.filtered[0].Archived {
		t.Fatal("failed save must remain visible without changing archive state")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != changed {
		t.Fatalf("stale save overwrote config: %s (%v)", data, err)
	}
}

func TestPickerArchiveAndRestore(t *testing.T) {
	for _, size := range []tea.WindowSizeMsg{{Width: 80, Height: 24}, {Width: 40, Height: 10}, {Width: 16, Height: 4}} {
		home, _ := configFixture(t, "servers: [{name: node, host: 192.0.2.1}]\n")
		t.Setenv("HOME", home)
		cfg, _, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		next, _ := initialModel(cfg, "").Update(size)
		m := next.(model)
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyF2})
		m = next.(model)
		if len(m.filtered) != 0 {
			t.Fatal("archived server remains in default picker")
		}
		cfg, _, err = loadConfig()
		if err != nil || !cfg.Servers[0].Archived || len(initialModel(cfg, "").filtered) != 0 {
			t.Fatal("archive not retained across restart")
		}
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyF3})
		m = next.(model)
		if len(m.filtered) != 1 || !strings.Contains(m.View(), "已归档") {
			t.Fatal("F3 did not reveal archived server")
		}
		next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		m = next.(model)
		if m.selected != nil || cmd != nil {
			t.Fatal("archived server must not connect before restoration")
		}
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyF2})
		m = next.(model)
		if len(m.filtered) != 1 || m.filtered[0].Archived {
			t.Fatal("F2 did not restore server")
		}
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyF3})
		m = next.(model)
		if len(m.filtered) != 1 || lipgloss.Width(m.View()) > size.Width || lipgloss.Height(m.View()) > size.Height {
			t.Fatal("restored picker lost server or exceeded terminal")
		}
	}
}
