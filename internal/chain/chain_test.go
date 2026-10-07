package chain

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// repo is a throwaway git repository isolated from the user's git config.
type repo struct {
	t   *testing.T
	dir string
	n   int
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	r := &repo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	r.code("base") // the merge base every test branches from
	return r
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// code makes a commit that changes the tree and returns its SHA.
func (r *repo) code(msg string) string {
	r.t.Helper()
	r.n++
	if err := os.WriteFile(filepath.Join(r.dir, fmt.Sprintf("f%d.txt", r.n)), []byte(msg), 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.git("add", "-A")
	r.git("commit", "-q", "-m", msg)
	return r.git("rev-parse", "HEAD")
}

// check makes a raw check commit with the given Of and body, bypassing Commit's
// guards so tests can forge bad ones.
func (r *repo) check(gate, of, body string) string {
	r.t.Helper()
	r.git("commit", "--allow-empty", "-q", "-m", SubjectPrefix+gate, "-m", body,
		"--trailer", TrailerCheck+": "+gate,
		"--trailer", TrailerOf+": "+of,
		"--trailer", TrailerTool+": anthropic/claude-code/x")
	return r.git("rev-parse", "HEAD")
}

func (r *repo) verify(gates ...string) map[string]string {
	r.t.Helper()
	got, err := Verify(r.dir, "main", "HEAD", gates)
	if err != nil {
		r.t.Fatal(err)
	}
	return got
}

func (r *repo) branch() { r.git("checkout", "-q", "-b", "feature") }

func want(t *testing.T, got map[string]string, gate, status string) {
	t.Helper()
	if got[gate] != status {
		t.Errorf("%s = %q, want %q (all: %v)", gate, got[gate], status, got)
	}
}

func TestValidChain(t *testing.T) {
	r := newRepo(t)
	r.branch()
	a := r.code("A")
	r.check("simplify", a, "looks fine")
	r.check("verify", a, "ran it")
	got := r.verify("simplify", "verify")
	want(t, got, "simplify", Valid)
	want(t, got, "verify", Valid)
	if !AllValid(got) {
		t.Error("AllValid = false")
	}
}

func TestCodeAfterCheckSupersedes(t *testing.T) {
	r := newRepo(t)
	r.branch()
	a := r.code("A")
	r.check("simplify", a, "ok")
	r.code("B")
	want(t, r.verify("simplify"), "simplify", broken("superseded"))
}

func TestOfMismatch(t *testing.T) {
	r := newRepo(t)
	base := r.git("rev-parse", "HEAD")
	r.branch()
	r.code("A")
	r.check("simplify", base, "claims to review the wrong commit")
	want(t, r.verify("simplify"), "simplify", broken("of-mismatch"))
}

func TestNotEmpty(t *testing.T) {
	r := newRepo(t)
	r.branch()
	a := r.code("A")
	if err := os.WriteFile(filepath.Join(r.dir, "sneaky.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.git("add", "sneaky.txt")
	c := r.check("simplify", a, "hides a change")
	got := r.verify("simplify", "verify")
	want(t, got, "simplify", broken("not-empty"))

	// The smuggled change counts as code: a later check must point at it.
	r.check("verify", a, "points past the smuggled change")
	want(t, r.verify("verify"), "verify", broken("of-mismatch"))
	r.check("verify", c, "points at it")
	want(t, r.verify("verify"), "verify", Valid)
}

func TestBodyTooLong(t *testing.T) {
	r := newRepo(t)
	r.branch()
	a := r.code("A")
	r.check("simplify", a, strings.Repeat("line\n", MaxBodyLines+1))
	want(t, r.verify("simplify"), "simplify", broken("body-too-long"))

	r2 := newRepo(t)
	r2.branch()
	a2 := r2.code("A")
	r2.check("simplify", a2, strings.Repeat("line\n", MaxBodyLines))
	want(t, r2.verify("simplify"), "simplify", Valid)
}

func TestMissing(t *testing.T) {
	r := newRepo(t)
	r.branch()
	a := r.code("A")
	r.check("simplify", a, "ok")
	got := r.verify("simplify", "verify")
	want(t, got, "simplify", Valid)
	want(t, got, "verify", Missing)
	if AllValid(got) {
		t.Error("AllValid = true with a missing gate")
	}
}

func TestLaterCheckSupersedesEarlier(t *testing.T) {
	r := newRepo(t)
	r.branch()
	a := r.code("A")
	r.check("simplify", a, strings.Repeat("x\n", MaxBodyLines+5)) // malformed
	r.check("simplify", a, "fixed summary")
	want(t, r.verify("simplify"), "simplify", Valid)
}

func TestCodeCommitWithCheckSubjectButNoTrailersIsCode(t *testing.T) {
	r := newRepo(t)
	r.branch()
	a := r.code("A")
	r.check("simplify", a, "ok")
	r.git("commit", "--allow-empty", "-q", "-m", SubjectPrefix+"verify", "-m", "no trailers")
	want(t, r.verify("simplify"), "simplify", broken("superseded"))
}

func TestCommitWritesTrailers(t *testing.T) {
	r := newRepo(t)
	r.branch()
	a := r.code("A")
	if _, err := Commit(r.dir, "simplify", "summary\n", "anthropic/claude-code/x"); err != nil {
		t.Fatal(err)
	}
	if got := r.git("log", "-1", "--format=%(trailers:key=Ship-Check-Of,valueonly)"); got != a {
		t.Errorf("Ship-Check-Of = %q, want %q", got, a)
	}
	// A second check stacks on the first and still points at the code commit.
	if _, err := Commit(r.dir, "verify", "ran it", "openai/codex/gpt"); err != nil {
		t.Fatal(err)
	}
	if got := r.git("log", "-1", "--format=%(trailers:key=Ship-Check-Of,valueonly)"); got != a {
		t.Errorf("stacked Ship-Check-Of = %q, want %q", got, a)
	}
	got := r.verify("simplify", "verify")
	want(t, got, "simplify", Valid)
	want(t, got, "verify", Valid)
}

func TestCommitRefuses(t *testing.T) {
	r := newRepo(t)
	r.branch()
	r.code("A")
	long := strings.Repeat("x\n", MaxBodyLines+1)
	for name, tc := range map[string]struct{ gate, summary, tool string }{
		"bad vendor":    {"simplify", "s", "acme/tool/m"},
		"short tool":    {"simplify", "s", "anthropic/claude-code"},
		"empty summary": {"simplify", "  \n", "anthropic/claude-code/x"},
		"long summary":  {"simplify", long, "anthropic/claude-code/x"},
		"bad gate":      {"sim plify", "s", "anthropic/claude-code/x"},
	} {
		if _, err := Commit(r.dir, tc.gate, tc.summary, tc.tool); err == nil {
			t.Errorf("%s: Commit succeeded, want error", name)
		}
	}

	if err := os.WriteFile(filepath.Join(r.dir, "staged.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.git("add", "staged.txt")
	if _, err := Commit(r.dir, "simplify", "s", "anthropic/claude-code/x"); err == nil ||
		!strings.Contains(err.Error(), "staged") {
		t.Errorf("staged changes: err = %v, want staged-changes error", err)
	}
}

func TestVerify200CommitsUnderOneSecond(t *testing.T) {
	r := newRepo(t)
	r.branch()
	var last string
	for i := 0; i < 190; i++ {
		last = r.code(fmt.Sprintf("c%d", i))
	}
	for i := 0; i < 10; i++ {
		r.check(fmt.Sprintf("g%d", i), last, "ok")
	}
	start := time.Now()
	got := r.verify("g0", "g9")
	if d := time.Since(start); d > time.Second {
		t.Errorf("Verify took %v, want < 1s", d)
	}
	want(t, got, "g9", Valid)
}
