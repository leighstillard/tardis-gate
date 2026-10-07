package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitRepo(t *testing.T) (dir string, git func(...string) string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR", "GIT_COMMITTER"} {
		t.Setenv(k+"_NAME", "t")
		t.Setenv(k+"_EMAIL", "t@example.com")
	}
	dir = t.TempDir()
	git = func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "base")
	git("checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("commit", "-q", "-m", "A")
	return dir, git
}

func runCLI(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestCheckCommitThenVerify(t *testing.T) {
	dir, git := gitRepo(t)
	a := git("rev-parse", "HEAD")
	summary := filepath.Join(t.TempDir(), "s.md")
	if err := os.WriteFile(summary, []byte("- low a.txt:1 fine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runCLI("check", "commit", "simplify", "--summary-file", summary,
		"--tool", "anthropic/claude-code/x", "--repo", dir)
	if code != 0 {
		t.Fatalf("check commit exit %d: %s", code, stderr)
	}
	if got := git("log", "-1", "--format=%(trailers:key=Ship-Check-Of,valueonly)"); got != a {
		t.Errorf("Ship-Check-Of = %q, want %q", got, a)
	}

	code, out, stderr := runCLI("chain", "verify", "main", "HEAD", "--gates", "simplify,verify", "--repo", dir)
	if code != 1 {
		t.Fatalf("chain verify exit %d, want 1: %s", code, stderr)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if got["simplify"] != "valid" || got["verify"] != "missing" {
		t.Errorf("statuses = %v", got)
	}

	code, _, _ = runCLI("chain", "verify", "main", "HEAD", "--gates", "simplify", "--repo", dir)
	if code != 0 {
		t.Errorf("all-valid chain verify exit %d, want 0", code)
	}
}

func TestUsageErrorsExit2(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"bogus"},
		{"chain", "verify", "main"},
		{"chain", "verify", "main", "HEAD"},
		{"check", "commit", "simplify"},
	} {
		if code, _, _ := runCLI(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}
