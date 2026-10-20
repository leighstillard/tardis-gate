package ghapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testApp is an App installed on o/r whose other calls go to repo.
func testApp(t *testing.T, repo http.HandlerFunc) *App {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "app.pem")
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600)
	app, err := Load("123", keyFile)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/o/r/installation":
			w.Write([]byte(`{"id": 7}`))
		case r.URL.Path == "/app/installations/7/access_tokens":
			json.NewEncoder(w).Encode(map[string]any{"token": "tok", "expires_at": time.Now().Add(time.Hour)})
		case r.URL.Path == "/app":
			w.Write([]byte(`{"id": 123, "slug": "tardis"}`))
		case r.Header.Get("Authorization") != "token tok":
			http.Error(w, "not the installation token", 401)
		default:
			repo(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	app.API = srv.URL
	return app
}

func TestPRCalls(t *testing.T) {
	var created map[string]string
	app := testApp(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/o/r/commits/abc/check-runs":
			if q.Get("page") == "1" {
				runs := make([]map[string]any, 100)
				for i := range runs {
					runs[i] = map[string]any{"name": "ci", "status": "completed", "conclusion": "success", "app": map[string]any{"id": 9}}
				}
				json.NewEncoder(w).Encode(map[string]any{"total_count": 101, "check_runs": runs})
				return
			}
			w.Write([]byte(`{"total_count": 101, "check_runs": [{"name": "tardis/review", "status": "completed", "conclusion": "failure", "app": {"id": 123}}]}`))
		case "GET /repos/o/r/pulls":
			if q.Get("head") == "o:b" {
				w.Write([]byte(`[{"number": 4, "html_url": "u4", "user": {"login": "x"}, "head": {"ref": "b", "sha": "abc"}}]`))
			} else if q.Get("head") != "" {
				w.Write([]byte(`[]`))
			} else if r.Header.Get("If-None-Match") == `"e1"` {
				w.WriteHeader(304)
			} else {
				w.Header().Set("ETag", `"e1"`)
				w.Write([]byte(`[{"number": 4, "base": {"ref": "main"}}]`))
			}
		case "POST /repos/o/r/pulls":
			json.NewDecoder(r.Body).Decode(&created)
			w.WriteHeader(201)
			w.Write([]byte(`{"number": 5, "html_url": "u5", "state": "open"}`))
		case "GET /repos/o/r/pulls/5":
			w.Write([]byte(`{"number": 5, "state": "closed", "merged": true, "merge_commit_sha": "m"}`))
		case "PUT /repos/o/r/pulls/5/merge":
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			if in["sha"] != "abc" || in["merge_method"] != "merge" {
				http.Error(w, "Head branch was modified", 409)
				return
			}
			w.Write([]byte(`{"sha": "m", "merged": true}`))
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), 404)
		}
	})
	ctx := context.Background()

	if id, login, err := app.Identity(ctx); id != 123 || login != "tardis[bot]" || err != nil {
		t.Errorf("Identity = %d, %q, %v", id, login, err)
	}
	runs, err := app.Runs(ctx, "o/r", "abc")
	if err != nil || len(runs) != 101 || runs[100].App.ID != 123 || runs[100].Conclusion != "failure" {
		t.Errorf("Runs: %d runs, %v", len(runs), err)
	}
	if pr, err := app.FindPR(ctx, "o/r", "b"); err != nil || pr == nil || pr.Number != 4 || pr.User.Login != "x" || pr.Head.SHA != "abc" {
		t.Errorf("FindPR(b) = %+v, %v", pr, err)
	}
	if pr, err := app.FindPR(ctx, "o/r", "none"); pr != nil || err != nil {
		t.Errorf("FindPR(none) = %+v, %v", pr, err)
	}
	prs, tag, err := app.OpenPRs(ctx, "o/r", "")
	if err != nil || tag != `"e1"` || len(prs) != 1 || prs[0].Base.Ref != "main" {
		t.Errorf("OpenPRs = %+v, %q, %v", prs, tag, err)
	}
	if _, _, err := app.OpenPRs(ctx, "o/r", tag); !errors.Is(err, ErrNotModified) {
		t.Errorf("OpenPRs at its own ETag: %v; want ErrNotModified", err)
	}
	if pr, err := app.CreatePR(ctx, "o/r", "b", "main", "t", "body"); err != nil || pr.Number != 5 || pr.URL != "u5" ||
		created["head"] != "b" || created["base"] != "main" || created["title"] != "t" {
		t.Errorf("CreatePR = %+v, %v; sent %v", pr, err, created)
	}
	if pr, err := app.GetPR(ctx, "o/r", 5); err != nil || !pr.Merged || pr.Merge != "m" {
		t.Errorf("GetPR = %+v, %v", pr, err)
	}
	if sha, err := app.Merge(ctx, "o/r", 5, "abc"); sha != "m" || err != nil {
		t.Errorf("Merge = %q, %v", sha, err)
	}
	if _, err := app.Merge(ctx, "o/r", 5, "old"); !errors.Is(err, ErrHeadMoved) {
		t.Errorf("Merge on a moved head: %v; want ErrHeadMoved", err)
	}
}

func TestApplyRulesets(t *testing.T) {
	stored := map[string]any{} // id → ruleset
	writes, refuseMode := 0, "pull_request"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer op" {
			http.Error(w, "not the operator", 401)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/repos/o/r/rulesets/")
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/rulesets":
			var list []map[string]any
			for id, rs := range stored {
				list = append(list, map[string]any{"id": json.Number(id), "name": rs.(map[string]any)["name"]})
			}
			json.NewEncoder(w).Encode(list)
		case r.Method == "GET":
			rs := stored[id].(map[string]any)
			out := map[string]any{"id": json.Number(id), "source": "o/r"} // GitHub adds fields
			for k, v := range rs {
				out[k] = v
			}
			json.NewEncoder(w).Encode(out)
		case r.Method == "POST" || r.Method == "PUT":
			var rs map[string]any
			json.NewDecoder(r.Body).Decode(&rs)
			if b, _ := json.Marshal(rs["bypass_actors"]); strings.Contains(string(b), refuseMode) {
				http.Error(w, "invalid bypass mode", 422)
				return
			}
			if r.Method == "POST" {
				id = string(rune('1' + len(stored)))
			}
			stored[id] = rs
			writes++
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	apply := func() string {
		out, err := ApplyRulesets(ctx, srv.URL, "op", "o/r", "main", 123)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(out, ", ")
	}

	if got := apply(); got != "tardis-gates created, tardis-merge-only created" {
		t.Errorf("first apply: %s", got)
	}
	gates := stored["1"].(map[string]any)
	if b, _ := json.Marshal(gates); !strings.Contains(string(b), `"context":"tardis/complete","integration_id":123`) ||
		!strings.Contains(string(b), `"bypass_actors":[]`) || !strings.Contains(string(b), `"refs/heads/main"`) {
		t.Errorf("tardis-gates: %s", b)
	}
	if b, _ := json.Marshal(stored["2"]); !strings.Contains(string(b), `"bypass_mode":"always"`) {
		t.Errorf("tardis-merge-only did not fall back to always: %s", b)
	}
	n := writes
	if got := apply(); got != "tardis-gates unchanged, tardis-merge-only unchanged" || writes != n {
		t.Errorf("second apply: %s, %d writes", got, writes-n)
	}
	gates["bypass_actors"] = []any{map[string]any{"actor_id": 1, "actor_type": "OrganizationAdmin", "bypass_mode": "always"}}
	if got := apply(); got != "tardis-gates updated, tardis-merge-only unchanged" {
		t.Errorf("apply over an added bypass: %s", got)
	}
	if b, _ := json.Marshal(stored["1"]); !strings.Contains(string(b), `"bypass_actors":[]`) {
		t.Errorf("the bypass survived: %s", b)
	}
}
