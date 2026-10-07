package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestGitLineReadsStdoutOnly(t *testing.T) {
	// A branch and a tag both named x: git answers on stdout and warns that
	// x is ambiguous on stderr. The warning is not part of the answer.
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "a"},
		{"tag", "x"}, {"branch", "x"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	head, _ := gitLine(dir, "rev-parse", "HEAD")
	if got, err := gitLine(dir, "rev-parse", "x"); err != nil || got != head {
		t.Errorf("gitLine(rev-parse x) = %q, %v; want %q", got, err, head)
	}
}

func TestRepoIDKeepsRepositoriesApart(t *testing.T) {
	for _, u := range []string{"https://github.com/Owner/Repo.git", "git@github.com:owner/repo.git", "ssh://git@github.com/owner/repo"} {
		if got := repoID(u); got != "owner/repo" {
			t.Errorf("repoID(%q) = %q, want owner/repo", u, got)
		}
	}
	a, b := repoID("https://gitlab.com/team-a/widget.git"), repoID("https://gitlab.com/team-b/widget.git")
	if a == b || repoID("/srv/a/widget") == repoID("/srv/b/widget") {
		t.Errorf("same-named repositories share an ID: %q %q", a, b)
	}
	if !strings.HasPrefix(a, "other/widget-") {
		t.Errorf("repoID = %q, want other/widget-<hash>", a)
	}
	if repoID("https://evilgithub.com/owner/repo") == "owner/repo" {
		t.Error("a non-GitHub host was read as GitHub")
	}
}

func TestCheckRemoteRefusesCredentials(t *testing.T) {
	for url, ok := range map[string]bool{
		"git@github.com:owner/repo.git":                 true,
		"https://github.com/owner/repo.git":             true,
		"ssh://git@github.com/owner/repo.git":           true,
		"/srv/git/repo.git":                             true,
		"file:///srv/git/repo.git":                      true,
		"https://x-access-token:ghs_abc@github.com/o/r": false,
		"https://ghp_abc@github.com/o/r.git":            false,
		"ssh://git:hunter2@example.com/o/r.git":         false,
		"https://git.example/o/r.git?access_token=x":    false,
		"https://github.com/o/r.git?":                   false,
		"https://github.com/o/r.git#tok":                false,
		"https://git.example/%zz":                       false,
	} {
		if err := checkRemote(url); (err == nil) != ok {
			t.Errorf("checkRemote(%q) = %v, want ok=%v", url, err, ok)
		}
	}
}
