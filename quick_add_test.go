package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestQuickAddYAMLLayouts(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":   "# keep me\nservers: []\ndefault: {user: admin, port: 2222}\n",
		"flow":    "# keep me\nservers: [{name: old, host: 192.0.2.1}]\ndefault: {port: 2222}\n",
		"missing": "# keep me\ndefault: {port: 2222}\n",
		"null":    "# keep me\nservers:\ndefault: {port: 2222}\n",
		"block":   "# keep me\nservers:\n  - name: old\n    host: 192.0.2.1\ndefault: {port: 2222}\n",
	} {
		t.Run(name, func(t *testing.T) {
			home, path := configFixture(t, raw)
			t.Setenv("HOME", home)
			if err := quickAddServer([]string{"[2001:db8::1234]", "password", "new"}); err != nil {
				t.Fatal(err)
			}
			cfg, _, err := loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			added := directMatch(cfg.Servers, "new")
			if added == nil || added.Host != "2001:db8::1234" || added.Port != 2222 || added.Password != "password" {
				t.Fatalf("wrong added server: %+v", added)
			}
			if strings.Contains(raw, "name: old") && directMatch(cfg.Servers, "old") == nil {
				t.Fatal("old server lost")
			}
			data, err := os.ReadFile(path)
			if err != nil || !strings.Contains(string(data), "# keep me") {
				t.Fatalf("comment lost: %s (%v)", data, err)
			}
		})
	}
}

func TestQuickAddRejectsMultipleDocuments(t *testing.T) {
	raw := "servers: []\n---\nservers: []\n"
	home, path := configFixture(t, raw)
	t.Setenv("HOME", home)
	if err := quickAddServer([]string{"192.0.2.2"}); err == nil {
		t.Fatal("multiple documents accepted")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != raw {
		t.Fatalf("rejected config changed: %q (%v)", data, err)
	}
}

func TestParseServerAddressBoundaries(t *testing.T) {
	for _, target := range []string{"2001:db8::1234", "[2001:db8::1234]"} {
		s, err := parseServerArgs([]string{target}, "admin", 2222)
		if err != nil || s.Host != "2001:db8::1234" || s.Port != 2222 {
			t.Fatalf("%s: %+v, %v", target, s, err)
		}
	}
	for _, target := range []string{"host:0", "host:65536", "host:abc", "host:", "[2001:db8::1]:0", "[bad]"} {
		if s, err := parseServerArgs([]string{target}, "root", 22); err == nil {
			t.Fatalf("invalid target %q accepted: %+v", target, s)
		}
	}
	s, err := parseServerArgs([]string{"host", "2200", "secret"}, "admin", 2222)
	if err != nil || s.Port != 2200 || s.Password != "secret" {
		t.Fatalf("explicit port not honored: %+v, %v", s, err)
	}
}

func TestQuickAddConcurrentProcesses(t *testing.T) {
	home, _ := configFixture(t, "servers: []\n")
	t.Setenv("HOME", home)
	const count = 8
	commands := make([]*exec.Cmd, count)
	for i := range commands {
		commands[i] = cliCommand(t, map[string]string{"HOME": home}, "add", fmt.Sprintf("192.0.2.%d", i+1))
		if err := commands[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for _, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	cfg, _, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	for i := range count {
		if directMatch(cfg.Servers, fmt.Sprintf("192.0.2.%d", i+1)) == nil {
			t.Fatalf("concurrent addition %d lost", i+1)
		}
	}
}

func TestQuickAddCLIUsageAndKeyDisplay(t *testing.T) {
	home, path := configFixture(t, "servers: []\n")
	if out, err := cliCommand(t, map[string]string{"HOME": home}, "add", "--help").CombinedOutput(); err != nil || !strings.Contains(string(out), "用法: s add") {
		t.Fatalf("s add --help failed: %v %s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "servers: []\n" {
		t.Fatalf("s add --help modified config: %q (%v)", data, err)
	}
	if out, err := cliCommand(t, map[string]string{"HOME": home}, "add", "--", "192.0.2.9", "22", "-secret").CombinedOutput(); err != nil {
		t.Fatalf("s add -- failed: %v %s", err, out)
	}
	t.Setenv("HOME", home)
	cfg, _, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if added := directMatch(cfg.Servers, "192.0.2.9"); added == nil || added.Password != "-secret" {
		t.Fatalf("s add -- saved wrong entry: %+v", cfg.Servers)
	}
	if out, err := cliCommand(t, map[string]string{"HOME": home}, "-a").CombinedOutput(); err == nil || !strings.Contains(string(out), "用法: s -a") {
		t.Fatalf("s -a usage mismatch: %v %s", err, out)
	}
	if out, err := cliCommand(t, map[string]string{"HOME": home}, "-e", "192.0.2.1", "22", "pass", "n", "Default", "extra").CombinedOutput(); err == nil || !strings.Contains(string(out), "参数过多") {
		t.Fatalf("s -e accepted excess args: %v %s", err, out)
	}
	if out, err := cliCommand(t, map[string]string{"HOME": home}, "-h").CombinedOutput(); err != nil || !strings.Contains(string(out), "add") || !strings.Contains(string(out), "run") || !strings.Contains(string(out), "[name] [group]") {
		t.Fatalf("s -h missing usage details: %v %s", err, out)
	}

	// Key auth display uses default.key when present and avoids dangling ": " when empty.
	out, err := cliCommand(t, map[string]string{"HOME": home}, "-e", "192.0.2.10").CombinedOutput()
	if err != nil || strings.Contains(string(out), "密钥认证:") {
		t.Fatalf("unexpected key description without default.key: %v %s", err, out)
	}
	homeDefaultKey, _ := configFixture(t, "default: {key: ~/.ssh/id_ed25519}\nservers: []\n")
	out, err = cliCommand(t, map[string]string{"HOME": homeDefaultKey}, "-e", "192.0.2.11").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "密钥认证: ~/.ssh/id_ed25519") {
		t.Fatalf("default.key not shown: %v %s", err, out)
	}

	t.Setenv("HOME", home)
	if err := os.WriteFile(path, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := editServersConfig(func(*Config, *yaml.Node) error { return nil }); err == nil {
		t.Fatal("empty config should return error, not succeed or panic")
	}
}
