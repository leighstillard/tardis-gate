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

	mu    sync.Mutex
	locks map[string]*sync.Mutex // one per repository URL, guarding its clone
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

// sync fetches the repository and leaves the base branch checked out, so
// nothing in the working tree comes from the branch under review.
func (r *Runner) sync(ctx context.Context, repoURL, base string) (string, error) {
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
	if _, err := gitOut(ctx, dir, "checkout", "-q", "--force", "--detach", "origin/"+base); err != nil {
		return "", err
	}
	return dir, nil
}

// Resolve lists the gates that apply to sha, from the base branch's manifest.
func (r *Runner) Resolve(ctx context.Context, in ResolveIn) ([]GateInfo, error) {
	defer heartbeat(ctx)()
	defer r.lock(in.RepoURL)()
	dir, err := r.sync(ctx, in.RepoURL, in.Base)
	if err != nil {
		return nil, err
	}
	m, err := manifest.LoadRev(dir, "origin/"+in.Base)
	if err != nil {
		return nil, fmt.Errorf("manifest on %s: %w", in.Base, err)
	}
	changed, err := manifest.Changed(dir, "origin/"+in.Base, in.SHA)
	if err != nil {
		return nil, err
	}
	var out []GateInfo
	for _, g := range m.Resolve(changed) {
		out = append(out, GateInfo{Name: g.Name, Timeout: g.Timeout})
	}
	return out, nil
}

// Attest judges the check commits for gates on tip.
func (r *Runner) Attest(ctx context.Context, in AttestIn) (map[string]string, error) {
	defer heartbeat(ctx)()
	defer r.lock(in.RepoURL)()
	dir, err := r.sync(ctx, in.RepoURL, in.Base)
	if err != nil {
		return nil, err
	}
	return chain.Verify(dir, "origin/"+in.Base, in.Tip, in.Gates)
}

// Rerun is this build's stand-in for the independent review: the operator's
// command, never one from the repository under review.
func (r *Runner) Rerun(ctx context.Context, in RerunIn) (Verdict, error) {
	if len(r.RerunCmd) == 0 {
		return Verdict{Reason: "this runner has no re-run configured"}, nil
	}
	defer heartbeat(ctx)()
	unlock := r.lock(in.RepoURL)
	dir, err := r.sync(ctx, in.RepoURL, in.Base)
	unlock()
	if err != nil {
		return Verdict{}, err
	}
	res, err := r.Exec.Run(ctx, executor.Job{
		Argv: r.RerunCmd,
		Dir:  dir,
		Env:  []string{"TARDIS_GATE=" + in.Gate, "TARDIS_SHA=" + in.Tip, "TARDIS_BASE=origin/" + in.Base},
	}, func(string) {})
	if err != nil {
		return Verdict{}, err
	}
	switch res.ExitCode {
	case 0:
		return Verdict{Pass: true}, nil
	case 1:
		return Verdict{Reason: tail(res.Output)}, nil
	}
	return Verdict{}, fmt.Errorf("re-run exited %d: %s", res.ExitCode, tail(res.Output))
}

// PostCheck records a check run. ponytail: log line until the GitHub App posts them.
func (r *Runner) PostCheck(_ context.Context, in CheckIn) error {
	fmt.Fprintf(r.Log, "check %s %s on %s %s\n", in.Name, in.Conclusion, in.SHA, in.Summary)
	return nil
}

// OpenPR records the PR the App will open. ponytail: log line until the GitHub App lands.
func (r *Runner) OpenPR(_ context.Context, in OpenPRIn) (string, error) {
	fmt.Fprintf(r.Log, "open-pr %s into %s at %s\n", in.Branch, in.Base, in.Head)
	return "local:" + in.Branch + "@" + in.Head[:min(12, len(in.Head))], nil
}

// gitOut runs git, killed if ctx ends (an activity's deadline or cancellation).
func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}
