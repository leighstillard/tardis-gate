package ship

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	// The base as last fetched from the remote, as the runner reads it.
	base := a.Remote + "/" + in.Base
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

	defer heartbeat(ctx)()
	res, err := a.Exec.Run(ctx, executor.Job{
		Argv: gate.Run,
		Dir:  a.Dir,
		Env: []string{
			"TARDIS_GATE=" + in.Gate, "TARDIS_BASE=" + base, "TARDIS_HEAD=" + in.Code,
			"TARDIS_DIFF_FILE=" + diffFile, "TARDIS_RUNBOOK=" + m.VerifyRunbook,
			"TARDIS_FINDINGS_OUT=" + findings,
		},
	}, func(string) {})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("%s exited %d: %s", strings.Join(gate.Run, " "), res.ExitCode, tail(res.Output)), "Rejected", nil)
	}
	summary, err := os.ReadFile(findings)
	if err != nil || strings.TrimSpace(string(summary)) == "" {
		return temporal.NewNonRetryableApplicationError("the gate wrote no findings to $TARDIS_FINDINGS_OUT", "Malformed", nil)
	}
	// The review covered in.Tip; never let it vouch for code committed meanwhile.
	if now, err := gitOut(ctx, a.Dir, "rev-parse", "HEAD"); err != nil {
		return err
	} else if now != in.Tip {
		return temporal.NewNonRetryableApplicationError(
			"branch moved during the review (HEAD is now "+now+"); run tardis request again", "Malformed", nil)
	}
	_, err = chain.Commit(a.Dir, in.Gate, string(summary), a.Tool)
	return err
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
