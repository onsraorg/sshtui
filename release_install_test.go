package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtractChangelogScript 测试更新记录提取逻辑（精确匹配、版本边界、缺章节报错、空内容报错）
func TestExtractChangelogScript(t *testing.T) {
	scriptPath, err := filepath.Abs("scripts/extract-changelog.sh")
	if err != nil {
		t.Fatalf("failed to resolve script path: %v", err)
	}

	tempDir := t.TempDir()
	changelogFile := filepath.Join(tempDir, "CHANGELOG.md")

	changelogContent := `# 更新记录

## 0.1.10 — 2026-10-25

- 较新版本的更新说明内容

## 0.1.1 — 2026-10-21

- 修复了版本边界问题
- 另一个更新点

## 0.1.0 — 2026-10-18

`
	if err := os.WriteFile(changelogFile, []byte(changelogContent), 0644); err != nil {
		t.Fatalf("failed to write dummy changelog: %v", err)
	}

	t.Run("ExactMatchAndBoundary", func(t *testing.T) {
		outFile := filepath.Join(tempDir, "out_0.1.1.md")
		cmd := exec.Command(scriptPath, "0.1.1", changelogFile, outFile)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("expected extract 0.1.1 to succeed, got error: %v, output: %s", err, string(out))
		}
		data, err := os.ReadFile(outFile)
		if err != nil {
			t.Fatalf("failed to read out file: %v", err)
		}
		body := string(data)
		if !strings.Contains(body, "修复了版本边界问题") {
			t.Errorf("output missing expected 0.1.1 content: %s", body)
		}
		if strings.Contains(body, "较新版本的更新说明内容") {
			t.Errorf("output accidentally matched 0.1.10 content: %s", body)
		}
	})

	t.Run("MissingSectionFails", func(t *testing.T) {
		outFile := filepath.Join(tempDir, "out_missing.md")
		cmd := exec.Command(scriptPath, "0.9.9", changelogFile, outFile)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected extract 0.9.9 to fail, but succeeded with output: %s", string(out))
		}
		if !strings.Contains(string(out), "未找到") {
			t.Errorf("expected error message to mention 未找到, got: %s", string(out))
		}
	})

	t.Run("EmptySectionFails", func(t *testing.T) {
		outFile := filepath.Join(tempDir, "out_empty.md")
		cmd := exec.Command(scriptPath, "0.1.0", changelogFile, outFile)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected extract 0.1.0 (empty body) to fail, but succeeded with output: %s", string(out))
		}
		if !strings.Contains(string(out), "为空") {
			t.Errorf("expected error message to mention 为空, got: %s", string(out))
		}
	})
}

// TestInstallSpaceSafe 测试实际调用 make -f <Makefile> install -o build 在 scratch 目录下的安装与软链
func TestInstallSpaceSafe(t *testing.T) {
	makefilePath, err := filepath.Abs("Makefile")
	if err != nil {
		t.Fatalf("failed to resolve Makefile path: %v", err)
	}

	tempDir := t.TempDir()
	installDirWithSpace := filepath.Join(tempDir, "my space dir", "bin")

	// 在 scratch cwd 下创建 dummy sshtui 可执行文件
	dummyContent := "#!/bin/sh\necho 'sshtui executed'\n"
	dummyBin := filepath.Join(tempDir, "sshtui")
	if err := os.WriteFile(dummyBin, []byte(dummyContent), 0755); err != nil {
		t.Fatalf("failed to write dummy bin: %v", err)
	}

	// 实际调用 make -f <Makefile> install -o build INSTALL_DIR=...
	cmd := exec.Command("make", "-f", makefilePath, "install", "-o", "build", "INSTALL_DIR="+installDirWithSpace)
	cmd.Dir = tempDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make install failed: %v, output: %s", err, string(out))
	}

	installedTarget := filepath.Join(installDirWithSpace, "sshtui")
	if _, err := os.Stat(installedTarget); err != nil {
		t.Fatalf("expected installed target to exist: %v", err)
	}

	symlinkTarget := filepath.Join(installDirWithSpace, "s")
	linkDest, err := os.Readlink(symlinkTarget)
	if err != nil {
		t.Fatalf("expected symlink s to exist and be readable: %v", err)
	}
	if linkDest != installedTarget {
		t.Errorf("expected symlink target %s, got %s", installedTarget, linkDest)
	}

	// 验证软链指向的可执行文件可正常运行
	runCmd := exec.Command(symlinkTarget)
	runOut, err := runCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to run installed symlink: %v", err)
	}
	if !strings.Contains(string(runOut), "sshtui executed") {
		t.Errorf("unexpected output from installed binary: %s", string(runOut))
	}
}

// TestReadVersionScript 测试 scripts/read-version.sh 的严格 X.Y.Z 语义版本解析
func TestReadVersionScript(t *testing.T) {
	scriptPath, err := filepath.Abs("scripts/read-version.sh")
	if err != nil {
		t.Fatalf("failed to resolve script: %v", err)
	}

	cases := []struct {
		input   string
		valid   bool
		wantVer string
	}{
		{"0.2.0\n", true, "0.2.0"},
		{"1.10.3", true, "1.10.3"},
		{"0.0.1\n\n", true, "0.0.1"},
		{" 0.2.0 ", true, "0.2.0"},
		{"0. 2.0", false, ""},
		{"0.2. 0", false, ""},
		{"v0.2.0", false, ""},
		{"01.2.0", false, ""},
		{"0.02.0", false, ""},
		{"0.2.0-rc.1", false, ""},
		{"0.2", false, ""},
		{"", false, ""},
	}

	tempDir := t.TempDir()
	for i, tc := range cases {
		vf := filepath.Join(tempDir, "ver_"+string(rune('a'+i)))
		if err := os.WriteFile(vf, []byte(tc.input), 0644); err != nil {
			t.Fatalf("write ver file failed: %v", err)
		}

		cmd := exec.Command(scriptPath, vf)
		out, err := cmd.CombinedOutput()
		isValid := (err == nil)
		if isValid != tc.valid {
			t.Errorf("case %q valid=%v, got=%v (output: %s)", tc.input, tc.valid, isValid, string(out))
		}
		if isValid {
			got := strings.TrimSpace(string(out))
			if got != tc.wantVer {
				t.Errorf("case %q want %s, got %s", tc.input, tc.wantVer, got)
			}
		}
	}
}

// TestGitReleaseWorkflowLocal 测试本地 scratch git 仓库与 bare remote 交互（不联网）
// 覆盖: detached HEAD、非默认分支、push 失败远端有/无/未知、以及 tag 清理与保留逻辑
func TestGitReleaseWorkflowLocal(t *testing.T) {
	releaseCheckScript, err := filepath.Abs("scripts/release-check.sh")
	if err != nil {
		t.Fatalf("resolve release-check failed: %v", err)
	}
	pushReleaseScript, err := filepath.Abs("scripts/push-release.sh")
	if err != nil {
		t.Fatalf("resolve push-release failed: %v", err)
	}
	readVerScript, err := filepath.Abs("scripts/read-version.sh")
	if err != nil {
		t.Fatalf("resolve read-version failed: %v", err)
	}

	// 辅助运行 git 命令
	runGit := func(t *testing.T, dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s in %s failed: %v, out: %s", strings.Join(args, " "), dir, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}

	setupRepos := func(t *testing.T) (bareDir, workDir string) {
		baseDir := t.TempDir()
		bareDir = filepath.Join(baseDir, "remote.git")
		workDir = filepath.Join(baseDir, "work")

		runGit(t, baseDir, "init", "--bare", bareDir)

		if err := os.MkdirAll(workDir, 0755); err != nil {
			t.Fatalf("mkdir workDir failed: %v", err)
		}
		runGit(t, workDir, "init", "-b", "main")
		runGit(t, workDir, "remote", "add", "origin", bareDir)

		scriptsDir := filepath.Join(workDir, "scripts")
		if err := os.MkdirAll(scriptsDir, 0755); err != nil {
			t.Fatalf("mkdir scriptsDir failed: %v", err)
		}
		// 复制必要脚本
		for _, s := range []struct{ src, dst string }{
			{releaseCheckScript, filepath.Join(scriptsDir, "release-check.sh")},
			{pushReleaseScript, filepath.Join(scriptsDir, "push-release.sh")},
			{readVerScript, filepath.Join(scriptsDir, "read-version.sh")},
		} {
			data, err := os.ReadFile(s.src)
			if err != nil {
				t.Fatalf("read %s failed: %v", s.src, err)
			}
			if err := os.WriteFile(s.dst, data, 0755); err != nil {
				t.Fatalf("write %s failed: %v", s.dst, err)
			}
		}

		if err := os.WriteFile(filepath.Join(workDir, "VERSION"), []byte("0.2.0\n"), 0644); err != nil {
			t.Fatalf("write VERSION failed: %v", err)
		}
		runGit(t, workDir, "add", ".")
		runGit(t, workDir, "commit", "-m", "init commit")
		runGit(t, workDir, "push", "origin", "main")

		// 设置远端 HEAD 指向 main
		runGit(t, bareDir, "symbolic-ref", "HEAD", "refs/heads/main")
		return bareDir, workDir
	}

	t.Run("ReleaseCheck_SuccessOnDefaultBranch", func(t *testing.T) {
		_, workDir := setupRepos(t)
		cmd := exec.Command("./scripts/release-check.sh")
		cmd.Dir = workDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("expected release-check to pass on main, got %v: %s", err, string(out))
		}
	})

	t.Run("ReleaseCheck_FailsOnFeatureBranch", func(t *testing.T) {
		_, workDir := setupRepos(t)
		runGit(t, workDir, "checkout", "-b", "feature-x")
		cmd := exec.Command("./scripts/release-check.sh")
		cmd.Dir = workDir
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected release-check to fail on feature-x, but passed: %s", string(out))
		}
		if !strings.Contains(string(out), "不是 origin 默认分支") {
			t.Errorf("unexpected error output: %s", string(out))
		}
	})

	t.Run("ReleaseCheck_FailsOnDetachedHead", func(t *testing.T) {
		_, workDir := setupRepos(t)
		commit := runGit(t, workDir, "rev-parse", "HEAD")
		runGit(t, workDir, "checkout", commit) // 进入游离 HEAD
		cmd := exec.Command("./scripts/release-check.sh")
		cmd.Dir = workDir
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected release-check to fail on detached HEAD, but passed: %s", string(out))
		}
		if !strings.Contains(string(out), "游离 HEAD") {
			t.Errorf("unexpected error output: %s", string(out))
		}
	})

	t.Run("PushRelease_RemoteAlreadyHasTag", func(t *testing.T) {
		bareDir, workDir := setupRepos(t)
		// 在远端直接创建 v0.2.0 tag
		runGit(t, bareDir, "tag", "v0.2.0")

		cmd := exec.Command("./scripts/push-release.sh", "v0.2.0", "main")
		cmd.Dir = workDir
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected push-release to fail when remote tag exists, but succeeded: %s", string(out))
		}
		if !strings.Contains(string(out), "远端已存在标签") {
			t.Errorf("unexpected error output: %s", string(out))
		}
	})

	t.Run("PushRelease_OldTagNotMatchingHEAD", func(t *testing.T) {
		_, workDir := setupRepos(t)
		// 创建本地 tag 指向初始 commit
		runGit(t, workDir, "tag", "v0.2.0")
		// 新建一个 commit，使 HEAD 与 tag 不一致
		if err := os.WriteFile(filepath.Join(workDir, "extra.txt"), []byte("new"), 0644); err != nil {
			t.Fatalf("write extra: %v", err)
		}
		runGit(t, workDir, "add", "extra.txt")
		runGit(t, workDir, "commit", "-m", "second commit")

		cmd := exec.Command("./scripts/push-release.sh", "v0.2.0", "main")
		cmd.Dir = workDir
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected push-release to reject non-HEAD tag, but succeeded: %s", string(out))
		}
		if !strings.Contains(string(out), "未指向当前 HEAD") {
			t.Errorf("unexpected error output: %s", string(out))
		}
	})

	t.Run("PushRelease_PushFailed_RemoteHasNoTag_CleansNewlyCreatedLocalTag", func(t *testing.T) {
		bareDir, workDir := setupRepos(t)
		// 在远端安装 update hook 拒绝 refs/tags/v0.2.0 推送
		hookPath := filepath.Join(bareDir, "hooks", "update")
		hookContent := "#!/bin/sh\nexit 1\n"
		if err := os.WriteFile(hookPath, []byte(hookContent), 0755); err != nil {
			t.Fatalf("write hook failed: %v", err)
		}

		cmd := exec.Command("./scripts/push-release.sh", "v0.2.0", "main")
		cmd.Dir = workDir
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected push-release to fail due to rejected push, but succeeded: %s", string(out))
		}
		if !strings.Contains(string(out), "安全清理本次新建的本地 tag") {
			t.Errorf("expected clean message in output: %s", string(out))
		}

		// 验证本地 tag 已被删除
		verifyCmd := exec.Command("git", "rev-parse", "-q", "--verify", "refs/tags/v0.2.0")
		verifyCmd.Dir = workDir
		if err := verifyCmd.Run(); err == nil {
			t.Errorf("expected local tag v0.2.0 to be deleted, but it still exists")
		}
	})

	t.Run("PushRelease_PushFailed_RemoteHasTag_PreservesLocalTag", func(t *testing.T) {
		bareDir, workDir := setupRepos(t)

		// update hook 可更新已存在的 HEAD 对象，模拟远端已写入而客户端报错。
		hookPath := filepath.Join(bareDir, "hooks", "update")
		hookContent := `#!/bin/sh
git update-ref refs/tags/v0.2.0 HEAD
exit 1
`
		if err := os.WriteFile(hookPath, []byte(hookContent), 0755); err != nil {
			t.Fatalf("write hook failed: %v", err)
		}

		cmd := exec.Command("./scripts/push-release.sh", "v0.2.0", "main")
		cmd.Dir = workDir
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected push to fail, but succeeded")
		}
		if !strings.Contains(string(out), "远端已存在标签") || !strings.Contains(string(out), "保留本地 tag") {
			t.Errorf("expected output to mention preserving tag, got: %s", string(out))
		}

		// 验证本地 tag 仍然保留
		verifyCmd := exec.Command("git", "rev-parse", "-q", "--verify", "refs/tags/v0.2.0")
		verifyCmd.Dir = workDir
		if err := verifyCmd.Run(); err != nil {
			t.Errorf("expected local tag v0.2.0 to be preserved, but got err: %v", err)
		}
	})

	t.Run("PushRelease_PushFailed_RemoteUnknown_PreservesLocalTag", func(t *testing.T) {
		bareDir, workDir := setupRepos(t)

		// 先允许远端查询，在拒绝推送的 hook 中使后续查询无法访问远端。
		hookPath := filepath.Join(bareDir, "hooks", "update")
		hookContent := "#!/bin/sh\ngit --git-dir=\"$SSHTUI_RELEASE_TEST_WORK/.git\" remote set-url origin \"$SSHTUI_RELEASE_TEST_WORK/missing.git\"\nexit 1\n"
		if err := os.WriteFile(hookPath, []byte(hookContent), 0755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("./scripts/push-release.sh", "v0.2.0", "main")
		cmd.Dir = workDir
		cmd.Env = append(os.Environ(), "SSHTUI_RELEASE_TEST_WORK="+workDir)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected unknown state to exit with error, but succeeded")
		}
		if !strings.Contains(string(out), "无法核实远端标签状态") || !strings.Contains(string(out), "保留本地 tag") {
			t.Errorf("unexpected output for unknown state: %s", string(out))
		}

		// 验证本地 tag 未被删除
		verifyCmd := exec.Command("git", "rev-parse", "-q", "--verify", "refs/tags/v0.2.0")
		verifyCmd.Dir = workDir
		if err := verifyCmd.Run(); err != nil {
			t.Errorf("expected local tag v0.2.0 to be preserved on unknown remote, got: %v", err)
		}
	})
}
