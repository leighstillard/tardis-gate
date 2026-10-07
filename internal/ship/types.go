// Package ship is the Temporal workflow that drives a diff through its gates,
// plus the activities the author's machine and the runner provide.
package ship

import (
	"fmt"
	"strings"
	"time"
)

// Task queues, signal and query names.
const (
	WorkflowQueue = "tardis" // polled by the author's tardis and by the runner
	RunnerQueue   = "runner" // runner activities only
	SignalNewHead = "new-head"
	QueryHead     = "head"
	WorkflowName  = "Ship"
)

// Activity names, shared by the workflow and the workers that register them.
const (
	ActResolve      = "Resolve"
	ActAttest       = "Attest"
	ActRerun        = "Rerun"
	ActPostCheck    = "PostCheck"
	ActOpenPR       = "OpenPR"
	ActAuthorReview = "AuthorReview"
	ActNotify       = "Notify"
)

// WorkflowID is one ship run per branch: a new request on the branch signals it.
func WorkflowID(repoID, branch string) string { return "ship/" + repoID + "/" + branch }

// AuthorQueue is where the author's machine serves activities for one run.
func AuthorQueue(workflowID string) string { return "author/" + workflowID }

// Input starts a ship run.
type Input struct {
	RepoURL string // what the runner clones
	RepoID  string // owner/repo, for the workflow ID
	Branch  string
	Base    string // base branch name
	Head    string // the code commit to ship
}

// NewHead is the payload of SignalNewHead.
type NewHead struct{ SHA string }

// Event is one thing the author hears about.
type Event struct {
	Kind   string // started | passed | rejected | failed | pr-created | completed
	Gate   string
	SHA    string
	Detail string
}

// Terminal reports whether the author's tardis can stop listening after e.
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
	}
	return strings.TrimSpace(e.Kind + " " + short + " " + e.Detail)
}

// GateInfo is what the workflow needs to know about a gate.
type GateInfo struct {
	Name    string
	Timeout time.Duration
}

// Verdict is the runner's independent judgement of one gate.
type Verdict struct {
	Pass   bool
	Reason string
}

// Activity inputs.
type (
	ResolveIn struct{ RepoURL, Base, SHA string }
	AttestIn  struct {
		RepoURL, Base, Tip string
		Gates              []string
	}
	RerunIn        struct{ RepoURL, Base, Tip, Gate string }
	CheckIn        struct{ RepoURL, SHA, Name, Conclusion, Summary string }
	OpenPRIn       struct{ RepoURL, Branch, Base, Head string }
	AuthorReviewIn struct{ Gate, Base, Code, Tip string }
)
