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
