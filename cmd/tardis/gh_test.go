package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHookInstallsOnceAndUninstalls(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "rc")
	os.WriteFile(rc, []byte("export A=1"), 0o644)
	for range 2 {
		if code, _, stderr := runCLI("hook", "install", "--rc", rc); code != 0 {
			t.Fatalf("install: exit %d: %s", code, stderr)
		}
	}
	b, _ := os.ReadFile(rc)
	if s := string(b); strings.Count(s, hookStart) != 1 || !strings.HasPrefix(s, "export A=1\n"+hookStart) || !strings.Contains(s, "tardis hook check || return 1") {
		t.Errorf("installed twice:\n%s", s)
	}
	if code, _, stderr := runCLI("hook", "uninstall", "--rc", rc); code != 0 {
		t.Fatalf("uninstall: exit %d: %s", code, stderr)
	}
	if b, _ := os.ReadFile(rc); string(b) != "export A=1\n" {
		t.Errorf("uninstalled: %q", b)
	}
}

func TestHookCheckRefusesAMissingGate(t *testing.T) {
	dir, git := gitRepo(t)
	if code, _, stderr := runCLI("hook", "check", "--repo", dir); code != 0 {
		t.Errorf("not enrolled: exit %d: %s", code, stderr)
	}
	git("checkout", "-q", "main")
	os.MkdirAll(filepath.Join(dir, ".tardis"), 0o755)
	os.WriteFile(filepath.Join(dir, ".tardis", "config.yml"), []byte("gates:\n  - name: simplify\n"), 0o644)
	git("add", "-A")
	git("commit", "-q", "-m", "enrol")
	git("update-ref", "refs/remotes/origin/main", "main")
	git("checkout", "-q", "feature")
	git("rebase", "-q", "main")
	code, _, stderr := runCLI("hook", "check", "--repo", dir)
	if code != 1 || !strings.Contains(stderr, "refusing gh pr create: simplify missing") {
		t.Errorf("missing simplify: exit %d: %s", code, stderr)
	}
	if code, _, stderr := runCLI("hook", "check", "--repo", filepath.Join(dir, ".tardis")); code != 1 {
		t.Errorf("from a subdirectory: exit %d: %s; want the same refusal", code, stderr)
	}
	git("checkout", "-q", "main")
	git("commit", "-q", "--allow-empty", "-m", "base moves on")
	git("update-ref", "refs/remotes/origin/main", "main")
	git("checkout", "-q", "feature")
	if code, _, stderr := runCLI("hook", "check", "--repo", dir); code != 0 || !strings.Contains(stderr, "not checked") {
		t.Errorf("behind the base: exit %d: %s; want let through, not an empty refusal", code, stderr)
	}
	git("update-ref", "-d", "refs/remotes/origin/main")
	if code, _, stderr := runCLI("hook", "check", "--repo", dir); code != 0 || !strings.Contains(stderr, "not checked") {
		t.Errorf("no origin/main: exit %d: %s; want let through", code, stderr)
	}
}
