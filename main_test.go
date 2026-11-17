package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

// Run this test binary as a local SSH substitute; never connect to real hosts.
func TestMain(m *testing.M) {
	if os.Getenv("SSHTUI_TEST_PROCESS") == "1" {
		switch filepath.Base(os.Args[0]) {
		case "osascript":
			if len(os.Args) == 6 {
				if delayMS, err := strconv.Atoi(os.Getenv("SSHTUI_TEST_OSASCRIPT_DELAY_MS")); err == nil && delayMS > 0 {
					time.Sleep(time.Duration(delayMS) * time.Millisecond)
				}
				writeTestRecord(".title-set", os.Args[4:])
				if os.Getenv("SSHTUI_TEST_TITLE_FAIL") == "1" {
					os.Exit(1)
				}
				if os.Getenv("SSHTUI_TEST_GHOSTTY_NOT_RUNNING") == "1" {
					os.Exit(0)
				}
				fmt.Print("test-terminal-id")
			} else {
				writeTestRecord(".title-reset", os.Args[4:])
			}
			os.Exit(0)
		case "ansible":
			writeTestRecord(".ansible", os.Args[1:])
			os.Exit(7)
		case "ssh":
			if mode := os.Getenv("SSHTUI_TEST_CONNECTING"); mode != "" {
				if mode == "failure" {
					time.Sleep(400 * time.Millisecond)
					os.Exit(255)
				}
				if mode == "slow-failure" {
					time.Sleep(700 * time.Millisecond)
					os.Exit(255)
				}
				time.Sleep(10 * time.Second)
				os.Exit(255)
			}
			if os.Getenv("SSHTUI_TEST_TTY_BG") == "1" {
				writeTestRecord(".tty", [3]bool{
					term.IsTerminal(int(os.Stdin.Fd())),
					term.IsTerminal(int(os.Stdout.Fd())),
					term.IsTerminal(int(os.Stderr.Fd())),
				})
				bg := exec.Command("/bin/sh", "-c", "sleep 3")
				bg.Stdout = os.Stdout
				bg.Stderr = os.Stderr
				if err := bg.Start(); err != nil {
					panic(err)
				}
				writeTestRecord(".bgpid", bg.Process.Pid)
				time.Sleep(120 * time.Millisecond)
				os.Exit(17)
			}
			for _, value := range append(os.Environ(), os.Args...) {
				if strings.Contains(value, "audit-private-marker") {
					panic("password leaked into subprocess arguments or environment")
				}
			}
			if os.Getenv("SSHTUI_TEST_BACKEND") == "askpass" {
				path := os.Getenv("SSHTUI_ASKPASS_SOCKET")
				info, err := os.Stat(filepath.Dir(path))
				if err != nil || info.Mode().Perm() != 0700 || os.Getenv("SSH_ASKPASS_REQUIRE") != "force" {
					panic("ASKPASS requires a private socket directory and force mode")
				}
				writeTestRecord(".socket", path)
			}
			writeTestRecord(".args", os.Args[1:])
			writeTestRecord(".term", os.Getenv("TERM"))
			if mode := os.Getenv("SSHTUI_TEST_SESSION"); mode != "" {
				if strings.HasPrefix(mode, "dirty") {
					if _, err := term.MakeRaw(int(os.Stdin.Fd())); err != nil {
						panic(err)
					}
					fmt.Print("\x1b[>7u\x1b[>7u")
					if mode == "dirty" {
						fmt.Print("\x1b[?1049h\x1b[>7u\x1b[>7u")
					}
					fmt.Print("\x1b[?1000h\x1b[?1006h\x1b[?2004h")
					os.Exit(0)
				}
				time.Sleep(300 * time.Millisecond)
				if _, err := term.MakeRaw(int(os.Stdin.Fd())); err != nil {
					panic(err)
				}
				reader := bufio.NewReader(os.Stdin)
				if mode == "password" {
					if os.Getenv("SSHTUI_TEST_BACKEND") == "askpass" {
						out, err := exec.Command(os.Getenv("SSH_ASKPASS"), "root@dummy's password:").Output()
						if err != nil {
							panic(err)
						}
						writeTestRecord(".password", strings.TrimSuffix(string(out), "\n"))
					} else {
						tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
						if err != nil {
							panic(err)
						}
						if _, err := term.MakeRaw(int(tty.Fd())); err != nil {
							panic(err)
						}
						fmt.Fprint(tty, "Password:")
						password, _ := bufio.NewReader(tty).ReadString('\n')
						tty.Close()
						writeTestRecord(".password", strings.TrimSuffix(password, "\n"))
					}
				}
				fmt.Print("\r\nREADY\r\n")
				for {
					command, err := reader.ReadString('\r')
					if err != nil {
						os.Exit(1)
					}
					switch strings.TrimSuffix(command, "\r") {
					case "size":
						size, _ := unix.IoctlGetWinsize(int(os.Stdin.Fd()), unix.TIOCGWINSZ)
						fmt.Printf("SIZE %d %d\r\n", size.Row, size.Col)
					case "again":
						fmt.Print("password:")
						password, _ := reader.ReadString('\r')
						writeTestRecord(".second-password", strings.TrimSuffix(password, "\r"))
						fmt.Print("MANUAL-OK\r\n")
					case "exit":
						os.Exit(17)
					case "interrupt":
						b, _ := reader.ReadByte()
						if b == 3 {
							fmt.Print("CTRL-C-OK\r\n")
							os.Exit(130)
						}
					}
				}
			}
			os.Exit(17)
		default:
			main()
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

func writeTestRecord(suffix string, value interface{}) {
	data, _ := json.Marshal(value)
	if err := os.WriteFile(os.Getenv("SSHTUI_TEST_LOG")+suffix, data, 0600); err != nil {
		panic(err)
	}
}

func configFixture(t *testing.T, contents string) (string, string) {
	t.Helper()
	home := t.TempDir()
	path := filepath.Join(home, ".config", "sshtui", "servers.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}
	return home, path
}

func testEnv(overrides map[string]string) []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[key]; replaced {
			continue
		}
		switch key {
		case "TERM_PROGRAM", "SSH_ASKPASS_PROMPT", "SSHTUI_ASKPASS_HELPER", "SSHTUI_ASKPASS_SOCKET":
			continue
		}
		env = append(env, entry)
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

func cliCommand(t *testing.T, env map[string]string, args ...string) *exec.Cmd {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, binary, args...)
	env["SSHTUI_TEST_PROCESS"] = "1"
	cmd.Env = testEnv(env)
	return cmd
}

func linkHelper(t *testing.T, dir, name, target string) {
	t.Helper()
	if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
}

func readRecord[T any](t *testing.T, path string) T {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestConfigPermissionsAndDefaults(t *testing.T) {
	home, path := configFixture(t, "servers:\n  - {name: node, host: 192.0.2.1}\n")
	t.Setenv("HOME", home)
	cfg, _, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 || cfg.Servers[0].User != "root" || cfg.Servers[0].Port != 22 || cfg.Default.ServerAliveInterval != 15 {
		t.Fatalf("permissions/defaults: mode=%o cfg=%+v", info.Mode().Perm(), cfg)
	}
	before, _ := os.ReadFile(path)
	if err := createDefaultConfig(path); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("existing config was overwritten")
	}
}

func TestDefaultConfigCreationIsConcurrentSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".config", "sshtui", "servers.yaml")
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- createDefaultConfig(path)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var cfg Config
	if err := yaml.NewDecoder(f).Decode(&cfg); err != nil || len(cfg.Servers) != 3 {
		t.Fatalf("default config is incomplete: servers=%d, err=%v", len(cfg.Servers), err)
	}
}

func TestVersionWorksWithoutValidConfig(t *testing.T) {
	t.Parallel()
	home, _ := configFixture(t, "servers: [broken\n")
	for _, arg := range []string{"--version", "-v"} {
		out, err := cliCommand(t, map[string]string{"HOME": home}, arg).CombinedOutput()
		if err != nil || string(out) != "sshtui "+strings.TrimSpace(version)+"\n" {
			t.Fatalf("%s: got %q, %v", arg, out, err)
		}
	}
}

func TestConfigRejectsUnusableCredentials(t *testing.T) {
	for _, password := range []string{"line\nline", "line\rline", "nul\x00byte", strings.Repeat("x", 1024)} {
		t.Run(fmt.Sprintf("bytes-%d", len(password)), func(t *testing.T) {
			data, _ := yaml.Marshal(Config{Servers: []ServerConfig{{Name: "node", Host: "192.0.2.1", Auth: "password", Password: password}}})
			home, _ := configFixture(t, string(data))
			t.Setenv("HOME", home)
			if _, _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "密码") {
				t.Fatalf("invalid credential was accepted or rejected for wrong reason: %v", err)
			}
		})
	}
}

func TestConfigRejectsUnknownAndControlFields(t *testing.T) {
	control, _ := yaml.Marshal(Config{Servers: []ServerConfig{{Name: "node\x1b]52;c;bad\a", Host: "192.0.2.1"}}})
	for name, contents := range map[string]string{
		"unknown": "servers:\n  - {name: node, host: 192.0.2.1, uesr: deploy}\n",
		"control": string(control),
	} {
		t.Run(name, func(t *testing.T) {
			home, _ := configFixture(t, contents)
			t.Setenv("HOME", home)
			if _, _, err := loadConfig(); err == nil || strings.Contains(err.Error(), "port") {
				t.Fatalf("unsafe config was accepted or rejected for wrong reason: %v", err)
			}
		})
	}
}

func TestConfigPortZeroDefaultValidationAndGroupNormalization(t *testing.T) {
	for name, tc := range map[string]struct {
		raw     string
		wantErr string
	}{
		"server-port-zero":           {"servers:\n  - {name: node, host: 192.0.2.1, port: 0}\n", "1–65535"},
		"server-port-hex-zero":       {"servers:\n  - {name: node, host: 192.0.2.1, port: 0x0}\n", "1–65535"},
		"server-merge-zero":          {"servers:\n  - &base {name: base, host: 192.0.2.1, port: 0}\n  - {<<: *base, name: child}\n", "1–65535"},
		"server-merge-override-zero": {"servers:\n  - &base {name: base, host: 192.0.2.1, port: 2222}\n  - {<<: *base, name: child, port: 0}\n", "1–65535"},
		"default-port-zero":          {"default: {port: 0}\nservers: []\n", "default.port"},
		"default-port-too-high":      {"default: {port: 70000}\nservers: []\n", "default.port"},
		"default-port-negative":      {"default: {port: -1}\nservers: []\n", "default.port"},
		"default-user-control":       {"default: {user: \"root\\e\"}\nservers: []\n", "default.user"},
		"default-key-nul":            {"default: {key: \"k\\0\"}\nservers: []\n", "NUL"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeConfig([]byte(tc.raw)); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("config %q: got err=%v, want %q", name, err, tc.wantErr)
			}
		})
	}

	cfg, err := decodeConfig([]byte("default: {port: 2222}\nservers:\n  - {name: node, host: 192.0.2.1, group: \"   \"}\n"))
	if err != nil {
		t.Fatalf("valid omitted port failed: %v", err)
	}
	if cfg.Servers[0].Port != 2222 || cfg.Servers[0].Group != "Default" || inventoryGroup(cfg.Servers[0]) != "Default" {
		t.Fatalf("unexpected normalization: %+v", cfg.Servers[0])
	}
	if got := inventoryGroup(ServerConfig{Group: "   "}); got != "Default" {
		t.Fatalf("inventoryGroup blank = %q, want Default", got)
	}
	jsonOut, err := generateAnsibleJSON(cfg)
	if err != nil || !strings.Contains(string(jsonOut), `"Default"`) || strings.Contains(string(jsonOut), "ungrouped") {
		t.Fatalf("JSON group mismatch: %s (%v)", jsonOut, err)
	}
}

func TestIPv6EndpointFormattingAcrossViewsAndCLI(t *testing.T) {
	cfg := &Config{Servers: []ServerConfig{{Name: "v6", Group: "Default", Host: "2001:db8::1", User: "u", Port: 2222, Auth: "key"}}}
	for _, size := range []tea.WindowSizeMsg{{Width: 80, Height: 24}, {Width: 40, Height: 10}} {
		next, _ := initialModel(cfg, "").Update(size)
		view := next.(model).View()
		if !strings.Contains(view, "u@[2001:db8::1]:2222") {
			t.Fatalf("view %+v missing bracketed IPv6 endpoint: %q", size, view)
		}
	}

	home, _ := configFixture(t, "servers:\n  - {name: v6, host: 2001:db8::1, user: u, port: 2222}\n")
	listOut, err := cliCommand(t, map[string]string{"HOME": home}, "-l").CombinedOutput()
	if err != nil || !strings.Contains(string(listOut), "u@[2001:db8::1]:2222") {
		t.Fatalf("-l missing bracketed IPv6 endpoint: %v %q", err, listOut)
	}

	bin := t.TempDir()
	binary, _ := os.Executable()
	linkHelper(t, bin, "ssh", binary)
	log := filepath.Join(t.TempDir(), "log")
	connOut, err := cliCommand(t, map[string]string{"HOME": home, "PATH": bin, "SSHTUI_TEST_LOG": log}, "v6").CombinedOutput()
	if err == nil || !strings.Contains(string(connOut), "(u@[2001:db8::1]:2222)") {
		t.Fatalf("connectToServer missing bracketed IPv6 endpoint: %v %q", err, connOut)
	}
}

func TestEditorAndPathWorkWithBrokenYAML(t *testing.T) {
	t.Parallel()
	home, path := configFixture(t, "servers: [broken\n")
	bin := t.TempDir()
	editor := filepath.Join(bin, "editor with spaces")
	// Simulate an editor that saves by replacing the inode with a 0644 file.
	script := "#!/bin/sh\n[ \"$1\" = --wait ] || exit 2\nprintf 'servers: []\\n' > \"$2.new\"\nchmod 644 \"$2.new\"\nmv \"$2.new\" \"$2\"\n"
	if err := os.WriteFile(editor, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"HOME": home, "EDITOR": shellQuote(editor) + " --wait"}
	if out, err := cliCommand(t, env, "-p").CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != path {
		t.Fatalf("path: %s %v", out, err)
	}
	if out, err := cliCommand(t, env, "-e").CombinedOutput(); err != nil {
		t.Fatalf("editor: %s %v", out, err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("editor left permissions %o", info.Mode().Perm())
	}
}

func TestSplitEditor(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  []string
	}{
		{`code --wait`, []string{"code", "--wait"}},
		{`'/path with spaces/editor' -u "a b" ''`, []string{"/path with spaces/editor", "-u", "a b", ""}},
		{`editor a\ b "c\q"`, []string{"editor", "a b", `c\q`}},
		{`editor '$(touch marker)'`, []string{"editor", "$(touch marker)"}},
	} {
		got, err := splitEditor(tc.input)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: got %q, %v; want %q", tc.input, got, err, tc.want)
		}
	}
	for _, input := range []string{"", `editor 'bad`, `editor bad\`} {
		if _, err := splitEditor(input); err == nil {
			t.Errorf("accepted malformed command %q", input)
		}
	}
}

func TestMissingAnsibleReportsError(t *testing.T) {
	out, err := cliCommand(t, map[string]string{"PATH": t.TempDir()}, "run", "uptime").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "未找到 ansible") {
		t.Fatalf("got %q, %v", out, err)
	}
}

func TestAnsibleInvocation(t *testing.T) {
	bin := t.TempDir()
	binary, _ := os.Executable()
	linkHelper(t, bin, "ansible", binary)
	log := filepath.Join(t.TempDir(), "log")
	out, err := cliCommand(t, map[string]string{"PATH": bin, "SSHTUI_TEST_LOG": log}, "run", "-g", "Prod", "printf 'hello world'").CombinedOutput()
	if err == nil || err.(*exec.ExitError).ExitCode() != 7 {
		t.Fatalf("Ansible exit code was not preserved: %v %s", err, out)
	}
	want := []string{"Prod", "-i", binary, "-m", "shell", "-a", "printf 'hello world'"}
	if got := readRecord[[]string](t, log+".ansible"); !slices.Equal(got, want) {
		t.Fatalf("Ansible arguments: got %q, want %q", got, want)
	}
}

func TestInventoryRejectsCollisions(t *testing.T) {
	for _, cfg := range []*Config{
		{Servers: []ServerConfig{{Name: "same", Group: "Prod", Host: "192.0.2.1"}, {Name: " same ", Group: "Dev", Host: "192.0.2.2"}}},
		{Servers: []ServerConfig{{Group: "Dev", Host: "node"}, {Name: "node", Group: "Prod", Host: "192.0.2.2"}}},
		{Servers: []ServerConfig{{Name: "node", Group: "_meta"}}},
		{Servers: []ServerConfig{{Name: "node", Group: "all"}}},
	} {
		if _, err := generateAnsibleJSON(cfg); err == nil {
			t.Fatal("JSON accepted an ambiguous inventory")
		}
		if _, err := generateAnsibleINI(cfg); err == nil {
			t.Fatal("INI accepted an ambiguous inventory")
		}
	}
}

func TestInventoryINIRejectsGroupWhitespace(t *testing.T) {
	cfg := &Config{Servers: []ServerConfig{{Name: "node", Group: "my group", Host: "192.0.2.1"}}}
	if _, err := generateAnsibleJSON(cfg); err != nil {
		t.Fatalf("JSON should accept spaced group name: %v", err)
	}
	if _, err := generateAnsibleINI(cfg); err == nil || !strings.Contains(err.Error(), "--ansible") {
		t.Fatalf("INI should reject spaced group name: %v", err)
	}
}

func TestInventoryINIRoundTrip(t *testing.T) {
	ansible, err := exec.LookPath("ansible-inventory")
	if err != nil {
		t.Skip("ansible-inventory is not installed")
	}
	cfg := &Config{}
	cfg.Default.ServerAliveInterval, cfg.Default.ServerAliveCountMax = 30, 3
	for i, password := range []string{`ab"cd`, `a'b\c $[x];#`, "12345", "True", "None", "中文🔑"} {
		cfg.Servers = append(cfg.Servers, ServerConfig{Name: fmt.Sprintf("password-node-%d", i), Host: "192.0.2.1", Group: "production", User: "root", Port: 22, Auth: "password", Password: password, Args: []string{"-J", "jump", "-o", "ProxyCommand=ssh -W %h:%p jump"}})
	}
	cfg.Servers = append(cfg.Servers, ServerConfig{Name: "key node", Host: "192.0.2.2", Group: "Dev", User: "admin", Port: 2222, Auth: "key", Key: filepath.Join(t.TempDir(), `key 'with spaces'`)})
	ini, err := generateAnsibleINI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts.ini")
	if err := os.WriteFile(path, []byte(ini), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ansible, "-i", path, "--list")
	cmd.Env = testEnv(map[string]string{"HOME": dir, "ANSIBLE_LOCAL_TEMP": filepath.Join(dir, "ansible-tmp"), "ANSIBLE_NOCOLOR": "1"})
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ansible: %v", err)
	}
	var actual struct {
		Meta struct {
			Hostvars map[string]map[string]interface{} `json:"hostvars"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(out, &actual); err != nil {
		t.Fatal(err)
	}
	for _, s := range cfg.Servers {
		vars := actual.Meta.Hostvars[s.Name]
		if vars["ansible_host"] != s.Host || vars["ansible_port"] != float64(s.Port) || vars["ansible_user"] != s.User {
			t.Errorf("connection target did not round trip for %s: %#v", s.Name, vars)
		}
		if s.Auth == "password" && (vars["ansible_password"] != s.Password || vars["ansible_ssh_pass"] != s.Password) {
			t.Errorf("password did not round trip as a string for %s", s.Name)
		}
		if s.Auth == "key" && vars["ansible_ssh_private_key_file"] != s.Key {
			t.Errorf("key path did not round trip for %s", s.Name)
		}
		common, _ := vars["ansible_ssh_common_args"].(string)
		args, err := splitEditor(common)
		if err != nil || len(args) < len(s.Args) || !slices.Equal(args[:len(s.Args)], s.Args) {
			t.Errorf("SSH arguments did not round trip for %s: %q", s.Name, args)
		}
	}
	data, err := generateAnsibleJSON(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var dynamic struct {
		Meta struct {
			Hostvars map[string]map[string]interface{} `json:"hostvars"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(data, &dynamic); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(dynamic.Meta.Hostvars, actual.Meta.Hostvars) {
		t.Fatal("JSON and INI disagree on connection settings")
	}
}

func TestSSHOptionsRespectOverrides(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("ssh is not installed")
	}
	cfg := &Config{}
	cfg.Default.ServerAliveInterval, cfg.Default.ServerAliveCountMax = 30, 3
	s := ServerConfig{Args: []string{"-o", "ServerAliveInterval=60", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=25", "-o", "ConnectionAttempts=2", "-J", "jump"}}
	args := append([]string{"-G", "-F", "/dev/null"}, sshOptions(&s, cfg)...)
	out, err := exec.Command(ssh, append(args, "dummy.invalid")...).Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"serveraliveinterval 60\n", "stricthostkeychecking true\n", "connecttimeout 25\n", "connectionattempts 2\n", "proxyjump jump\n", "tcpkeepalive no\n", "ipqos none none\n"} {
		if !strings.Contains(string(out), expected) {
			t.Errorf("effective options missing %q", expected)
		}
	}
}

func TestSSHHandshakeTimeout(t *testing.T) {
	t.Parallel()
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("ssh is not installed")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		accepted <- err
		if err == nil {
			defer conn.Close()
			// Accept TCP but never send an SSH banner, simulating a stalled server.
			_, _ = io.Copy(io.Discard, conn)
		}
	}()
	home, _ := configFixture(t, fmt.Sprintf("servers:\n  - {name: node, host: 127.0.0.1, port: %d, args: ['-F', /dev/null]}\n", listener.Addr().(*net.TCPAddr).Port))
	bin := t.TempDir()
	linkHelper(t, bin, "ssh", ssh)
	binary, _ := os.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "node")
	cmd.Env = testEnv(map[string]string{"HOME": home, "PATH": bin, "SSHTUI_TEST_PROCESS": "1"})
	start := time.Now()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)
	if ctx.Err() != nil {
		t.Fatalf("stalled SSH connection did not time out: %v; output: %s", ctx.Err(), out)
	}
	if err == nil || err.(*exec.ExitError).ExitCode() != 255 || !strings.Contains(string(out), "Connection timed out") {
		t.Fatalf("expected SSH timeout and exit 255: %v; output: %s", err, out)
	}
	select {
	case err := <-accepted:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("SSH never reached the stalled server")
	}
	if elapsed < 9*time.Second || elapsed > 14*time.Second {
		t.Fatalf("expected default 10-second timeout, took %s", elapsed)
	}
	t.Logf("stalled handshake timed out after %s with SSH exit 255", elapsed)
}

func TestDirectMatch(t *testing.T) {
	servers := []ServerConfig{{Name: "Prod"}, {Name: "Prod DB"}, {Name: "Dev"}}
	for _, tc := range []struct{ term, want string }{{"prod", "Prod"}, {"DB", "Prod DB"}, {"no-match", ""}} {
		got := directMatch(servers, tc.term)
		if (got == nil) != (tc.want == "") || (got != nil && got.Name != tc.want) {
			t.Errorf("direct match %q: %+v", tc.term, got)
		}
	}
	servers = append(servers, ServerConfig{Name: "Prod"})
	if directMatch(servers, "Prod") != nil || directMatch(servers, "Pro") != nil {
		t.Fatal("ambiguous names must open the picker")
	}
}

func TestExtraFilterArgumentsAreRejected(t *testing.T) {
	home, _ := configFixture(t, "servers:\n  - {name: prod, host: 192.0.2.1}\n  - {name: 'prod db', host: 192.0.2.2}\n")
	out, err := cliCommand(t, map[string]string{"HOME": home}, "prod", "db").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "名称含空格时请使用引号") {
		t.Fatalf("extra filter argument was ignored: %v; output: %s", err, out)
	}
}

func TestViewFitsWindowAndKeepsSelectionVisible(t *testing.T) {
	cfg := &Config{}
	for i := 0; i < 30; i++ {
		cfg.Servers = append(cfg.Servers, ServerConfig{Name: fmt.Sprintf("server-%02d", i), Group: strings.Repeat("长分组", 20), Host: "192.0.2.1", User: "root", Port: 22})
	}
	for _, size := range []tea.WindowSizeMsg{{Width: 80, Height: 24}, {Width: 40, Height: 24}, {Width: 80, Height: 10}, {Width: 16, Height: 4}, {Width: 8, Height: 2}} {
		for _, filter := range []string{"", strings.Repeat("search", 100)} {
			next, _ := initialModel(cfg, filter).Update(size)
			m := next.(model)
			for step := 0; step < 32; step++ {
				view := m.View()
				if lipgloss.Width(view) > size.Width || lipgloss.Height(view) > size.Height {
					t.Fatalf("view %dx%d exceeds window %dx%d", lipgloss.Width(view), lipgloss.Height(view), size.Width, size.Height)
				}
				if size.Width >= 40 && filter == "" && !strings.Contains(view, m.filtered[m.cursor].Name) {
					t.Fatal("selected server is outside the viewport")
				}
				next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
				m = next.(model)
			}
		}
	}
}

func TestPickerShowsAllServersWhenTheyFit(t *testing.T) {
	for _, tc := range []struct {
		count, width, height int
	}{
		{24, 80, 24},
		{30, 80, 24},
		{42, 80, 24},
		{50, 120, 24},
		{30, 80, 45},
		{30, 40, 40},
	} {
		t.Run(fmt.Sprintf("%d_servers_%dx%d", tc.count, tc.width, tc.height), func(t *testing.T) {
			cfg := &Config{}
			for i := range tc.count {
				cfg.Servers = append(cfg.Servers, ServerConfig{Name: fmt.Sprintf("node-%02d", i), Host: "192.0.2.1", User: "root", Port: 22})
			}
			next, _ := initialModel(cfg, "").Update(tea.WindowSizeMsg{Width: tc.width, Height: tc.height})
			m := next.(model)
			for range tc.count + 1 {
				view := m.View()
				if lipgloss.Width(view) > tc.width || lipgloss.Height(view) > tc.height {
					t.Fatalf("view exceeds window: %dx%d", lipgloss.Width(view), lipgloss.Height(view))
				}
				for _, s := range cfg.Servers {
					if !strings.Contains(view, s.Name) {
						t.Fatalf("server %s hidden while all servers fit", s.Name)
					}
				}
				next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
				m = next.(model)
			}
		})
	}
}

func TestPickerColumnNavigationAndResize(t *testing.T) {
	cfg := &Config{}
	for i := range 50 {
		cfg.Servers = append(cfg.Servers, ServerConfig{Name: fmt.Sprintf("node-%02d", i), Host: "192.0.2.1", User: "root", Port: 22})
	}
	next, _ := initialModel(cfg, "").Update(tea.WindowSizeMsg{Width: 120, Height: 24})
	m := next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = next.(model)
	if m.filtered[m.cursor].Name != "node-21" {
		t.Fatalf("right did not select adjacent column: %s", m.filtered[m.cursor].Name)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m = next.(model)
	if m.cursor != 0 {
		t.Fatalf("left did not return to first column: %d", m.cursor)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = next.(model)
	for _, size := range []tea.WindowSizeMsg{{Width: 40, Height: 10}, {Width: 80, Height: 24}, {Width: 120, Height: 24}} {
		next, _ = m.Update(size)
		m = next.(model)
		view := m.View()
		if !strings.Contains(view, "node-49") || lipgloss.Width(view) > size.Width || lipgloss.Height(view) > size.Height {
			t.Fatalf("resize lost selection or overflowed window %dx%d: %s", size.Width, size.Height, view)
		}
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("node-49")})
	m = next.(model)
	if len(m.filtered) != 1 || !strings.Contains(m.View(), "node-49") {
		t.Fatal("filter did not retain matching server")
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if m.selected == nil || m.selected.Name != "node-49" {
		t.Fatal("enter did not select the filtered server")
	}
}

func TestPickerFilterDisplaySpaceAndViewportClamping(t *testing.T) {
	cfg := &Config{}
	for i := range 30 {
		cfg.Servers = append(cfg.Servers, ServerConfig{Name: fmt.Sprintf("node-%02d", i), Host: "192.0.2.1", User: "root", Port: 22})
	}
	cfg.Servers = append(cfg.Servers, ServerConfig{Name: "prod db", Host: "192.0.2.99", User: "root", Port: 22})

	for _, size := range []tea.WindowSizeMsg{{Width: 80, Height: 24}, {Width: 40, Height: 10}} {
		next, _ := initialModel(cfg, "").Update(size)
		m := next.(model)
		if strings.Contains(m.View(), "Filter: …") || strings.Contains(m.View(), "❯ …") {
			t.Fatalf("empty filter rendered ellipsis at %+v: %q", size, m.View())
		}
		for _, r := range "prod" {
			next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
			m = next.(model)
		}
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
		m = next.(model)
		for _, r := range "db" {
			next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
			m = next.(model)
		}
		view := m.View()
		if m.filter != "prod db" || !strings.Contains(view, "prod db") || len(m.filtered) != 1 {
			t.Fatalf("space filter failed at %+v: filter=%q filtered=%d view=%q", size, m.filter, len(m.filtered), view)
		}
	}

	// Long and control-laden filters must keep trailing text and stay within terminal bounds.
	longFilter := strings.Repeat("a", 1500) + "TAIL"
	next, _ := initialModel(cfg, "prod\ndb\x1b[31m").Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m := next.(model)
	if m.filter != "prod db[31m" || lipgloss.Height(m.View()) > 24 {
		t.Fatalf("control filter not sanitized: filter=%q height=%d", m.filter, lipgloss.Height(m.View()))
	}
	m.filter = longFilter
	m.applyFilter()
	view := m.View()
	if !strings.Contains(view, "…") || !strings.Contains(view, "TAIL") || lipgloss.Width(view) > 80 || lipgloss.Height(view) > 24 {
		t.Fatalf("long filter overflowed or lost tail: %dx%d", lipgloss.Width(view), lipgloss.Height(view))
	}
	for _, tc := range []struct {
		in   string
		w    int
		want string
	}{
		{"", 56, ""},
		{"prod", 56, "prod"},
		{"中文测试", 5, "…测试"},
		{"a👨‍👩‍👧b", 3, "…b"},
		{"abc", 1, "…"},
		{"abc", 0, ""},
	} {
		if got := truncateLeft(tc.in, tc.w); got != tc.want {
			t.Fatalf("truncateLeft(%q, %d) = %q, want %q", tc.in, tc.w, got, tc.want)
		}
	}

	// Viewport top must clamp when filtered results shrink or terminal height grows.
	next, _ = initialModel(cfg, "").Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	m = next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = next.(model)
	for _, r := range "node-1" {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(model)
	}
	if m.viewportTop != 3 || !strings.Contains(m.View(), "node-13") || !strings.Contains(m.View(), "node-19") {
		t.Fatalf("shrunk filter did not fill viewport: top=%d view=%q", m.viewportTop, m.View())
	}

	next, _ = initialModel(cfg, "").Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	m = next.(model)
	for range 23 {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = next.(model)
	}
	next, _ = m.Update(tea.WindowSizeMsg{Width: 40, Height: 24})
	m = next.(model)
	if m.viewportTop != 10 || !strings.Contains(m.View(), "node-10") {
		t.Fatalf("resized taller viewport left blank rows: top=%d view=%q", m.viewportTop, m.View())
	}
}

func TestSSHInvocation(t *testing.T) {
	for _, backend := range []string{"key", "askpass"} {
		t.Run(backend, func(t *testing.T) {
			auth := "password"
			if backend == "key" {
				auth = "key"
			}
			home, _ := configFixture(t, "servers:\n  - {name: node, host: 192.0.2.1, auth: "+auth+", password: dummy-secret, key: '~/.ssh/key', args: ['-J', jump]}\n")
			bin := t.TempDir()
			binary, _ := os.Executable()
			linkHelper(t, bin, "ssh", binary)
			log := filepath.Join(t.TempDir(), "log")
			out, err := cliCommand(t, map[string]string{"HOME": home, "PATH": bin, "SSHTUI_TEST_LOG": log}, "node").CombinedOutput()
			if err == nil || err.(*exec.ExitError).ExitCode() != 17 {
				t.Fatalf("SSH exit code was not preserved: %v %s", err, out)
			}
			if strings.Contains(string(out), "\x1b[") || strings.Contains(string(out), "正在连接") {
				t.Fatalf("non-terminal output contains animation: %q", out)
			}
			args := readRecord[[]string](t, log+".args")
			if !strings.Contains(strings.Join(args, " "), "-J jump") || readRecord[string](t, log+".term") != "xterm-256color" {
				t.Fatalf("SSH arguments/environment: %q", args)
			}
			for _, option := range []string{"ConnectTimeout=10", "ConnectionAttempts=1"} {
				if !slices.Contains(args, option) {
					t.Errorf("SSH invocation missing %s: %q", option, args)
				}
			}
		})
	}
}

func TestConnectionProgressFailureAndCancel(t *testing.T) {
	t.Parallel()
	expect, err := exec.LookPath("expect")
	if err != nil {
		t.Skip("expect is not installed")
	}
	for _, mode := range []string{"failure", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			home, _ := configFixture(t, "servers:\n  - {name: node, host: 192.0.2.1, auth: key}\n")
			bin := t.TempDir()
			binary, _ := os.Executable()
			linkHelper(t, bin, "ssh", binary)
			driver := `
set timeout 4
spawn -noecho $env(SSHTUI_TEST_BINARY) node
expect {
    -re {0\.[1-9]s} {}
    timeout { puts stderr "No waiting animation"; exit 101 }
    eof { exit 102 }
}
if {$env(SSHTUI_TEST_CONNECTING) eq "cancel"} { send -- "\003" }
expect {
    eof {}
    timeout { puts stderr "SSH failed to exit"; exit 103 }
}
set result [wait]
exit [lindex $result 3]
`
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, expect, "-c", driver)
			cmd.Env = testEnv(map[string]string{"HOME": home, "PATH": bin, "TERM": "xterm-256color", "SSHTUI_TEST_PROCESS": "1", "SSHTUI_TEST_BINARY": binary, "SSHTUI_TEST_CONNECTING": mode})
			out, err := cmd.CombinedOutput()
			wantExit := 255
			if mode == "cancel" {
				wantExit = 130
			}
			if ctx.Err() != nil || err == nil || err.(*exec.ExitError).ExitCode() != wantExit {
				t.Fatalf("connection %s: %v; output: %s", mode, err, out)
			}
			if !strings.Contains(string(out), "\r\x1b[2K") {
				t.Fatalf("connection animation was not cleared: %q", out)
			}
		})
	}
}

func TestRunSSHPreservesTTYAndDoesNotWaitOnBackgroundChild(t *testing.T) {
	t.Parallel()
	expect, err := exec.LookPath("expect")
	if err != nil {
		t.Skip("expect is not installed")
	}
	home, _ := configFixture(t, "servers:\n  - {name: node, host: 192.0.2.1, auth: key}\n")
	bin := t.TempDir()
	binary, _ := os.Executable()
	linkHelper(t, bin, "ssh", binary)
	log := filepath.Join(t.TempDir(), "log")
	t.Cleanup(func() {
		if data, err := os.ReadFile(log + ".bgpid"); err == nil {
			var pid int
			if json.Unmarshal(data, &pid) == nil && pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	driver := `
set timeout 2
spawn -noecho /bin/sh -c {"$SSHTUI_TEST_BINARY" node; echo "DONE:$?"}
expect {
    "DONE:17" {}
    timeout { puts stderr "runSSH stalled waiting on backgrounded child"; exit 101 }
    eof { exit 102 }
}
`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, expect, "-c", driver)
	cmd.Env = testEnv(map[string]string{
		"HOME": home, "PATH": bin + string(os.PathListSeparator) + os.Getenv("PATH"), "TERM": "xterm-256color",
		"SSHTUI_TEST_PROCESS": "1", "SSHTUI_TEST_BINARY": binary,
		"SSHTUI_TEST_LOG": log, "SSHTUI_TEST_TTY_BG": "1",
	})
	start := time.Now()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)
	if ctx.Err() != nil || err != nil {
		t.Fatalf("TTY/background child check failed: %v (%v); output: %q", err, ctx.Err(), out)
	}
	if elapsed >= 1500*time.Millisecond {
		t.Fatalf("runSSH blocked waiting on backgrounded child: took %s", elapsed)
	}
	if tty := readRecord[[3]bool](t, log+".tty"); tty != [3]bool{true, true, true} {
		t.Fatalf("child stdio TTY flags = %v, want [true true true]", tty)
	}
}

func TestSSHRestoresTerminalAfterDirtyChild(t *testing.T) {
	t.Parallel()
	expect, err := exec.LookPath("expect")
	if err != nil {
		t.Skip("expect is not installed")
	}
	for _, tc := range []struct {
		mode     string
		redirect bool
	}{{"dirty", false}, {"dirty", true}, {"dirty-main", false}, {"dirty-main", true}} {
		t.Run(fmt.Sprintf("%s/redirect-stderr=%t", tc.mode, tc.redirect), func(t *testing.T) {
			home, _ := configFixture(t, "servers:\n  - {name: node, host: 192.0.2.1, auth: key}\n")
			bin := t.TempDir()
			binary, _ := os.Executable()
			linkHelper(t, bin, "ssh", binary)
			stty, err := exec.LookPath("stty")
			if err != nil {
				t.Fatal(err)
			}
			linkHelper(t, bin, "stty", stty)
			driver := `
set timeout 4
spawn -noecho /bin/sh -c {if [ "$SSHTUI_TEST_REDIRECT" = 1 ]; then "$SSHTUI_TEST_BINARY" node 2>"$SSHTUI_TEST_STDERR"; else "$SSHTUI_TEST_BINARY" node; fi; state=$(stty -a); case " $state " in *" -icanon "*|*" -echo "*) exit 102;; esac}
expect {
    eof {}
    timeout { puts stderr "SSH did not exit"; exit 101 }
}
set result [wait]
exit [lindex $result 3]
`
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, expect, "-c", driver)
			env := map[string]string{"HOME": home, "PATH": bin, "TERM": "xterm-256color", "SSHTUI_TEST_PROCESS": "1", "SSHTUI_TEST_BINARY": binary, "SSHTUI_TEST_LOG": filepath.Join(t.TempDir(), "log"), "SSHTUI_TEST_SESSION": tc.mode, "SSHTUI_TEST_STDERR": filepath.Join(t.TempDir(), "stderr")}
			if tc.redirect {
				env["SSHTUI_TEST_REDIRECT"] = "1"
			}
			cmd.Env = testEnv(env)
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil || err != nil {
				t.Fatalf("dirty SSH terminal cleanup: %v; output: %q", err, out)
			}
			for _, sequence := range []string{"\x1b[?1049l", "\x1b[?1000l", "\x1b[?1006l", "\x1b[?2004l"} {
				if !strings.Contains(string(out), sequence) {
					t.Errorf("terminal cleanup missing %q: %q", sequence, out)
				}
			}
			// Interpret the actual output using Kitty's independent per-screen stacks.
			stacks := [2][]int{{0}, {0}}
			screen := 0
			for _, sequence := range regexp.MustCompile(`\x1b\[([<>?]?)([0-9]*)([uhl])`).FindAllStringSubmatch(string(out), -1) {
				prefix, parameter, action := sequence[1], sequence[2], sequence[3]
				if prefix == "?" && parameter == "1049" {
					if action == "h" {
						screen = 1
					} else if action == "l" {
						screen = 0
					}
				} else if action == "u" {
					n, _ := strconv.Atoi(parameter)
					switch prefix {
					case ">":
						stacks[screen] = append(stacks[screen], n)
					case "<":
						if parameter == "" {
							n = 1
						}
						depth := len(stacks[screen]) - n
						if depth <= 0 {
							stacks[screen] = []int{0}
						} else {
							stacks[screen] = stacks[screen][:depth]
						}
					}
				}
			}
			if screen != 0 || !slices.Equal(stacks[0], []int{0}) || !slices.Equal(stacks[1], []int{0}) {
				t.Errorf("keyboard mode left active after SSH: screen=%d stacks=%v", screen, stacks)
			}
		})
	}
}

func TestPasswordHelperInteractiveSession(t *testing.T) {
	t.Parallel()
	expect, err := exec.LookPath("expect")
	if err != nil {
		t.Skip("expect is not installed")
	}
	for _, tc := range []struct {
		backend, mode         string
		ghostty, titleFailure bool
	}{
		{"askpass", "password", false, false},
		{"askpass", "agent", false, false},
		{"key", "agent", true, false},
		{"askpass", "password", true, false},
		{"key", "agent", true, true},
	} {
		t.Run(fmt.Sprintf("%s/%s/ghostty=%t/failure=%t", tc.backend, tc.mode, tc.ghostty, tc.titleFailure), func(t *testing.T) {
			if tc.ghostty && runtime.GOOS != "darwin" {
				t.Skip("Ghostty tab integration is macOS-only")
			}
			mode := tc.mode
			marker := filepath.Join(t.TempDir(), "must-not-exist")
			password := `audit-private-marker中文$word"\[close [open ` + marker + ` w]];` + strings.Repeat("x", 600)
			cfg := Config{Servers: []ServerConfig{{Name: "node", Host: "192.0.2.1", Auth: "password", Password: password, Args: []string{"-J", "jump with spaces"}}}}
			if tc.backend == "key" {
				cfg.Servers[0].Auth = "key"
			}
			if tc.ghostty {
				cfg.Servers[0].Name = "node 测试 \"引号\" \\路径"
			}
			data, _ := yaml.Marshal(cfg)
			home, _ := configFixture(t, string(data))
			bin := t.TempDir()
			binary, _ := os.Executable()
			linkHelper(t, bin, "ssh", binary)
			if tc.ghostty {
				linkHelper(t, bin, "osascript", binary)
			}
			stty, err := exec.LookPath("stty")
			if err != nil {
				t.Fatal(err)
			}
			linkHelper(t, bin, "stty", stty)
			log := filepath.Join(t.TempDir(), "log")
			// Expect drives the terminal and ASKPASS authentication.
			driver := `
set timeout 4
spawn -noecho $env(SSHTUI_TEST_BINARY) node
expect {
    -re {0\.[1-9]s} {}
    timeout { puts stderr "Connection animation did not advance"; exit 110 }
    eof { exit 111 }
}
expect {
    READY {}
    timeout { puts stderr "No interactive session after authentication"; exit 101 }
    eof { puts stderr "Session closed before READY"; exit 102 }
}
after 200
expect -timeout 0 {
    -re {[0-9]+\.[0-9]s} { puts stderr "Connection animation continued into session"; exit 112 }
    timeout {}
    eof { exit 113 }
}
exec stty rows 30 columns 100 < $spawn_out(slave,name) 2>@stderr
send -- "size\r"
expect {
    "SIZE 30 100" {}
    timeout { puts stderr "Window size was not forwarded"; exit 103 }
    eof { exit 104 }
}
send -- "again\r"
expect {
    "password:" {}
    timeout { exit 105 }
    eof { exit 106 }
}
send -- "manual\r"
expect {
    "MANUAL-OK" {}
    timeout { exit 107 }
    eof { exit 108 }
}
if {$env(SSHTUI_TEST_SESSION) eq "password"} {
    send -- "exit\r"
} else {
    send -- "interrupt\r\003"
}
expect {
    eof {}
    timeout { puts stderr "SSH did not exit"; exit 109 }
}
set result [wait]
exit [lindex $result 3]
`
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, expect, "-c", driver)
			env := map[string]string{"HOME": home, "PATH": bin, "SSHTUI_TEST_PROCESS": "1", "SSHTUI_TEST_BINARY": binary, "SSHTUI_TEST_LOG": log, "SSHTUI_TEST_SESSION": mode, "SSHTUI_TEST_BACKEND": tc.backend, "TMUX": "", "TERM": "xterm-256color"}
			if tc.ghostty {
				env["TERM_PROGRAM"] = "ghostty"
			}
			if tc.titleFailure {
				env["SSHTUI_TEST_TITLE_FAIL"] = "1"
			}
			cmd.Env = testEnv(env)
			out, err := cmd.CombinedOutput()
			wantExit := 17
			if mode == "agent" {
				wantExit = 130
			}
			if err == nil || err.(*exec.ExitError).ExitCode() != wantExit {
				t.Fatalf("interactive session: %v; output: %s", err, out)
			}
			if mode == "password" {
				if readRecord[string](t, log+".password") != password {
					t.Fatal("password was evaluated, truncated or incorrectly encoded")
				}
			}
			if readRecord[string](t, log+".second-password") != "manual" {
				t.Fatal("SSH password was automatically sent to a remote program")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("password executed Tcl code")
			}
			if args := readRecord[[]string](t, log+".args"); !strings.Contains(strings.Join(args, "|"), "-J|jump with spaces") {
				t.Fatalf("SSH arguments were split or lost: %q", args)
			}
			if tc.ghostty {
				set := readRecord[[]string](t, log+".title-set")
				if len(set) != 2 || !strings.HasPrefix(set[0], "sshtui-") || set[1] != "node 测试 \"引号\" \\路径" {
					t.Fatalf("server remark was not safely passed as title: %q", set)
				}
				if tc.titleFailure {
					if !strings.Contains(string(out), "设置 Ghostty 标签标题失败，继续连接") {
						t.Fatalf("missing title failure message: %s", out)
					}
					if !strings.Contains(string(out), "\x1b]2;\x1b\\") {
						t.Fatalf("failed title setup was not cleared: %q", out)
					}
				} else if reset := readRecord[[]string](t, log+".title-reset"); !slices.Equal(reset, []string{"test-terminal-id"}) {
					t.Fatalf("title reset did not target the original terminal: %q", reset)
				}
			}
			if tc.backend == "askpass" {
				path := readRecord[string](t, log+".socket")
				if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
					t.Fatal("ASKPASS socket directory was not cleaned up")
				}
			}
		})
	}
}

func TestGhosttyNotRunningIsSilentNoOp(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "darwin" {
		t.Skip("Ghostty tab integration is macOS-only")
	}
	expect, err := exec.LookPath("expect")
	if err != nil {
		t.Skip("expect is not installed")
	}
	home, _ := configFixture(t, "servers:\n  - {name: node, host: 192.0.2.1, auth: key}\n")
	bin := t.TempDir()
	binary, _ := os.Executable()
	linkHelper(t, bin, "ssh", binary)
	linkHelper(t, bin, "osascript", binary)
	log := filepath.Join(t.TempDir(), "log")
	driver := `
set timeout 4
spawn -noecho $env(SSHTUI_TEST_BINARY) node
expect {
    eof {}
    timeout { puts stderr "SSH did not exit"; exit 101 }
}
set result [wait]
exit [lindex $result 3]
`
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, expect, "-c", driver)
	cmd.Env = testEnv(map[string]string{
		"HOME": home, "PATH": bin, "TERM": "xterm-256color", "TERM_PROGRAM": "ghostty", "TMUX": "",
		"SSHTUI_TEST_PROCESS": "1", "SSHTUI_TEST_BINARY": binary, "SSHTUI_TEST_LOG": log,
		"SSHTUI_TEST_GHOSTTY_NOT_RUNNING": "1",
	})
	out, err := cmd.CombinedOutput()
	if err == nil || err.(*exec.ExitError).ExitCode() != 17 {
		t.Fatalf("expected SSH exit 17, got %v; output: %q", err, out)
	}
	if set := readRecord[[]string](t, log+".title-set"); len(set) != 2 || set[1] != "node" {
		t.Fatalf("unexpected title-set args: %q", set)
	}
	if _, err := os.Stat(log + ".title-reset"); !os.IsNotExist(err) {
		t.Fatal("restore script ran when Ghostty was not running")
	}
	if !strings.Contains(string(out), "\x1b]2;\x1b\\") {
		t.Fatalf("OSC title marker was not cleared: %q", out)
	}
	if strings.Contains(string(out), "设置 Ghostty 标签标题失败") || strings.Contains(string(out), "恢复 Ghostty 标签标题失败") {
		t.Fatalf("unexpected Ghostty error output when not running: %q", out)
	}
}

func TestConnectionProgressContinuesDuringAsyncGhosttyTitle(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "darwin" {
		t.Skip("Ghostty tab integration is macOS-only")
	}
	expect, err := exec.LookPath("expect")
	if err != nil {
		t.Skip("expect is not installed")
	}
	home, _ := configFixture(t, "servers:\n  - {name: Oracle-大阪, host: 192.0.2.1, auth: key}\n")
	bin := t.TempDir()
	binary, _ := os.Executable()
	linkHelper(t, bin, "ssh", binary)
	linkHelper(t, bin, "osascript", binary)
	log := filepath.Join(t.TempDir(), "log")
	driver := `
set timeout 4
spawn -noecho $env(SSHTUI_TEST_BINARY) Oracle-大阪
expect {
    -re {0\.[12]s} {}
    timeout { puts stderr "Spinner did not start"; exit 101 }
    eof { exit 102 }
}
expect {
    -re {0\.[5-9]s} {}
    timeout { puts stderr "Spinner froze around 0.2s after async Ghostty title write"; exit 103 }
    eof { puts stderr "Spinner stopped advancing past 0.2s"; exit 104 }
}
expect {
    eof {}
    timeout { exit 105 }
}
set result [wait]
exit [lindex $result 3]
`
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, expect, "-c", driver)
	cmd.Env = testEnv(map[string]string{
		"HOME": home, "PATH": bin, "TERM": "xterm-256color", "TERM_PROGRAM": "ghostty", "TMUX": "",
		"SSHTUI_TEST_PROCESS": "1", "SSHTUI_TEST_BINARY": binary, "SSHTUI_TEST_LOG": log,
		"SSHTUI_TEST_OSASCRIPT_DELAY_MS": "180", "SSHTUI_TEST_CONNECTING": "slow-failure",
	})
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil || err == nil || err.(*exec.ExitError).ExitCode() != 255 {
		t.Fatalf("expected slow-failure exit 255 with advancing spinner, got %v; output: %q", err, out)
	}
	if !strings.HasSuffix(strings.Split(string(out), "\x1b[?2026l")[0], "\r\x1b[2K") {
		if !strings.Contains(string(out), "\r\x1b[2K") {
			t.Fatalf("spinner was not cleared on exit: %q", out)
		}
	}
}

func TestAskpassRejectsNonPasswordPrompts(t *testing.T) {
	for _, prompt := range []string{"Enter passphrase for key '/tmp/key':", "Are you sure you want to continue connecting?"} {
		out, err := cliCommand(t, map[string]string{"SSHTUI_ASKPASS_HELPER": "1"}, prompt).CombinedOutput()
		if err == nil || len(out) != 0 {
			t.Fatalf("non-password prompt was answered: %v, %q", err, out)
		}
	}
}

func TestPasswordBoundaries(t *testing.T) {
	if err := validatePassword(strings.Repeat("p", 1023)); err != nil {
		t.Fatal(err)
	}
	for _, password := range []string{strings.Repeat("p", 1024), "nul\x00byte", "line\nfeed", "carriage\rreturn"} {
		if err := validatePassword(password); err == nil {
			t.Fatal("invalid password accepted")
		}
	}
}

func TestParseServerArgs(t *testing.T) {
	cases := []struct {
		args     []string
		wantUser string
		wantHost string
		wantPort int
		wantAuth string
		wantPass string
		wantKey  string
		wantName string
	}{
		{
			args:     []string{"192.168.1.100", "22", "MyPassword123"},
			wantUser: "root",
			wantHost: "192.168.1.100",
			wantPort: 22,
			wantAuth: "password",
			wantPass: "MyPassword123",
			wantName: "192.168.1.100",
		},
		{
			args:     []string{"192.168.1.100", "22", "~/.ssh/my_vps.pem"},
			wantUser: "root",
			wantHost: "192.168.1.100",
			wantPort: 22,
			wantAuth: "key",
			wantKey:  "~/.ssh/my_vps.pem",
			wantName: "192.168.1.100",
		},
		{
			args:     []string{"192.168.1.100", "MyPassword123"},
			wantUser: "root",
			wantHost: "192.168.1.100",
			wantPort: 22,
			wantAuth: "password",
			wantPass: "MyPassword123",
			wantName: "192.168.1.100",
		},
		{
			args:     []string{"ubuntu@1.2.3.4", "22", "MyPassword123", "测试机"},
			wantUser: "ubuntu",
			wantHost: "1.2.3.4",
			wantPort: 22,
			wantAuth: "password",
			wantPass: "MyPassword123",
			wantName: "测试机",
		},
		{
			args:     []string{"root@1.2.3.4:2222", "MyPassword123"},
			wantUser: "root",
			wantHost: "1.2.3.4",
			wantPort: 2222,
			wantAuth: "password",
			wantPass: "MyPassword123",
			wantName: "1.2.3.4:2222",
		},
		{
			args:     []string{"[2001:db8::1]:2222", "MyPassword123"},
			wantUser: "root",
			wantHost: "2001:db8::1",
			wantPort: 2222,
			wantAuth: "password",
			wantPass: "MyPassword123",
			wantName: "[2001:db8::1]:2222",
		},
		{
			args:     []string{"192.0.2.1", "22", " pass with spaces "},
			wantUser: "root",
			wantHost: "192.0.2.1",
			wantPort: 22,
			wantAuth: "password",
			wantPass: " pass with spaces ",
			wantName: "192.0.2.1",
		},
		{
			args:     []string{"fe80::1%eth0"},
			wantUser: "root",
			wantHost: "fe80::1%eth0",
			wantPort: 22,
			wantAuth: "key",
			wantName: "fe80::1%eth0",
		},
		{
			args:     []string{"[fe80::1%eth0]"},
			wantUser: "root",
			wantHost: "fe80::1%eth0",
			wantPort: 22,
			wantAuth: "key",
			wantName: "fe80::1%eth0",
		},
		{
			args:     []string{"192.0.2.1", "22", "22"},
			wantUser: "root",
			wantHost: "192.0.2.1",
			wantPort: 22,
			wantAuth: "password",
			wantPass: "22",
			wantName: "192.0.2.1",
		},
		{
			args:     []string{"192.0.2.1:2222", "2222"},
			wantUser: "root",
			wantHost: "192.0.2.1",
			wantPort: 2222,
			wantAuth: "password",
			wantPass: "2222",
			wantName: "192.0.2.1:2222",
		},
		{
			args:     []string{"192.0.2.1", "22", "pass", "22"},
			wantUser: "root",
			wantHost: "192.0.2.1",
			wantPort: 22,
			wantAuth: "password",
			wantPass: "pass",
			wantName: "22",
		},
	}

	for _, tc := range cases {
		s, err := parseServerArgs(tc.args, "root", 22)
		if err != nil {
			t.Fatalf("parseServerArgs(%v) failed: %v", tc.args, err)
		}
		if s.User != tc.wantUser || s.Host != tc.wantHost || s.Port != tc.wantPort || s.Auth != tc.wantAuth || s.Password != tc.wantPass || s.Key != tc.wantKey || s.Name != tc.wantName {
			t.Fatalf("parseServerArgs(%v) = %+v, want %+v", tc.args, s, tc)
		}
	}
	if _, err := parseServerArgs([]string{"192.0.2.1", "/tmp/k\x00ey"}, "root", 22); err == nil {
		t.Fatal("NUL byte in key path was accepted")
	}
	if _, err := parseServerArgs([]string{"192.0.2.1", "22", "pass", "name", "Default", "extra"}, "root", 22); err == nil || !strings.Contains(err.Error(), "参数过多") {
		t.Fatalf("extra arg after explicit Default group was accepted: %v", err)
	}
	if s, err := parseServerArgs([]string{"192.0.2.1", "22", "pass", "name", "   "}, "root", 22); err != nil || s.Group != "Default" {
		t.Fatalf("blank group did not normalize to Default: %+v (%v)", s, err)
	}
}

func TestQuickAddServer(t *testing.T) {
	home, path := configFixture(t, "# header comment\nservers:\n  - {name: old, host: 192.0.2.1}\n")
	t.Setenv("HOME", home)

	err := quickAddServer([]string{"ubuntu@198.51.100.1", "2222", "secret123", "new-node"})
	if err != nil {
		t.Fatalf("quickAddServer failed: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "# header comment") {
		t.Fatal("original comments were overwritten")
	}

	cfg, _, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig failed after quickAddServer: %v", err)
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(cfg.Servers))
	}
	added := directMatch(cfg.Servers, "new-node")
	if added == nil || added.Name != "new-node" || added.User != "ubuntu" || added.Host != "198.51.100.1" || added.Port != 2222 || added.Password != "secret123" || added.Auth != "password" {
		t.Fatalf("added server mismatch: %+v", added)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("file permissions = %o, want 0600", info.Mode().Perm())
	}
}
