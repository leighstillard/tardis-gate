package manifest

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/leighstillard/tardis-gate/internal/chain"
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
	if m.BaseBranch != "main" {
		t.Errorf("default base branch = %q, want main", m.BaseBranch)
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
		"bare number":  {"name: lint\nrun: [make]\ntimeout: 15\n", "cannot unmarshal !!int `15` into time.Duration"},
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

func TestRejectsUnsafeInput(t *testing.T) {
	for name, tc := range map[string]struct {
		edits map[string]string
		want  string
	}{
		"runbook escapes":  {map[string]string{ConfigPath: "verify_runbook: ../../etc/passwd\n"}, "must be a relative path inside the repository"},
		"dir escapes":      {map[string]string{ConfigPath: "gates:\n  - name: lint\n    dir: ../x\n"}, "must be a relative path inside the repository"},
		"absolute dir":     {map[string]string{ConfigPath: "gates:\n  - name: lint\n    dir: /etc\n"}, "must be a relative path inside the repository"},
		"slash in name":    {map[string]string{ConfigPath: "gates:\n  - name: ../simplify\n"}, "without spaces"},
		"comma in name":    {map[string]string{ConfigPath: "gates:\n  - name: a,b\n"}, "without spaces"},
		"two documents":    {map[string]string{ConfigPath: "gates:\n  - name: simplify\n---\ngates: []\n"}, "only one YAML document"},
		"huge glob":        {map[string]string{ConfigPath: "gates:\n  - name: simplify\n    applies_when: [\"" + strings.Repeat("*", maxGlob+1) + "\"]\n"}, "pattern must be 1 to"},
		"oversized config": {map[string]string{ConfigPath: "gates:\n  - name: simplify\n#" + strings.Repeat("x", maxFile)}, "larger than"},
		"glob class":       {map[string]string{ConfigPath: "gates:\n  - name: simplify\n    applies_when: [\"web/*.[jt]s\"]\n"}, "only *, ** and ? are supported"},
		"glob alternative": {map[string]string{ConfigPath: "gates:\n  - name: simplify\n    applies_when: [\"{web,app}/**\"]\n"}, "only *, ** and ? are supported"},
		"absolute glob":    {map[string]string{ConfigPath: "gates:\n  - name: simplify\n    applies_when: [\"/web/**\"]\n"}, "must be a relative path"},
		"parent glob":      {map[string]string{ConfigPath: "gates:\n  - name: simplify\n    applies_when: [\"../web/**\"]\n"}, "must be a relative path"},
	} {
		_, err := Load(copySample(t, tc.edits))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestLoadDoesNotFollowSymlinksOut(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := write(outside, "secret"); err != nil {
		t.Fatal(err)
	}
	root := copySample(t, map[string]string{"docs/RUNBOOK.md": ""})
	if err := os.Symlink(outside, filepath.Join(root, "docs", "RUNBOOK.md")); err != nil {
		t.Fatal(err)
	}
	wantErr(t, root, "verify_runbook: docs/RUNBOOK.md is a symlink; not supported")
}

// committedSample is copySample committed to main in a new repository.
func committedSample(t *testing.T, edits map[string]string) string {
	t.Helper()
	root := copySample(t, edits)
	commitAll(t, root)
	return root
}

// commitAll commits everything under root to main in a new repository.
func commitAll(t *testing.T, root string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "base"}} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func TestLoadRevFailsClosedOnANonBlobGate(t *testing.T) {
	// gate.yml is a directory: the local override exists but is unreadable, so
	// LoadRev must fail rather than use the reference simplify gate.
	root := committedSample(t, map[string]string{".tardis/gates/simplify/gate.yml/x": "x"})
	if _, err := LoadRev(root, "main"); err == nil || !strings.Contains(err.Error(), ".tardis/gates/simplify/gate.yml") {
		t.Errorf("err = %v, want a failure naming the override", err)
	}
}

func TestSymlinkedGateDirIsRefusedByLintAndResolve(t *testing.T) {
	// .tardis/gates/simplify -> ../../tools/simplify. git cannot list through
	// the link, so both loaders must refuse it rather than use the reference.
	root := copySample(t, map[string]string{"tools/simplify/gate.yml": "name: simplify\nrun: [make]\ntimeout: 1m\n"})
	if err := os.MkdirAll(filepath.Join(root, ".tardis", "gates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../tools/simplify", filepath.Join(root, ".tardis", "gates", "simplify")); err != nil {
		t.Fatal(err)
	}
	wantErr(t, root, "symlink")
	commitAll(t, root)
	if _, err := LoadRev(root, "main"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("LoadRev err = %v, want a symlink refusal", err)
	}
}

func TestLoadRevReadsGlobCharactersLiterally(t *testing.T) {
	// dir "pol/l[ab]" must name that directory, never pol/la.
	root := committedSample(t, map[string]string{
		ConfigPath:           "gates:\n  - name: lint\n    dir: pol/l[ab]\n",
		"pol/la/gate.yml":    "name: lint\nrun: [narrow]\ntimeout: 1m\n",
		"pol/l[ab]/gate.yml": "name: lint\nrun: [literal]\ntimeout: 1m\n",
	})
	m, err := LoadRev(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Gates[0].Run[0]; got != "literal" {
		t.Errorf("run = %q, want the literal directory's gate", got)
	}
}

func TestLintRefusesASubmoduleDirectory(t *testing.T) {
	root := copySample(t, map[string]string{
		".tardis/gates/simplify/.git":     "gitdir: ../../../.git/modules/simplify\n",
		".tardis/gates/simplify/gate.yml": "name: simplify\nrun: [make]\ntimeout: 1m\n",
	})
	wantErr(t, root, ".tardis/gates/simplify is a submodule")
}

func TestLoadRevRefusesAnOversizedFileBeforeReadingIt(t *testing.T) {
	root := committedSample(t, map[string]string{
		".tardis/gates/simplify/gate.yml": "name: simplify\nrun: [make]\ntimeout: 1m\n#" + strings.Repeat("x", maxFile),
		"docs/RUNBOOK.md":                 strings.Repeat("a long runbook\n", maxFile/10), // any size is fine
	})
	_, err := LoadRev(root, "main")
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("err = %v, want the gate.yml refused as too large", err)
	}
	if err != nil && strings.Contains(err.Error(), "verify_runbook") {
		t.Errorf("a large runbook was refused: %v", err)
	}
}

func TestLintRefusesAnUninitialisedSubmodule(t *testing.T) {
	// A gitlink in the index over an empty directory: lint must agree with
	// LoadRev and refuse it, not fall back to the reference gate.
	root := committedSample(t, nil)
	sub := committedSample(t, nil)
	out, err := exec.Command("git", "-C", sub, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	gitlink := "160000," + strings.TrimSpace(string(out)) + ",.tardis/gates/simplify"
	if out, err := exec.Command("git", "-C", root, "update-index", "--add", "--cacheinfo", gitlink).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(root, ".tardis", "gates", "simplify"), 0o755); err != nil {
		t.Fatal(err)
	}
	wantErr(t, root, ".tardis/gates/simplify is a submodule")
}

func TestLoadRevIgnoresReplaceRefs(t *testing.T) {
	// A local replacement for main's commit drops every gate but simplify;
	// the committed policy must still be read.
	root := committedSample(t, nil)
	for _, k := range []string{"GIT_AUTHOR", "GIT_COMMITTER"} {
		t.Setenv(k+"_NAME", "t")
		t.Setenv(k+"_EMAIL", "t@x")
	}
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := write(filepath.Join(root, ConfigPath), "gates:\n  - name: simplify\n"); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	tree := git("write-tree")
	fake := git("commit-tree", tree, "-m", "replacement")
	git("replace", git("rev-parse", "main"), fake)
	m, err := LoadRev(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	if got := names(m.Gates); len(got) != 4 {
		t.Errorf("gates = %v; a replace ref changed the committed policy", got)
	}
}

func TestChangedSeesSubmoduleChangesDespiteConfig(t *testing.T) {
	root := committedSample(t, nil)
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	for _, k := range []string{"GIT_AUTHOR", "GIT_COMMITTER"} {
		t.Setenv(k+"_NAME", "t")
		t.Setenv(k+"_EMAIL", "t@x")
	}
	base := git("rev-parse", "HEAD")
	git("checkout", "-q", "-b", "f")
	git("update-index", "--add", "--cacheinfo", "160000,"+base+",web/vendor")
	git("commit", "-q", "-m", "add a submodule under web/")
	git("config", "diff.ignoreSubmodules", "all")
	changed, err := Changed(root, "main", "f")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(changed, []string{"web/vendor"}) {
		t.Errorf("changed = %v, want [web/vendor] (design must apply)", changed)
	}
}

func TestLoadRevIgnoresAHooksGitDir(t *testing.T) {
	root := committedSample(t, nil)
	other := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", other).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	if _, err := LoadRev(root, "main"); err != nil {
		t.Errorf("LoadRev with a hook's GIT_DIR set: %v", err)
	}
}

func TestLoadRevFailsClosedOnAMissingObject(t *testing.T) {
	// The override's directory object is gone (a partial or damaged clone):
	// LoadRev must fail rather than read the override as absent.
	root := committedSample(t, map[string]string{".tardis/gates/simplify/gate.yml": "name: simplify\nrun: [make]\ntimeout: 1m\n"})
	out, err := exec.Command("git", "-C", root, "rev-parse", "main:.tardis/gates/simplify").Output()
	if err != nil {
		t.Fatal(err)
	}
	tree := strings.TrimSpace(string(out))
	if err := os.Remove(filepath.Join(root, ".git", "objects", tree[:2], tree[2:])); err != nil {
		t.Fatal(err)
	}
	_, err = LoadRev(root, "main")
	if ge := (*GitError)(nil); !errors.As(err, &ge) {
		t.Errorf("LoadRev with the override's tree missing: err = %v; want a GitError, not the reference gate", err)
	}
}

func TestGitFailuresKeepTheirType(t *testing.T) {
	// A missing object behind an explicit dir or the runbook is git failing,
	// not the manifest being wrong.
	for _, tc := range []struct {
		edits map[string]string
		obj   string
	}{
		{map[string]string{".tardis/config.yml": "verify_runbook: docs/RUNBOOK.md\ngates:\n  - name: lint\n    dir: tools/lint\n", "tools/lint/gate.yml": "name: lint\nrun: [make]\ntimeout: 1m\n"}, "main:tools/lint"},
		{nil, "main:docs/RUNBOOK.md"},
	} {
		root := committedSample(t, tc.edits)
		out, err := exec.Command("git", "-C", root, "rev-parse", tc.obj).Output()
		if err != nil {
			t.Fatal(err)
		}
		id := strings.TrimSpace(string(out))
		if err := os.Remove(filepath.Join(root, ".git", "objects", id[:2], id[2:])); err != nil {
			t.Fatal(err)
		}
		_, err = LoadRev(root, "main")
		if ge := (*GitError)(nil); !errors.As(err, &ge) {
			t.Errorf("%s missing: err = %v; want a GitError", tc.obj, err)
		}
	}
}

func TestGateCountIsCapped(t *testing.T) {
	cfg, edits := "gates:\n", map[string]string{}
	for i := range maxGates + 1 {
		cfg += fmt.Sprintf("  - name: g%d\n", i)
		edits[fmt.Sprintf(".tardis/gates/g%d/gate.yml", i)] = fmt.Sprintf("name: g%d\nrun: [make]\ntimeout: 1m\n", i)
	}
	edits[".tardis/config.yml"] = cfg
	root := copySample(t, edits)
	wantErr(t, root, fmt.Sprintf("gates: %d listed; at most %d", maxGates+1, maxGates))
}

func TestConfiguredPathsAreShallow(t *testing.T) {
	root := copySample(t, map[string]string{".tardis/config.yml": "gates:\n  - name: lint\n    dir: a/b/c/d/e/f/g/h/i\n"})
	wantErr(t, root, `gate "lint": dir: "a/b/c/d/e/f/g/h/i" is more than 8 directories deep`)
}

func TestReservedGateNamesAndBackslashes(t *testing.T) {
	for cfg, want := range map[string]string{
		"gates:\n  - name: ..\n": `gate name ".." is reserved`,
		"gates:\n  - name: " + strings.Repeat("a", chain.MaxGateName+1) + "\n": "is longer than 64 bytes",
		"gates:\n  - name: '{security}'\n":                                     `gate name "{security}" is reserved`,
		"gates:\n  - name: lint\n    dir: tools\\lint\n":                       `"tools\\lint": separate directories with /`,
		"gates:\n  - name: 'foo\\bar'\n":                                       `gate name "foo\\bar" is reserved`,
		"gates:\n  - name: lint\n    dir: C:/tools\n":                          `"C:/tools" must be a relative path inside the repository`,
		// A custom dir named like the embedded-gate source.
		"gates:\n  - name: security\n    dir: reference:security\n": `"reference:security" must be a relative path`,
	} {
		root := copySample(t, map[string]string{".tardis/config.yml": cfg})
		wantErr(t, root, want)
	}
}

func TestPolicyLintCanTrust(t *testing.T) {
	// Paths git cannot commit, and argv no process can take, never pass lint.
	for cfg, want := range map[string]string{
		"gates:\n  - name: lint\n    dir: .git/tardis\n": `".git/tardis" is inside a .git directory`,
		"gates:\n  - name: lint\n    dir: tools/lint\n":  "run has a NUL byte",
	} {
		root := copySample(t, map[string]string{".tardis/config.yml": cfg, "tools/lint/gate.yml": "name: lint\nrun: [\"true\", \"x\\0y\"]\ntimeout: 1m\n"})
		wantErr(t, root, want)
	}
	// A FIFO for the runbook would block the open.
	root := copySample(t, map[string]string{"docs/RUNBOOK.md": ""}) // "" deletes it
	if err := syscall.Mkfifo(filepath.Join(root, "docs", "RUNBOOK.md"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantErr(t, root, "docs/RUNBOOK.md is not a regular file")
	// Nor one standing where a directory should be.
	root = copySample(t, map[string]string{"docs/RUNBOOK.md": ""})
	if err := os.RemoveAll(filepath.Join(root, "docs")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "docs"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantErr(t, root, "docs is not a directory")
}

func TestCheckHeadRefusesAShallowClone(t *testing.T) {
	root := committedSample(t, nil)
	clone := t.TempDir()
	if out, err := exec.Command("git", "clone", "-q", "--depth=1", "file://"+root, clone).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	id, _ := CommitID(clone, "HEAD")
	if err := CheckHead(clone, id, id); err == nil || !strings.Contains(err.Error(), "shallow") {
		t.Errorf("CheckHead in a shallow clone: err = %v", err)
	}
}

func TestAppliesWhenIsCapped(t *testing.T) {
	globs := strings.Repeat(`"a/**",`, maxGlobs+1)
	root := copySample(t, map[string]string{".tardis/config.yml": "gates:\n  - name: simplify\n    applies_when: [" + strings.TrimSuffix(globs, ",") + "]\n"})
	wantErr(t, root, fmt.Sprintf("applies_when: %d patterns; at most %d", maxGlobs+1, maxGlobs))
}

func TestChangedRefusesAnEnormousDiff(t *testing.T) {
	root := committedSample(t, nil)
	git := func(stdin string, args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	blob := git("x", "hash-object", "-w", "--stdin")
	var index strings.Builder
	for i := range maxChanged + 1 {
		fmt.Fprintf(&index, "100644 %s\tmany/%d\n", blob, i)
	}
	git(index.String(), "update-index", "--index-info")
	head := git("", "commit-tree", git("", "write-tree"), "-p", "main", "-m", "many")
	if _, err := Changed(root, "main", head); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("at most %d", maxChanged)) {
		t.Errorf("Changed on %d paths: err = %v", maxChanged+1, err)
	}
}

func TestGateDirMustBeBelowTheRoot(t *testing.T) {
	root := copySample(t, map[string]string{".tardis/config.yml": "gates:\n  - name: lint\n    dir: .\n", "gate.yml": "name: lint\nrun: [make]\ntimeout: 1m\n"})
	wantErr(t, root, `gate "lint": dir: must be a directory below the repository root`)
}

func TestPolicyChangesGetEveryGate(t *testing.T) {
	// Every gate is scoped away from the policy files; changing them must
	// still be reviewed by all of them.
	root := copySample(t, map[string]string{
		".tardis/config.yml":  "verify_runbook: docs/RUNBOOK.md\ngates:\n  - name: simplify\n    applies_when: [\"web/**\"]\n  - name: lint\n    dir: tools/lint\n",
		"tools/lint/gate.yml": "name: lint\napplies_when: [\"web/**\"]\nrun: [make]\ntimeout: 1m\n",
	})
	m, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".tardis/config.yml", ".tardis/gates/new/gate.yml", "tools/lint/run.sh", "docs/RUNBOOK.md"} {
		if got := len(m.Resolve([]string{p})); got != 2 {
			t.Errorf("change to %s: %d gates, want all 2", p, got)
		}
	}
	if got := len(m.Resolve([]string{"internal/x.go", "tools/lintx"})); got != 0 {
		t.Errorf("change outside the policy and web/: %d gates, want 0", got)
	}
}

func TestExactNameRefusesAnotherSpelling(t *testing.T) {
	// A case-insensitive filesystem would open Tools for tools; git would not.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tools", "lint"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for q, want := range map[string]bool{"tools": true, "tools/lint": true, "Tools": false, "tools/Lint": false} {
		if got, err := exactName(r, q); err != nil || got != want {
			t.Errorf("exactName(%q) = %v, %v; want %v", q, got, err, want)
		}
	}
}

func TestLoadRevReadsTheCommittedPolicy(t *testing.T) {
	root := committedSample(t, nil)
	// An uncommitted edit changes Load but not LoadRev.
	if err := write(filepath.Join(root, ConfigPath), "gates:\n  - name: simplify\n"); err != nil {
		t.Fatal(err)
	}
	m, err := LoadRev(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(m.Gates), []string{"simplify", "verify", "design", "review"}; !reflect.DeepEqual(got, want) {
		t.Errorf("LoadRev gates = %v, want %v", got, want)
	}
	if _, err := LoadRev(root, "nosuch"); err == nil {
		t.Error("LoadRev of a missing revision succeeded")
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
		{"web/**", "web", true},                               // a submodule at web changes as the one path "web"
		{"migrations/**", "migrations/001\nhotfix.sql", true}, // git allows a newline in a path
		{"**/*.sql", "a\nb/x.sql", true},
		{"web/**", "website", false},
		{"a/**/b/**", "a/x/b", true},
		{"**/*.tmpl", "x.tmpl", true},
		{"**/*.tmpl", "a/b/x.tmpl", true},
		{"*.go", "main.go", true},
		{"*.go", "cmd/main.go", false},
		{"cmd/?/x", "cmd/a/x", true},
		{"docs/ünï.md", "docs/ünï.md", true},
		{"a.b", "aXb", false},
	} {
		re, err := globRegexp(tc.glob)
		if err != nil {
			t.Fatal(err)
		}
		if got := re.MatchString(tc.path); got != tc.want {
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
