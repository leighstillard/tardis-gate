// Package ship is the Temporal workflow that drives a diff through its gates,
// plus the activities the worker's machine and the auditor provide.
package ship

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Task queues, signal and query names.
const (
	WorkflowQueue = "tardis" // polled by the worker's tardis and by the auditor
	AuditorQueue  = "runner" // auditor activities only; ponytail: v0.3 wire name, kept so in-flight runs survive an upgrade (rename with tardis-gate-yfg)
	SignalNewHead = "new-head"
	QueryHead     = "head"
	QueryPass     = "pass"
	WorkflowName  = "Ship"
)

// Activity names, shared by the workflow and the workers that register them.
const (
	ActResolve      = "Resolve"
	ActAttest       = "Attest"
	ActRerun        = "Rerun"
	ActPostCheck    = "PostCheck"
	ActOpenPR       = "OpenPR"
	ActWatchPR      = "WatchPR"
	ActMerge        = "Merge"
	ActWorkerReview = "AuthorReview" // v0.3 wire name, as AuditorQueue
	ActNotify       = "Notify"
)

// WorkflowID is one ship run per branch: a new request on the branch signals it.
func WorkflowID(repoID, branch string) string { return "ship/" + repoID + "/" + branch }

// WorkerQueue is where the worker's machine serves activities for one run.
func WorkerQueue(workflowID string) string { return "author/" + workflowID } // v0.3 wire name, as AuditorQueue

// Input starts a ship run.
type Input struct {
	RepoURL string // what the auditor clones
	RepoID  string // owner/repo, for the workflow ID
	Branch  string
	Head    string // the code commit to ship
}

// NewHead is the payload of SignalNewHead.
type NewHead struct{ SHA string }

// Event is one thing the worker hears about.
type Event struct {
	Kind   string // started | passed | rejected | failed | pr-created | ci | merged | completed
	Pass   string // the pass it belongs to: <run ID>/<n>
	Gate   string
	SHA    string
	Detail string
}

// Terminal reports whether the worker's tardis can stop listening after e.
func (e Event) Terminal() bool {
	return e.Kind == "completed" || e.Kind == "rejected" || e.Kind == "failed"
}

func (e Event) String() string {
	short := e.SHA
	if len(short) > 12 {
		short = short[:12]
	}
	switch e.Kind {
	case "started":
		return fmt.Sprintf("started %s", short)
	case "passed":
		return fmt.Sprintf("%s passed on %s", e.Gate, short)
	case "rejected", "failed":
		return fmt.Sprintf("%s %s: %s", e.Gate, e.Kind, e.Detail)
	case "pr-created":
		return "pr-created " + e.Detail
	case "ci":
		return "ci " + e.Detail
	}
	return strings.TrimSpace(e.Kind + " " + short + " " + e.Detail)
}

// ResolveOut is the base commit the run is pinned to and the gates that apply.
type ResolveOut struct {
	Base   string // the base branch, as the default branch's manifest names it
	BaseID string // the base branch's commit when resolved; every later step uses it
	Gates  []GateInfo
	// GatesDigest is the auditor's reference gates (gates.Digest), so the
	// worker's tardis review can warn when it holds other ones.
	GatesDigest string
}

// GateInfo is what the workflow needs to know about a gate.
type GateInfo struct {
	Name    string
	Timeout time.Duration
	Retry   int // retries after a failed attempt at the gate's review
}

// Verdict is the auditor's independent judgement of one gate.
type Verdict struct {
	Pass   bool
	Reason string
}

// Activity inputs.
type (
	ResolveIn struct{ RepoURL, SHA string }
	AttestIn  struct {
		RepoURL, BaseID, Tip string
		Gates                []string
	}
	RerunIn  struct{ RepoURL, Base, BaseID, Tip, Gate string }
	CheckIn  struct{ RepoURL, Base, BaseID, SHA, Gate, Conclusion, Summary string }
	OpenPRIn struct{ RepoURL, Branch, Base, BaseID, Head string }
	WatchIn  struct {
		RepoURL, Head string
		Number        int
	}
	MergeIn struct {
		RepoURL, Base, BaseID, Head string
		Number                      int
	}
	WorkerReviewIn struct{ Gate, Base, BaseID, Code, Tip, GatesDigest string }
)

// PR is the PR OpenPR opened. Number is 0 when no GitHub App opened one.
type PR struct {
	URL    string
	Number int
}

// UnmarshalJSON also reads OpenPR's v0.4 result, a bare URL, so the runs
// recorded then still replay.
func (p *PR) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &p.URL)
	}
	type plain PR
	return json.Unmarshal(b, (*plain)(p))
}

// CI is how the PR's head came out: Kind success or failure on its CI, or
// moved, closed or merged when the PR itself changed first.
type CI struct{ Kind, Detail string }
