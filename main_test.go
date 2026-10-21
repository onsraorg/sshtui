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
				writeTestRecord(".title-set", os.Args[4:])
				if os.Getenv("SSHTUI_TEST_TITLE_FAIL") == "1" {
					os.Exit(1)
				}
				fmt.Print("test-terminal-id")
			} else {
				writeTestRecord(".title-reset", os.Args[4:])
			}
			os.Exit(0)
		case "sshpass":
			fd, _ := strconv.Atoi(os.Args[2])
			f := os.NewFile(uintptr(fd), "password")
			data, err := io.ReadAll(f)
			if err != nil {
				panic(err)
			}
			f.Close()
			writeTestRecord(".password", string(data))
			writeTestRecord(".helper", os.Args)
			if err := syscall.Exec(os.Args[3], append([]string{"ssh"}, os.Args[4:]...), os.Environ()); err != nil {
				panic(err)
			}
		case "ansible":
			writeTestRecord(".ansible", os.Args[1:])
			os.Exit(7)
		case "ssh":
			if mode := os.Getenv("SSHTUI_TEST_CONNECTING"); mode != "" {
				if mode == "failure" {
					time.Sleep(400 * time.Millisecond)
					os.Exit(255)
				}
				time.Sleep(10 * time.Second)
				os.Exit(255)
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
				time.Sleep(300 * time.Millisecond)
				if _, err := term.MakeRaw(int(os.Stdin.Fd())); err != nil {
					panic(err)
				}
				if mode == "dirty" {
					fmt.Print("\x1b[?1049h\x1b[?1000h\x1b[?1006h\x1b[?2004h")
					os.Exit(0)
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
		if _, replaced := overrides[key]; !replaced && key != "TERM_PROGRAM" {
			env = append(env, entry)
		}
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
	if info.Mode().Perm() != 0600 || cfg.Servers[0].User != "root" || cfg.Servers[0].Port != 22 || cfg.Default.ServerAliveInterval != 30 {
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
			if _, _, err := loadConfig(); err == nil {
				t.Fatal("invalid credential was accepted")
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
			if _, _, err := loadConfig(); err == nil {
				t.Fatal("unsafe config was accepted")
			}
		})
	}
}

func TestEditorAndPathWorkWithBrokenYAML(t *testing.T) {
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
	for _, expected := range []string{"serveraliveinterval 60\n", "stricthostkeychecking true\n", "connecttimeout 25\n", "connectionattempts 2\n", "proxyjump jump\n"} {
		if !strings.Contains(string(out), expected) {
			t.Errorf("effective options missing %q", expected)
		}
	}
}

func TestSSHHandshakeTimeout(t *testing.T) {
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

func TestSSHInvocation(t *testing.T) {
	for _, backend := range []string{"key", "sshpass", "askpass"} {
		t.Run(backend, func(t *testing.T) {
			auth := "password"
			if backend == "key" {
				auth = "key"
			}
			home, _ := configFixture(t, "servers:\n  - {name: node, host: 192.0.2.1, auth: "+auth+", password: dummy-secret, key: '~/.ssh/key', args: ['-J', jump]}\n")
			bin := t.TempDir()
			binary, _ := os.Executable()
			linkHelper(t, bin, "ssh", binary)
			if backend == "sshpass" {
				linkHelper(t, bin, "sshpass", binary)
			}
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
			if backend == "sshpass" {
				helper := readRecord[[]string](t, log+".helper")
				if readRecord[string](t, log+".password") != "dummy-secret\n" || helper[1] != "-d" || strings.Contains(strings.Join(helper, " "), "dummy-secret") {
					t.Fatal("password must use a descriptor, not argv")
				}
			}
		})
	}
}

func TestConnectionProgressFailureAndCancel(t *testing.T) {
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
			if !strings.Contains(string(out), "\x1b[1A\r\x1b[2K") {
				t.Fatalf("connection animation was not cleared: %q", out)
			}
		})
	}
}

func TestSSHRestoresTerminalAfterDirtyChild(t *testing.T) {
	expect, err := exec.LookPath("expect")
	if err != nil {
		t.Skip("expect is not installed")
	}
	for _, redirect := range []bool{false, true} {
		t.Run(fmt.Sprintf("redirect-stderr=%t", redirect), func(t *testing.T) {
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
			env := map[string]string{"HOME": home, "PATH": bin, "TERM": "xterm-256color", "SSHTUI_TEST_PROCESS": "1", "SSHTUI_TEST_BINARY": binary, "SSHTUI_TEST_LOG": filepath.Join(t.TempDir(), "log"), "SSHTUI_TEST_SESSION": "dirty", "SSHTUI_TEST_STDERR": filepath.Join(t.TempDir(), "stderr")}
			if redirect {
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
		})
	}
}

func TestPasswordHelperInteractiveSession(t *testing.T) {
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
		{"sshpass", "password", false, false},
		{"key", "agent", true, false},
		{"askpass", "password", true, false},
		{"sshpass", "password", true, false},
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
			if tc.backend == "sshpass" {
				sshpass, err := exec.LookPath("sshpass")
				if err != nil {
					t.Skip("sshpass is not installed")
				}
				linkHelper(t, bin, "sshpass", sshpass)
			}
			stty, err := exec.LookPath("stty")
			if err != nil {
				t.Fatal(err)
			}
			linkHelper(t, bin, "stty", stty)
			log := filepath.Join(t.TempDir(), "log")
			// Expect drives the terminal; the app uses real sshpass or ASKPASS.
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

func TestAskpassRejectsNonPasswordPrompts(t *testing.T) {
	for _, prompt := range []string{"Enter passphrase for key '/tmp/key':", "Are you sure you want to continue connecting?"} {
		out, err := cliCommand(t, map[string]string{"SSHTUI_ASKPASS_HELPER": "1"}, prompt).CombinedOutput()
		if err == nil || len(out) != 0 {
			t.Fatalf("non-password prompt was answered: %v, %q", err, out)
		}
	}
}

func TestPasswordPipe(t *testing.T) {
	password := strings.Repeat("p", 1023)
	f, err := passwordPipe(password)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil || string(data) != password+"\n" {
		t.Fatal("password pipe lost bytes or did not reach EOF")
	}
	flags, err := unix.FcntlInt(f.Fd(), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC != 0 {
		t.Fatal("password descriptor would not survive Exec")
	}
	if f, err := passwordPipe(password + "p"); err == nil {
		f.Close()
		t.Fatal("overlong password must be rejected rather than truncated")
	}
}
