package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFakeSSHScript(t *testing.T, expectedPassword string) string {
	t.Helper()
	dir := t.TempDir()

	scriptPath := filepath.Join(dir, "fake_ssh.sh")
	script := fmt.Sprintf(`#!/bin/sh
mode="$1"
case "$mode" in
	direct)
		"$SSH_ASKPASS" "user@host's password:" > "$0.out"
		rc=$?
		out=$(cat "$0.out")
		if [ $rc -ne 0 ] || [ "$out" != "%s" ]; then
			exit 10
		fi
		exit 0
		;;
	nested)
		# 模拟 ProxyCommand / ProxyJump 子 SSH：通过子 shell 调用 askpass
		sh -c '"$SSH_ASKPASS" "user@jump'\''s password:"; exit $?'
		rc=$?
		if [ $rc -eq 0 ]; then
			# 若嵌套子进程成功取得密码则测试应报错
			exit 20
		fi
		exit 0
		;;
	multiple)
		"$SSH_ASKPASS" "user@host's password:" > "$0.out"
		rc1=$?
		out1=$(cat "$0.out")
		"$SSH_ASKPASS" "user@host's password:" > "$0.out"
		rc2=$?
		out2=$(cat "$0.out")
		if [ $rc1 -ne 0 ] || [ "$out1" != "%s" ] || [ $rc2 -ne 0 ] || [ "$out2" != "%s" ]; then
			exit 30
		fi
		exit 0
		;;
	proxy_dial)
		# 模拟 ProxyCommand 进程自身（非 helper 程序）直接读取 socket
		python3 -c '
import socket, os, sys
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.connect(os.environ["SSHTUI_ASKPASS_SOCKET"])
data = s.recv(1024)
s.close()
sys.exit(41 if data else 0)
'
		exit $?
		;;
	*)
		exit 1
		;;
esac
`, expectedPassword, expectedPassword, expectedPassword)

	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("写入 fake ssh 脚本失败: %v", err)
	}
	return scriptPath
}

func TestRunWithAskpassDirectAllowed(t *testing.T) {
	t.Parallel()
	secret := "secret-direct-pass"
	fakeSSH := writeFakeSSHScript(t, secret)

	err := runWithAskpass(fakeSSH, []string{"direct"}, secret, testEnv(map[string]string{"SSHTUI_TEST_PROCESS": "1"}))
	if err != nil {
		t.Fatalf("真实 runWithAskpass 直连应成功，实际返回错误: %v", err)
	}
}

func TestRunWithAskpassNestedSSHRejected(t *testing.T) {
	t.Parallel()
	secret := "secret-target-pass"
	fakeSSH := writeFakeSSHScript(t, secret)

	err := runWithAskpass(fakeSSH, []string{"nested"}, secret, testEnv(map[string]string{"SSHTUI_TEST_PROCESS": "1"}))
	if err != nil {
		t.Fatalf("真实 runWithAskpass 嵌套代理 SSH 应被拒绝，但脚本返回非零: %v", err)
	}
}

func TestRunWithAskpassMultipleRequestsAllowed(t *testing.T) {
	t.Parallel()
	secret := "secret-multi-pass"
	fakeSSH := writeFakeSSHScript(t, secret)

	err := runWithAskpass(fakeSSH, []string{"multiple"}, secret, testEnv(map[string]string{"SSHTUI_TEST_PROCESS": "1"}))
	if err != nil {
		t.Fatalf("真实 runWithAskpass 多次连续请求应成功，实际返回错误: %v", err)
	}
}

func TestRunWithAskpassDirectProxyDialRejected(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed")
	}
	secret := "secret-proxy-dial"
	fakeSSH := writeFakeSSHScript(t, secret)

	err := runWithAskpass(fakeSSH, []string{"proxy_dial"}, secret, testEnv(map[string]string{"SSHTUI_TEST_PROCESS": "1"}))
	if err != nil {
		t.Fatalf("ProxyCommand 自身直接连接应被拒绝且读不到密码，但测试失败: %v", err)
	}
}

func TestRunWithAskpassIgnoresPreexistingAskpassPrompt(t *testing.T) {
	t.Parallel()
	secret := "secret-prompt-env"
	fakeSSH := writeFakeSSHScript(t, secret)

	env := testEnv(map[string]string{"SSHTUI_TEST_PROCESS": "1", "SSH_ASKPASS_PROMPT": "confirm"})
	if err := runWithAskpass(fakeSSH, []string{"direct"}, secret, env); err != nil {
		t.Fatalf("预置 SSH_ASKPASS_PROMPT 时密码认证仍应成功: %v", err)
	}
}

func TestRunWithAskpassLongTMPDIR(t *testing.T) {
	secret := "secret-long-tmpdir"
	fakeSSH := writeFakeSSHScript(t, secret)
	longDir := filepath.Join(t.TempDir(), strings.Repeat("d", 70))
	if err := os.MkdirAll(longDir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", longDir)

	if err := runWithAskpass(fakeSSH, []string{"direct"}, secret, testEnv(map[string]string{"SSHTUI_TEST_PROCESS": "1"})); err != nil {
		t.Fatalf("长 TMPDIR 下应回退短 socket 路径并成功: %v", err)
	}
}

func TestRunWithAskpassViaSymlink(t *testing.T) {
	t.Parallel()
	secret := "secret-symlink-pass"
	home, _ := configFixture(t, fmt.Sprintf("servers:\n  - {name: node, host: 192.0.2.1, auth: password, password: %s}\n", secret))
	bin := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sym := filepath.Join(bin, "s")
	if err := os.Symlink(binary, sym); err != nil {
		t.Fatal(err)
	}
	fakeSSH := filepath.Join(bin, "ssh")
	script := fmt.Sprintf("#!/bin/sh\nout=$(\"$SSH_ASKPASS\" \"user@host's password:\") || exit 11\n[ \"$out\" = %q ] || exit 12\n", secret)
	if err := os.WriteFile(fakeSSH, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, sym, "node")
	cmd.Env = testEnv(map[string]string{"HOME": home, "PATH": bin, "SSHTUI_TEST_PROCESS": "1"})
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("通过软链接调用时 ASKPASS 应校验通过: %v (%s)", err, out)
	}
}
