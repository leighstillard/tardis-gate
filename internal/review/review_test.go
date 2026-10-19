package review

import (
	"context"
	"strings"
	"testing"
)

func TestBlindAndJudgeParseFencedAnswers(t *testing.T) {
	var prompts []string
	ask := func(_ context.Context, system, user string) (string, error) {
		prompts = append(prompts, system+"\n"+user)
		if strings.Contains(system, "independent reviewer") {
			return "Sure:\n```json\n{\"missed\": [], \"disputed\": [\"says no bugs\"]}\n```", nil
		}
		return "```json\n[{\"path\":\"a.go\",\"line\":3,\"severity\":\"high\",\"claim\":\"nil\\nderef\"}]\n```", nil
	}
	ctx := context.Background()
	f, err := Blind(ctx, ask, "Find bugs.", "+x", "")
	if err != nil || len(f) != 1 || f[0].String() != "a.go:3 [high] nil deref" {
		t.Fatalf("blind: %v, %v", f, err)
	}
	missed, disputed, err := Judge(ctx, ask, "No bugs found.", f)
	if err != nil || len(missed) != 0 || len(disputed) != 1 {
		t.Fatalf("judge: %v %v %v", missed, disputed, err)
	}
	if strings.Contains(prompts[0], "No bugs found.") || !strings.Contains(prompts[1], "No bugs found.") {
		t.Error("the worker's summary must reach the judge and only the judge")
	}
	if _, err := Blind(ctx, func(context.Context, string, string) (string, error) { return "looks fine", nil }, "", "", ""); err == nil {
		t.Error("an answer without JSON was accepted")
	}
	for _, out := range []string{`[{}]`, `[null]`} {
		if _, err := Blind(ctx, func(context.Context, string, string) (string, error) { return out, nil }, "", "", ""); err == nil {
			t.Errorf("blind accepted %s", out)
		}
	}
	for _, out := range []string{`{}`, `{"missed": null, "disputed": []}`, `{"missed": []}`} {
		if _, _, err := Judge(ctx, func(context.Context, string, string) (string, error) { return out, nil }, "", f); err == nil {
			t.Errorf("judge accepted %s", out)
		}
	}
}

func TestRenderCaps(t *testing.T) {
	f := make([]Finding, 5)
	if got := strings.Count(Render(f, 3), "\n"); got != 3 || !strings.Contains(Render(f, 3), "and 3 more") {
		t.Errorf("Render(5, 3) =\n%s", Render(f, 3))
	}
	if got := strings.Count(Render(f, 5), "\n"); got != 5 {
		t.Errorf("Render(5, 5) has %d lines", got)
	}
}
