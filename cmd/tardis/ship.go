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
	"os/user"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/leighstillard/tardis-gate/internal/chain"
	"github.com/leighstillard/tardis-gate/internal/executor"
	"github.com/leighstillard/tardis-gate/internal/ghapp"
	"github.com/leighstillard/tardis-gate/internal/manifest"
	"github.com/leighstillard/tardis-gate/internal/ship"
)

// workerFlags are shared by request and wait.
type workerFlags struct{ repo, remote, tool *string }

func addWorkerFlags(fs *flag.FlagSet) workerFlags {
	return workerFlags{
		repo:   fs.String("repo", ".", "working copy of the branch to ship"),
		remote: fs.String("remote", "origin", "remote the auditor fetches from"),
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
	// The auditor reads the policy from the base branch; this only catches a
	// working copy that is not enrolled, or on the base itself, before
	// anything starts. The workflow checks the base again once resolved.
	m, err := manifest.Load(repo)
	if err != nil {
		return ship.Input{}, "", err
	}
	if branch == m.BaseBranch {
		return ship.Input{}, "", fmt.Errorf("%s is the base branch; ship from a feature branch", branch)
	}
	in := ship.Input{RepoURL: url, RepoID: repoID(url), Branch: branch}
	return in, ship.WorkflowID(in.RepoID, branch), nil
}

// checkRemote refuses a remote URL that carries credentials: the URL is
// stored in the run's Temporal history, which every worker can read. A query
// or fragment can hold a token too, and a URL that does not parse cannot be
// checked at all.
func checkRemote(raw string) error {
	if !strings.Contains(raw, "://") {
		return nil // scp-like (git@host:owner/repo) or a local path
	}
	const why = ", which would be stored in Temporal history; use a git credential helper instead"
	u, err := neturl.Parse(raw)
	if err != nil {
		return errors.New("the remote URL does not parse, so it cannot be checked for credentials" + why)
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("the remote URL has a query string or fragment, which can carry credentials" + why)
	}
	if u.User == nil {
		return nil
	}
	if _, hasPassword := u.User.Password(); hasPassword || u.Scheme == "http" || u.Scheme == "https" {
		return errors.New("the remote URL carries credentials" + why)
	}
	return nil
}

// repoID names a repository for its workflow IDs: owner/repo on GitHub, and
// otherwise the name plus a hash of the whole remote URL, so two repositories
// with the same name elsewhere never share a run.
func repoID(remoteURL string) string {
	if r, ok := ghapp.Repo(remoteURL); ok {
		return r
	}
	u := strings.TrimSuffix(strings.TrimSuffix(remoteURL, "/"), ".git")
	sum := sha256.Sum256([]byte(remoteURL))
	return "other/" + path.Base(filepath.ToSlash(u)) + "-" + hex.EncodeToString(sum[:16]) // 128 bits: no two remotes share a run
}

func request(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("request", flag.ContinueOnError)
	af := addWorkerFlags(fs)
	pos, code, ok := parseRange(fs, "request [<sha>] --tool <vendor/tool/model>", args, 0, 1, stderr)
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
	if _, err := gitLine(*af.repo, "push", "-q", *af.remote, head+":refs/heads/"+in.Branch); err != nil {
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
	if d, err := c.DescribeTaskQueue(ctx, ship.AuditorQueue, enumspb.TASK_QUEUE_TYPE_ACTIVITY); err == nil && len(d.GetPollers()) == 0 {
		fmt.Fprintln(stderr, "request: warning: no auditor is polling; reviews wait until one starts")
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

// lockAttach takes this working copy's attach lock, so a run's events are
// not split between two listeners. It is an advisory lock the kernel drops
// when the process exits, so a crash leaves nothing behind. If another
// process holds it, holder is that process's PID (-1 if unknown).
func lockAttach(repo string) (release func(), holder int, err error) {
	gitDir, err := gitLine(repo, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, 0, err
	}
	f, err := os.OpenFile(filepath.Join(gitDir, "tardis-attach.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, 0, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		defer f.Close()
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, 0, err
		}
		pid := -1
		if b, err := io.ReadAll(f); err == nil {
			fmt.Sscan(string(b), &pid)
		}
		return nil, pid, nil
	}
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, 0, err
	}
	if _, err := f.WriteAt([]byte(fmt.Sprint(os.Getpid())), 0); err != nil {
		f.Close()
		return nil, 0, err
	}
	return func() { f.Truncate(0); f.Close() }, 0, nil
}

func wait(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	af := addWorkerFlags(fs)
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

// attach serves the run's workflow and worker activities until a terminal
// event from the current pass: 0 when the PR is open, 1 when a gate rejected
// or failed.
func attach(c client.Client, wfID, repo, remote, branch, tool string, stdout, stderr io.Writer) int {
	events := make(chan ship.Event, 8)
	a := &ship.Worker{Dir: repo, Remote: remote, Branch: branch, Tool: tool, Exec: executor.Local{}, Out: stdout, Events: events}

	wfw := workflowWorker(c)
	// Time to finish an in-flight activity, so a notification is acknowledged
	// rather than delivered again to the next attachment.
	aw := worker.New(c, ship.WorkerQueue(wfID), worker.Options{WorkerStopTimeout: 10 * time.Second})
	aw.RegisterActivityWithOptions(a.WorkerReview, activity.RegisterOptions{Name: ship.ActWorkerReview})
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
	// Watch for the run closing from the start, however it ends: completed,
	// timed out or terminated.
	closed := make(chan error, 1)
	go func() { closed <- c.GetWorkflow(context.Background(), wfID, "").Get(context.Background(), nil) }()
	for {
		select {
		case e := <-events:
			if e.Kind != "completed" && current(c, wfID, e, stderr) {
				return 1
			}
			// A head requested as the run finished starts it again, so stay
			// attached, serving its reviews, until the run has really closed.
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

// current reports whether e comes from the run's current pass. Notifications
// are delivered at least once, so one from an earlier pass, even about the
// same commit, can arrive again and must not end this attachment. By the
// time an event arrives this process serves the workflow, so the query is
// normally answered; if it cannot be confirmed, stay attached.
func current(c client.Client, wfID string, e ship.Event, stderr io.Writer) bool {
	if e.Pass == "" {
		return true
	}
	for try := 0; try < 3; try++ {
		if try > 0 {
			time.Sleep(2 * time.Second)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		var pass string
		v, err := c.QueryWorkflow(ctx, wfID, "", ship.QueryPass)
		if err == nil {
			err = v.Get(&pass)
		}
		cancel()
		if err == nil {
			return pass == e.Pass
		}
	}
	fmt.Fprintln(stderr, "could not tell whether that is about the current pass; staying attached (Ctrl-C to detach)")
	return false
}

func auditor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auditor", flag.ContinueOnError)
	home, _ := os.UserCacheDir()
	workDir := fs.String("work-dir", filepath.Join(home, "tardis", "runner") /* v0.3 path: keeps its passes */, "where repository clones and the record of passed re-runs live")
	appID := fs.String("app-id", os.Getenv("TARDIS_APP_ID"), "GitHub App ID that posts the check runs (or $TARDIS_APP_ID); without one they are only logged")
	appKey := fs.String("app-key", os.Getenv("TARDIS_APP_KEY"), "the App's private key, a PEM file (or $TARDIS_APP_KEY)")
	rerunUser := fs.String("rerun-user", "", "user a provider of type exec runs as, with an empty HOME and none of this "+
		"auditor's environment (the auditor must be able to switch to it; limit its network with a firewall rule). "+
		"Without it an exec provider is refused")
	repos := map[string]bool{}
	fs.Func("repo", "repository URL this auditor serves, exactly as workers' remotes name it (repeatable, at least one). "+
		"Run one auditor per Temporal namespace: auditors share one task queue, and each keeps the record of re-runs it passed in its own --work-dir", func(u string) error {
		repos[u] = true
		return nil
	})
	if _, code, ok := parse(fs, "auditor --repo <url>... [--work-dir <dir>] [--app-id <id> --app-key <pem>] [--rerun-user <user>]", args, 0, stderr); !ok {
		return code
	}
	if len(repos) == 0 {
		fmt.Fprintln(stderr, "auditor: --repo is required: name each repository URL this auditor serves")
		return 2
	}
	r := &ship.Auditor{Repos: repos, WorkDir: *workDir, Version: version(), Log: stdout}
	if (*appID == "") != (*appKey == "") {
		fmt.Fprintln(stderr, "auditor: --app-id and --app-key go together")
		return 2
	}
	if *appID != "" {
		app, err := ghapp.Load(*appID, *appKey)
		if err != nil {
			fmt.Fprintln(stderr, "auditor:", err)
			return 2
		}
		r.App = app
	} else {
		fmt.Fprintln(stderr, "tardis auditor: no GitHub App (--app-id, --app-key): check runs are only logged.")
	}
	if *rerunUser != "" {
		u, err := user.Lookup(*rerunUser)
		if err != nil {
			fmt.Fprintln(stderr, "auditor:", err)
			return 2
		}
		uid, _ := strconv.ParseUint(u.Uid, 10, 32) // numeric on unix
		gid, _ := strconv.ParseUint(u.Gid, 10, 32)
		if uid == 0 || gid == 0 || int(uid) == os.Geteuid() {
			fmt.Fprintf(stderr, "auditor: --rerun-user %s must be an unprivileged user other than the auditor's\n", *rerunUser)
			return 2
		}
		r.RerunUser = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	}
	fmt.Fprintln(stderr, "tardis auditor: PRs are only logged until build 5.")
	c, err := ship.Dial()
	if err != nil {
		fmt.Fprintln(stderr, "auditor: temporal:", err)
		return 2
	}
	defer c.Close()

	rw := worker.New(c, ship.AuditorQueue, worker.Options{})
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
	fmt.Fprintf(stdout, "auditor polling %s and %s\n", ship.WorkflowQueue, ship.AuditorQueue)
	<-worker.InterruptCh()
	return 0
}

func workflowWorker(c client.Client) worker.Worker {
	w := worker.New(c, ship.WorkflowQueue, worker.Options{})
	w.RegisterWorkflowWithOptions(ship.Ship, workflow.RegisterOptions{Name: ship.WorkflowName})
	return w
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
