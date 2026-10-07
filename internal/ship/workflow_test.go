package ship

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// fake records every activity call and lets a test script outcomes.
type fake struct {
	mu      sync.Mutex
	gates   []string
	retry   int      // every gate's retry count
	calls   []string // "author:<gate>@<code>", "rerun:<gate>@<tip>", ...
	events  []string
	checks  []string
	checkOn []string // "<name> <conclusion>@<sha>"
	openPRs int
	onOpen  func()                            // called inside OpenPR, before it returns
	openErr error                             // OpenPR's error; nil: ok
	onEvent func(e Event)                     // called inside Notify
	postErr func(in CheckIn) error            // PostCheck's result; nil: ok
	rerun   func(in RerunIn) (Verdict, error) // nil: pass
}

func (f *fake) log(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }

func (f *fake) register(env *testsuite.TestWorkflowEnvironment) {
	reg := func(name string, fn any) { env.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: name}) }
	reg(ActResolve, func(_ context.Context, in ResolveIn) (ResolveOut, error) {
		out := ResolveOut{Base: "main", BaseID: "b0"}
		for _, g := range f.gates {
			out.Gates = append(out.Gates, GateInfo{Name: g, Timeout: 10 * time.Minute, Retry: f.retry})
		}
		return out, nil
	})
	reg(ActAuthorReview, func(_ context.Context, in AuthorReviewIn) (string, error) {
		if in.Base != "main" || in.BaseID != "b0" {
			return "", fmt.Errorf("author review got base %q at %q, want the resolved main at b0", in.Base, in.BaseID)
		}
		f.log("author:" + in.Gate + "@" + in.Code)
		return in.Tip + "+" + in.Gate, nil // a new check commit on top
	})
	reg(ActAttest, func(_ context.Context, in AttestIn) (map[string]string, error) {
		out := map[string]string{}
		for _, g := range in.Gates {
			out[g] = "valid"
		}
		return out, nil
	})
	reg(ActRerun, func(_ context.Context, in RerunIn) (Verdict, error) {
		if in.Base != "main" || in.BaseID != "b0" {
			return Verdict{}, fmt.Errorf("rerun got base %q at %q, want the resolved main at b0", in.Base, in.BaseID)
		}
		f.log("rerun:" + in.Gate + "@" + in.Tip)
		if f.rerun != nil {
			return f.rerun(in)
		}
		return Verdict{Pass: true}, nil
	})
	reg(ActPostCheck, func(_ context.Context, in CheckIn) error {
		if f.postErr != nil {
			if err := f.postErr(in); err != nil {
				return err
			}
		}
		f.mu.Lock()
		f.checks = append(f.checks, "tardis/"+in.Gate+" "+in.Conclusion)
		f.checkOn = append(f.checkOn, "tardis/"+in.Gate+" "+in.Conclusion+"@"+in.SHA)
		f.mu.Unlock()
		return nil
	})
	reg(ActOpenPR, func(_ context.Context, in OpenPRIn) (string, error) {
		if in.Base != "main" || in.BaseID != "b0" {
			return "", fmt.Errorf("open PR into %q at %q, want the resolved main at b0", in.Base, in.BaseID)
		}
		f.mu.Lock()
		f.openPRs++
		open := f.onOpen
		f.mu.Unlock()
		if open != nil {
			open()
		}
		if f.openErr != nil {
			return "", f.openErr
		}
		return "pr://" + in.Branch, nil
	})
	reg(ActNotify, func(_ context.Context, e Event) error {
		f.mu.Lock()
		f.events = append(f.events, e.String())
		on := f.onEvent
		f.mu.Unlock()
		if on != nil {
			on(e)
		}
		return nil
	})
}

func newEnv(t *testing.T, f *fake) *testsuite.TestWorkflowEnvironment {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(Ship)
	f.register(env)
	return env
}

var in = Input{RepoURL: "file:///r.git", RepoID: "local/r", Branch: "feature", Head: "c1"}

func TestAllGatesPassOpensOnePR(t *testing.T) {
	f := &fake{gates: []string{"simplify", "verify", "review"}}
	env := newEnv(t, f)
	env.ExecuteWorkflow(Ship, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var url string
	env.GetWorkflowResult(&url)
	if url != "pr://feature" || f.openPRs != 1 {
		t.Errorf("url %q, OpenPR calls %d", url, f.openPRs)
	}
	want := []string{
		"author:simplify@c1", "rerun:simplify@c1+simplify",
		"author:verify@c1", "rerun:verify@c1+simplify+verify",
		"author:review@c1", "rerun:review@c1+simplify+verify+review",
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls\n got %v\nwant %v", f.calls, want)
	}
	if last := f.events[len(f.events)-1]; last != "completed c1+simplify+" {
		t.Errorf("last event %q; all %v", last, f.events)
	}
	if !contains(f.events, "pr-created pr://feature") {
		t.Errorf("no pr-created event: %v", f.events)
	}
	// Every gate's success is on the final tip, the PR's head.
	tip := "c1+simplify+verify+review"
	if got, want := strings.Join(f.checkOn, ","), "tardis/simplify success@"+tip+",tardis/verify success@"+tip+",tardis/review success@"+tip; got != want {
		t.Errorf("checks\n got %s\nwant %s", got, want)
	}
}

func TestNewHeadAsTheRunCompletesIsNotLost(t *testing.T) {
	f := &fake{gates: []string{"simplify"}}
	env := newEnv(t, f)
	f.onEvent = func(e Event) {
		if e.Kind == "completed" && e.SHA == "c1+simplify" {
			env.SignalWorkflow(SignalNewHead, NewHead{SHA: "c2"})
		}
	}
	env.ExecuteWorkflow(Ship, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if last := f.events[len(f.events)-1]; last != "completed c2+simplify" {
		t.Errorf("last event %q; all %v", last, f.events)
	}
}

func TestRefusedSuccessCheckStopsBeforeOpenPR(t *testing.T) {
	f := &fake{gates: []string{"simplify"}}
	f.postErr = func(in CheckIn) error {
		if in.Conclusion == "success" {
			return temporal.NewNonRetryableApplicationError("refusing tardis/simplify success", "Rejected", nil)
		}
		return nil
	}
	env := newEnv(t, f)
	env.RegisterDelayedCallback(func() { env.CancelWorkflow() }, time.Hour) // it waits for a fix
	env.ExecuteWorkflow(Ship, in)
	if f.openPRs != 0 {
		t.Errorf("OpenPR called %d times after the success check was refused", f.openPRs)
	}
	found := false
	for _, e := range f.events {
		found = found || strings.HasPrefix(e, "simplify failed: posting the success check")
	}
	if !found {
		t.Errorf("events %v; want a failed event for the refused check", f.events)
	}
}

func TestShippingTheBaseBranchIsRefused(t *testing.T) {
	f := &fake{gates: []string{"simplify"}}
	env := newEnv(t, f)
	env.RegisterDelayedCallback(func() { env.CancelWorkflow() }, time.Hour) // it waits for a new head
	onMain := in
	onMain.Branch = "main"
	env.ExecuteWorkflow(Ship, onMain)
	if len(f.calls) != 0 || !contains(f.events, "resolve failed: main is the base branch; ship from a feature branch") {
		t.Errorf("calls %v, events %v; want a refusal before any review", f.calls, f.events)
	}
}

func TestBranchMovedAtOpenPRTellsTheAuthor(t *testing.T) {
	// The branch moved without a tardis request, so no new head is coming.
	f := &fake{gates: []string{"simplify"}, openErr: temporal.NewNonRetryableApplicationError("moved", "BranchMoved", nil)}
	env := newEnv(t, f)
	env.RegisterDelayedCallback(func() { env.CancelWorkflow() }, time.Hour) // it waits for a new head
	env.ExecuteWorkflow(Ship, in)
	if !contains(f.events, "open-pr failed: the branch moved past the reviewed head; run tardis request on the new head") {
		t.Errorf("events %v; want the author told the branch moved", f.events)
	}
}

func TestBaseMovedAtOpenPRAsksForARebase(t *testing.T) {
	// A moved base may carry a policy the gates never ran under.
	f := &fake{gates: []string{"simplify"}, openErr: temporal.NewNonRetryableApplicationError("moved", "BaseMoved", nil)}
	env := newEnv(t, f)
	env.RegisterDelayedCallback(func() { env.CancelWorkflow() }, time.Hour) // it waits for a new head
	env.ExecuteWorkflow(Ship, in)
	if !contains(f.events, "open-pr failed: the base moved since the gates ran; rebase onto it and run tardis request again") || f.openPRs != 1 {
		t.Errorf("events %v, OpenPR %d; want one attempt and a rebase request", f.events, f.openPRs)
	}
}

func TestNewHeadDuringOpenPRIsNotLost(t *testing.T) {
	f := &fake{gates: []string{"simplify"}}
	env := newEnv(t, f)
	f.onOpen = func() {
		f.onOpen = nil
		env.SignalWorkflow(SignalNewHead, NewHead{SHA: "c2"})
	}
	env.ExecuteWorkflow(Ship, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	want := []string{"author:simplify@c1", "rerun:simplify@c1+simplify", "author:simplify@c2", "rerun:simplify@c2+simplify"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls\n got %v\nwant %v", f.calls, want)
	}
	if last := f.events[len(f.events)-1]; last != "completed c2+simplify" {
		t.Errorf("last event %q; all %v", last, f.events)
	}
}

func TestRejectThenNewHeadRerunsFromFirstGate(t *testing.T) {
	f := &fake{gates: []string{"simplify", "verify", "review"}}
	f.rerun = func(in RerunIn) (Verdict, error) {
		if in.Gate == "verify" && strings.HasPrefix(in.Tip, "c1") {
			return Verdict{Reason: "planted bug in x.go"}, nil
		}
		return Verdict{Pass: true}, nil
	}
	env := newEnv(t, f)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalNewHead, NewHead{SHA: "c2"})
	}, time.Hour)
	env.ExecuteWorkflow(Ship, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var authors []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "author:") {
			authors = append(authors, c)
		}
	}
	want := []string{"author:simplify@c1", "author:verify@c1", "author:simplify@c2", "author:verify@c2", "author:review@c2"}
	if !reflect.DeepEqual(authors, want) {
		t.Errorf("author calls\n got %v\nwant %v", authors, want)
	}
	if !contains(f.events, "verify rejected: planted bug in x.go") {
		t.Errorf("no rejection event: %v", f.events)
	}
	if !contains(f.checks, "tardis/verify failure") || f.openPRs != 1 {
		t.Errorf("checks %v, OpenPR %d", f.checks, f.openPRs)
	}
}

func TestRunnerDeathIsReportedAndCheckFails(t *testing.T) {
	f := &fake{gates: []string{"simplify", "verify"}}
	f.rerun = func(in RerunIn) (Verdict, error) {
		if in.Gate == "verify" {
			return Verdict{}, temporal.NewHeartbeatTimeoutError()
		}
		return Verdict{Pass: true}, nil
	}
	env := newEnv(t, f)
	env.RegisterDelayedCallback(func() {
		var head string
		v, err := env.QueryWorkflow(QueryHead)
		if err == nil {
			_ = v.Get(&head)
		}
		if head != "c1" {
			t.Errorf("query head = %q, %v", head, err)
		}
		env.SignalWorkflow(SignalNewHead, NewHead{SHA: "c2"})
	}, time.Hour)
	f2 := f.rerun
	f.rerun = func(in RerunIn) (Verdict, error) {
		if strings.HasPrefix(in.Tip, "c2") {
			return Verdict{Pass: true}, nil
		}
		return f2(in)
	}
	env.ExecuteWorkflow(Ship, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if !contains(f.events, "verify failed: runner died") {
		t.Errorf("events %v", f.events)
	}
	if !contains(f.checks, "tardis/verify failure") {
		t.Errorf("checks %v", f.checks)
	}
}

func TestGateRetryCountIsHonoured(t *testing.T) {
	for retry, want := range map[int]int{0: 1, 2: 3} {
		f := &fake{gates: []string{"simplify"}, retry: retry}
		f.rerun = func(RerunIn) (Verdict, error) { return Verdict{}, temporal.NewApplicationError("flaky", "Flaky") }
		env := newEnv(t, f)
		env.RegisterDelayedCallback(func() { env.CancelWorkflow() }, time.Hour) // it waits for a fix
		env.ExecuteWorkflow(Ship, in)
		if got := len(f.calls) - 1; got != want { // calls: one author review, then the reruns
			t.Errorf("retry %d: %d rerun attempts, want %d (%v)", retry, got, want, f.calls)
		}
	}
}

func TestFailureCheckDoesNotWaitForTheAuthor(t *testing.T) {
	// The author's notification is stuck until the failure check is posted;
	// sent one after the other, the check would never go out.
	f := &fake{gates: []string{"simplify"}}
	f.rerun = func(RerunIn) (Verdict, error) { return Verdict{Reason: "no"}, nil }
	posted := false
	f.onEvent = func(e Event) {
		if e.Kind != "rejected" {
			return
		}
		for end := time.Now().Add(2 * time.Second); time.Now().Before(end) && !posted; time.Sleep(10 * time.Millisecond) {
			f.mu.Lock()
			posted = contains(f.checks, "tardis/simplify failure")
			f.mu.Unlock()
		}
	}
	env := newEnv(t, f)
	env.RegisterDelayedCallback(func() { env.CancelWorkflow() }, time.Hour)
	env.ExecuteWorkflow(Ship, in)
	if !posted {
		t.Errorf("failure check not posted while the notification was undelivered; checks %v", f.checks)
	}
}

func TestQueuedSignalsKeepTheNewestHead(t *testing.T) {
	f := &fake{gates: []string{"simplify"}}
	f.rerun = func(in RerunIn) (Verdict, error) {
		return Verdict{Pass: !strings.HasPrefix(in.Tip, "c1")}, nil
	}
	env := newEnv(t, f)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalNewHead, NewHead{SHA: "c2"})
		env.SignalWorkflow(SignalNewHead, NewHead{SHA: "c3"})
	}, time.Hour)
	env.ExecuteWorkflow(Ship, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	want := []string{"author:simplify@c1", "rerun:simplify@c1+simplify", "author:simplify@c3", "rerun:simplify@c3+simplify"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls\n got %v\nwant %v", f.calls, want)
	}
}

func TestContinuesAsNewAfterMaxPasses(t *testing.T) {
	f := &fake{gates: []string{"simplify"}}
	f.rerun = func(RerunIn) (Verdict, error) { return Verdict{Reason: "no"}, nil }
	env := newEnv(t, f)
	for i := 1; i <= MaxPasses; i++ {
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(SignalNewHead, NewHead{SHA: fmt.Sprintf("c%d", i+1)})
		}, time.Duration(i)*time.Hour)
	}
	env.ExecuteWorkflow(Ship, in)
	var can *workflow.ContinueAsNewError
	if err := env.GetWorkflowError(); !errors.As(err, &can) {
		t.Fatalf("err = %v; want continue-as-new after %d passes", err, MaxPasses)
	}
	var next Input
	if err := converter.GetDefaultDataConverter().FromPayloads(can.Input, &next); err != nil || next.Head != fmt.Sprintf("c%d", MaxPasses+1) {
		t.Errorf("continued with %+v, %v; want the newest head", next, err)
	}
}

func TestAbandonedAfterAWeekWithoutAFix(t *testing.T) {
	f := &fake{gates: []string{"simplify"}}
	f.rerun = func(RerunIn) (Verdict, error) { return Verdict{Reason: "no"}, nil }
	env := newEnv(t, f)
	env.ExecuteWorkflow(Ship, in)
	err := env.GetWorkflowError()
	if err == nil || !strings.Contains(err.Error(), "no new head") {
		t.Errorf("err = %v", err)
	}
}

func TestReasonNamesDeadRunner(t *testing.T) {
	if got := reason(temporal.NewHeartbeatTimeoutError()); got != "runner died" {
		t.Errorf("reason = %q", got)
	}
	if got := reason(fmt.Errorf("boom")); got != "boom" {
		t.Errorf("reason = %q", got)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
