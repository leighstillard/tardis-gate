package ship

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/temporal"

	"github.com/leighstillard/tardis-gate/internal/chain"
	"github.com/leighstillard/tardis-gate/internal/executor"
)

// runnerFixture is a bare origin whose main is enrolled and whose feature
// branch has one code commit and a valid simplify check on top. It returns
// the runner, the origin URL, main's commit and the check commit.
func runnerFixture(t *testing.T) (*Runner, string, string, string) {
	t.Helper()
	a, git := authorRepo(t)
	git("checkout", "-q", "main")
	if err := os.MkdirAll(filepath.Join(a.Dir, ".tardis"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.Dir, ".tardis", "config.yml"), []byte("gates:\n  - name: simplify\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "enrol")
	git("push", "-q", "origin", "main")
	git("checkout", "-q", "feature")
	git("rebase", "-q", "main")
	check, err := chain.Commit(a.Dir, "simplify", "ok", a.Tool)
	if err != nil {
		t.Fatal(err)
	}
	git("push", "-q", "origin", "feature")
	url := git("remote", "get-url", "origin")
	r := &Runner{Repos: map[string]bool{url: true}, WorkDir: t.TempDir(), RerunCmd: []string{"true"}, Exec: executor.Local{}, Log: io.Discard}
	return r, url, git("rev-parse", "main"), check
}

func TestRunnerPostsSuccessOnlyForItsOwnPass(t *testing.T) {
	r, url, base, check := runnerFixture(t)
	ctx := context.Background()
	success := CheckIn{RepoURL: url, Base: "main", BaseID: base, SHA: check, Gate: "simplify", Conclusion: "success"}

	// A workflow asking for a success the runner never re-ran is refused.
	if err := r.PostCheck(ctx, success); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("success without a re-run: err = %v, want refusal", err)
	}
	v, err := r.Rerun(ctx, RerunIn{RepoURL: url, Base: "main", BaseID: base, Tip: check, Gate: "simplify"})
	if err != nil || !v.Pass {
		t.Fatalf("rerun: %+v, %v", v, err)
	}
	if err := r.PostCheck(ctx, success); err != nil {
		t.Errorf("success after a passing re-run: %v", err)
	}
	// A restarted runner still knows what it passed.
	restarted := &Runner{Repos: r.Repos, WorkDir: r.WorkDir, RerunCmd: r.RerunCmd, Exec: r.Exec, Log: io.Discard}
	if err := restarted.PostCheck(ctx, success); err != nil {
		t.Errorf("success after a restart: %v", err)
	}
	// Failures are posted as asked: they can only block.
	if err := r.PostCheck(ctx, CheckIn{RepoURL: url, SHA: check, Gate: "verify", Conclusion: "failure"}); err != nil {
		t.Errorf("failure: %v", err)
	}
}

func TestRunnerPassIsBoundToItsBaseAndClaim(t *testing.T) {
	r, url, _, check := runnerFixture(t)
	ctx := context.Background()
	w := t.TempDir()
	g := func(args ...string) string {
		return strings.TrimSpace(run(t, "git", append([]string{"-C", w}, args...)...))
	}
	g("clone", "-q", url, ".")
	g("config", "user.name", "t")
	g("config", "user.email", "t@example.com")
	// main moves on twice and the branch does not, so both base commits have
	// the same merge base with it.
	g("checkout", "-q", "main")
	g("commit", "-q", "--allow-empty", "-m", "m1")
	m1 := g("rev-parse", "HEAD")
	g("commit", "-q", "--allow-empty", "-m", "m2")
	m2 := g("rev-parse", "HEAD")
	g("push", "-q", "origin", "main")
	success := func(baseID, sha string) error {
		return r.PostCheck(ctx, CheckIn{RepoURL: url, Base: "main", BaseID: baseID, SHA: sha, Gate: "simplify", Conclusion: "success"})
	}

	if v, err := r.Rerun(ctx, RerunIn{RepoURL: url, Base: "main", BaseID: m1, Tip: check, Gate: "simplify"}); err != nil || !v.Pass {
		t.Fatalf("rerun on m1: %+v, %v", v, err)
	}
	if err := success(m2, check); err == nil {
		t.Error("a pass on base m1 authorised a success on base m2")
	}
	if err := success(m1, check); err != nil {
		t.Errorf("success on the base the gate passed on: %v", err)
	}

	// The same code under a rewritten claim was never re-run.
	g("checkout", "-q", "feature")
	g("reset", "-q", "--hard", check+"^")
	rewritten, err := chain.Commit(w, "simplify", "a different claim", "anthropic/claude-code/test")
	if err != nil {
		t.Fatal(err)
	}
	g("push", "-q", "--force", "origin", "feature")
	if g("rev-parse", rewritten+"^{tree}") != g("rev-parse", check+"^{tree}") {
		t.Fatal("rewritten check changed the tree")
	}
	if err := success(m1, rewritten); err == nil {
		t.Error("a pass for one check commit authorised a success for a rewritten one")
	}
}

func TestRunnerFollowsTheDefaultBranch(t *testing.T) {
	r, url, _, check := runnerFixture(t)
	ctx := context.Background()
	if out, err := r.Resolve(ctx, ResolveIn{RepoURL: url, SHA: check}); err != nil || out.Base != "main" {
		t.Fatalf("resolve: %+v, %v", out, err)
	}
	// The default branch moves to trunk, whose manifest names itself the
	// base; main stays behind with its old policy.
	w := t.TempDir()
	g := func(args ...string) string {
		return strings.TrimSpace(run(t, "git", append([]string{"-C", w}, args...)...))
	}
	g("clone", "-q", url, ".")
	g("config", "user.name", "t")
	g("config", "user.email", "t@example.com")
	g("checkout", "-q", "-b", "trunk", "origin/main")
	if err := os.WriteFile(filepath.Join(w, ".tardis", "config.yml"), []byte("base_branch: trunk\ngates:\n  - name: simplify\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g("commit", "-q", "-am", "trunk is the base")
	g("push", "-q", "origin", "trunk")
	run(t, "git", "--git-dir="+strings.TrimPrefix(url, "file://"), "symbolic-ref", "HEAD", "refs/heads/trunk")
	if out, err := r.Resolve(ctx, ResolveIn{RepoURL: url, SHA: check}); err != nil || out.Base != "trunk" {
		t.Errorf("resolve after the default branch moved: %+v, %v; want base trunk", out, err)
	}
}

func TestOpenPRRefusesAMovedBase(t *testing.T) {
	r, url, base, check := runnerFixture(t)
	ctx := context.Background()
	open := func(baseID string) error {
		_, err := r.OpenPR(ctx, OpenPRIn{RepoURL: url, Branch: "feature", Base: "main", BaseID: baseID, Head: check})
		return err
	}
	if err := open(base); err != nil {
		t.Fatalf("open on the resolved base: %v", err)
	}
	w := t.TempDir()
	run(t, "git", "-C", w, "clone", "-q", url, ".")
	run(t, "git", "-C", w, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "main moves")
	run(t, "git", "-C", w, "push", "-q", "origin", "main")
	var ae *temporal.ApplicationError
	if err := open(base); !errors.As(err, &ae) || ae.Type() != "BaseMoved" {
		t.Errorf("open after main moved: err = %v; want BaseMoved", err)
	}
}

func TestRunnerRefusesABaseItDidNotChoose(t *testing.T) {
	r, url, base, check := runnerFixture(t)
	ctx := context.Background()
	// The base comes from the default branch's manifest, not the request.
	if out, err := r.Resolve(ctx, ResolveIn{RepoURL: url, SHA: check}); err != nil || out.Base != "main" || out.BaseID != base {
		t.Errorf("resolve: %+v, %v; want base main at %.7s", out, err, base)
	}
	if _, err := r.Rerun(ctx, RerunIn{RepoURL: url, Base: "feature", BaseID: check, Tip: check, Gate: "simplify"}); err == nil || !strings.Contains(err.Error(), "not this repository's base branch") {
		t.Errorf("rerun against feature: err = %v", err)
	}
	// A base commit that main never held (here: the check itself).
	if _, err := r.Rerun(ctx, RerunIn{RepoURL: url, Base: "main", BaseID: check, Tip: check, Gate: "simplify"}); err == nil || !strings.Contains(err.Error(), "never on main") {
		t.Errorf("rerun with a foreign base commit: err = %v", err)
	}
}

func TestRunnerRerunJudgesTheChainFirst(t *testing.T) {
	r, url, base, check := runnerFixture(t)
	// The code commit under the check has no check of its own for verify.
	code := strings.TrimSpace(run(t, "git", "-C", r.WorkDir, "--git-dir="+filepath.Join(strings.TrimPrefix(url, "file://")), "rev-parse", check+"^"))
	v, err := r.Rerun(context.Background(), RerunIn{RepoURL: url, Base: "main", BaseID: base, Tip: code, Gate: "simplify"})
	if err != nil || v.Pass || !strings.Contains(v.Reason, "missing") {
		t.Errorf("rerun on a tip without the check: %+v, %v; want a missing-check rejection", v, err)
	}
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}
