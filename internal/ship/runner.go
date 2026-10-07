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

	mu sync.Mutex // ponytail: one lock for all clones; per-repo locks if runs queue up
}

// sync fetches the repository and leaves the base branch checked out, so the
// gate list comes from reviewed code rather than the branch under review.
func (r *Runner) sync(repoURL, base string) (string, error) {
	sum := sha256.Sum256([]byte(repoURL))
	dir := filepath.Join(r.WorkDir, hex.EncodeToString(sum[:8]))
	if _, err := os.Stat(filepath.Join(dir, ".git")); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(r.WorkDir, 0o700); err != nil {
			return "", err
		}
		if _, err := gitOut("", "clone", "-q", "--no-checkout", repoURL, dir); err != nil {
			return "", err
		}
	}
	if _, err := gitOut(dir, "fetch", "-q", "--prune", "origin", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return "", err
	}
	if _, err := gitOut(dir, "checkout", "-q", "--force", "--detach", "origin/"+base); err != nil {
		return "", err
	}
	return dir, nil
}

// Resolve lists the gates that apply to sha, from the base branch's manifest.
func (r *Runner) Resolve(_ context.Context, in ResolveIn) ([]GateInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	dir, err := r.sync(in.RepoURL, in.Base)
	if err != nil {
		return nil, err
	}
	m, err := manifest.Load(dir)
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
func (r *Runner) Attest(_ context.Context, in AttestIn) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	dir, err := r.sync(in.RepoURL, in.Base)
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
	r.mu.Lock()
	dir, err := r.sync(in.RepoURL, in.Base)
	r.mu.Unlock()
	if err != nil {
		return Verdict{}, err
	}
	defer heartbeat(ctx)()
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

func gitOut(dir string, args ...string) (string, error) {
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}
