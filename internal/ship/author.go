package ship

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/leighstillard/tardis-gate/internal/chain"
	"github.com/leighstillard/tardis-gate/internal/executor"
	"github.com/leighstillard/tardis-gate/internal/manifest"
)

// Author serves the activities that run on the author's machine for one run.
type Author struct {
	Dir    string // the author's working copy, on the branch being shipped
	Remote string
	Branch string
	Tool   string // Ship-Check-Tool for check commits this machine writes
	Exec   executor.Executor
	Out    io.Writer
	Events chan<- Event // terminal events are sent here as well as printed
}

// AuthorReview makes sure gate has a check commit on the branch and pushes it.
// A valid check already on top (written by the author's own session) is used
// as is; otherwise the gate's run command is executed and its findings become
// the check commit's summary. It returns the new branch tip.
func (a *Author) AuthorReview(ctx context.Context, in AuthorReviewIn) (string, error) {
	defer heartbeat(ctx)() // from the start: a slow fetch or diff must not look like a dead author
	tip, err := gitOut(ctx, a.Dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if tip != in.Tip {
		// A retry after this activity already wrote its check (and perhaps
		// lost the push or its completion) finds that check on top of the
		// expected tip: resume from it. Anything else is the branch moving.
		parent, _ := gitOut(ctx, a.Dir, "rev-parse", "HEAD^")
		subject, _ := gitOut(ctx, a.Dir, "log", "-1", "--format=%s", "HEAD")
		if parent != in.Tip || subject != chain.SubjectPrefix+in.Gate {
			return "", temporal.NewNonRetryableApplicationError(
				fmt.Sprintf("branch moved: HEAD is %s, the run expects %s; run tardis request again", tip, in.Tip),
				"Malformed", nil)
		}
	}
	// The base commit the runner pinned for this pass. The gate's command is
	// read from it and run here, so check it ourselves first.
	base := in.BaseID
	if err := a.checkBase(ctx, in.Base, base); err != nil {
		return "", err
	}
	if err := a.clean(ctx); err != nil {
		return "", err
	}
	st, err := chain.Verify(a.Dir, base, "HEAD", []string{in.Gate})
	if err != nil {
		return "", err
	}
	if st[in.Gate] != chain.Valid {
		if err := a.review(ctx, in, base); err != nil {
			return "", err
		}
	}
	if _, err := gitOut(ctx, a.Dir, "push", "-q", a.Remote, "HEAD:refs/heads/"+a.Branch); err != nil {
		return "", err
	}
	return gitOut(ctx, a.Dir, "rev-parse", "HEAD")
}

// onFirstParent reports whether ref itself once pointed at id: id is on its
// first-parent line, not merely reachable, as a merged branch's commits are.
// ponytail: the last 1000 commits of ref; an older id is refused.
func onFirstParent(ctx context.Context, dir, ref, id string) (bool, error) {
	out, err := gitOut(ctx, dir, "rev-list", "--first-parent", "--max-count=1000", ref)
	if err != nil {
		return false, err
	}
	return slices.Contains(strings.Fields(out), id), nil
}

// checkBase makes sure base is the branch the remote's default branch names
// as base_branch, and baseID a commit it has held. The workflow names both,
// and anyone who can start one could otherwise point this machine at gate
// commands from any commit.
func (a *Author) checkBase(ctx context.Context, base, baseID string) error {
	malformed := func(msg string) error { return temporal.NewNonRetryableApplicationError(msg, "Malformed", nil) }
	out, err := gitOut(ctx, a.Dir, "ls-remote", "--symref", a.Remote, "HEAD")
	if err != nil {
		return err
	}
	line, _, _ := strings.Cut(out, "\n")
	ref, _, _ := strings.Cut(strings.TrimPrefix(line, "ref: "), "\t")
	def, ok := strings.CutPrefix(ref, "refs/heads/")
	if !strings.HasPrefix(line, "ref: ") || !ok {
		return malformed("cannot tell " + a.Remote + "'s default branch")
	}
	for _, b := range []string{def, base} {
		if _, err := gitOut(ctx, a.Dir, "fetch", "-q", a.Remote, "+refs/heads/"+b+":refs/remotes/"+a.Remote+"/"+b); err != nil {
			return err
		}
	}
	m, err := manifest.LoadRev(a.Dir, "refs/remotes/"+a.Remote+"/"+def)
	if err != nil {
		return fmt.Errorf("manifest on %s/%s: %w", a.Remote, def, err)
	}
	if base != m.BaseBranch {
		return malformed(fmt.Sprintf("base %q is not %s's base branch %q", base, a.Remote, m.BaseBranch))
	}
	if ok, err := onFirstParent(ctx, a.Dir, "refs/remotes/"+a.Remote+"/"+base, baseID); err != nil {
		return err
	} else if !ok {
		return malformed("base commit " + baseID + " was never " + a.Remote + "/" + base + " itself")
	}
	return nil
}

func (a *Author) review(ctx context.Context, in AuthorReviewIn, base string) error {
	// The gate's command comes from base, like the gate list, so the branch
	// under review cannot change what runs here.
	m, err := manifest.LoadRev(a.Dir, base)
	if err != nil {
		return temporal.NewNonRetryableApplicationError(err.Error(), "Malformed", nil)
	}
	var gate *manifest.Gate
	for i := range m.Gates {
		if m.Gates[i].Name == in.Gate {
			gate = &m.Gates[i]
		}
	}
	if gate == nil {
		return temporal.NewNonRetryableApplicationError("gate "+in.Gate+" is not enabled on "+base, "Malformed", nil)
	}

	tmp, err := os.MkdirTemp("", "tardis-review-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	diff, err := gitOut(ctx, a.Dir, "diff", base+"..."+in.Code)
	if err != nil {
		return err
	}
	diffFile, findings := filepath.Join(tmp, "diff"), filepath.Join(tmp, "findings")
	if err := os.WriteFile(diffFile, []byte(diff), 0o600); err != nil {
		return err
	}

	// The gate runs in the working copy, so the branch can change what it
	// runs: it gets no Temporal credentials.
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TEMPORAL_") {
			env = append(env, kv)
		}
	}
	res, err := a.Exec.Run(ctx, executor.Job{
		Argv: gate.Run,
		Dir:  a.Dir,
		Env: append(env,
			"TARDIS_GATE="+in.Gate, "TARDIS_BASE="+base, "TARDIS_HEAD="+in.Code,
			"TARDIS_DIFF_FILE="+diffFile, "TARDIS_RUNBOOK="+m.VerifyRunbook,
			"TARDIS_FINDINGS_OUT="+findings),
		Clean: true,
	}, func(string) {})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		// What the gate printed stays on this machine: errors go into
		// Temporal history, which every worker in the namespace can read.
		cmd := strings.Join(gate.Run, " ")
		if a.Out != nil {
			fmt.Fprintf(a.Out, "%s: %s exited %d:\n%s\n", in.Gate, cmd, res.ExitCode, tail(res.Output))
		}
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("%s exited %d; its output is on the author's machine", cmd, res.ExitCode), "Rejected", nil)
	}
	summary, err := os.ReadFile(findings)
	if err != nil || strings.TrimSpace(string(summary)) == "" {
		return temporal.NewNonRetryableApplicationError("the gate wrote no findings to $TARDIS_FINDINGS_OUT", "Malformed", nil)
	}
	// The review covered in.Tip and nothing else; never let it vouch for code
	// committed, or left uncommitted, meanwhile.
	if err := a.clean(ctx); err != nil {
		return err
	}
	if now, err := gitOut(ctx, a.Dir, "rev-parse", "HEAD"); err != nil {
		return err
	} else if now != in.Tip {
		return temporal.NewNonRetryableApplicationError(
			"branch moved during the review (HEAD is now "+now+"); run tardis request again", "Malformed", nil)
	}
	sha, err := chain.Commit(a.Dir, in.Gate, string(summary), a.Tool)
	if err != nil {
		return err
	}
	// The review covered in.Tip. If a commit slipped in between the check above
	// and the commit, take this check back off it (only if nothing has moved
	// again) rather than let it vouch for unreviewed code.
	if parent, err := gitOut(ctx, a.Dir, "rev-parse", sha+"^"); err != nil {
		return err
	} else if parent != in.Tip {
		if ref, err := gitOut(ctx, a.Dir, "symbolic-ref", "-q", "HEAD"); err == nil {
			_, _ = gitOut(ctx, a.Dir, "update-ref", ref, parent, sha)
		}
		return temporal.NewNonRetryableApplicationError(
			"branch moved during the review (a commit landed on "+in.Tip+"); run tardis request again", "Malformed", nil)
	}
	return nil
}

// clean refuses a working tree with uncommitted or untracked changes: the
// review runs in the working copy and must see exactly the pushed commit.
func (a *Author) clean(ctx context.Context) error {
	if out, err := gitOut(ctx, a.Dir, "status", "--porcelain", "--untracked-files=all"); err != nil {
		return err
	} else if out != "" {
		return temporal.NewNonRetryableApplicationError(
			"the working copy has uncommitted or untracked changes; commit or stash them, then run tardis request again:\n"+tail(out),
			"Malformed", nil)
	}
	return nil
}

// Notify prints one event for the author.
func (a *Author) Notify(_ context.Context, e Event) error {
	fmt.Fprintln(a.Out, e)
	if e.Terminal() && a.Events != nil {
		select {
		case a.Events <- e:
		default:
		}
	}
	return nil
}

// heartbeat records a heartbeat every 10 s until the returned stop is called.
func heartbeat(ctx context.Context) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				activity.RecordHeartbeat(ctx)
			}
		}
	}()
	return func() { close(done) }
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 2000 {
		s = "…" + s[len(s)-2000:]
	}
	return s
}
