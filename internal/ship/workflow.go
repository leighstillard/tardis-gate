package ship

import (
	"errors"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// Timeouts. A dead runner shows within HeartbeatTimeout while an activity
// runs, or ScheduleToStart when no runner is polling at all.
const (
	HeartbeatTimeout = 45 * time.Second
	ScheduleToStart  = 2 * time.Minute
	MaxGateTime      = 20 * time.Minute
	ScheduleToClose  = 60 * time.Minute
	AwaitDelivery    = 24 * time.Hour // notifications and check runs wait this long for a worker
	AwaitFix         = 7 * 24 * time.Hour
)

var retry = &temporal.RetryPolicy{
	InitialInterval:        10 * time.Second,
	BackoffCoefficient:     2,
	MaximumInterval:        2 * time.Minute,
	MaximumAttempts:        3,
	NonRetryableErrorTypes: []string{"Rejected", "Malformed"},
}

func runnerOpts(ctx workflow.Context, timeout time.Duration) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:              RunnerQueue,
		ScheduleToStartTimeout: ScheduleToStart,
		StartToCloseTimeout:    min(timeout, MaxGateTime),
		ScheduleToCloseTimeout: ScheduleToClose,
		HeartbeatTimeout:       HeartbeatTimeout,
		RetryPolicy:            retry,
	})
}

func authorOpts(ctx workflow.Context, queue string, timeout time.Duration) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:              queue,
		StartToCloseTimeout:    min(timeout, MaxGateTime),
		ScheduleToCloseTimeout: AwaitDelivery, // the author may attach later
		HeartbeatTimeout:       HeartbeatTimeout,
		RetryPolicy:            retry,
		// A cancelled review still holds the working copy until it stops;
		// the next must not start before then.
		WaitForCancellation: true,
	})
}

// gateRetries applies a gate's retry count from its manifest to the activity
// that runs that gate's review.
func gateRetries(ctx workflow.Context, retries int) workflow.Context {
	opts := workflow.GetActivityOptions(ctx)
	p := *retry
	p.MaximumAttempts = int32(retries) + 1 // 0 would mean no limit
	opts.RetryPolicy = &p
	return workflow.WithActivityOptions(ctx, opts)
}

func deliveryOpts(ctx workflow.Context, queue string) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:              queue,
		StartToCloseTimeout:    time.Minute,
		ScheduleToCloseTimeout: AwaitDelivery,
		HeartbeatTimeout:       HeartbeatTimeout,
	})
}

// outcome is how one pass over the gates ended.
type outcome struct {
	Tip      string // branch tip after the last check commit
	Base     string // base branch, as resolved
	BaseID   string // its commit when resolved
	Gate     string // gate that stopped the pass, "" if all passed
	Kind     string // "rejected" or "failed"
	Reason   string
	Canceled bool
}

// Ship drives one branch through its gates. It restarts from the first gate
// whenever a new head arrives, and opens the PR once every gate passes.
func Ship(ctx workflow.Context, in Input) (string, error) {
	head := in.Head
	if err := workflow.SetQueryHandler(ctx, QueryHead, func() (string, error) { return head, nil }); err != nil {
		return "", err
	}
	heads := workflow.GetSignalChannel(ctx, SignalNewHead)
	authorQ := AuthorQueue(workflow.GetInfo(ctx).WorkflowExecution.ID)

	fresh := false // the next pass reviews afresh: the base, and so its policy, moved
	for {
		head = latest(heads, head)
		notify(ctx, authorQ, Event{Kind: "started", SHA: head})

		passCtx, cancel := workflow.WithCancel(ctx)
		done := workflow.NewBufferedChannel(ctx, 1)
		passFresh := fresh
		fresh = false
		workflow.Go(passCtx, func(gctx workflow.Context) {
			done.Send(gctx, runGates(gctx, in, head, authorQ, passFresh))
		})
		var res outcome
		var sig NewHead
		signalled := false
		sel := workflow.NewSelector(ctx)
		sel.AddReceive(done, func(c workflow.ReceiveChannel, _ bool) { c.Receive(ctx, &res) })
		sel.AddReceive(heads, func(c workflow.ReceiveChannel, _ bool) { c.Receive(ctx, &sig); signalled = true })
		sel.Select(ctx)
		cancel()
		if signalled {
			done.Receive(ctx, &res) // let the cancelled pass unwind
			head = sig.SHA
			continue
		}

		if res.Gate == "" {
			// A head requested after the pass finished is not lost: start
			// again on it, whether it came before or during OpenPR.
			if newer := latest(heads, ""); newer != "" {
				head = newer
				continue
			}
			var url string
			err := workflow.ExecuteActivity(runnerOpts(ctx, 5*time.Minute), ActOpenPR,
				OpenPRIn{RepoURL: in.RepoURL, Branch: in.Branch, Base: res.Base, BaseID: res.BaseID, Head: res.Tip}).Get(ctx, &url)
			var ae *temporal.ApplicationError
			if errors.As(err, &ae) && ae.Type() == "BaseMoved" {
				// The base's policy may have changed: same head, gates again.
				notify(ctx, authorQ, Event{Kind: "base-moved", SHA: res.Tip, Detail: "reviewing again against the new base"})
				head, fresh = res.Tip, true
				continue
			}
			moved := errors.As(err, &ae) && ae.Type() == "BranchMoved"
			if err != nil && !moved {
				notify(ctx, authorQ, Event{Kind: "failed", Gate: "open-pr", SHA: res.Tip, Detail: reason(err)})
				return "", err
			}
			if !moved {
				notify(ctx, authorQ, Event{Kind: "pr-created", SHA: res.Tip, Detail: url})
				notify(ctx, authorQ, Event{Kind: "completed", SHA: res.Tip})
				// Last look, with nothing blocking before the return: Temporal
				// does not let a run complete past a signal that arrived
				// meanwhile, so a head requested up to here starts it again.
				if newer := latest(heads, ""); newer != "" {
					head = newer
					continue
				}
				return url, nil
			}
			// The branch moved past the reviewed head, so no PR for it. Say so:
			// a push made without tardis request signals nothing.
			notify(ctx, authorQ, Event{Kind: "failed", Gate: "open-pr", SHA: res.Tip,
				Detail: "the branch moved past the reviewed head; run tardis request on the new head"})
		}

		// Rejected or failed: wait for the author to push a fix and ask again.
		timedOut := false
		wait := workflow.NewSelector(ctx)
		wait.AddReceive(heads, func(c workflow.ReceiveChannel, _ bool) { c.Receive(ctx, &sig) })
		wait.AddFuture(workflow.NewTimer(ctx, AwaitFix), func(workflow.Future) { timedOut = true })
		wait.Select(ctx)
		if timedOut {
			return "", temporal.NewApplicationError("no new head within "+AwaitFix.String(), "Abandoned")
		}
		head = sig.SHA
	}
}

// runGates makes one pass over the gates for head. It never returns an error:
// every way a pass can stop is an outcome the author is told about.
func runGates(ctx workflow.Context, in Input, head, authorQ string, fresh bool) outcome {
	stop := func(gate, kind, why, tip string) outcome {
		if ctx.Err() != nil {
			return outcome{Canceled: true}
		}
		// Both at once: a dead runner must not hold up the author's news, nor
		// an absent author the failure check.
		n := workflow.ExecuteActivity(deliveryOpts(ctx, authorQ), ActNotify, Event{Kind: kind, Gate: gate, SHA: tip, Detail: why})
		c := postCheck(ctx, in, "", "", tip, gate, "failure", why)
		_ = n.Get(ctx, nil)
		_ = c.Get(ctx, nil) // best effort: a failure only blocks
		return outcome{Gate: gate, Kind: kind, Reason: why, Tip: tip}
	}

	var res ResolveOut
	if err := workflow.ExecuteActivity(runnerOpts(ctx, 5*time.Minute), ActResolve,
		ResolveIn{RepoURL: in.RepoURL, SHA: head}).Get(ctx, &res); err != nil {
		return stop("resolve", "failed", reason(err), head)
	}
	if in.Branch == res.Base {
		return stop("resolve", "failed", in.Branch+" is the base branch; ship from a feature branch", head)
	}
	gates := res.Gates

	tip := head
	var names []string
	for _, g := range gates {
		names = append(names, g.Name)
		if err := workflow.ExecuteActivity(gateRetries(authorOpts(ctx, authorQ, g.Timeout), g.Retry), ActAuthorReview,
			AuthorReviewIn{Gate: g.Name, Base: res.Base, BaseID: res.BaseID, Code: head, Tip: tip, Fresh: fresh}).Get(ctx, &tip); err != nil {
			return stop(g.Name, "failed", "author review: "+reason(err), tip)
		}
		var st map[string]string
		if err := workflow.ExecuteActivity(runnerOpts(ctx, 5*time.Minute), ActAttest,
			AttestIn{RepoURL: in.RepoURL, BaseID: res.BaseID, Tip: tip, Gates: []string{g.Name}}).Get(ctx, &st); err != nil {
			return stop(g.Name, "failed", reason(err), tip)
		}
		if st[g.Name] != "valid" {
			return stop(g.Name, "rejected", "check commit "+st[g.Name], tip)
		}
		var v Verdict
		if err := workflow.ExecuteActivity(gateRetries(runnerOpts(ctx, g.Timeout), g.Retry), ActRerun,
			RerunIn{RepoURL: in.RepoURL, Base: res.Base, BaseID: res.BaseID, Tip: tip, Gate: g.Name}).Get(ctx, &v); err != nil {
			return stop(g.Name, "failed", reason(err), tip)
		}
		if !v.Pass {
			return stop(g.Name, "rejected", v.Reason, tip)
		}
		notify(ctx, authorQ, Event{Kind: "passed", Gate: g.Name, SHA: tip})
	}

	// Every check must still hold on the final tip before a PR is opened.
	var st map[string]string
	if err := workflow.ExecuteActivity(runnerOpts(ctx, 5*time.Minute), ActAttest,
		AttestIn{RepoURL: in.RepoURL, BaseID: res.BaseID, Tip: tip, Gates: names}).Get(ctx, &st); err != nil {
		return stop("chain", "failed", reason(err), tip)
	}
	for _, n := range names {
		if st[n] != "valid" {
			return stop(n, "rejected", "check commit "+st[n]+" on the final tip", tip)
		}
	}
	// Every gate's success goes on the final tip, the commit the PR is opened
	// for: GitHub shows, and OpenPR reads, the checks on the head only.
	for _, n := range names {
		// No PR without its checks: a refused or failed success stops the pass.
		if err := postCheck(ctx, in, res.Base, res.BaseID, tip, n, "success", "").Get(ctx, nil); err != nil {
			return stop(n, "failed", "posting the success check: "+reason(err), tip)
		}
	}
	return outcome{Tip: tip, Base: res.Base, BaseID: res.BaseID}
}

// reason turns an activity error into words for the author. A timeout means
// the worker that should have answered is gone.
func reason(err error) string {
	var te *temporal.TimeoutError
	if errors.As(err, &te) {
		return "runner died"
	}
	var ae *temporal.ApplicationError
	if errors.As(err, &ae) {
		return ae.Error()
	}
	return err.Error()
}

// notify tells the author; delivery problems never stop the run.
func notify(ctx workflow.Context, authorQ string, e Event) {
	_ = workflow.ExecuteActivity(deliveryOpts(ctx, authorQ), ActNotify, e).Get(ctx, nil)
}

// postCheck asks the runner to post a tardis/<gate> check run, waiting for a
// runner if none is up. The runner posts a success only for a gate it re-ran
// and passed itself; the workflow cannot vouch for one.
func postCheck(ctx workflow.Context, in Input, base, baseID, sha, gate, conclusion, summary string) workflow.Future {
	return workflow.ExecuteActivity(deliveryOpts(ctx, RunnerQueue), ActPostCheck,
		CheckIn{RepoURL: in.RepoURL, Base: base, BaseID: baseID, SHA: sha, Gate: gate, Conclusion: conclusion, Summary: summary})
}

// latest drains queued new-head signals and returns the newest head.
func latest(ch workflow.ReceiveChannel, head string) string {
	var sig NewHead
	for ch.ReceiveAsync(&sig) {
		head = sig.SHA
	}
	return head
}
