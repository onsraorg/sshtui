package main

import (
	"strings"
	"testing"
)

func TestArchivedServersExcludedFromConsumers(t *testing.T) {
	cfg := &Config{Servers: []ServerConfig{
		{Name: "live", Group: "prod", Host: "192.0.2.1", User: "root", Port: 22},
		{Name: "retired", Group: "_meta", Host: "192.0.2.2", Auth: "password", Password: "archive-secret", Archived: true},
		{Name: "live", Group: "prod", Host: "192.0.2.3", Archived: true},
	}}
	jsonData, err := generateAnsibleJSON(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ini, err := generateAnsibleINI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{string(jsonData), ini} {
		if !strings.Contains(output, "192.0.2.1") {
			t.Fatal("active server missing from inventory")
		}
		for _, hidden := range []string{"retired", "192.0.2.2", "192.0.2.3", "archive-secret"} {
			if strings.Contains(output, hidden) {
				t.Fatalf("archived data leaked to inventory: %s", hidden)
			}
		}
	}
	if directMatch(cfg.Servers, "retired") != nil || directMatch(cfg.Servers, "ret") != nil {
		t.Fatal("archived server can be directly connected")
	}
	if target := directMatch(cfg.Servers, "live"); target == nil || target.Host != "192.0.2.1" {
		t.Fatal("archived duplicate interferes with active direct match")
	}
}

func TestCLIExcludesArchivedServers(t *testing.T) {
	t.Parallel()
	home, _ := configFixture(t, "servers:\n  - {name: live, host: 192.0.2.1}\n  - {name: retired, host: 192.0.2.2, archived: true, password: archive-secret}\n")
	for _, arg := range []string{"-l", "--ansible", "--list", "--ansible-ini"} {
		out, err := cliCommand(t, map[string]string{"HOME": home}, arg).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %s (%v)", arg, out, err)
		}
		if !strings.Contains(string(out), "live") || strings.Contains(string(out), "retired") || strings.Contains(string(out), "archive-secret") {
			t.Fatalf("%s did not exclude archived server: %s", arg, out)
		}
	}
}
