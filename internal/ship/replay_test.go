package ship

import (
	"testing"

	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// TestReplayRecordedRun replays a real start-dev run (three gates, PR opened)
// against the current workflow code. A failure means a change is not
// deterministic for runs already in flight.
func TestReplayRecordedRun(t *testing.T) {
	r := worker.NewWorkflowReplayer()
	r.RegisterWorkflowWithOptions(Ship, workflow.RegisterOptions{Name: WorkflowName})
	if err := r.ReplayWorkflowHistoryFromJSONFile(nil, "testdata/ship-happy.json"); err != nil {
		t.Fatal(err)
	}
}
