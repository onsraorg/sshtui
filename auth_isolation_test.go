package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeFakeSSHScript(t *testing.T, expectedPassword string) string {
	t.Helper()
	dir, err := os.MkdirTemp(os.TempDir(), "ap-sh-")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

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
	secret := "secret-direct-pass"
	fakeSSH := writeFakeSSHScript(t, secret)

	err := runWithAskpass(fakeSSH, []string{"direct"}, secret, append(os.Environ(), "SSHTUI_TEST_PROCESS=1"))
	if err != nil {
		t.Fatalf("真实 runWithAskpass 直连应成功，实际返回错误: %v", err)
	}
}

func TestRunWithAskpassNestedSSHRejected(t *testing.T) {
	secret := "secret-target-pass"
	fakeSSH := writeFakeSSHScript(t, secret)

	err := runWithAskpass(fakeSSH, []string{"nested"}, secret, append(os.Environ(), "SSHTUI_TEST_PROCESS=1"))
	if err != nil {
		t.Fatalf("真实 runWithAskpass 嵌套代理 SSH 应被拒绝，但脚本返回非零: %v", err)
	}
}

func TestRunWithAskpassMultipleRequestsAllowed(t *testing.T) {
	secret := "secret-multi-pass"
	fakeSSH := writeFakeSSHScript(t, secret)

	err := runWithAskpass(fakeSSH, []string{"multiple"}, secret, append(os.Environ(), "SSHTUI_TEST_PROCESS=1"))
	if err != nil {
		t.Fatalf("真实 runWithAskpass 多次连续请求应成功，实际返回错误: %v", err)
	}
}

func TestRunWithAskpassDirectProxyDialRejected(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed")
	}
	secret := "secret-proxy-dial"
	fakeSSH := writeFakeSSHScript(t, secret)

	err := runWithAskpass(fakeSSH, []string{"proxy_dial"}, secret, append(os.Environ(), "SSHTUI_TEST_PROCESS=1"))
	if err != nil {
		t.Fatalf("ProxyCommand 自身直接连接应被拒绝且读不到密码，但测试失败: %v", err)
	}
}

func TestAskpassNonPasswordPromptRejected(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "Are you sure you want to continue connecting (yes/no/[fingerprint])?")
	cmd.Env = append(os.Environ(), "SSHTUI_TEST_PROCESS=1", "SSHTUI_ASKPASS_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("非密码提示应被 answerAskpass 拒绝，但命令执行成功: %s", out)
	}
	if len(out) != 0 && strings.Contains(string(out), "password") {
		t.Fatalf("非密码提示不应输出密码: %s", out)
	}
}
