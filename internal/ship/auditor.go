package ship

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"go.temporal.io/sdk/temporal"

	"github.com/leighstillard/tardis-gate/gates"
	"github.com/leighstillard/tardis-gate/internal/chain"
	"github.com/leighstillard/tardis-gate/internal/ghapp"
	"github.com/leighstillard/tardis-gate/internal/manifest"
	"github.com/leighstillard/tardis-gate/internal/provider"
	"github.com/leighstillard/tardis-gate/internal/review"
)

// Auditor serves the auditor-side activities: it attests check commits,
// re-runs each gate's review blind through the base's provider, and posts
// check runs as the GitHub App. PRs are written to the log until build 5.
type Auditor struct {
	Repos     map[string]bool     // the repository URLs this auditor serves; any other is refused
	WorkDir   string              // clones live here, one per repository URL
	App       *ghapp.App          // posts check runs; nil writes them to Log
	Version   string              // this tardis, named on every check run
	RerunUser *syscall.Credential // an exec provider runs as this user; without one it is refused
	Getenv    func(string) string // provider keys; nil is os.Getenv
	Log       io.Writer

	mu     sync.Mutex
	locks  map[string]chan struct{} // one per repository URL, guarding its clone
	passed map[string]bool          // passKey of every re-run this auditor passed; also in WorkDir/passes
	// ponytail: one auditor host owns its passes file; replicas would need a
	// shared store.
}

// rerun is what judging one gate on one tip takes, all read from the base
// commit and the gate's check commit.
type rerun struct {
	m             *manifest.Manifest
	gate          manifest.Gate
	summary, tool string          // the worker's claim
	cfg           provider.Config // the reviewer, resolved against the claim's vendor
	key           string          // passKey
}

// load reads gate's policy at baseID and its valid check commit on tip.
//
// The pass key names what a re-run judged: the gate, the base commit it ran
// on, the gate's check commit, which fixes the claim and the code under it,
// the reviewer's config and this binary's reference gates, so a pass from a
// reviewer or prompt since replaced counts for nothing. Later checks are
// empty commits on top, so the final tip of a pass yields the same check
// commit as the tip the gate was re-run on.
func (r *Auditor) load(dir, repoURL, gate, baseID, tip string) (rerun, error) {
	m, err := manifest.LoadRev(dir, baseID)
	if err != nil {
		return rerun{}, fmt.Errorf("manifest on %.12s: %w", baseID, err)
	}
	i := slices.IndexFunc(m.Gates, func(g manifest.Gate) bool { return g.Name == gate })
	if i < 0 {
		return rerun{}, temporal.NewNonRetryableApplicationError("gate "+gate+" is not enabled on the base", "Malformed", nil)
	}
	check, err := chain.CheckCommit(dir, baseID, tip, gate)
	if err != nil {
		return rerun{}, err
	}
	summary, tool, err := chain.Claim(dir, check)
	if err != nil {
		return rerun{}, err
	}
	vendor, _, _ := strings.Cut(tool, "/")
	cfg := m.Provider.Resolve(vendor)
	sum := sha256.Sum256([]byte(strings.Join([]string{repoURL, gate, baseID, check, cfg.Digest(), gates.Digest, r.Version}, "\x00")))
	return rerun{m: m, gate: m.Gates[i], summary: summary, tool: tool, cfg: cfg, key: hex.EncodeToString(sum[:])}, nil
}

// differs is why a must_differ_from gate refuses the claim before any
// provider call, or "" if the reviewer's vendor differs from the worker's.
func (rr rerun) differs() string {
	vendor, _, _ := strings.Cut(rr.tool, "/")
	pv := rr.cfg.ModelVendor()
	switch {
	case len(rr.m.WorkerVendors) == 0:
		return "same vendor: cannot tell; set worker_vendors in .tardis/config.yml for a must_differ_from gate"
	case vendor == "other": // any vendor not in the fixed list, so it could be the reviewer's
		return "same vendor: cannot tell; the claim's vendor is \"other\""
	case !slices.Contains(rr.m.WorkerVendors, vendor):
		return fmt.Sprintf("same vendor: the claim's vendor %q is not in worker_vendors", vendor)
	case pv == "":
		return "same vendor: cannot tell the reviewer's vendor; set provider.vendor"
	case pv == vendor:
		return fmt.Sprintf("same vendor: the worker and the reviewer (%s) are both %s", rr.cfg, vendor)
	}
	return ""
}

// loadPasses reads WorkDir/passes once, so a restarted auditor still knows
// every re-run it passed. Call with r.mu held.
func (r *Auditor) loadPasses() error {
	if r.passed != nil {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(r.WorkDir, "passes"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	r.passed = map[string]bool{}
	for _, k := range strings.Fields(string(b)) {
		r.passed[k] = true
	}
	return nil
}

// recordPass notes a passed re-run, on disk before in memory.
func (r *Auditor) recordPass(key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.loadPasses(); err != nil {
		return err
	}
	if err := os.MkdirAll(r.WorkDir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(r.WorkDir, "passes"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(f, key)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	r.passed[key] = true
	return nil
}

func (r *Auditor) hasPass(key string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	err := r.loadPasses()
	return r.passed[key], err
}

// Remote refs are always named in full here: a tag called origin/main would
// otherwise win over the remote-tracking branch.

// checkBase refuses a base the auditor did not choose. The workflow, which an
// worker's machine can run, names the base; the auditor trusts only
// base_branch from the manifest on the repository's default branch, and only
// commits that branch has held.
func checkBase(ctx context.Context, dir, base, baseID string) error {
	m, err := manifest.LoadRev(dir, "refs/remotes/origin/HEAD")
	if err != nil {
		return fmt.Errorf("manifest on the default branch: %w", err)
	}
	if base != m.BaseBranch {
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("base %q is not this repository's base branch %q", base, m.BaseBranch), "Malformed", nil)
	}
	if ok, err := onFirstParent(ctx, dir, "refs/remotes/origin/"+base, baseID); err != nil {
		return err
	} else if !ok {
		return temporal.NewNonRetryableApplicationError(baseID+" was never "+base+" itself", "Malformed", nil)
	}
	return nil
}

// lock serialises work on one repository's clone, so a slow fetch of one
// repository never holds up another. Waiting ends with ctx: an activity that
// timed out must not queue behind one Temporal has already given up on.
func (r *Auditor) lock(ctx context.Context, repoURL string) (unlock func(), err error) {
	r.mu.Lock()
	if r.locks == nil {
		r.locks = map[string]chan struct{}{}
	}
	l := r.locks[repoURL]
	if l == nil {
		l = make(chan struct{}, 1)
		r.locks[repoURL] = l
	}
	r.mu.Unlock()
	select {
	case l <- struct{}{}:
		return func() { <-l }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// sync clones the repository once and fetches its branches; reads go through
// commit IDs, never the working tree.
func (r *Auditor) sync(ctx context.Context, repoURL string) (string, error) {
	// The URL comes from a workflow anyone with Temporal access can start;
	// clone only what the operator listed.
	if !r.Repos[repoURL] {
		return "", temporal.NewNonRetryableApplicationError(
			"repository "+repoURL+" is not served by this auditor (tardis auditor --repo)", "Malformed", nil)
	}
	sum := sha256.Sum256([]byte(repoURL))
	dir := filepath.Join(r.WorkDir, hex.EncodeToString(sum[:8]))
	// A clone killed half way must not poison the cache: a clone that is not
	// this repository's is replaced, and a new one is made aside and moved
	// into place only once complete.
	if url, err := gitOut(ctx, dir, "remote", "get-url", "origin"); err != nil || url != repoURL {
		if err := os.MkdirAll(r.WorkDir, 0o700); err != nil {
			return "", err
		}
		tmp, err := os.MkdirTemp(r.WorkDir, ".clone-")
		if err != nil {
			return "", err
		}
		if _, err := gitOut(ctx, "", "clone", "-q", "--no-checkout", repoURL, tmp); err != nil {
			os.RemoveAll(tmp)
			return "", err
		}
		if err := os.RemoveAll(dir); err != nil {
			return "", err
		}
		if err := os.Rename(tmp, dir); err != nil {
			return "", err
		}
	}
	if _, err := gitOut(ctx, dir, "fetch", "-q", "--prune", "origin", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return "", err
	}
	// fetch never moves origin/HEAD; ask the remote, since the default
	// branch's manifest names the base.
	if _, err := gitOut(ctx, dir, "remote", "set-head", "origin", "--auto"); err != nil {
		return "", err
	}
	return dir, nil
}

// Resolve pins the base branch's current commit and lists the gates that
// apply to sha under that commit's manifest. Every later step of the pass
// uses the pinned commit, so the policy cannot change half way through. The
// base branch is the one the default branch's manifest names, never one the
// request brings: a branch cannot pick the policy it is held to.
func (r *Auditor) Resolve(ctx context.Context, in ResolveIn) (ResolveOut, error) {
	defer heartbeat(ctx)()
	unlock, err := r.lock(ctx, in.RepoURL)
	if err != nil {
		return ResolveOut{}, err
	}
	defer unlock()
	dir, err := r.sync(ctx, in.RepoURL)
	if err != nil {
		return ResolveOut{}, err
	}
	def, err := manifest.LoadRev(dir, "refs/remotes/origin/HEAD")
	if err != nil {
		return ResolveOut{}, fmt.Errorf("manifest on the default branch: %w", err)
	}
	base := def.BaseBranch
	baseID, err := manifest.CommitID(dir, "refs/remotes/origin/"+base)
	if err != nil {
		return ResolveOut{}, err
	}
	applied, err := gatesFor(dir, base, baseID, in.SHA)
	if err != nil {
		return ResolveOut{}, err
	}
	out := ResolveOut{Base: base, BaseID: baseID, GatesDigest: gates.Digest}
	for _, g := range applied {
		out.Gates = append(out.Gates, GateInfo{Name: g.Name, Timeout: g.Timeout, Retry: g.Retry})
	}
	return out, nil
}

// gatesFor lists the gates that apply to sha under baseID's manifest. sha
// must contain baseID and its own policy must load (manifest.CheckHead).
func gatesFor(dir, base, baseID, sha string) ([]manifest.Gate, error) {
	m, err := manifest.LoadRev(dir, baseID)
	if err != nil {
		return nil, fmt.Errorf("manifest on %s: %w", base, err)
	}
	if err := manifest.CheckHead(dir, baseID, sha); errors.Is(err, manifest.ErrBehind) {
		return nil, temporal.NewNonRetryableApplicationError(fmt.Sprintf("%.12s does not contain %s; rebase onto %s and request again", sha, base, base), "Malformed", nil)
	} else if ge := (*manifest.GitError)(nil); errors.As(err, &ge) {
		return nil, err
	} else if err != nil {
		return nil, temporal.NewNonRetryableApplicationError(fmt.Sprintf("%.12s: %v", sha, err), "Malformed", nil)
	}
	changed, err := manifest.Changed(dir, baseID, sha)
	if err != nil {
		return nil, err
	}
	return m.Resolve(changed), nil
}

// Attest judges the check commits for gates on tip.
func (r *Auditor) Attest(ctx context.Context, in AttestIn) (map[string]string, error) {
	defer heartbeat(ctx)()
	unlock, err := r.lock(ctx, in.RepoURL)
	if err != nil {
		return nil, err
	}
	defer unlock()
	dir, err := r.sync(ctx, in.RepoURL)
	if err != nil {
		return nil, err
	}
	return chain.Verify(dir, in.BaseID, in.Tip, in.Gates)
}

// Rerun reviews the diff blind through the base's provider, then has the
// judge compare the findings with the worker's claim. A finding the claim
// misses, or a statement in it the findings contradict, rejects the gate.
func (r *Auditor) Rerun(ctx context.Context, in RerunIn) (Verdict, error) {
	defer heartbeat(ctx)()
	// The lock is held through the review, which reads the shared clone.
	// ponytail: serial per repository; release it after reading the diff if runs queue up.
	unlock, err := r.lock(ctx, in.RepoURL)
	if err != nil {
		return Verdict{}, err
	}
	defer unlock()
	dir, err := r.sync(ctx, in.RepoURL)
	if err != nil {
		return Verdict{}, err
	}
	if err := checkBase(ctx, dir, in.Base, in.BaseID); err != nil {
		return Verdict{}, err
	}
	// Judge the claim here too: a pass is recorded only for a valid chain.
	if st, err := chain.Verify(dir, in.BaseID, in.Tip, []string{in.Gate}); err != nil {
		return Verdict{}, err
	} else if st[in.Gate] != chain.Valid {
		return Verdict{Reason: claimReason(st[in.Gate])}, nil
	}
	rr, err := r.load(dir, in.RepoURL, in.Gate, in.BaseID, in.Tip)
	if err != nil {
		return Verdict{}, err
	}
	if rr.gate.MustDifferFrom != "" {
		if why := rr.differs(); why != "" {
			return Verdict{Reason: why}, nil
		}
	}
	// Activities run at least once: a retry after the pass was recorded but
	// its completion lost gets that pass, not a second, costly review.
	if done, err := r.hasPass(rr.key); err != nil {
		return Verdict{}, err
	} else if done {
		return Verdict{Pass: true}, nil
	}
	diff := &capped{max: review.MaxDiff}
	if err := gitTo(ctx, dir, diff, "diff", in.BaseID+"..."+in.Tip); diff.over {
		return Verdict{Reason: review.ErrTooBig.Error()}, nil
	} else if err != nil {
		return Verdict{}, err
	}
	var runbook string
	if in.Gate == "verify" && rr.m.VerifyRunbook != "" {
		rb := &capped{max: review.MaxRunbook}
		if err := gitTo(ctx, dir, rb, "show", in.BaseID+":"+rr.m.VerifyRunbook); rb.over {
			return Verdict{Reason: fmt.Sprintf("runbook %s is over %d KiB", rr.m.VerifyRunbook, review.MaxRunbook>>10)}, nil
		} else if err != nil {
			return Verdict{}, err
		}
		runbook = rb.String()
	}
	if rr.cfg.Type == "exec" && r.RerunUser == nil {
		return Verdict{}, temporal.NewNonRetryableApplicationError(
			"the base's provider is a command; this auditor runs one only as another user (tardis auditor --rerun-user)", "Malformed", nil)
	}
	getenv := r.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	ask, err := provider.New(rr.cfg, getenv, r.RerunUser)
	if err != nil {
		return Verdict{}, temporal.NewNonRetryableApplicationError(err.Error(), "Malformed", nil)
	}
	findings, err := review.Blind(ctx, ask, rr.gate.Prompt, diff.String(), runbook)
	if err != nil {
		return Verdict{}, err
	}
	if len(findings) > 0 { // nothing found leaves nothing for the claim to miss
		missed, disputed, err := review.Judge(ctx, ask, rr.summary, findings)
		if err != nil {
			return Verdict{}, err
		}
		if len(missed)+len(disputed) > 0 {
			oneLine := func(v []string) string { return strings.Join(strings.Fields(strings.Join(v, "; ")), " ") }
			var why []string
			if len(missed) > 0 {
				why = append(why, "missed: "+oneLine(missed))
			}
			if len(disputed) > 0 {
				why = append(why, "disputed: "+oneLine(disputed))
			}
			// ponytail: model text is unbounded; keep it well under Temporal's 2 MiB payload limit
			return Verdict{Reason: trunc(strings.Join(why, "; "), 8<<10) + "\n\nFindings:\n" + trunc(review.Render(findings, 50), 48<<10) + "\nReviewer: " + rr.cfg.String()}, nil
		}
	}
	if err := r.recordPass(rr.key); err != nil {
		return Verdict{}, err
	}
	return Verdict{Pass: true}, nil
}

// trunc cuts s to at most n bytes on a rune boundary.
func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// capped keeps the first max bytes written to it and notes any more.
type capped struct {
	bytes.Buffer
	max  int
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if c.Len()+len(p) > c.max {
		c.over = true
		return 0, review.ErrTooBig // stops git
	}
	return c.Buffer.Write(p)
}

// PostCheck posts a tardis/<gate> check run as the App. A failure is posted
// as asked: it can only block a merge. A success is posted only if this
// auditor passed a re-run of the gate on the same diff and the chain on the
// SHA is valid, so a workflow run by anyone else cannot produce one. The
// check names this tardis, its reference gates and, on a success, the
// reviewer, so it is clear which policy passed it.
func (r *Auditor) PostCheck(ctx context.Context, in CheckIn) error {
	if !r.Repos[in.RepoURL] { // failures too: only listed repositories get checks
		return temporal.NewNonRetryableApplicationError(
			"repository "+in.RepoURL+" is not served by this auditor (tardis auditor --repo)", "Malformed", nil)
	}
	defer heartbeat(ctx)()
	footer := fmt.Sprintf("tardis %s, reference gates %.12s", r.Version, gates.Digest)
	if in.Conclusion == "success" {
		unlock, err := r.lock(ctx, in.RepoURL)
		if err != nil {
			return err
		}
		defer unlock()
		dir, err := r.sync(ctx, in.RepoURL)
		if err != nil {
			return err
		}
		if err := checkBase(ctx, dir, in.Base, in.BaseID); err != nil {
			return err
		}
		passed, rr, why, err := r.passedHere(dir, in.RepoURL, in.Gate, in.BaseID, in.SHA)
		if err != nil {
			return err
		}
		if !passed {
			return temporal.NewNonRetryableApplicationError(fmt.Sprintf(
				"refusing tardis/%s success on %s: %s", in.Gate, in.SHA, why), "Rejected", nil)
		}
		in.Summary = "passed"
		footer += ", reviewer " + rr.cfg.String()
	}
	first, _, _ := strings.Cut(in.Summary, "\n")
	c := ghapp.CheckRun{Name: "tardis/" + in.Gate, Conclusion: in.Conclusion, Title: first, Summary: first,
		Text: strings.TrimSpace(in.Summary) + "\n\n" + footer}
	if repo, ok := ghapp.Repo(in.RepoURL); ok && r.App != nil {
		return r.App.PostCheck(ctx, repo, in.SHA, c)
	}
	fmt.Fprintf(r.Log, "check %s %s on %s: %s (no GitHub App for %s)\n", c.Name, c.Conclusion, in.SHA, first, in.RepoURL)
	return nil
}

// passedHere reports whether gate holds a valid check on sha and this
// auditor recorded a pass of it on baseID; if not, why.
func (r *Auditor) passedHere(dir, repoURL, gate, baseID, sha string) (bool, rerun, string, error) {
	st, err := chain.Verify(dir, baseID, sha, []string{gate})
	if err != nil {
		return false, rerun{}, "", err
	}
	if st[gate] != chain.Valid {
		return false, rerun{}, claimReason(st[gate]), nil
	}
	rr, err := r.load(dir, repoURL, gate, baseID, sha)
	if err != nil {
		return false, rerun{}, "", err
	}
	passed, err := r.hasPass(rr.key)
	if err != nil || !passed {
		return false, rr, fmt.Sprintf("no re-run by %s passed here on %.12s", rr.cfg, baseID), err
	}
	return true, rr, "", nil
}

// OpenPR records the PR the App will open, only while the branch still points
// at the head the gates passed on and the base at the commit they were
// resolved on: anything pushed to either since has not been reviewed.
// ponytail: log line until the GitHub App lands; there, the check and the
// open are two calls, and the merge (build 5) is pinned to the SHA.
func (r *Auditor) OpenPR(ctx context.Context, in OpenPRIn) (string, error) {
	defer heartbeat(ctx)()
	unlock, err := r.lock(ctx, in.RepoURL)
	if err != nil {
		return "", err
	}
	defer unlock()
	dir, err := r.sync(ctx, in.RepoURL)
	if err != nil {
		return "", err
	}
	if now, err := gitOut(ctx, dir, "rev-parse", "refs/remotes/origin/"+in.Branch); err != nil {
		return "", err
	} else if now != in.Head {
		return "", temporal.NewNonRetryableApplicationError(
			in.Branch+" is at "+now+", not the reviewed "+in.Head, "BranchMoved", nil)
	}
	// The gates were resolved on BaseID; a base that moved since may hold a
	// policy they never ran under.
	if now, err := gitOut(ctx, dir, "rev-parse", "refs/remotes/origin/"+in.Base); err != nil {
		return "", err
	} else if now != in.BaseID {
		return "", temporal.NewNonRetryableApplicationError(
			in.Base+" is at "+now+", not "+in.BaseID+", where the gates were resolved", "BaseMoved", nil)
	}
	if err := checkBase(ctx, dir, in.Base, in.BaseID); err != nil {
		return "", err
	}
	// The workflow is not trusted to say the gates passed. Every gate the
	// current base requires must hold a valid check on this head and a pass
	// this auditor recorded on this base: one earned on any other base opens
	// nothing.
	applied, err := gatesFor(dir, in.Base, in.BaseID, in.Head)
	if err != nil {
		return "", err
	}
	for _, g := range applied {
		passed, _, why, err := r.passedHere(dir, in.RepoURL, g.Name, in.BaseID, in.Head)
		if err != nil {
			return "", err
		}
		if !passed {
			return "", temporal.NewNonRetryableApplicationError(fmt.Sprintf(
				"refusing to open a PR: %s on %.12s: %s", g.Name, in.Head, why), "Rejected", nil)
		}
	}
	fmt.Fprintf(r.Log, "open-pr %s into %s at %s\n", in.Branch, in.Base, in.Head)
	// Not a URL: no PR exists until the GitHub App lands (build 5).
	return "stand-in:" + in.Branch + "@" + in.Head[:min(12, len(in.Head))] + " (no PR opened)", nil
}

// gitOut runs git, killed if ctx ends (an activity's deadline or cancellation).
// Replacement objects are off: a local refs/replace entry must not change
// what a commit ID means.
func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	var stdout bytes.Buffer
	if err := gitTo(ctx, dir, &stdout, args...); err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}

// gitTo is gitOut writing git's output to w as it comes.
func gitTo(ctx context.Context, dir string, w io.Writer, args ...string) error {
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	args = append([]string{"--no-replace-objects"}, args...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = chain.GitEnv() // a hook's GIT_DIR must not override -C
	// Cancelling kills git's helpers (ssh, credential helpers) too, and a
	// helper that keeps the pipe open cannot hold the activity, or the
	// repository lock, past a few seconds.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = w, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(stderr.String()))
	}
	return nil
}
