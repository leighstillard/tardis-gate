package ship

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leighstillard/tardis-gate/internal/chain"
)

// authorRepo is a clone of a bare origin with main pushed and feature checked
// out one code commit ahead.
func authorRepo(t *testing.T) (*Author, func(...string) string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR", "GIT_COMMITTER"} {
		t.Setenv(k+"_NAME", "t")
		t.Setenv(k+"_EMAIL", "t@example.com")
	}
	origin, dir := filepath.Join(t.TempDir(), "origin.git"), t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", origin).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	git("init", "-q", "-b", "main")
	git("remote", "add", "origin", origin)
	git("commit", "-q", "--allow-empty", "-m", "base")
	git("push", "-q", "origin", "main")
	git("checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("commit", "-q", "-m", "A")
	return &Author{Dir: dir, Remote: "origin", Branch: "feature", Tool: "anthropic/a/b"}, git
}

func TestAuthorReviewRetryResumesFromItsOwnCheck(t *testing.T) {
	a, git := authorRepo(t)
	tip := git("rev-parse", "HEAD")
	// The first attempt wrote its check, then lost the push.
	check, err := chain.Commit(a.Dir, "simplify", "ok", a.Tool)
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.AuthorReview(context.Background(), AuthorReviewIn{Gate: "simplify", Base: "main", Code: tip, Tip: tip})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got != check || git("rev-parse", "origin/feature") != check {
		t.Errorf("returned %s, origin/feature %s; want the existing check %s pushed", got, git("rev-parse", "origin/feature"), check)
	}
}

func TestAuthorReviewRefusesARealMove(t *testing.T) {
	a, git := authorRepo(t)
	tip := git("rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-m", "ship-check: verify") // not this gate's check
	_, err := a.AuthorReview(context.Background(), AuthorReviewIn{Gate: "simplify", Base: "main", Code: tip, Tip: tip})
	if err == nil || !strings.Contains(err.Error(), "branch moved") {
		t.Errorf("err = %v, want branch moved", err)
	}
}
