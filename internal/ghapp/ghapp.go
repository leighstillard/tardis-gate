// Package ghapp acts as the auditor's GitHub App: an RS256 JWT for the App,
// an installation token per repository, cached until shortly before it
// expires, check runs, pull requests and merges. No SDK.
package ghapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// App is a GitHub App the auditor acts as.
type App struct {
	ID  string // App ID or client ID: the JWT's issuer
	Key *rsa.PrivateKey
	API string // default https://api.github.com

	mu     sync.Mutex
	tokens map[string]token // installation token per owner/repo
	appID  int64            // from GET /app, once
	login  string           // the App's bot user, which opens its PRs
}

type token struct {
	value   string
	expires time.Time
}

// Load reads the App's private key, PKCS#1 or PKCS#8 PEM as GitHub issues it.
func Load(id, keyFile string) (*App, error) {
	b, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("%s: no PEM block", keyFile)
	}
	key, err := x509.ParsePKCS1PrivateKey(blk.Bytes)
	if err != nil {
		k, err8 := x509.ParsePKCS8PrivateKey(blk.Bytes)
		var ok bool
		if key, ok = k.(*rsa.PrivateKey); err8 != nil || !ok {
			return nil, fmt.Errorf("%s: not an RSA private key", keyFile)
		}
	}
	return &App{ID: id, Key: key}, nil
}

// Repo returns owner/repo for a GitHub remote URL.
func Repo(remoteURL string) (string, bool) {
	var path string
	if rest, ok := strings.CutPrefix(strings.ToLower(remoteURL), "git@github.com:"); ok { // scp-like
		path = rest
	} else if u, err := url.Parse(remoteURL); err == nil && strings.EqualFold(u.Hostname(), "github.com") &&
		(u.Scheme == "https" || u.Scheme == "ssh" && u.User.Username() == "git") {
		path = strings.TrimPrefix(u.Path, "/")
	}
	path = strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git")
	if o, r, ok := strings.Cut(path, "/"); !ok || o == "" || r == "" || strings.Contains(r, "/") {
		return "", false
	}
	return strings.ToLower(path), true
}

func (a *App) jwt() (string, error) {
	now := time.Now()
	enc := base64.RawURLEncoding
	claims, _ := json.Marshal(map[string]any{ // plain values: always marshals
		"iat": now.Add(-time.Minute).Unix(), // GitHub's clock may run behind
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": a.ID,
	})
	msg := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(msg))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return msg + "." + enc.EncodeToString(sig), nil
}

// token returns an installation token for repo, limited to that repository.
func (a *App) token(ctx context.Context, repo string) (string, error) {
	a.mu.Lock()
	t, ok := a.tokens[repo]
	a.mu.Unlock()
	if ok && time.Now().Before(t.expires) {
		return t.value, nil
	}
	jwt, err := a.jwt()
	if err != nil {
		return "", err
	}
	var inst struct{ ID int64 }
	if err := a.do(ctx, "GET", "/repos/"+repo+"/installation", "Bearer "+jwt, nil, &inst); err != nil {
		return "", fmt.Errorf("the App is not installed on %s: %w", repo, err)
	}
	_, name, _ := strings.Cut(repo, "/")
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := a.do(ctx, "POST", fmt.Sprintf("/app/installations/%d/access_tokens", inst.ID), "Bearer "+jwt,
		map[string]any{"repositories": []string{name}}, &out); err != nil {
		return "", err
	}
	a.mu.Lock()
	if a.tokens == nil {
		a.tokens = map[string]token{}
	}
	a.tokens[repo] = token{out.Token, out.ExpiresAt.Add(-5 * time.Minute)} // tokens last 1 h
	a.mu.Unlock()
	return out.Token, nil
}

// CheckRun is a completed check run.
type CheckRun struct {
	Name, Conclusion, Title, Summary, Text string
}

// GitHub's limits on a check run's title, and on its summary and text.
const (
	maxTitle  = 255
	maxOutput = 65535
)

// PostCheck posts c on sha in repo.
func (a *App) PostCheck(ctx context.Context, repo, sha string, c CheckRun) error {
	_, err := a.repoDo(ctx, repo, "POST", "/repos/"+repo+"/check-runs", "", map[string]any{
		"name": c.Name, "head_sha": sha, "status": "completed", "conclusion": c.Conclusion,
		"output": map[string]string{"title": clipTo(c.Title, maxTitle), "summary": clip(c.Summary), "text": clip(c.Text)},
	}, nil)
	return err
}

// Identity is the App's numeric ID, which its check runs carry, and the
// login of its bot user, which opens its PRs.
func (a *App) Identity(ctx context.Context) (int64, string, error) {
	a.mu.Lock()
	id, login := a.appID, a.login
	a.mu.Unlock()
	if id != 0 {
		return id, login, nil
	}
	jwt, err := a.jwt()
	if err != nil {
		return 0, "", err
	}
	var app struct {
		ID   int64
		Slug string
	}
	if err := a.do(ctx, "GET", "/app", "Bearer "+jwt, nil, &app); err != nil {
		return 0, "", err
	}
	a.mu.Lock()
	a.appID, a.login = app.ID, app.Slug+"[bot]"
	a.mu.Unlock()
	return app.ID, app.Slug + "[bot]", nil
}

// Run is one check run on a commit.
type Run struct {
	Name, Status, Conclusion string
	App                      struct{ ID int64 }
}

// Runs lists the latest check run of each name on sha.
func (a *App) Runs(ctx context.Context, repo, sha string) ([]Run, error) {
	var all []Run
	for page := 1; ; page++ {
		var out struct {
			Total int   `json:"total_count"`
			Runs  []Run `json:"check_runs"`
		}
		if _, err := a.repoDo(ctx, repo, "GET", fmt.Sprintf("/repos/%s/commits/%s/check-runs?filter=latest&per_page=100&page=%d",
			repo, url.PathEscape(sha), page), "", nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Runs...)
		if len(out.Runs) < 100 || len(all) >= out.Total {
			return all, nil
		}
	}
}

// PR is a pull request, as far as tardis needs one.
type PR struct {
	Number int    `json:"number"`
	URL    string `json:"html_url"`
	State  string `json:"state"`
	Merged bool   `json:"merged"` // only on a single PR's GET
	Merge  string `json:"merge_commit_sha"`
	User   struct{ Login string }
	Head   struct{ Ref, SHA string }
	Base   struct{ Ref string }
}

// ErrNotModified is a list that has not changed since its ETag.
var ErrNotModified = errors.New("not modified")

// OpenPRs lists repo's open PRs, or returns ErrNotModified if they are as
// they were at etag; a 304 costs no rate limit.
// ponytail: the first 100 only; page on if a repository holds more open PRs.
func (a *App) OpenPRs(ctx context.Context, repo, etag string) ([]PR, string, error) {
	var prs []PR
	tag, err := a.repoDo(ctx, repo, "GET", "/repos/"+repo+"/pulls?state=open&per_page=100", etag, nil, &prs)
	return prs, tag, err
}

// FindPR returns the open PR from branch in repo itself into base, or nil.
func (a *App) FindPR(ctx context.Context, repo, branch, base string) (*PR, error) {
	owner, _, _ := strings.Cut(repo, "/")
	var prs []PR
	if _, err := a.repoDo(ctx, repo, "GET", "/repos/"+repo+"/pulls?state=open&head="+url.QueryEscape(owner+":"+branch)+"&base="+url.QueryEscape(base), "", nil, &prs); err != nil {
		return nil, err
	}
	if len(prs) == 0 {
		return nil, nil
	}
	return &prs[0], nil
}

// GetPR reads PR n.
func (a *App) GetPR(ctx context.Context, repo string, n int) (PR, error) {
	var pr PR
	_, err := a.repoDo(ctx, repo, "GET", fmt.Sprintf("/repos/%s/pulls/%d", repo, n), "", nil, &pr)
	return pr, err
}

// CreatePR opens a PR from branch into base, as the App.
func (a *App) CreatePR(ctx context.Context, repo, branch, base, title, body string) (PR, error) {
	var pr PR
	_, err := a.repoDo(ctx, repo, "POST", "/repos/"+repo+"/pulls", "",
		map[string]string{"head": branch, "base": base, "title": title, "body": body}, &pr)
	return pr, err
}

// ErrHeadMoved is a merge GitHub refused because the PR's head is no longer
// the SHA it was asked to merge.
var ErrHeadMoved = errors.New("the PR's head moved")

// Merge merges PR n by method (merge, squash or rebase), only while its head
// is sha, and returns the commit it made.
func (a *App) Merge(ctx context.Context, repo string, n int, sha, method string) (string, error) {
	var out struct{ SHA string }
	_, err := a.repoDo(ctx, repo, "PUT", fmt.Sprintf("/repos/%s/pulls/%d/merge", repo, n), "",
		map[string]string{"sha": sha, "merge_method": method}, &out)
	if se := statusError(0); errors.As(err, &se) && se == http.StatusConflict {
		return "", fmt.Errorf("%w: %w", ErrHeadMoved, err)
	}
	return out.SHA, err
}

// repoDo calls the API on repo with its installation token. A list sent
// with etag that has not changed returns ErrNotModified.
func (a *App) repoDo(ctx context.Context, repo, method, path, etag string, in, out any) (string, error) {
	tok, err := a.token(ctx, repo)
	if err != nil {
		return "", err
	}
	tag, err := a.call(ctx, method, path, "token "+tok, etag, in, out)
	var se statusError
	if errors.As(err, &se) && se == http.StatusUnauthorized {
		a.mu.Lock()
		delete(a.tokens, repo) // revoked early: fetch a new one on the retry
		a.mu.Unlock()
	}
	return tag, err
}

func clip(s string) string { return clipTo(s, maxOutput) }

// clipTo cuts s to at most n characters.
func clipTo(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

type statusError int

func (s statusError) Error() string { return fmt.Sprintf("status %d", int(s)) }

func (a *App) do(ctx context.Context, method, path, auth string, in, out any) error {
	_, err := a.call(ctx, method, path, auth, "", in, out)
	return err
}

// call is do, sending etag as If-None-Match, and returns the response's ETag.
func (a *App) call(ctx context.Context, method, path, auth, etag string, in, out any) (string, error) {
	api := a.API
	if api == "" {
		api = "https://api.github.com"
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return "", err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, api+path, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && etag != "" {
		return etag, ErrNotModified
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("github: %s %s: %w: %.300s", method, path, statusError(resp.StatusCode), b)
	}
	if out != nil {
		return resp.Header.Get("ETag"), json.Unmarshal(b, out)
	}
	return resp.Header.Get("ETag"), nil
}
