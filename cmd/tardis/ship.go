package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	neturl "net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/leighstillard/tardis-gate/internal/chain"
	"github.com/leighstillard/tardis-gate/internal/executor"
	"github.com/leighstillard/tardis-gate/internal/manifest"
	"github.com/leighstillard/tardis-gate/internal/ship"
)

// authorFlags are shared by request and wait.
type authorFlags struct{ repo, remote, tool *string }

func addAuthorFlags(fs *flag.FlagSet) authorFlags {
	return authorFlags{
		repo:   fs.String("repo", ".", "working copy of the branch to ship"),
		remote: fs.String("remote", "origin", "remote the runner fetches from"),
		tool:   fs.String("tool", os.Getenv("TARDIS_TOOL"), "<vendor>/<tool>/<model> for check commits this machine writes (default $TARDIS_TOOL)"),
	}
}

// checkToolFlag checks --tool before anything is pushed or dialled: this
// machine's reviews are written as check commits under that name.
func checkToolFlag(tool string) error {
	if tool == "" {
		return errors.New("--tool or $TARDIS_TOOL is required, e.g. anthropic/claude-code/opus")
	}
	return chain.CheckTool(tool)
}

// branchRun works out the run for the branch checked out in repo.
func branchRun(repo, remote string) (ship.Input, string, error) {
	if _, err := gitLine(repo, "rev-parse", "--git-dir"); err != nil {
		return ship.Input{}, "", err
	}
	branch, err := gitLine(repo, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return ship.Input{}, "", fmt.Errorf("not on a branch: %w", err)
	}
	url, err := gitLine(repo, "remote", "get-url", remote)
	if err != nil {
		return ship.Input{}, "", err
	}
	if err := checkRemote(url); err != nil {
		return ship.Input{}, "", err
	}
	// The runner reads the policy from the base branch; this only catches a
	// working copy that is not enrolled before anything starts.
	if _, err := manifest.Load(repo); err != nil {
		return ship.Input{}, "", err
	}
	in := ship.Input{RepoURL: url, RepoID: repoID(url), Branch: branch}
	return in, ship.WorkflowID(in.RepoID, branch), nil
}

// checkRemote refuses a remote URL that carries credentials: the URL is
// stored in the run's Temporal history, which every worker can read.
func checkRemote(raw string) error {
	u, err := neturl.Parse(raw)
	if err != nil || u.User == nil {
		return nil // scp-like (git@host:owner/repo) or a local path
	}
	if _, hasPassword := u.User.Password(); hasPassword || u.Scheme == "http" || u.Scheme == "https" {
		return errors.New("the remote URL carries credentials, which would be stored in Temporal history; use a git credential helper instead")
	}
	return nil
}

// repoID names a repository for its workflow IDs: owner/repo on GitHub, and
// otherwise the name plus a hash of the whole remote URL, so two repositories
// with the same name elsewhere never share a run.
func repoID(remoteURL string) string {
	u := strings.TrimSuffix(strings.TrimSuffix(remoteURL, "/"), ".git")
	for _, p := range []string{"https://github.com/", "ssh://git@github.com/", "git@github.com:"} {
		if rest, ok := strings.CutPrefix(u, p); ok && strings.Count(rest, "/") == 1 {
			return strings.ToLower(rest)
		}
	}
	sum := sha256.Sum256([]byte(remoteURL))
	return "other/" + path.Base(filepath.ToSlash(u)) + "-" + hex.EncodeToString(sum[:4])
}

func request(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("request", flag.ContinueOnError)
	af := addAuthorFlags(fs)
	pos, code, ok := parse(fs, "request [<sha>] [--tool <vendor/tool/model>]", args, countPositional(args), stderr)
	if !ok {
		return code
	}
	if err := checkToolFlag(*af.tool); err != nil {
		fmt.Fprintln(stderr, "request:", err)
		return 2
	}
	in, wfID, err := branchRun(*af.repo, *af.remote)
	if err != nil {
		fmt.Fprintln(stderr, "request:", err)
		return 2
	}
	head, err := gitLine(*af.repo, "rev-parse", "HEAD")
	if err != nil {
		fmt.Fprintln(stderr, "request:", err)
		return 2
	}
	if len(pos) == 1 {
		want, err := gitLine(*af.repo, "rev-parse", "--verify", pos[0]+"^{commit}")
		if err != nil || want != head {
			fmt.Fprintf(stderr, "request: %s is not HEAD (%s); check it out first\n", pos[0], head)
			return 2
		}
	}
	if _, err := gitLine(*af.repo, "push", "-q", *af.remote, "HEAD:refs/heads/"+in.Branch); err != nil {
		fmt.Fprintln(stderr, "request:", err)
		return 2
	}
	in.Head = head

	c, err := ship.Dial()
	if err != nil {
		fmt.Fprintln(stderr, "request: temporal:", err)
		return 2
	}
	defer c.Close()
	ctx := context.Background()
	if d, err := c.DescribeTaskQueue(ctx, ship.RunnerQueue, enumspb.TASK_QUEUE_TYPE_ACTIVITY); err == nil && len(d.GetPollers()) == 0 {
		fmt.Fprintln(stderr, "request: warning: no runner is polling; reviews wait until one starts")
	}
	if _, err := c.SignalWithStartWorkflow(ctx, wfID, ship.SignalNewHead, ship.NewHead{SHA: head},
		client.StartWorkflowOptions{ID: wfID, TaskQueue: ship.WorkflowQueue}, ship.WorkflowName, in); err != nil {
		fmt.Fprintln(stderr, "request:", err)
		return 2
	}
	fmt.Fprintf(stdout, "requested %s on %s (%s)\n", head[:12], in.Branch, wfID)
	release, holder, err := lockAttach(*af.repo)
	if err != nil {
		fmt.Fprintln(stderr, "request:", err)
		return 2
	}
	if holder != 0 {
		fmt.Fprintf(stdout, "tardis (pid %d) is already attached to this working copy and prints the run's events\n", holder)
		return 0
	}
	defer release()
	return attach(c, wfID, *af.repo, *af.remote, in.Branch, *af.tool, stdout, stderr)
}

// lockAttach makes this process the one tardis attached to the working copy,
// so a run's events are not split between two listeners. If another live
// process holds it, holder is that PID.
// ponytail: check-then-write, racy only if two tardis start in the same instant.
func lockAttach(repo string) (release func(), holder int, err error) {
	gitDir, err := gitLine(repo, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, 0, err
	}
	path := filepath.Join(gitDir, "tardis-attach.pid")
	if b, err := os.ReadFile(path); err == nil {
		var pid int
		if _, err := fmt.Sscan(string(b), &pid); err == nil && pid != os.Getpid() && syscall.Kill(pid, 0) == nil {
			return nil, pid, nil
		}
	}
	if err := os.WriteFile(path, []byte(fmt.Sprint(os.Getpid())), 0o600); err != nil {
		return nil, 0, err
	}
	return func() { os.Remove(path) }, 0, nil
}

func wait(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	af := addAuthorFlags(fs)
	if _, code, ok := parse(fs, "wait", args, 0, stderr); !ok {
		return code
	}
	if err := checkToolFlag(*af.tool); err != nil {
		fmt.Fprintln(stderr, "wait:", err)
		return 2
	}
	in, wfID, err := branchRun(*af.repo, *af.remote)
	if err != nil {
		fmt.Fprintln(stderr, "wait:", err)
		return 2
	}
	release, holder, err := lockAttach(*af.repo)
	if err != nil {
		fmt.Fprintln(stderr, "wait:", err)
		return 2
	}
	if holder != 0 {
		fmt.Fprintf(stderr, "wait: tardis (pid %d) is already attached to this working copy\n", holder)
		return 2
	}
	defer release()
	c, err := ship.Dial()
	if err != nil {
		fmt.Fprintln(stderr, "wait: temporal:", err)
		return 2
	}
	defer c.Close()
	return attach(c, wfID, *af.repo, *af.remote, in.Branch, *af.tool, stdout, stderr)
}

// attach serves the run's workflow and author activities until a terminal
// event: 0 when the PR is open, 1 when a gate rejected or failed.
func attach(c client.Client, wfID, repo, remote, branch, tool string, stdout, stderr io.Writer) int {
	events := make(chan ship.Event, 1)
	a := &ship.Author{Dir: repo, Remote: remote, Branch: branch, Tool: tool, Exec: executor.Local{}, Out: stdout, Events: events}

	wfw := workflowWorker(c)
	aw := worker.New(c, ship.AuthorQueue(wfID), worker.Options{})
	aw.RegisterActivityWithOptions(a.AuthorReview, activity.RegisterOptions{Name: ship.ActAuthorReview})
	aw.RegisterActivityWithOptions(a.Notify, activity.RegisterOptions{Name: ship.ActNotify})
	for _, w := range []worker.Worker{wfw, aw} {
		if err := w.Start(); err != nil {
			fmt.Fprintln(stderr, "worker:", err)
			return 2
		}
		defer w.Stop()
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	var closed chan error // set once "completed" is seen
	for {
		select {
		case e := <-events:
			if e.Kind != "completed" {
				return 1
			}
			// A head requested as the run finished starts it again, so stay
			// attached, serving its reviews, until the run has really closed.
			closed = make(chan error, 1)
			go func() { closed <- c.GetWorkflow(context.Background(), wfID, "").Get(context.Background(), nil) }()
		case err := <-closed:
			if err != nil {
				return 1
			}
			return 0
		case <-sigs:
			fmt.Fprintln(stderr, "detached; the run continues. Re-attach with: tardis wait")
			return 130
		}
	}
}

func runner(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("runner", flag.ContinueOnError)
	home, _ := os.UserCacheDir()
	workDir := fs.String("work-dir", filepath.Join(home, "tardis", "runner"), "where repository clones live")
	rerun := fs.String("rerun-cmd", "", "shell command run per gate; exit 0 passes, 1 rejects (stand-in until provider re-runs). "+
		"It runs in a clean checkout of the base, never the branch's code: read the change with git through $TARDIS_SHA and $TARDIS_BASE")
	repos := map[string]bool{}
	fs.Func("repo", "repository URL this runner serves, exactly as authors' remotes name it (repeatable, at least one)", func(u string) error {
		repos[u] = true
		return nil
	})
	if _, code, ok := parse(fs, "runner --repo <url>... [--work-dir <dir>] [--rerun-cmd <sh>]", args, 0, stderr); !ok {
		return code
	}
	if len(repos) == 0 {
		fmt.Fprintln(stderr, "runner: --repo is required: name each repository URL this runner serves")
		return 2
	}
	r := &ship.Runner{Repos: repos, WorkDir: *workDir, Exec: executor.Local{}, Log: stdout}
	if *rerun != "" {
		r.RerunCmd = []string{"sh", "-c", *rerun}
	}
	c, err := ship.Dial()
	if err != nil {
		fmt.Fprintln(stderr, "runner: temporal:", err)
		return 2
	}
	defer c.Close()

	rw := worker.New(c, ship.RunnerQueue, worker.Options{})
	for name, fn := range map[string]any{
		ship.ActResolve: r.Resolve, ship.ActAttest: r.Attest, ship.ActRerun: r.Rerun,
		ship.ActPostCheck: r.PostCheck, ship.ActOpenPR: r.OpenPR,
	} {
		rw.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: name})
	}
	wfw := workflowWorker(c)
	for _, w := range []worker.Worker{wfw, rw} {
		if err := w.Start(); err != nil {
			fmt.Fprintln(stderr, "worker:", err)
			return 2
		}
		defer w.Stop()
	}
	fmt.Fprintf(stdout, "runner polling %s and %s\n", ship.WorkflowQueue, ship.RunnerQueue)
	<-worker.InterruptCh()
	return 0
}

func workflowWorker(c client.Client) worker.Worker {
	w := worker.New(c, ship.WorkflowQueue, worker.Options{})
	w.RegisterWorkflowWithOptions(ship.Ship, workflow.RegisterOptions{Name: ship.WorkflowName})
	return w
}

func countPositional(args []string) int {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return 1
	}
	return 0
}

func gitLine(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"--no-replace-objects", "-C", dir}, args...)...)
	cmd.Env = chain.GitEnv() // a hook's GIT_DIR must not override -C
	out, err := cmd.Output() // stdout only: a warning on stderr is not part of the answer
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
		return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(ee.Stderr)))
	} else if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}
