package ship

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"

	"github.com/leighstillard/tardis-gate/gates"
	"github.com/leighstillard/tardis-gate/internal/chain"
)

// fakeModel is an OpenAI-compatible endpoint that answers the blind review
// with blind and the judge with judge, counting calls.
type fakeModel struct {
	blind, judge string
	calls        atomic.Int32
}

// config is the provider section pointing at m; the model is openai's.
func (m *fakeModel) config(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.calls.Add(1)
		var in struct{ Messages []struct{ Content string } }
		json.NewDecoder(r.Body).Decode(&in)
		answer := m.blind
		if strings.HasPrefix(in.Messages[0].Content, "You compare") {
			answer = m.judge
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": answer}}}})
	}))
	t.Cleanup(srv.Close)
	return "provider:\n  base_url: " + srv.URL + "\n  model: openai/m\n  vendor: openai\n"
}

// auditorFixture is workerRepo's origin with a valid simplify check on top of
// the feature branch, reviewed by a model that finds nothing. It returns
// the auditor, the origin URL, main's commit and the check commit.
func auditorFixture(t *testing.T) (*Auditor, string, string, string) {
	t.Helper()
	r, url, base, check, _ := modelFixture(t, "simplify", "")
	return r, url, base, check
}

// modelFixture is auditorFixture for gate, with config added to the base's
// and the model returned for the test to script.
func modelFixture(t *testing.T, gate, config string) (*Auditor, string, string, string, *fakeModel) {
	t.Helper()
	m := &fakeModel{blind: "[]", judge: `{"missed": [], "disputed": []}`}
	a, git := workerRepo(t, config+m.config(t)) // config first: it may add gates
	check, err := chain.Commit(a.Dir, gate, "ok", a.Tool)
	if err != nil {
		t.Fatal(err)
	}
	git("push", "-q", "origin", "feature")
	url := git("remote", "get-url", "origin")
	r := &Auditor{Repos: map[string]bool{url: true}, WorkDir: t.TempDir(), Log: io.Discard}
	return r, url, git("rev-parse", "main"), check, m
}

func TestAuditorPostsSuccessOnlyForItsOwnPass(t *testing.T) {
	r, url, base, check := auditorFixture(t)
	ctx := context.Background()
	success := CheckIn{RepoURL: url, Base: "main", BaseID: base, SHA: check, Gate: "simplify", Conclusion: "success"}

	// A workflow asking for a success the auditor never re-ran is refused.
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
	// A restarted auditor still knows what it passed.
	restarted := &Auditor{Repos: r.Repos, WorkDir: r.WorkDir, Log: io.Discard}
	if err := restarted.PostCheck(ctx, success); err != nil {
		t.Errorf("success after a restart: %v", err)
	}
	// Failures are posted as asked: they can only block.
	if err := r.PostCheck(ctx, CheckIn{RepoURL: url, SHA: check, Gate: "verify", Conclusion: "failure"}); err != nil {
		t.Errorf("failure: %v", err)
	}
	if err := r.PostCheck(ctx, CheckIn{RepoURL: url + ".evil", SHA: check, Gate: "verify", Conclusion: "failure"}); err == nil {
		t.Error("posted a check for a repository the auditor does not serve")
	}
	if err := r.PostCheck(ctx, CheckIn{RepoURL: url, SHA: check, Gate: "verify", Conclusion: "neutral"}); err == nil {
		t.Error("posted a neutral check, which satisfies a required check without a re-run")
	}
}

func TestAuditorPassIsBoundToItsBaseAndClaim(t *testing.T) {
	r, url, _, check := auditorFixture(t)
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
	// Nor does it once the base has moved past m1: m2 may require more.
	var ae *temporal.ApplicationError
	if err := success(m1, check); !errors.As(err, &ae) || ae.Type() != "BaseMoved" {
		t.Errorf("success on m1 after main moved to m2: %v; want BaseMoved", err)
	}
	if v, err := r.Rerun(ctx, RerunIn{RepoURL: url, Base: "main", BaseID: m2, Tip: check, Gate: "simplify"}); err != nil || !v.Pass {
		t.Fatalf("rerun on m2: %+v, %v", v, err)
	}
	if err := success(m2, check); err != nil {
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
	if err := success(m2, rewritten); err == nil {
		t.Error("a pass for one check commit authorised a success for a rewritten one")
	}
}

func TestAuditorFollowsTheDefaultBranch(t *testing.T) {
	r, url, _, check := auditorFixture(t)
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
	// The head was cut from main, so the auditor, now reading trunk's
	// policy, wants it rebased onto trunk.
	if _, err := r.Resolve(ctx, ResolveIn{RepoURL: url, SHA: check}); err == nil || !strings.Contains(err.Error(), "rebase onto trunk") {
		t.Errorf("resolve after the default branch moved: %v; want trunk as the base", err)
	}
}

func TestOpenPRRefusesAMovedBase(t *testing.T) {
	r, url, base, check := auditorFixture(t)
	ctx := context.Background()
	open := func(baseID string) error {
		_, err := r.OpenPR(ctx, OpenPRIn{RepoURL: url, Branch: "feature", Base: "main", BaseID: baseID, Head: check})
		return err
	}
	var ae *temporal.ApplicationError
	if err := open(base); !errors.As(err, &ae) || ae.Type() != "Rejected" {
		t.Errorf("open before any re-run: err = %v; want Rejected", err)
	}
	if v, err := r.Rerun(ctx, RerunIn{RepoURL: url, Base: "main", BaseID: base, Tip: check, Gate: "simplify"}); err != nil || !v.Pass {
		t.Fatalf("rerun: %+v, %v", v, err)
	}
	if err := open(base); err != nil {
		t.Fatalf("open on the resolved base after a pass: %v", err)
	}
	w := t.TempDir()
	run(t, "git", "-C", w, "clone", "-q", url, ".")
	run(t, "git", "-C", w, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "main moves")
	run(t, "git", "-C", w, "push", "-q", "origin", "main")
	if err := open(base); !errors.As(err, &ae) || ae.Type() != "BaseMoved" {
		t.Errorf("open after main moved: err = %v; want BaseMoved", err)
	}
	// A pass earned on the old base opens nothing on the new one.
	now := strings.TrimSpace(run(t, "git", "-C", w, "rev-parse", "HEAD"))
	if err := open(now); err == nil {
		t.Error("a pass on the old base opened a PR on the new one")
	}
}

func TestAuditorRefusesAHeadThatWouldStrandTheBase(t *testing.T) {
	r, url, _, _ := auditorFixture(t)
	w := t.TempDir()
	g := func(args ...string) string {
		return strings.TrimSpace(run(t, "git", append([]string{"-C", w, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...))
	}
	g("clone", "-q", url, ".")
	g("checkout", "-q", "feature")
	g("rm", "-q", ".tardis/config.yml")
	g("commit", "-q", "-m", "unenrol")
	g("push", "-q", "origin", "feature")
	if _, err := r.Resolve(context.Background(), ResolveIn{RepoURL: url, SHA: g("rev-parse", "HEAD")}); err == nil || !strings.Contains(err.Error(), "not enrolled") {
		t.Errorf("resolve on a head that removes the config: err = %v", err)
	}
}

func TestAuditorRefusesAMergedSideCommitAsBase(t *testing.T) {
	// Reachable from main through a merge, but main never pointed at it.
	r, url, _, check := auditorFixture(t)
	w := t.TempDir()
	g := func(args ...string) string {
		return strings.TrimSpace(run(t, "git", append([]string{"-C", w, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...))
	}
	g("clone", "-q", url, ".")
	g("checkout", "-q", "-b", "side", "origin/main")
	g("commit", "-q", "--allow-empty", "-m", "side")
	side := g("rev-parse", "HEAD")
	g("checkout", "-q", "main")
	g("merge", "-q", "--no-ff", "-m", "merge side", "side")
	g("push", "-q", "origin", "main")
	if _, err := r.Rerun(context.Background(), RerunIn{RepoURL: url, Base: "main", BaseID: side, Tip: check, Gate: "simplify"}); err == nil || !strings.Contains(err.Error(), "was never main itself") {
		t.Errorf("rerun with a merged side commit as base: err = %v", err)
	}
}

func TestAuditorIgnoresATagNamedLikeTheBase(t *testing.T) {
	// A tag called origin/main would win over the remote-tracking branch if
	// the auditor named refs in short.
	r, url, base, check := auditorFixture(t)
	run(t, "git", "--git-dir="+strings.TrimPrefix(url, "file://"), "tag", "origin/main", check)
	run(t, "git", "--git-dir="+strings.TrimPrefix(url, "file://"), "tag", "origin/HEAD", check)
	out, err := r.Resolve(context.Background(), ResolveIn{RepoURL: url, SHA: check})
	if err != nil || out.BaseID != base {
		t.Errorf("resolve with tags named origin/main and origin/HEAD: %+v, %v; want base %.7s", out, err, base)
	}
}

func TestAPassUnderReplacedReferenceGatesCountsForNothing(t *testing.T) {
	r, url, base, check := auditorFixture(t)
	ctx := context.Background()
	if v, err := r.Rerun(ctx, RerunIn{RepoURL: url, Base: "main", BaseID: base, Tip: check, Gate: "simplify"}); err != nil || !v.Pass {
		t.Fatalf("rerun: %+v, %v", v, err)
	}
	defer func(d string) { gates.Digest = d }(gates.Digest)
	gates.Digest = "a tardis with other prompts"
	if err := r.PostCheck(ctx, CheckIn{RepoURL: url, Base: "main", BaseID: base, SHA: check, Gate: "simplify", Conclusion: "success"}); err == nil {
		t.Error("a pass under the old reference gates authorised a success under new ones")
	}
}

func TestRerunIsNotRepeatedOnceRecorded(t *testing.T) {
	r, url, base, check, m := modelFixture(t, "simplify", "")
	ctx := context.Background()
	in := RerunIn{RepoURL: url, Base: "main", BaseID: base, Tip: check, Gate: "simplify"}
	if v, err := r.Rerun(ctx, in); err != nil || !v.Pass {
		t.Fatalf("rerun: %+v, %v", v, err)
	}
	if n := m.calls.Load(); n != 1 {
		t.Errorf("%d model calls for a review with no findings; want 1, no judge", n)
	}
	// The retry of an attempt whose completion was lost must not ask again.
	m.blind = `[{"path": "a.txt", "line": 1, "severity": "high", "claim": "new"}]`
	if v, err := r.Rerun(ctx, in); err != nil || !v.Pass {
		t.Errorf("retried rerun: %+v, %v; want the recorded pass", v, err)
	}
	if n := m.calls.Load(); n != 1 {
		t.Errorf("%d model calls after the retry; want still 1", n)
	}
}

func TestRerunRejectsWhatTheClaimMisses(t *testing.T) {
	r, url, base, check, m := modelFixture(t, "simplify", "")
	m.blind = "Here you go:\n```json\n" + `[{"path": "a.txt", "line": 1, "severity": "high", "claim": "a is wrong"}]` + "\n```"
	m.judge = `{"missed": ["a.txt:1 a is wrong"], "disputed": ["\"ok\""]}`
	v, err := r.Rerun(context.Background(), RerunIn{RepoURL: url, Base: "main", BaseID: base, Tip: check, Gate: "simplify"})
	first, rest, _ := strings.Cut(v.Reason, "\n")
	if err != nil || v.Pass || first != `missed: a.txt:1 a is wrong; disputed: "ok"` ||
		!strings.Contains(rest, "a.txt:1 [high] a is wrong") || !strings.Contains(rest, "Reviewer: openaicompat openai/m") {
		t.Errorf("rerun: %+v, %v", v, err)
	}
	if n := m.calls.Load(); n != 2 {
		t.Errorf("%d model calls; want blind and judge", n)
	}
}

func TestMustDifferFromRefusesBeforeAnyCall(t *testing.T) {
	// The worker's tool is anthropic/a/b.
	for name, tc := range map[string]struct{ config, tool, want string }{
		"no worker_vendors":   {"", "", "set worker_vendors"},
		"unlisted vendor":     {"worker_vendors: [openai]\n", "", `"anthropic" is not in worker_vendors`},
		"same vendor":         {"worker_vendors: [anthropic, openai]\n", "openai/x/y", "both openai"},
		"other is anyone":     {"worker_vendors: [other]\n", "other/x/y", `vendor is "other"`},
		"reviewer is another": {"worker_vendors: [anthropic]\n", "", "pass"},
	} {
		t.Run(name, func(t *testing.T) {
			config := "  - name: review\n" + tc.config
			r, url, base, check, m := modelFixture(t, "review", config)
			if tc.tool != "" { // rewrite the claim's tool
				w := t.TempDir()
				run(t, "git", "-C", w, "clone", "-q", "-b", "feature", url, ".")
				run(t, "git", "-C", w, "reset", "-q", "--hard", "HEAD^")
				var err error
				if check, err = chain.Commit(w, "review", "ok", tc.tool); err != nil {
					t.Fatal(err)
				}
				run(t, "git", "-C", w, "push", "-q", "--force", "origin", "feature")
			}
			v, err := r.Rerun(context.Background(), RerunIn{RepoURL: url, Base: "main", BaseID: base, Tip: check, Gate: "review"})
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "pass" {
				if !v.Pass {
					t.Errorf("rerun: %+v; want a pass", v)
				}
				return
			}
			if v.Pass || !strings.HasPrefix(v.Reason, "same vendor") || !strings.Contains(v.Reason, tc.want) || m.calls.Load() != 0 {
				t.Errorf("rerun: %+v after %d calls; want a same-vendor refusal naming %q and no call", v, m.calls.Load(), tc.want)
			}
		})
	}
}

func TestAuditorLockGivesUpWithItsContext(t *testing.T) {
	r, url, _, _ := auditorFixture(t)
	unlock, err := r.lock(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := r.lock(ctx, url); err == nil {
		t.Error("a second lock on a held repository did not give up with its context")
	}
}

func TestAuditorRecoversFromAnInterruptedClone(t *testing.T) {
	// A clone killed half way leaves a .git behind; the auditor must not
	// keep failing on it.
	r, url, base, check := auditorFixture(t)
	sum := sha256.Sum256([]byte(url))
	if err := os.MkdirAll(filepath.Join(r.WorkDir, hex.EncodeToString(sum[:8]), ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := r.Resolve(context.Background(), ResolveIn{RepoURL: url, SHA: check}); err != nil || out.BaseID != base {
		t.Errorf("resolve over a broken clone: %+v, %v", out, err)
	}
}

func TestGitOutStopsWithItsContext(t *testing.T) {
	// A helper git starts (ssh, a credential helper) that keeps the pipe
	// open must not hold a cancelled call, or the repository lock.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := gitOut(ctx, "", "-c", "alias.hang=!sh -c 'sleep 60 & sleep 60'", "hang")
	if err == nil || time.Since(start) > 8*time.Second {
		t.Errorf("gitOut returned %v after %v; want an error within a few seconds", err, time.Since(start))
	}
}

func TestAuditorRefusesABaseItDidNotChoose(t *testing.T) {
	r, url, base, check := auditorFixture(t)
	ctx := context.Background()
	// The base comes from the default branch's manifest, not the request.
	if out, err := r.Resolve(ctx, ResolveIn{RepoURL: url, SHA: check}); err != nil || out.Base != "main" || out.BaseID != base {
		t.Errorf("resolve: %+v, %v; want base main at %.7s", out, err, base)
	}
	if _, err := r.Rerun(ctx, RerunIn{RepoURL: url, Base: "feature", BaseID: check, Tip: check, Gate: "simplify"}); err == nil || !strings.Contains(err.Error(), "not this repository's base branch") {
		t.Errorf("rerun against feature: err = %v", err)
	}
	// A base commit that main never held (here: the check itself).
	if _, err := r.Rerun(ctx, RerunIn{RepoURL: url, Base: "main", BaseID: check, Tip: check, Gate: "simplify"}); err == nil || !strings.Contains(err.Error(), "was never main itself") {
		t.Errorf("rerun with a foreign base commit: err = %v", err)
	}
}

func TestAuditorRerunJudgesTheChainFirst(t *testing.T) {
	r, url, base, check := auditorFixture(t)
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
