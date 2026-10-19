package ghapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRepo(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/O/R.git":      "o/r",
		"https://GitHub.com/o/r/":         "o/r",
		"git@github.com:o/r.git":          "o/r",
		"ssh://git@github.com/o/r":        "o/r",
		"ssh://git@github.com:22/o/r.git": "o/r",
		"https://github.com/o":            "",
		"https://github.com/o/r/x":        "",
		"https://github.com.evil.com/o/r": "",
		"ssh://evil@github.com/o/r":       "",
		"http://gitlab.com/o/r":           "",
	} {
		if got, ok := Repo(in); got != want || ok != (want != "") {
			t.Errorf("Repo(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

func TestPostCheckAsTheApp(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "app.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(keyFile, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	app, err := Load("123", keyFile)
	if err != nil {
		t.Fatal(err)
	}

	checkJWT := func(r *http.Request) bool {
		parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
		if len(parts) != 3 {
			return false
		}
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var c struct {
			Iss string
			Exp int64
		}
		json.Unmarshal(claims, &c)
		return rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig) == nil &&
			c.Iss == "123" && c.Exp > time.Now().Unix() && c.Exp <= time.Now().Add(10*time.Minute).Unix()
	}
	tokens := 0
	var posted map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/installation" && checkJWT(r):
			w.Write([]byte(`{"id": 7}`))
		case r.Method == "POST" && r.URL.Path == "/app/installations/7/access_tokens" && checkJWT(r):
			var in struct{ Repositories []string }
			json.NewDecoder(r.Body).Decode(&in)
			if len(in.Repositories) != 1 || in.Repositories[0] != "r" {
				http.Error(w, "token not limited to the repository", 400)
				return
			}
			tokens++
			json.NewEncoder(w).Encode(map[string]any{"token": "tok", "expires_at": time.Now().Add(time.Hour)})
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/check-runs" && r.Header.Get("Authorization") == "token tok":
			json.NewDecoder(r.Body).Decode(&posted)
			w.WriteHeader(201)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, 401)
		}
	}))
	defer srv.Close()
	app.API = srv.URL

	ctx := context.Background()
	long := strings.Repeat("x", 70000)
	for range 2 {
		if err := app.PostCheck(ctx, "o/r", "abc", CheckRun{Name: "tardis/review", Conclusion: "failure", Title: long, Summary: "s", Text: long}); err != nil {
			t.Fatal(err)
		}
	}
	if tokens != 1 {
		t.Errorf("fetched %d installation tokens for two checks; want 1 (cached)", tokens)
	}
	out := posted["output"].(map[string]any)
	if posted["name"] != "tardis/review" || posted["head_sha"] != "abc" || posted["conclusion"] != "failure" ||
		utf8.RuneCountInString(out["text"].(string)) > maxOutput || utf8.RuneCountInString(out["title"].(string)) > maxTitle {
		t.Errorf("posted %v", posted)
	}
	if err := app.PostCheck(ctx, "o/other", "abc", CheckRun{Name: "x"}); err == nil {
		t.Error("posting where the App is not installed succeeded")
	}
}
