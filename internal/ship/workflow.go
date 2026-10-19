package ship

import (
	"errors"
	"fmt"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// Timeouts. A dead auditor shows within HeartbeatTimeout while an activity
// runs, or ScheduleToStart when no auditor is polling at all.
const (
	HeartbeatTimeout = 45 * time.Second
	ScheduleToStart  = 2 * time.Minute
	MaxGateTime      = 20 * time.Minute
	ScheduleToClose  = 60 * time.Minute
	AwaitDelivery    = 24 * time.Hour // notifications and check runs wait this long for a worker
	AwaitFix         = 7 * 24 * time.Hour
	MaxPasses        = 50 // then the run continues as new, to keep its history bounded
)

var retry = &temporal.RetryPolicy{
	InitialInterval:        10 * time.Second,
	BackoffCoefficient:     2,
	MaximumInterval:        2 * time.Minute,
	MaximumAttempts:        3,
	NonRetryableErrorTypes: []string{"Rejected", "Malformed"},
}

func auditorOpts(ctx workflow.Context, timeout time.Duration) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:              AuditorQueue,
		ScheduleToStartTimeout: ScheduleToStart,
		StartToCloseTimeout:    min(timeout, MaxGateTime),
		ScheduleToCloseTimeout: ScheduleToClose,
		HeartbeatTimeout:       HeartbeatTimeout,
		RetryPolicy:            retry,
	})
}

func workerOpts(ctx workflow.Context, queue string, timeout time.Duration) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:              queue,
		StartToCloseTimeout:    min(timeout, MaxGateTime),
		ScheduleToCloseTimeout: AwaitDelivery, // the worker may attach later
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
	// Room for every attempt at its full length, plus the backoff between.
	if need := time.Duration(retries+1)*opts.StartToCloseTimeout + time.Duration(retries)*p.MaximumInterval; need > opts.ScheduleToCloseTimeout {
		opts.ScheduleToCloseTimeout = need
	}
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
	// Each pass has an ID, carried on its events, so a notification delivered
	// again from an earlier pass can be told apart, even for the same commit.
	pass := ""
	if err := workflow.SetQueryHandler(ctx, QueryPass, func() (string, error) { return pass, nil }); err != nil {
		return "", err
	}
	heads := workflow.GetSignalChannel(ctx, SignalNewHead)
	info := workflow.GetInfo(ctx)
	workerQ := WorkerQueue(info.WorkflowExecution.ID)

	for passes := 0; ; passes++ {
		head = latest(heads, head)
		// Bound history by pass count, or sooner if Temporal says it is big.
		if passes == MaxPasses || passes > 0 && workflow.GetInfo(ctx).GetContinueAsNewSuggested() {
			// Queued heads are drained above, so the newest goes with it.
			next := in
			next.Head = head
			return "", workflow.NewContinueAsNewError(ctx, Ship, next)
		}
		pass = fmt.Sprintf("%s/%d", info.WorkflowExecution.RunID, passes)
		pctx := workflow.WithValue(ctx, passKey{}, pass)
		notify(pctx, workerQ, Event{Kind: "started", SHA: head})

		passCtx, cancel := workflow.WithCancel(pctx)
		done := workflow.NewBufferedChannel(ctx, 1)
		workflow.Go(passCtx, func(gctx workflow.Context) {
			done.Send(gctx, runGates(gctx, in, head, workerQ))
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
			err := workflow.ExecuteActivity(auditorOpts(ctx, 5*time.Minute), ActOpenPR,
				OpenPRIn{RepoURL: in.RepoURL, Branch: in.Branch, Base: res.Base, BaseID: res.BaseID, Head: res.Tip}).Get(ctx, &url)
			var ae *temporal.ApplicationError
			moved := errors.As(err, &ae) && (ae.Type() == "BranchMoved" || ae.Type() == "BaseMoved")
			if err != nil && !moved {
				notify(pctx, workerQ, Event{Kind: "failed", Gate: "open-pr", SHA: res.Tip, Detail: reason(err, "auditor")})
				return "", err
			}
			if !moved {
				notify(pctx, workerQ, Event{Kind: "pr-created", SHA: res.Tip, Detail: url})
				notify(pctx, workerQ, Event{Kind: "completed", SHA: res.Tip})
				// Last look, with nothing blocking before the return: Temporal
				// does not let a run complete past a signal that arrived
				// meanwhile, so a head requested up to here starts it again.
				if newer := latest(heads, ""); newer != "" {
					head = newer
					continue
				}
				return url, nil
			}
			// The branch or the base moved since the gates ran, so no PR. Say so:
			// a push made without tardis request signals nothing. A moved base
			// may carry a new policy, which only a rebase brings under review.
			detail := "the branch moved past the reviewed head; run tardis request on the new head"
			if ae.Type() == "BaseMoved" {
				detail = "the base moved since the gates ran; rebase onto it and run tardis request again"
			}
			notify(pctx, workerQ, Event{Kind: "failed", Gate: "open-pr", SHA: res.Tip, Detail: detail})
		}

		// Rejected or failed: wait for the worker to push a fix and ask again.
		timedOut := false
		timerCtx, stopTimer := workflow.WithCancel(ctx)
		wait := workflow.NewSelector(ctx)
		wait.AddReceive(heads, func(c workflow.ReceiveChannel, _ bool) { c.Receive(ctx, &sig) })
		wait.AddFuture(workflow.NewTimer(timerCtx, AwaitFix), func(workflow.Future) { timedOut = true })
		wait.Select(ctx)
		stopTimer() // a head won: the timer must not linger in history
		if timedOut {
			return "", temporal.NewApplicationError("no new head within "+AwaitFix.String(), "Abandoned")
		}
		head = sig.SHA
	}
}

// runGates makes one pass over the gates for head. It never returns an error:
// every way a pass can stop is an outcome the worker is told about.
func runGates(ctx workflow.Context, in Input, head, workerQ string) outcome {
	stop := func(gate, kind, why, tip string) outcome {
		if ctx.Err() != nil {
			return outcome{Canceled: true}
		}
		// Both at once: a dead auditor must not hold up the worker's news, nor
		// an absent worker the failure check.
		// Kept for as long as the run waits for a fix, so a worker who
		// detached during the pass still hears why it stopped.
		n := workflow.ExecuteActivity(workflow.WithScheduleToCloseTimeout(deliveryOpts(ctx, workerQ), AwaitFix), ActNotify, Event{Kind: kind, Gate: gate, SHA: tip, Detail: why, Pass: passOf(ctx)})
		c := postCheck(ctx, in, "", "", tip, gate, "failure", why)
		_ = n.Get(ctx, nil)
		_ = c.Get(ctx, nil) // best effort: a failure only blocks
		return outcome{Gate: gate, Kind: kind, Reason: why, Tip: tip}
	}

	var res ResolveOut
	if err := workflow.ExecuteActivity(auditorOpts(ctx, 5*time.Minute), ActResolve,
		ResolveIn{RepoURL: in.RepoURL, SHA: head}).Get(ctx, &res); err != nil {
		return stop("resolve", "failed", reason(err, "auditor"), head)
	}
	if in.Branch == res.Base {
		return stop("resolve", "failed", in.Branch+" is the base branch; ship from a feature branch", head)
	}
	gates := res.Gates

	tip := head
	var names []string
	for _, g := range gates {
		names = append(names, g.Name)
		if err := workflow.ExecuteActivity(gateRetries(workerOpts(ctx, workerQ, g.Timeout), g.Retry), ActWorkerReview,
			WorkerReviewIn{Gate: g.Name, Base: res.Base, BaseID: res.BaseID, Code: head, Tip: tip, GatesDigest: res.GatesDigest}).Get(ctx, &tip); err != nil {
			return stop(g.Name, "failed", "worker review: "+reason(err, "worker"), tip)
		}
		var st map[string]string
		if err := workflow.ExecuteActivity(auditorOpts(ctx, 5*time.Minute), ActAttest,
			AttestIn{RepoURL: in.RepoURL, BaseID: res.BaseID, Tip: tip, Gates: []string{g.Name}}).Get(ctx, &st); err != nil {
			return stop(g.Name, "failed", reason(err, "auditor"), tip)
		}
		if st[g.Name] != "valid" {
			return stop(g.Name, "rejected", claimReason(st[g.Name]), tip)
		}
		var v Verdict
		if err := workflow.ExecuteActivity(gateRetries(auditorOpts(ctx, g.Timeout), g.Retry), ActRerun,
			RerunIn{RepoURL: in.RepoURL, Base: res.Base, BaseID: res.BaseID, Tip: tip, Gate: g.Name}).Get(ctx, &v); err != nil {
			return stop(g.Name, "failed", reason(err, "auditor"), tip)
		}
		if !v.Pass {
			return stop(g.Name, "rejected", v.Reason, tip)
		}
		notify(ctx, workerQ, Event{Kind: "passed", Gate: g.Name, SHA: tip})
	}

	// Every check must still hold on the final tip before a PR is opened.
	var st map[string]string
	if err := workflow.ExecuteActivity(auditorOpts(ctx, 5*time.Minute), ActAttest,
		AttestIn{RepoURL: in.RepoURL, BaseID: res.BaseID, Tip: tip, Gates: names}).Get(ctx, &st); err != nil {
		return stop("chain", "failed", reason(err, "auditor"), tip)
	}
	for _, n := range names {
		if st[n] != "valid" {
			return stop(n, "rejected", claimReason(st[n])+" on the final tip", tip)
		}
	}
	// Every gate's success goes on the final tip, the commit the PR is opened
	// for: GitHub shows, and OpenPR reads, the checks on the head only.
	for _, n := range names {
		// No PR without its checks: a refused or failed success stops the pass.
		if err := postCheck(ctx, in, res.Base, res.BaseID, tip, n, "success", "").Get(ctx, nil); err != nil {
			return stop(n, "failed", "posting the success check: "+reason(err, "auditor"), tip)
		}
	}
	return outcome{Tip: tip, Base: res.Base, BaseID: res.BaseID}
}

// reason turns an activity error from side ("auditor" or "worker") into words
// for the worker. A timeout means that side is gone, unless the work simply
// ran past its own time limit.
func reason(err error, side string) string {
	var te *temporal.TimeoutError
	if errors.As(err, &te) {
		if te.TimeoutType() == enumspb.TIMEOUT_TYPE_START_TO_CLOSE {
			return "took longer than its time limit"
		}
		return side + " died" // stopped heartbeating, or nothing was polling
	}
	var ae *temporal.ApplicationError
	if errors.As(err, &ae) {
		return ae.Error()
	}
	return err.Error()
}

// claimReason words a chain status that is not valid for the worker and the
// check run: "missing check" or "broken check: <reason>".
func claimReason(status string) string {
	if r, ok := strings.CutPrefix(status, "broken("); ok {
		return "broken check: " + strings.TrimSuffix(r, ")")
	}
	return status + " check"
}

// passKey holds the current pass's ID in a workflow context.
type passKey struct{}

func passOf(ctx workflow.Context) string {
	p, _ := ctx.Value(passKey{}).(string)
	return p
}

// notify tells the worker; delivery problems never stop the run.
func notify(ctx workflow.Context, workerQ string, e Event) {
	e.Pass = passOf(ctx)
	_ = workflow.ExecuteActivity(deliveryOpts(ctx, workerQ), ActNotify, e).Get(ctx, nil)
}

// postCheck asks the auditor to post a tardis/<gate> check run, waiting for a
// auditor if none is up. The auditor posts a success only for a gate it re-ran
// and passed itself; the workflow cannot vouch for one.
func postCheck(ctx workflow.Context, in Input, base, baseID, sha, gate, conclusion, summary string) workflow.Future {
	return workflow.ExecuteActivity(deliveryOpts(ctx, AuditorQueue), ActPostCheck,
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
