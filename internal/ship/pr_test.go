package ship

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"

	"github.com/leighstillard/tardis-gate/internal/ghapp"
)

func TestCIState(t *testing.T) {
	run := func(name, status, conclusion string, app int64) ghapp.Run {
		r := ghapp.Run{Name: name, Status: status, Conclusion: conclusion}
		r.App.ID = app
		return r
	}
	ours := run("tardis/complete", "completed", "failure", 1) // the App's own runs never count
	for _, c := range []struct {
		runs         []ghapp.Run
		waited       bool
		kind, detail string
	}{
		{nil, false, "", ""},
		{[]ghapp.Run{ours}, true, "success", "(no CI check runs)"},
		{[]ghapp.Run{ours, run("ci", "in_progress", "", 2)}, true, "", ""},
		{[]ghapp.Run{run("ci", "completed", "success", 2), run("lint", "completed", "skipped", 2)}, true, "success", ""},
		{[]ghapp.Run{run("ci", "completed", "success", 2)}, false, "", ""}, // another CI may not have posted yet
		{[]ghapp.Run{run("ci", "in_progress", "", 2), run("lint", "completed", "timed_out", 2)}, false, "failure", "lint timed_out"},
	} {
		if k, d := ciState(c.runs, 1, c.waited); k != c.kind || d != c.detail {
			t.Errorf("ciState(%v, %v) = %q, %q; want %q, %q", c.runs, c.waited, k, d, c.kind, c.detail)
		}
	}
}

// fakeGitHub is o/r on GitHub as the App sees it: the check runs posted, the
// PRs open and CI, the one other app's run on every head.
type fakeGitHub struct {
	mu     sync.Mutex
	runs   map[string][]map[string]any // sha → check runs
	prs    []map[string]any
	ci     string // CI's status: in_progress or completed (success)
	merged string // the head merged, and how
	method string
	lists  int    // open-PR lists answered in full
	origin string // the repository, for the head of a PR opened
}

func (g *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	p := r.URL.Path
	switch {
	case p == "/repos/o/r/installation":
		w.Write([]byte(`{"id": 7}`))
	case p == "/app/installations/7/access_tokens":
		json.NewEncoder(w).Encode(map[string]any{"token": "tok", "expires_at": time.Now().Add(time.Hour)})
	case p == "/app":
		w.Write([]byte(`{"id": 1, "slug": "tardis"}`))
	case r.Method == "POST" && p == "/repos/o/r/check-runs":
		var c map[string]any
		json.NewDecoder(r.Body).Decode(&c)
		c["app"] = map[string]any{"id": 1}
		sha := c["head_sha"].(string)
		g.runs[sha] = append(g.runs[sha], c)
	case r.Method == "GET" && strings.HasSuffix(p, "/check-runs"):
		sha := strings.TrimSuffix(strings.TrimPrefix(p, "/repos/o/r/commits/"), "/check-runs")
		runs := append([]map[string]any{{"name": "ci", "status": g.ci, "conclusion": "success", "app": map[string]any{"id": 2}}}, g.runs[sha]...)
		json.NewEncoder(w).Encode(map[string]any{"total_count": len(runs), "check_runs": runs})
	case r.Method == "GET" && p == "/repos/o/r/pulls":
		if h := r.URL.Query().Get("head"); h != "" {
			var out []map[string]any
			for _, pr := range g.prs {
				if "o:"+pr["head"].(map[string]any)["ref"].(string) == h {
					out = append(out, pr)
				}
			}
			json.NewEncoder(w).Encode(out)
			return
		}
		tag := fmt.Sprintf(`"%d"`, len(g.prs))
		if r.Header.Get("If-None-Match") == tag {
			w.WriteHeader(304)
			return
		}
		g.lists++
		w.Header().Set("ETag", tag)
		json.NewEncoder(w).Encode(g.prs)
	case r.Method == "GET" && strings.HasPrefix(p, "/repos/o/r/pulls/"):
		n, _ := strconv.Atoi(strings.TrimPrefix(p, "/repos/o/r/pulls/"))
		json.NewEncoder(w).Encode(g.prs[n-1])
	case r.Method == "POST" && p == "/repos/o/r/pulls":
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		out, _ := exec.Command("git", "-C", g.origin, "rev-parse", "refs/heads/"+in["head"]).Output()
		g.prs = append(g.prs, pr(len(g.prs)+1, "tardis[bot]", in["head"], strings.TrimSpace(string(out)), in["base"]))
		json.NewEncoder(w).Encode(g.prs[len(g.prs)-1])
	case r.Method == "PUT" && strings.HasSuffix(p, "/merge"):
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		g.merged, g.method = in["sha"], in["merge_method"]
		w.Write([]byte(`{"sha": "m1"}`))
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), 404)
	}
}

func pr(n int, by, branch, sha, base string) map[string]any {
	return map[string]any{"number": n, "html_url": fmt.Sprintf("https://github.com/o/r/pull/%d", n), "state": "open",
		"user": map[string]any{"login": by}, "head": map[string]any{"ref": branch, "sha": sha}, "base": map[string]any{"ref": base}}
}

// appAuditor is modelFixture's auditor, serving its origin as o/r on gh.
func appAuditor(t *testing.T, gh http.Handler, config string) (*Auditor, string, string, string) {
	t.Helper()
	local, url, base, check, _ := modelFixture(t, "simplify", config)
	const remote = "https://github.com/o/r"
	gitconfig := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(gitconfig, []byte("[url \""+url+"\"]\n\tinsteadOf = "+remote+"\n"), 0o600)
	t.Setenv("GIT_CONFIG_GLOBAL", gitconfig)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "app.pem")
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600)
	app, err := ghapp.Load("1", keyFile)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)
	app.API = srv.URL
	local.Repos, local.App = map[string]bool{remote: true}, app
	if g, ok := gh.(*fakeGitHub); ok {
		g.origin = url
	}
	return local, remote, base, check
}

func TestTheAppOpensAndMergesOnlyWhatPassed(t *testing.T) {
	gh := &fakeGitHub{runs: map[string][]map[string]any{}, ci: "in_progress"}
	r, url, base, check := appAuditor(t, gh, "auto_merge: true\nmerge_method: squash\n")
	ctx := context.Background()
	var ae *temporal.ApplicationError
	open := OpenPRIn{RepoURL: url, Branch: "feature", Base: "main", BaseID: base, Head: check}

	if v, err := r.Rerun(ctx, RerunIn{RepoURL: url, Base: "main", BaseID: base, Tip: check, Gate: "simplify"}); err != nil || !v.Pass {
		t.Fatalf("rerun: %+v, %v", v, err)
	}
	// The auditor's pass alone opens nothing: GitHub must hold the check.
	if _, err := r.OpenPR(ctx, open); !errors.As(err, &ae) || ae.Type() != "Rejected" {
		t.Errorf("open before tardis/simplify was posted: %v; want Rejected", err)
	}
	if err := r.PostCheck(ctx, CheckIn{RepoURL: url, Base: "main", BaseID: base, SHA: check, Gate: "simplify", Conclusion: "success"}); err != nil {
		t.Fatal(err)
	}
	got, err := r.OpenPR(ctx, open)
	if err != nil || got.Number != 1 || got.URL != "https://github.com/o/r/pull/1" {
		t.Fatalf("OpenPR = %+v, %v", got, err)
	}
	if again, err := r.OpenPR(ctx, open); err != nil || again != got || len(gh.prs) != 1 {
		t.Errorf("a retried OpenPR = %+v, %v, %d PRs; want the same PR", again, err, len(gh.prs))
	}
	if !succeeded(runsOf(gh, check), 1, ghapp.Check) {
		t.Errorf("no tardis/complete success on the head: %v", gh.runs[check])
	}

	merge := MergeIn{RepoURL: url, Base: "main", BaseID: base, Head: check, Number: 1}
	if _, err := r.Merge(ctx, merge); !errors.As(err, &ae) || ae.Type() != "Rejected" || gh.merged != "" {
		t.Errorf("merge while CI runs: %v; want Rejected", err)
	}
	gh.ci = "completed"
	gh.prs[0]["base"] = map[string]any{"ref": "elsewhere"}
	if _, err := r.Merge(ctx, merge); !errors.As(err, &ae) || ae.Type() != "Rejected" || gh.merged != "" {
		t.Errorf("merge of a PR retargeted since it was opened: %v; want Rejected", err)
	}
	gh.prs[0]["base"] = map[string]any{"ref": "main"}
	if sha, err := r.Merge(ctx, merge); sha != "m1" || err != nil || gh.merged != check || gh.method != "squash" {
		t.Errorf("Merge = %q, %v, merged %q by %q", sha, err, gh.merged, gh.method)
	}
	if _, err := r.Merge(ctx, MergeIn{RepoURL: url, Base: "main", BaseID: base, Head: "f00d", Number: 1}); !errors.As(err, &ae) || ae.Type() != "Rejected" {
		t.Errorf("merge of a head without tardis/complete: %v; want Rejected", err)
	}
}

func runsOf(g *fakeGitHub, sha string) []ghapp.Run {
	var runs []ghapp.Run
	b, _ := json.Marshal(g.runs[sha])
	json.Unmarshal(b, &runs)
	return runs
}

func TestMergeWaitsForAutoMerge(t *testing.T) {
	gh := &fakeGitHub{runs: map[string][]map[string]any{}, ci: "completed"}
	r, url, base, check := appAuditor(t, gh, "")
	if sha, err := r.Merge(context.Background(), MergeIn{RepoURL: url, Base: "main", BaseID: base, Head: check, Number: 1}); sha != "" || err != nil || gh.merged != "" {
		t.Errorf("Merge without auto_merge = %q, %v; merged %q", sha, err, gh.merged)
	}
}

func TestForeignPRsAreMarkedIncomplete(t *testing.T) {
	gh := &fakeGitHub{runs: map[string][]map[string]any{}, ci: "completed"}
	r, url, base, check := appAuditor(t, gh, "")
	ctx := context.Background()
	gh.prs = []map[string]any{
		pr(1, "tardis[bot]", "feature", check, "main"),
		pr(2, "someone", "hand", "abc", "main"),
		pr(3, "someone", "other", "def", "release"),
		pr(5, "someone", "copy", check, "main"), // the App's PR's head: failing it fails the App's PR
	}
	etags, marked := map[string]string{}, map[string]bool{}
	// Marking waits for the repository lock OpenPR holds while it opens a PR.
	unlock, err := r.lock(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := r.markForeign(short, url, etags, marked); err == nil || gh.lists != 0 {
		t.Errorf("marked under a held lock: %v, %d lists", err, gh.lists)
	}
	unlock()
	for range 2 {
		if err := r.markForeign(ctx, url, etags, marked); err != nil {
			t.Fatal(err)
		}
	}
	if gh.lists != 1 {
		t.Errorf("listed open PRs in full %d times; want 1, then 304", gh.lists)
	}
	if runs := runsOf(gh, "abc"); len(runs) != 1 || runs[0].Name != ghapp.Check || runs[0].Conclusion != "failure" {
		t.Errorf("the foreign PR's head: %+v; want one tardis/complete failure", runs)
	}
	if len(gh.runs) != 1 {
		t.Errorf("checks posted on %d heads; want only the foreign PR's into main", len(gh.runs))
	}
	// Someone else's PR on the App's branch is refused, not adopted.
	gh.prs = []map[string]any{pr(4, "someone", "feature", check, "main")}
	if err := r.PostCheck(ctx, CheckIn{RepoURL: url, Base: "main", BaseID: base, SHA: check, Gate: "simplify", Conclusion: "failure"}); err != nil {
		t.Fatal(err)
	}
	if v, err := r.Rerun(ctx, RerunIn{RepoURL: url, Base: "main", BaseID: base, Tip: check, Gate: "simplify"}); err != nil || !v.Pass {
		t.Fatalf("rerun: %+v, %v", v, err)
	}
	if err := r.PostCheck(ctx, CheckIn{RepoURL: url, Base: "main", BaseID: base, SHA: check, Gate: "simplify", Conclusion: "success"}); err != nil {
		t.Fatal(err)
	}
	var ae *temporal.ApplicationError
	if _, err := r.OpenPR(ctx, OpenPRIn{RepoURL: url, Branch: "feature", Base: "main", BaseID: base, Head: check}); !errors.As(err, &ae) ||
		ae.Type() != "Rejected" || !strings.Contains(err.Error(), "opened by someone") {
		t.Errorf("open over someone else's PR: %v; want Rejected", err)
	}
}
