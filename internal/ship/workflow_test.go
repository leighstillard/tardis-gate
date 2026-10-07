package ship

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

// fake records every activity call and lets a test script outcomes.
type fake struct {
	mu      sync.Mutex
	gates   []string
	calls   []string // "author:<gate>@<code>", "rerun:<gate>@<tip>", ...
	events  []string
	checks  []string
	openPRs int
	rerun   func(in RerunIn) (Verdict, error) // nil: pass
}

func (f *fake) log(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }

func (f *fake) register(env *testsuite.TestWorkflowEnvironment) {
	reg := func(name string, fn any) { env.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: name}) }
	reg(ActResolve, func(_ context.Context, in ResolveIn) ([]GateInfo, error) {
		var out []GateInfo
		for _, g := range f.gates {
			out = append(out, GateInfo{Name: g, Timeout: 10 * time.Minute})
		}
		return out, nil
	})
	reg(ActAuthorReview, func(_ context.Context, in AuthorReviewIn) (string, error) {
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
		f.log("rerun:" + in.Gate + "@" + in.Tip)
		if f.rerun != nil {
			return f.rerun(in)
		}
		return Verdict{Pass: true}, nil
	})
	reg(ActPostCheck, func(_ context.Context, in CheckIn) error {
		f.mu.Lock()
		f.checks = append(f.checks, in.Name+" "+in.Conclusion)
		f.mu.Unlock()
		return nil
	})
	reg(ActOpenPR, func(_ context.Context, in OpenPRIn) (string, error) {
		f.mu.Lock()
		f.openPRs++
		f.mu.Unlock()
		return "pr://" + in.Branch, nil
	})
	reg(ActNotify, func(_ context.Context, e Event) error {
		f.mu.Lock()
		f.events = append(f.events, e.String())
		f.mu.Unlock()
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

var in = Input{RepoURL: "file:///r.git", RepoID: "local/r", Branch: "feature", Base: "main", Head: "c1"}

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
	if got := strings.Join(f.checks, ","); got != "tardis/simplify success,tardis/verify success,tardis/review success" {
		t.Errorf("checks %s", got)
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
