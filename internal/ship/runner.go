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
	"strings"
	"sync"

	"go.temporal.io/sdk/temporal"

	"github.com/leighstillard/tardis-gate/internal/chain"
	"github.com/leighstillard/tardis-gate/internal/executor"
	"github.com/leighstillard/tardis-gate/internal/manifest"
)

// Runner serves the runner-side activities. This build attests check commits
// for real; the independent re-run is an operator-configured command, and
// check runs and PRs are written to the log until the GitHub App lands.
type Runner struct {
	Repos    map[string]bool // the repository URLs this runner serves; any other is refused
	WorkDir  string          // clones live here, one per repository URL
	RerunCmd []string        // run per gate; exit 0 passes, 1 rejects; empty rejects everything
	Exec     executor.Executor
	Log      io.Writer

	mu     sync.Mutex
	locks  map[string]chan struct{} // one per repository URL, guarding its clone
	passed map[string]bool          // passKey of every re-run this runner passed; also in WorkDir/passes
	// ponytail: one runner host owns its passes file; replicas would need a
	// shared store.
}

// passKey names what a re-run judged: the gate, the base commit it ran on,
// and the gate's check commit, which fixes the claim and the code under it.
// Later checks are empty commits on top, so the final tip of a pass yields
// the same check commit as the tip the gate was re-run on.
func passKey(dir, repoURL, gate, baseID, tip string) (string, error) {
	check, err := chain.CheckCommit(dir, baseID, tip, gate)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{repoURL, gate, baseID, check}, "\x00")))
	return hex.EncodeToString(sum[:]), nil
}

// loadPasses reads WorkDir/passes once, so a restarted runner still knows
// every re-run it passed. Call with r.mu held.
func (r *Runner) loadPasses() error {
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
func (r *Runner) recordPass(key string) error {
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

func (r *Runner) hasPass(key string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	err := r.loadPasses()
	return r.passed[key], err
}

// Remote refs are always named in full here: a tag called origin/main would
// otherwise win over the remote-tracking branch.

// checkBase refuses a base the runner did not choose. The workflow, which an
// author's machine can run, names the base; the runner trusts only
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
func (r *Runner) lock(ctx context.Context, repoURL string) (unlock func(), err error) {
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
func (r *Runner) sync(ctx context.Context, repoURL string) (string, error) {
	// The URL comes from a workflow anyone with Temporal access can start;
	// clone only what the operator listed.
	if !r.Repos[repoURL] {
		return "", temporal.NewNonRetryableApplicationError(
			"repository "+repoURL+" is not served by this runner (tardis runner --repo)", "Malformed", nil)
	}
	sum := sha256.Sum256([]byte(repoURL))
	dir := filepath.Join(r.WorkDir, hex.EncodeToString(sum[:8]))
	if _, err := os.Stat(filepath.Join(dir, ".git")); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(r.WorkDir, 0o700); err != nil {
			return "", err
		}
		if _, err := gitOut(ctx, "", "clone", "-q", "--no-checkout", repoURL, dir); err != nil {
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
func (r *Runner) Resolve(ctx context.Context, in ResolveIn) (ResolveOut, error) {
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
	gates, err := gatesFor(dir, base, baseID, in.SHA)
	if err != nil {
		return ResolveOut{}, err
	}
	out := ResolveOut{Base: base, BaseID: baseID}
	for _, g := range gates {
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
func (r *Runner) Attest(ctx context.Context, in AttestIn) (map[string]string, error) {
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

// Rerun is this build's stand-in for the independent review: the operator's
// command, never one from the repository under review.
func (r *Runner) Rerun(ctx context.Context, in RerunIn) (Verdict, error) {
	if len(r.RerunCmd) == 0 {
		return Verdict{Reason: "this runner has no re-run configured"}, nil
	}
	defer heartbeat(ctx)()
	// The lock is held through the command: re-runs of one repository share
	// its clone, so they take turns, each on a clean checkout of the base.
	// ponytail: serial per repository; a worktree per job if runs queue up.
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
		return Verdict{Reason: "check commit " + st[in.Gate]}, nil
	}
	// Activities run at least once: a retry after the pass was recorded but
	// its completion lost gets that pass, not a second, costly review.
	key, err := passKey(dir, in.RepoURL, in.Gate, in.BaseID, in.Tip)
	if err != nil {
		return Verdict{}, err
	}
	if done, err := r.hasPass(key); err != nil {
		return Verdict{}, err
	} else if done {
		return Verdict{Pass: true}, nil
	}
	if _, err := gitOut(ctx, dir, "checkout", "-q", "--force", "--detach", in.BaseID); err != nil {
		return Verdict{}, err
	}
	if _, err := gitOut(ctx, dir, "clean", "-q", "-ffdx"); err != nil {
		return Verdict{}, err
	}
	home, err := os.MkdirTemp("", "tardis-rerun-")
	if err != nil {
		return Verdict{}, err
	}
	defer os.RemoveAll(home)
	res, err := r.Exec.Run(ctx, executor.Job{
		Argv: r.RerunCmd,
		Dir:  dir,
		Env: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home,
			"TARDIS_GATE=" + in.Gate, "TARDIS_SHA=" + in.Tip, "TARDIS_BASE=" + in.BaseID},
		// None of the runner's environment, and an empty HOME. It still runs
		// as the runner's user; the provider re-run (build 4) gets a sandbox.
		Clean: true,
	}, func(string) {})
	if err != nil {
		return Verdict{}, err
	}
	switch res.ExitCode {
	case 0:
		if err := r.recordPass(key); err != nil {
			return Verdict{}, err
		}
		return Verdict{Pass: true}, nil
	case 1:
		return Verdict{Reason: tail(res.Output)}, nil
	}
	return Verdict{}, fmt.Errorf("re-run exited %d: %s", res.ExitCode, tail(res.Output))
}

// PostCheck records a check run. A failure is posted as asked: it can only
// block a merge. A success is posted only if this runner passed a re-run of
// the gate on the same diff and the chain on the SHA is valid, so a workflow
// run by anyone else cannot produce one.
// ponytail: log line until the GitHub App posts them.
func (r *Runner) PostCheck(ctx context.Context, in CheckIn) error {
	if in.Conclusion == "success" {
		defer heartbeat(ctx)()
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
		st, err := chain.Verify(dir, in.BaseID, in.SHA, []string{in.Gate})
		if err != nil {
			return err
		}
		key, err := passKey(dir, in.RepoURL, in.Gate, in.BaseID, in.SHA)
		if err != nil {
			return err
		}
		passed, err := r.hasPass(key)
		if err != nil {
			return err
		}
		if st[in.Gate] != chain.Valid || !passed {
			return temporal.NewNonRetryableApplicationError(fmt.Sprintf(
				"refusing tardis/%s success on %s: chain %s, re-run passed here: %v", in.Gate, in.SHA, st[in.Gate], passed), "Rejected", nil)
		}
	}
	fmt.Fprintf(r.Log, "check tardis/%s %s on %s %s\n", in.Gate, in.Conclusion, in.SHA, in.Summary)
	return nil
}

// OpenPR records the PR the App will open, only while the branch still points
// at the head the gates passed on and the base at the commit they were
// resolved on: anything pushed to either since has not been reviewed.
// ponytail: log line until the GitHub App lands; there, the check and the
// open are two calls, and the merge (build 5) is pinned to the SHA.
func (r *Runner) OpenPR(ctx context.Context, in OpenPRIn) (string, error) {
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
	// this runner recorded on this base: one earned on any other base opens
	// nothing.
	gates, err := gatesFor(dir, in.Base, in.BaseID, in.Head)
	if err != nil {
		return "", err
	}
	for _, g := range gates {
		st, err := chain.Verify(dir, in.BaseID, in.Head, []string{g.Name})
		if err != nil {
			return "", err
		}
		key, err := passKey(dir, in.RepoURL, g.Name, in.BaseID, in.Head)
		if err != nil {
			return "", err
		}
		passed, err := r.hasPass(key)
		if err != nil {
			return "", err
		}
		if st[g.Name] != chain.Valid || !passed {
			return "", temporal.NewNonRetryableApplicationError(fmt.Sprintf(
				"refusing to open a PR: %s on %.12s: chain %s, re-run passed here on %.12s: %v", g.Name, in.Head, st[g.Name], in.BaseID, passed), "Rejected", nil)
		}
	}
	fmt.Fprintf(r.Log, "open-pr %s into %s at %s\n", in.Branch, in.Base, in.Head)
	return "local:" + in.Branch + "@" + in.Head[:min(12, len(in.Head))], nil
}

// gitOut runs git, killed if ctx ends (an activity's deadline or cancellation).
// Replacement objects are off: a local refs/replace entry must not change
// what a commit ID means.
func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	args = append([]string{"--no-replace-objects"}, args...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = chain.GitEnv() // a hook's GIT_DIR must not override -C
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}
