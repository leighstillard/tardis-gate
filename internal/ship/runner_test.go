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
	// Failures are posted as asked: they can only block.
	if err := r.PostCheck(ctx, CheckIn{RepoURL: url, SHA: check, Gate: "verify", Conclusion: "failure"}); err != nil {
		t.Errorf("failure: %v", err)
	}
}

func TestRunnerRefusesABaseItDidNotChoose(t *testing.T) {
	r, url, _, check := runnerFixture(t)
	ctx := context.Background()
	if _, err := r.Resolve(ctx, ResolveIn{RepoURL: url, Base: "feature", SHA: check}); err == nil || !strings.Contains(err.Error(), "not this repository's base branch") {
		t.Errorf("resolve against feature: err = %v", err)
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
