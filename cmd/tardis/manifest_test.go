package main

import (
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
	if code != 0 || resolved != "[\"simplify\",\"verify\"]\n" {
		t.Fatalf("resolve: exit %d out %q err %q", code, resolved, stderr)
	}

	// The resolved list goes straight into chain verify.
	code, out, stderr := runCLI("chain", "verify", "main", "HEAD", "--gates", strings.TrimSpace(resolved), "--repo", dir)
	if code != 1 || !strings.Contains(out, `"simplify": "missing"`) || !strings.Contains(out, `"verify": "missing"`) {
		t.Errorf("chain verify with resolved gates: exit %d out %q err %q", code, out, stderr)
	}
}

func TestManifestLintNotEnrolled(t *testing.T) {
	code, _, stderr := runCLI("manifest", "lint", "--repo", t.TempDir())
	if code != 1 || !strings.Contains(stderr, "not enrolled") {
		t.Errorf("exit %d stderr %q, want 1 and 'not enrolled'", code, stderr)
	}
}
