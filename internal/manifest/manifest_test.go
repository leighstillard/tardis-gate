package manifest

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const sample = "../../testdata/sample-repo"

// copySample copies the sample repo into a temp dir and applies edits
// (path relative to the repo → content; "" deletes).
func copySample(t *testing.T, edits map[string]string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(sample, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(sample, p)
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return write(filepath.Join(dst, rel), string(data))
	})
	if err != nil {
		t.Fatal(err)
	}
	for rel, content := range edits {
		p := filepath.Join(dst, rel)
		if content == "" {
			os.Remove(p)
			continue
		}
		if err := write(p, content); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func write(p, content string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o644)
}

func names(gs []Gate) []string {
	var out []string
	for _, g := range gs {
		out = append(out, g.Name)
	}
	return out
}

func wantErr(t *testing.T, root, substr string) {
	t.Helper()
	_, err := Load(root)
	if err == nil || !strings.Contains(err.Error(), substr) {
		t.Errorf("Load error = %v, want it to mention %q", err, substr)
	}
}

func TestLoadSample(t *testing.T) {
	m, err := Load(sample)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(m.Gates), []string{"simplify", "verify", "design", "review"}; !reflect.DeepEqual(got, want) {
		t.Errorf("gates = %v, want %v", got, want)
	}
	for _, g := range m.Gates {
		if g.Source != "reference:"+g.Name {
			t.Errorf("%s source = %q, want the reference gate", g.Name, g.Source)
		}
		if g.Timeout <= 0 || len(g.Run) == 0 {
			t.Errorf("%s: timeout %v run %v", g.Name, g.Timeout, g.Run)
		}
	}
	if m.Gates[3].MustDifferFrom != "author" {
		t.Errorf("review must_differ_from = %q", m.Gates[3].MustDifferFrom)
	}
	if !strings.HasSuffix(m.VerifyRunbook, "docs/RUNBOOK.md") {
		t.Errorf("runbook = %q", m.VerifyRunbook)
	}
}

func TestNotEnrolled(t *testing.T) {
	if _, err := Load(t.TempDir()); err != ErrNotEnrolled {
		t.Errorf("err = %v, want ErrNotEnrolled", err)
	}
}

func TestDuplicateGate(t *testing.T) {
	root := copySample(t, map[string]string{ConfigPath: "verify_runbook: docs/RUNBOOK.md\ngates:\n  - name: simplify\n  - name: simplify\n"})
	wantErr(t, root, `duplicate gate "simplify"`)
}

func TestDirWithoutGateYML(t *testing.T) {
	root := copySample(t, map[string]string{ConfigPath: "gates:\n  - name: lint\n    dir: tools/lint\n"})
	wantErr(t, root, filepath.Join("tools", "lint", "gate.yml"))
}

func TestUnknownGate(t *testing.T) {
	root := copySample(t, map[string]string{ConfigPath: "gates:\n  - name: nosuch\n"})
	wantErr(t, root, `gate "nosuch": no .tardis/gates/nosuch/gate.yml and no reference gate`)
}

func TestRunbook(t *testing.T) {
	wantErr(t, copySample(t, map[string]string{"docs/RUNBOOK.md": ""}), "verify_runbook: docs/RUNBOOK.md not found")
	wantErr(t, copySample(t, map[string]string{ConfigPath: "gates:\n  - name: verify\n"}), "verify_runbook: required")
	if _, err := Load(copySample(t, map[string]string{ConfigPath: "gates:\n  - name: simplify\n"})); err != nil {
		t.Errorf("no verify gate, no runbook: %v", err)
	}
}

func TestGateValidation(t *testing.T) {
	cfg := "gates:\n  - name: lint\n"
	for name, tc := range map[string]struct{ yml, want string }{
		"empty run":    {"name: lint\nrun: []\ntimeout: 1m\n", "run is empty"},
		"no timeout":   {"name: lint\nrun: [make, lint]\n", "timeout must be a positive duration"},
		"bad timeout":  {"name: lint\nrun: [make]\ntimeout: soon\n", "line 3: cannot unmarshal !!str `soon` into time.Duration"},
		"name differs": {"name: other\nrun: [make]\ntimeout: 1m\n", `name is "other", want "lint"`},
		"bad differ":   {"name: lint\nrun: [make]\ntimeout: 1m\nmust_differ_from: me\n", "must_differ_from"},
		"neg retry":    {"name: lint\nrun: [make]\ntimeout: 1m\nretry: -1\n", "retry must not be negative"},
		"unknown key":  {"name: lint\nrun: [make]\ntimeout: 1m\nrunn: [x]\n", "runn"},
	} {
		root := copySample(t, map[string]string{ConfigPath: cfg, ".tardis/gates/lint/gate.yml": tc.yml})
		_, err := Load(root)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestReportsEveryProblem(t *testing.T) {
	root := copySample(t, map[string]string{
		ConfigPath:        "verify_runbook: missing.md\ngates:\n  - name: a\n  - name: a\n  - name: b\n    dir: nowhere\n",
		"docs/RUNBOOK.md": "",
	})
	_, err := Load(root)
	for _, want := range []string{`gate "a"`, `duplicate gate "a"`, filepath.Join("nowhere", "gate.yml"), "missing.md not found"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
}

func TestLocalGateOverridesReference(t *testing.T) {
	root := copySample(t, map[string]string{
		".tardis/gates/simplify/gate.yml": "name: simplify\nrun: [make, simplify]\ntimeout: 2m\n",
	})
	m, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if g := m.Gates[0]; g.Source != filepath.Join(".tardis", "gates", "simplify", "gate.yml") || g.Run[0] != "make" {
		t.Errorf("simplify = %+v, want the local gate", g)
	}
}

func TestDefaultsAndDisabled(t *testing.T) {
	m, err := Load(copySample(t, map[string]string{ConfigPath: "verify_runbook: docs/RUNBOOK.md\n"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := names(m.Gates); !reflect.DeepEqual(got, DefaultGates) {
		t.Errorf("default gates = %v", got)
	}
	m, err = Load(copySample(t, map[string]string{ConfigPath: "gates:\n  - name: simplify\n  - name: verify\n    enabled: false\n"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := names(m.Gates); !reflect.DeepEqual(got, []string{"simplify"}) {
		t.Errorf("gates with verify disabled = %v", got)
	}
	wantErr(t, copySample(t, map[string]string{ConfigPath: "gates:\n  - name: simplify\n    enabled: false\n"}), "no gate is enabled")
}

func TestGlobs(t *testing.T) {
	for _, tc := range []struct {
		glob, path string
		want       bool
	}{
		{"web/**", "web/app.js", true},
		{"web/**", "web/a/b/c.css", true},
		{"web/**", "webapp/x.js", false},
		{"**/*.tmpl", "x.tmpl", true},
		{"**/*.tmpl", "a/b/x.tmpl", true},
		{"*.go", "main.go", true},
		{"*.go", "cmd/main.go", false},
		{"cmd/?/x", "cmd/a/x", true},
		{"docs/ünï.md", "docs/ünï.md", true},
		{"a.b", "aXb", false},
	} {
		if got := globRegexp(tc.glob).MatchString(tc.path); got != tc.want {
			t.Errorf("glob %q on %q = %v, want %v", tc.glob, tc.path, got, tc.want)
		}
	}
}

func TestResolveOnRealDiff(t *testing.T) {
	root := copySample(t, nil)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("add", "-A")
	git("commit", "-q", "-m", "base")

	m, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	resolve := func(branch, file string) []string {
		t.Helper()
		git("checkout", "-q", "-b", branch, "main")
		if err := write(filepath.Join(root, file), "x"); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-q", "-m", branch)
		changed, err := Changed(root, "main", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		return names(m.Resolve(changed))
	}
	if got, want := resolve("backend", "internal/x.go"), []string{"simplify", "verify", "review"}; !reflect.DeepEqual(got, want) {
		t.Errorf("internal/ change: %v, want %v", got, want)
	}
	if got, want := resolve("frontend", "web/app.js"), []string{"simplify", "verify", "design", "review"}; !reflect.DeepEqual(got, want) {
		t.Errorf("web/ change: %v, want %v", got, want)
	}
}
