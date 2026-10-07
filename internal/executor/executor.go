// Package executor runs one review job somewhere and reports how it ended.
package executor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Job is one command to run for a gate.
type Job struct {
	Argv   []string
	Dir    string
	Env    []string // added to the current environment
	Clean  bool     // Env replaces the environment instead (no inherited secrets)
	Resume string   // heartbeat detail from a previous attempt, "" on the first
}

// Result is how a job ended.
type Result struct {
	ExitCode int
	Output   string // the last MaxOutput bytes of combined stdout and stderr
}

// Executor runs a job. hb records progress a retry can resume from; executors
// that cannot resume ignore it.
type Executor interface {
	Run(ctx context.Context, job Job, hb func(resume string)) (Result, error)
}

// Local runs the job as a child process on this machine.
type Local struct{}

func (Local) Run(ctx context.Context, job Job, _ func(string)) (Result, error) {
	if len(job.Argv) == 0 {
		return Result{}, errors.New("executor: empty argv")
	}
	cmd := exec.CommandContext(ctx, job.Argv[0], job.Argv[1:]...)
	cmd.Dir = job.Dir
	cmd.Env = append(os.Environ(), job.Env...)
	if job.Clean {
		cmd.Env = job.Env
	}
	// Run in its own process group so cancelling kills grandchildren too, and
	// stop waiting on output a few seconds after the process itself is gone:
	// an orphaned grandchild holding the pipe must not hang the activity.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	out := &tailBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && ctx.Err() == nil {
		return Result{ExitCode: exit.ExitCode(), Output: out.String()}, nil
	}
	if err != nil {
		return Result{Output: out.String()}, err
	}
	return Result{Output: out.String()}, nil
}

// MaxOutput is how much of a job's output is kept: its end, where the error is.
const MaxOutput = 64 << 10

// tailBuffer keeps the last MaxOutput bytes written to it, so a job that
// prints without end cannot use up memory.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 2*MaxOutput {
		t.b = append(t.b[:0], t.b[len(t.b)-MaxOutput:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b[max(0, len(t.b)-MaxOutput):]) }

// ErrNotImplemented is returned by executors whose contract is defined but not built.
var ErrNotImplemented = errors.New("executor: not implemented")

// Actions will run the job as a GitHub Actions workflow_dispatch. Contract:
//  1. POST /repos/{o}/{r}/actions/workflows/{file}/dispatches with inputs
//     {run, gate, sha}; the workflow sets run-name "tardis <run> <gate>".
//  2. The dispatch returns 204 with no ID, so find the run by run-name, then
//     call hb(runID) so a retry resumes via Job.Resume instead of dispatching again.
//  3. Poll GET /repos/{o}/{r}/actions/runs/{id} every 20 s until conclusion != null.
//  4. Read the job's result from its tardis-result.json artifact.
type Actions struct{}

func (Actions) Run(context.Context, Job, func(string)) (Result, error) {
	return Result{}, ErrNotImplemented
}
