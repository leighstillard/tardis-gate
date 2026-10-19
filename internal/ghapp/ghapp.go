// Package ghapp acts as the auditor's GitHub App: an RS256 JWT for the App,
// an installation token per repository, cached until shortly before it
// expires, and check runs. No SDK.
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
	u := strings.TrimSuffix(strings.TrimSuffix(remoteURL, "/"), ".git")
	for _, p := range []string{"https://github.com/", "ssh://git@github.com/", "git@github.com:"} {
		if rest, ok := strings.CutPrefix(u, p); ok && strings.Count(rest, "/") == 1 {
			return strings.ToLower(rest), true
		}
	}
	return "", false
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
	tok, err := a.token(ctx, repo)
	if err != nil {
		return err
	}
	err = a.do(ctx, "POST", "/repos/"+repo+"/check-runs", "token "+tok, map[string]any{
		"name": c.Name, "head_sha": sha, "status": "completed", "conclusion": c.Conclusion,
		"output": map[string]string{"title": clipTo(c.Title, maxTitle), "summary": clip(c.Summary), "text": clip(c.Text)},
	}, nil)
	var se statusError
	if errors.As(err, &se) && se == http.StatusUnauthorized {
		a.mu.Lock()
		delete(a.tokens, repo) // revoked early: fetch a new one on the retry
		a.mu.Unlock()
	}
	return err
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
	api := a.API
	if api == "" {
		api = "https://api.github.com"
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, api+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("github: %s %s: %w: %.300s", method, path, statusError(resp.StatusCode), b)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}
