package ship

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leighstillard/tardis-gate/internal/chain"
	"github.com/leighstillard/tardis-gate/internal/executor"
)

// authorRepo is a clone of a bare origin with main, enrolled with one gate
// (simplify), pushed and feature checked out one code commit ahead.
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
	if err := os.MkdirAll(filepath.Join(dir, ".tardis"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".tardis", "config.yml"), []byte("gates:\n  - name: simplify\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "base, enrolled")
	git("push", "-q", "origin", "main")
	git("checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("commit", "-q", "-m", "A")
	return &Author{Dir: dir, Remote: "origin", Branch: "feature", Tool: "anthropic/a/b"}, git
}

// reviewIn is an AuthorReviewIn for gate on the current HEAD, pinned to main.
func reviewIn(git func(...string) string, gate string) AuthorReviewIn {
	tip := git("rev-parse", "HEAD")
	return AuthorReviewIn{Gate: gate, Base: "main", BaseID: git("rev-parse", "main"), Code: tip, Tip: tip}
}

func TestAuthorReviewRunsNoPolicyFromAnUncheckedBase(t *testing.T) {
	// The workflow names the base; it must not be able to point this machine
	// at gate commands from just any commit or branch.
	a, git := authorRepo(t)
	git("push", "-q", "origin", "feature")
	// A side branch merged into main: its commit is reachable from main
	// without main ever having pointed at it.
	git("checkout", "-q", "-b", "side", "main")
	git("commit", "-q", "--allow-empty", "-m", "side")
	side := git("rev-parse", "HEAD")
	git("checkout", "-q", "main")
	git("merge", "-q", "--no-ff", "-m", "merge side", "side")
	git("push", "-q", "origin", "main")
	git("checkout", "-q", "feature")
	for _, in := range []AuthorReviewIn{
		{Gate: "simplify", Base: "feature", BaseID: git("rev-parse", "feature")},
		{Gate: "simplify", Base: "main", BaseID: git("rev-parse", "feature")},
		{Gate: "simplify", Base: "main", BaseID: side},
	} {
		in.Code, in.Tip = git("rev-parse", "HEAD"), git("rev-parse", "HEAD")
		if _, err := a.AuthorReview(context.Background(), in); err == nil || !strings.Contains(err.Error(), "base") {
			t.Errorf("base %s at %.7s: err = %v, want a refusal", in.Base, in.BaseID, err)
		}
	}
}

func TestAuthorGateGetsNoTemporalKeyAndKeepsItsOutput(t *testing.T) {
	t.Setenv("TEMPORAL_API_KEY", "sentinel-key")
	a, git := authorRepo(t)
	var out strings.Builder
	a.Exec, a.Out = executor.Local{}, &out
	git("checkout", "-q", "main")
	gate := filepath.Join(a.Dir, ".tardis", "gates", "simplify", "gate.yml")
	if err := os.MkdirAll(filepath.Dir(gate), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gate, []byte("name: simplify\nrun: [sh, -c, 'echo key=$TEMPORAL_API_KEY; printf \"private %s\\\\n\" detail; exit 3']\ntimeout: 1m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "simplify prints")
	git("push", "-q", "origin", "main")
	git("checkout", "-q", "feature")
	_, err := a.AuthorReview(context.Background(), reviewIn(git, "simplify"))
	if err == nil || strings.Contains(err.Error(), "private detail") || !strings.Contains(err.Error(), "exited 3") {
		t.Errorf("err = %v; want the exit code without the output", err)
	}
	if strings.Contains(out.String(), "sentinel-key") || !strings.Contains(out.String(), "private detail") {
		t.Errorf("local output %q; want the gate's output, without the Temporal key", out.String())
	}
}

func TestAuthorReviewStaysOnItsBranch(t *testing.T) {
	// Another branch at the same commit: the check must not land on it.
	a, git := authorRepo(t)
	git("checkout", "-q", "-b", "other")
	if _, err := a.AuthorReview(context.Background(), reviewIn(git, "simplify")); err == nil || !strings.Contains(err.Error(), "no longer on feature") {
		t.Errorf("review on branch other: err = %v", err)
	}
}

func TestAuthorReviewRefusesOversizedFindings(t *testing.T) {
	a, git := authorRepo(t)
	a.Exec, a.Out = executor.Local{}, io.Discard
	git("checkout", "-q", "main")
	gate := filepath.Join(a.Dir, ".tardis", "gates", "simplify", "gate.yml")
	if err := os.MkdirAll(filepath.Dir(gate), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gate, []byte("name: simplify\nrun: [sh, -c, 'head -c 70000 /dev/zero | tr \"\\\\0\" x > \"$TARDIS_FINDINGS_OUT\"']\ntimeout: 1m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "simplify writes too much")
	git("push", "-q", "origin", "main")
	git("checkout", "-q", "feature")
	if _, err := a.AuthorReview(context.Background(), reviewIn(git, "simplify")); err == nil || !strings.Contains(err.Error(), "over 64 KiB") {
		t.Errorf("err = %v; want oversized findings refused", err)
	}
}

func TestAuthorReviewRetryResumesFromItsOwnCheck(t *testing.T) {
	a, git := authorRepo(t)
	in := reviewIn(git, "simplify")
	// The first attempt wrote its check, then lost the push.
	check, err := chain.Commit(a.Dir, "simplify", "ok", a.Tool)
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.AuthorReview(context.Background(), in)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got != check || git("rev-parse", "origin/feature") != check {
		t.Errorf("returned %s, origin/feature %s; want the existing check %s pushed", got, git("rev-parse", "origin/feature"), check)
	}
}

func TestAuthorReviewRefusesARealMove(t *testing.T) {
	a, git := authorRepo(t)
	in := reviewIn(git, "simplify")
	git("commit", "-q", "--allow-empty", "-m", "ship-check: verify") // not this gate's check
	_, err := a.AuthorReview(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "branch moved") {
		t.Errorf("err = %v, want branch moved", err)
	}
}

func TestAuthorReviewRefusesADirtyWorkingCopy(t *testing.T) {
	a, git := authorRepo(t)
	if err := os.WriteFile(filepath.Join(a.Dir, "fix.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := a.AuthorReview(context.Background(), reviewIn(git, "simplify"))
	if err == nil || !strings.Contains(err.Error(), "uncommitted or untracked") {
		t.Errorf("err = %v, want a dirty-tree refusal", err)
	}
}

func TestAuthorReviewFetchesThePinnedBase(t *testing.T) {
	// The base moved on the remote after this clone last fetched it; the run
	// is pinned to the new commit, which the author must fetch, not refuse.
	a, git := authorRepo(t)
	in := reviewIn(git, "simplify")
	other := t.TempDir()
	if out, err := exec.Command("git", "clone", "-q", git("remote", "get-url", "origin"), other).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := exec.Command("sh", "-c", "cd "+other+" && git commit -q --allow-empty -m moved && git push -q origin main && git rev-parse HEAD").Output(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	} else {
		in.BaseID = strings.TrimSpace(string(out))
	}
	if _, err := chain.Commit(a.Dir, "simplify", "ok", a.Tool); err != nil {
		t.Fatal(err)
	}
	in.Tip = in.Code // the retry path: HEAD is this gate's check on in.Tip
	if _, err := a.AuthorReview(context.Background(), in); err != nil {
		t.Errorf("pinned base %s not fetched: %v", in.BaseID, err)
	}
}
