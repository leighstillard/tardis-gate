package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManifestLintAndResolveFeedChainVerify(t *testing.T) {
	dir, git := gitRepo(t) // on branch feature, one code commit after main
	git("checkout", "-q", "main")
	for rel, body := range map[string]string{
		".tardis/config.yml": "verify_runbook: RUNBOOK.md\ngates:\n  - name: simplify\n  - name: verify\n  - name: design\n    applies_when: [\"web/**\"]\n",
		"RUNBOOK.md":         "run it\n",
	} {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	git("commit", "-q", "-m", "enrol")
	git("checkout", "-q", "feature")
	git("rebase", "-q", "main")

	if code, out, stderr := runCLI("manifest", "lint", "--repo", dir); code != 0 ||
		out != "ok: 3 gates in order: simplify, verify, design\n" {
		t.Fatalf("lint: exit %d out %q err %q", code, out, stderr)
	}
	code, resolved, stderr := runCLI("manifest", "resolve", "main", "HEAD", "--repo", dir)
	want := fmt.Sprintf(`{"base":%q,"head":%q,"gates":["simplify","verify"]}`+"\n", git("rev-parse", "main"), git("rev-parse", "HEAD"))
	if code != 0 || resolved != want {
		t.Fatalf("resolve: exit %d out %q err %q, want %q", code, resolved, stderr, want)
	}

	// The resolved list goes straight into chain verify.
	code, out, stderr := runCLI("chain", "verify", "main", "HEAD", "--gates", strings.TrimSpace(resolved), "--repo", dir)
	if code != 1 || !strings.Contains(out, `"simplify": "missing"`) || !strings.Contains(out, `"verify": "missing"`) {
		t.Errorf("chain verify with resolved gates: exit %d out %q err %q", code, out, stderr)
	}

	// A branch that rewrites the config, committed or not, cannot drop gates:
	// resolve reads the policy from base.
	cfg := filepath.Join(dir, ".tardis", "config.yml")
	if err := os.WriteFile(cfg, []byte("gates:\n  - name: simplify\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("commit", "-q", "-am", "drop verify")
	if code, out, stderr := runCLI("manifest", "resolve", "main", "HEAD", "--repo", dir); code != 0 || !strings.Contains(out, `"gates":["simplify","verify"]`) {
		t.Errorf("resolve after the branch edits the config: exit %d out %q err %q, want simplify and verify", code, out, stderr)
	}

	// The list resolved before that commit is bound to the old head: refused.
	if code, _, stderr := runCLI("chain", "verify", "main", "HEAD", "--gates", strings.TrimSpace(resolved), "--repo", dir); code != 2 || !strings.Contains(stderr, "resolved for other commits") {
		t.Errorf("stale resolved list: exit %d err %q, want 2 and a resolve-again message", code, stderr)
	}
}

func TestChainVerifyManifestUsesOneResolution(t *testing.T) {
	dir, git := gitRepo(t) // feature: one code commit after main
	git("checkout", "-q", "main")
	cfg := filepath.Join(dir, ".tardis", "config.yml")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("gates:\n  - name: simplify\n  - name: design\n    applies_when: [\"web/**\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "enrol")
	git("checkout", "-q", "feature")
	git("rebase", "-q", "main")

	code, out, stderr := runCLI("chain", "verify", "main", "HEAD", "--manifest", "--repo", dir)
	if code != 1 || !strings.Contains(out, `"simplify": "missing"`) || strings.Contains(out, "design") {
		t.Errorf("--manifest: exit %d out %q err %q, want simplify missing and no design", code, out, stderr)
	}
	if code, _, _ := runCLI("chain", "verify", "main", "HEAD", "--manifest", "--gates", "x", "--repo", dir); code != 2 {
		t.Errorf("--manifest with --gates: exit %d, want 2", code)
	}
}

func TestResolveRefusesAHeadThatBreaksThePolicy(t *testing.T) {
	// Merged, such a head would leave main unable to resolve anything.
	dir, git := gitRepo(t)
	git("checkout", "-q", "main")
	cfg := filepath.Join(dir, ".tardis", "config.yml")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("gates:\n  - name: simplify\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "enrol")
	git("checkout", "-q", "feature")
	git("rebase", "-q", "main")

	if err := os.WriteFile(cfg, []byte("gates:\n  - name: simplify\n    enable: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("commit", "-q", "-am", "break the policy")
	if code, _, stderr := runCLI("manifest", "resolve", "main", "HEAD", "--repo", dir); code != 1 || !strings.Contains(stderr, "HEAD: ") {
		t.Errorf("head with a broken manifest: exit %d err %q, want 1 naming HEAD", code, stderr)
	}
	git("rm", "-q", ".tardis/config.yml")
	git("commit", "-q", "-m", "unenrol")
	if code, _, stderr := runCLI("manifest", "resolve", "main", "HEAD", "--repo", dir); code != 1 || !strings.Contains(stderr, "unenrolment is an operator step") {
		t.Errorf("head without a manifest: exit %d err %q, want 1 and an operator-step message", code, stderr)
	}
}

func TestUnmovedCatchesAMovedHead(t *testing.T) {
	dir, git := gitRepo(t)
	pinned := [2]string{git("rev-parse", "main"), git("rev-parse", "HEAD")}
	if err := unmoved(dir, []string{"main", "HEAD"}, pinned); err != nil {
		t.Fatalf("nothing moved: %v", err)
	}
	git("commit", "-q", "--allow-empty", "-m", "pushed meanwhile")
	if err := unmoved(dir, []string{"main", "HEAD"}, pinned); err == nil || !strings.Contains(err.Error(), "HEAD is now") {
		t.Errorf("err = %v, want HEAD is now …", err)
	}
}

func TestParseResolvedFailsClosed(t *testing.T) {
	for _, s := range []string{
		`{"base":"a","head":"b"}`,
		`{"base":"a","head":"b","gates":null}`,
		`{"base":"a","head":"b","gatez":["x"]}`,
		`{"base":"a","head":"b","gates":[],"extra":1}`,
		`{"base":"a","head":"b","gates":[]} {}`,
		`{"base":"a","head":"b","gates":[]}]`,
		`{"base":"a","head":"b","gates":[]}}`,
		`{"head":"b","gates":[]}`,
	} {
		if _, err := parseResolved(s); err == nil {
			t.Errorf("parseResolved(%s) accepted", s)
		}
	}
	if r, err := parseResolved(`{"base":"a","head":"b","gates":[]}`); err != nil || r.Gates == nil {
		t.Errorf("explicit empty list: %+v, %v", r, err)
	}
}

func TestChainVerifyEmptyResolution(t *testing.T) {
	dir, git := gitRepo(t)
	none := fmt.Sprintf(`{"base":%q,"head":%q,"gates":[]}`, git("rev-parse", "main"), git("rev-parse", "HEAD"))
	if code, out, stderr := runCLI("chain", "verify", "main", "HEAD", "--gates", none, "--repo", dir); code != 0 || strings.TrimSpace(out) != "{}" {
		t.Errorf("no gate applies: exit %d out %q err %q, want 0 and {}", code, out, stderr)
	}
	if code, _, _ := runCLI("chain", "verify", "main", "HEAD", "--gates", "[]", "--repo", dir); code != 2 {
		t.Errorf("--gates []: exit %d, want 2 (an unbound list)", code)
	}
	if code, _, _ := runCLI("chain", "verify", "main", "HEAD", "--gates", "", "--repo", dir); code != 2 {
		t.Errorf("--gates \"\": exit %d, want 2", code)
	}
}

func TestManifestLintNotEnrolled(t *testing.T) {
	code, _, stderr := runCLI("manifest", "lint", "--repo", t.TempDir())
	if code != 1 || !strings.Contains(stderr, "not enrolled") {
		t.Errorf("exit %d stderr %q, want 1 and 'not enrolled'", code, stderr)
	}
}
