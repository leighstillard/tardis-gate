package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLocalExitCodeAndOutput(t *testing.T) {
	res, err := Local{}.Run(context.Background(), Job{
		Argv: []string{"sh", "-c", `echo "$GREETING"; echo oops >&2; exit 3`},
		Env:  []string{"GREETING=hi"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || !strings.Contains(res.Output, "hi") || !strings.Contains(res.Output, "oops") {
		t.Errorf("res = %+v", res)
	}
}

func TestLocalCancelIsAnError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := (Local{}).Run(ctx, Job{Argv: []string{"sleep", "5"}}, nil); err == nil {
		t.Error("cancelled job returned no error")
	}
}

func TestLocalCancelKillsGrandchildren(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	// The shell backgrounds a sleep that inherits stdout, then waits on it.
	if _, err := (Local{}).Run(ctx, Job{Argv: []string{"sh", "-c", "sleep 30 & wait"}}, nil); err == nil {
		t.Error("cancelled job returned no error")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Run returned after %v; the grandchild kept it waiting", d)
	}
}

func TestLocalOrphanHoldingPipeDoesNotHang(t *testing.T) {
	start := time.Now()
	// The shell exits at once, leaving a background sleep holding stdout open.
	res, err := Local{}.Run(context.Background(), Job{Argv: []string{"sh", "-c", "sleep 30 & echo done"}}, nil)
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("Run returned after %v, want about WaitDelay", d)
	}
	if err == nil && !strings.Contains(res.Output, "done") {
		t.Errorf("res = %+v", res)
	}
}

func TestLocalMissingBinary(t *testing.T) {
	if _, err := (Local{}).Run(context.Background(), Job{Argv: []string{"tardis-no-such-binary"}}, nil); err == nil {
		t.Error("missing binary returned no error")
	}
}

func TestActionsNotImplemented(t *testing.T) {
	var e Executor = Actions{}
	if _, err := e.Run(context.Background(), Job{}, nil); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("err = %v", err)
	}
}
