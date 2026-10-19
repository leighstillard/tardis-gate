package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewRunsTheBlindReview(t *testing.T) {
	var asked string
	answer := `"[]"`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		asked = string(b)
		w.Write([]byte(`{"choices": [{"message": {"content": ` + answer + `}}]}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	t.Chdir(dir)
	write := func(name, s string) {
		os.MkdirAll(filepath.Dir(name), 0o755)
		if err := os.WriteFile(name, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".tardis/config.yml", "verify_runbook: RUNBOOK.md\ngates:\n  - name: verify\nprovider:\n  base_url: "+srv.URL+"\n  model: openai/m\n")
	write("RUNBOOK.md", "start the frobnicator")
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "base"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write("diff", "+frob()\n")
	out := filepath.Join(dir, "findings")
	t.Setenv("TARDIS_BASE", "HEAD")
	t.Setenv("TARDIS_DIFF_FILE", filepath.Join(dir, "diff"))
	t.Setenv("TARDIS_FINDINGS_OUT", out)
	t.Setenv("TARDIS_TOOL", "anthropic/claude-code/x")
	t.Setenv("TARDIS_RUNBOOK", "RUNBOOK.md")
	t.Setenv("TARDIS_AUDITOR_GATES", "another")

	code, _, stderr := runCLI("review", "verify")
	b, _ := os.ReadFile(out)
	if code != 0 || !strings.HasPrefix(string(b), "No findings from openaicompat openai/m") {
		t.Errorf("no findings: exit %d, findings %q, stderr %q", code, b, stderr)
	}
	if !strings.Contains(asked, "frob()") || !strings.Contains(asked, "start the frobnicator") {
		t.Errorf("the request lacks the diff or the runbook: %s", asked)
	}
	if !strings.Contains(stderr, "are not the auditor's") {
		t.Errorf("no warning for other reference gates: %q", stderr)
	}

	answer = `"[{\"path\": \"x.go\", \"line\": 3, \"severity\": \"high\", \"claim\": \"frob leaks\"}]"`
	if code, stdout, _ := runCLI("review", "verify"); code != 1 || !strings.Contains(stdout, "x.go:3 [high] frob leaks") {
		t.Errorf("findings: exit %d, stdout %q", code, stdout)
	}
	if code, _, stderr := runCLI("review", "design"); code != 2 || !strings.Contains(stderr, "not enabled") {
		t.Errorf("a gate not enabled: exit %d, %q", code, stderr)
	}
}
