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
	WorkDir  string   // clones live here, one per repository URL
	RerunCmd []string // run per gate; exit 0 passes, 1 rejects; empty rejects everything
	Exec     executor.Executor
	Log      io.Writer

	mu     sync.Mutex
	locks  map[string]*sync.Mutex // one per repository URL, guarding its clone
	passed map[string]bool        // passKey of every re-run this runner passed
	// ponytail: in memory; a restart mid-run refuses success until the gate is
	// re-run. Persist it when check runs become real (build 4).
}

// passKey names what a re-run judged: the gate, and the diff it read, as the
// merge base and the tip's tree. Check commits change no tree, so the final
// tip of a pass has the same key as the tip each gate was re-run on.
func passKey(ctx context.Context, dir, repoURL, gate, baseID, tip string) (string, error) {
	mb, err := gitOut(ctx, dir, "merge-base", baseID, tip)
	if err != nil {
		return "", err
	}
	tree, err := gitOut(ctx, dir, "rev-parse", tip+"^{tree}")
	if err != nil {
		return "", err
	}
	return strings.Join([]string{repoURL, gate, mb, tree}, "\x00"), nil
}

// checkBase refuses a base the runner did not choose. The workflow, which an
// author's machine can run, names the base; the runner trusts only
// base_branch from the manifest on the repository's default branch, and only
// commits that branch has held.
func checkBase(ctx context.Context, dir, base, baseID string) error {
	m, err := manifest.LoadRev(dir, "origin/HEAD")
	if err != nil {
		return fmt.Errorf("manifest on the default branch: %w", err)
	}
	if base != m.BaseBranch {
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("base %q is not this repository's base branch %q", base, m.BaseBranch), "Malformed", nil)
	}
	if baseID == "" {
		return nil
	}
	if _, err := gitOut(ctx, dir, "merge-base", "--is-ancestor", baseID, "origin/"+base); err != nil {
		return temporal.NewNonRetryableApplicationError(baseID+" was never on "+base, "Malformed", nil)
	}
	return nil
}

// lock serialises work on one repository's clone, so a slow fetch of one
// repository never holds up another.
func (r *Runner) lock(repoURL string) (unlock func()) {
	r.mu.Lock()
	if r.locks == nil {
		r.locks = map[string]*sync.Mutex{}
	}
	l := r.locks[repoURL]
	if l == nil {
		l = &sync.Mutex{}
		r.locks[repoURL] = l
	}
	r.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// sync clones the repository once and fetches its branches; reads go through
// commit IDs, never the working tree.
func (r *Runner) sync(ctx context.Context, repoURL string) (string, error) {
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
	return dir, nil
}

// Resolve pins the base branch's current commit and lists the gates that
// apply to sha under that commit's manifest. Every later step of the pass
// uses the pinned commit, so the policy cannot change half way through.
func (r *Runner) Resolve(ctx context.Context, in ResolveIn) (ResolveOut, error) {
	defer heartbeat(ctx)()
	defer r.lock(in.RepoURL)()
	dir, err := r.sync(ctx, in.RepoURL)
	if err != nil {
		return ResolveOut{}, err
	}
	if err := checkBase(ctx, dir, in.Base, ""); err != nil {
		return ResolveOut{}, err
	}
	baseID, err := manifest.CommitID(dir, "origin/"+in.Base)
	if err != nil {
		return ResolveOut{}, err
	}
	m, err := manifest.LoadRev(dir, baseID)
	if err != nil {
		return ResolveOut{}, fmt.Errorf("manifest on %s: %w", in.Base, err)
	}
	changed, err := manifest.Changed(dir, baseID, in.SHA)
	if err != nil {
		return ResolveOut{}, err
	}
	out := ResolveOut{BaseID: baseID}
	for _, g := range m.Resolve(changed) {
		out.Gates = append(out.Gates, GateInfo{Name: g.Name, Timeout: g.Timeout})
	}
	return out, nil
}

// Attest judges the check commits for gates on tip.
func (r *Runner) Attest(ctx context.Context, in AttestIn) (map[string]string, error) {
	defer heartbeat(ctx)()
	defer r.lock(in.RepoURL)()
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
	defer r.lock(in.RepoURL)()
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
	if _, err := gitOut(ctx, dir, "checkout", "-q", "--force", "--detach", in.BaseID); err != nil {
		return Verdict{}, err
	}
	if _, err := gitOut(ctx, dir, "clean", "-q", "-ffdx"); err != nil {
		return Verdict{}, err
	}
	res, err := r.Exec.Run(ctx, executor.Job{
		Argv: r.RerunCmd,
		Dir:  dir,
		Env:  []string{"TARDIS_GATE=" + in.Gate, "TARDIS_SHA=" + in.Tip, "TARDIS_BASE=" + in.BaseID},
	}, func(string) {})
	if err != nil {
		return Verdict{}, err
	}
	switch res.ExitCode {
	case 0:
		key, err := passKey(ctx, dir, in.RepoURL, in.Gate, in.BaseID, in.Tip)
		if err != nil {
			return Verdict{}, err
		}
		r.mu.Lock()
		if r.passed == nil {
			r.passed = map[string]bool{}
		}
		r.passed[key] = true
		r.mu.Unlock()
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
		defer r.lock(in.RepoURL)()
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
		key, err := passKey(ctx, dir, in.RepoURL, in.Gate, in.BaseID, in.SHA)
		if err != nil {
			return err
		}
		r.mu.Lock()
		passed := r.passed[key]
		r.mu.Unlock()
		if st[in.Gate] != chain.Valid || !passed {
			return temporal.NewNonRetryableApplicationError(fmt.Sprintf(
				"refusing tardis/%s success on %s: chain %s, re-run passed here: %v", in.Gate, in.SHA, st[in.Gate], passed), "Rejected", nil)
		}
	}
	fmt.Fprintf(r.Log, "check tardis/%s %s on %s %s\n", in.Gate, in.Conclusion, in.SHA, in.Summary)
	return nil
}

// OpenPR records the PR the App will open. ponytail: log line until the GitHub App lands.
func (r *Runner) OpenPR(_ context.Context, in OpenPRIn) (string, error) {
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
