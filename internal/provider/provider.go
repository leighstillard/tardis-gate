// Package provider sends one prompt to a model and returns its answer: over
// an OpenAI-compatible API, Anthropic's API, or a local command. No SDKs.
package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Func asks the model once.
type Func func(ctx context.Context, system, user string) (string, error)

// Config is the provider section of .tardis/config.yml.
type Config struct {
	Type    string   `yaml:"type" json:"type"`         // openaicompat (default), anthropic or exec
	BaseURL string   `yaml:"base_url" json:"base_url"` // openaicompat and anthropic
	Model   string   `yaml:"model" json:"model"`
	Vendor  string   `yaml:"vendor" json:"vendor"`   // the model's vendor, where type and model don't say
	Command []string `yaml:"command" json:"command"` // exec: argv; prompt on stdin, answer on stdout
}

const (
	OpenRouter = "https://openrouter.ai/api/v1"
	anthropic  = "https://api.anthropic.com"
	maxAnswer  = 4 << 20
)

// freeModels are the defaults, in order of preference; the first whose
// vendor is not the worker's is used.
// ponytail: OpenRouter's free list changes; set provider.model to pin one.
var freeModels = []string{"qwen/qwen3-coder:free", "meta-llama/llama-3.3-70b-instruct:free"}

// keys names the environment variable holding the key for each host. A key
// goes only to its own host over https, so a config cannot send it elsewhere.
var keys = map[string]string{
	"openrouter.ai":     "OPENROUTER_API_KEY",
	"api.openai.com":    "OPENAI_API_KEY",
	"api.anthropic.com": "ANTHROPIC_API_KEY",
}

// Check reports what is wrong with c as written, before defaults.
func (c Config) Check() error {
	var errs []error
	switch c.Type {
	case "", "openaicompat", "anthropic":
		if len(c.Command) > 0 {
			errs = append(errs, errors.New("command is for type exec only"))
		}
		if c.BaseURL != "" {
			if u, err := url.Parse(c.BaseURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				errs = append(errs, fmt.Errorf("base_url %q is not an http(s) URL", c.BaseURL))
			} else if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				errs = append(errs, errors.New("base_url must not carry credentials, a query or a fragment; keys come from the environment"))
			} else if _, keyed := keys[strings.ToLower(u.Hostname())]; keyed && u.Scheme != "https" {
				errs = append(errs, fmt.Errorf("base_url %q takes a key, so it must be https", c.BaseURL))
			}
		}
		if c.Model == "" && (c.Type == "anthropic" || c.BaseURL != "" && strings.TrimSuffix(c.BaseURL, "/") != OpenRouter) {
			errs = append(errs, errors.New("model is required except on OpenRouter's default"))
		}
	case "exec":
		if len(c.Command) == 0 || c.Command[0] == "" {
			errs = append(errs, errors.New("command is required for type exec"))
		}
		if c.Vendor == "" {
			errs = append(errs, errors.New("vendor is required for type exec"))
		}
		if c.BaseURL != "" || c.Model != "" {
			errs = append(errs, errors.New("base_url and model are not used by type exec"))
		}
	default:
		errs = append(errs, fmt.Errorf("type %q: want openaicompat, anthropic or exec", c.Type))
	}
	return errors.Join(errs...)
}

// Resolve fills in the defaults. With no model on OpenRouter it picks a free
// one from a vendor other than workerVendor.
func (c Config) Resolve(workerVendor string) Config {
	if c.Type == "" {
		c.Type = "openaicompat"
	}
	if c.BaseURL == "" {
		c.BaseURL = map[string]string{"openaicompat": OpenRouter, "anthropic": anthropic}[c.Type]
	}
	c.BaseURL = strings.TrimSuffix(c.BaseURL, "/")
	if c.Model == "" && c.BaseURL == OpenRouter {
		for _, m := range freeModels {
			if c.Model = m; vendorOf(m) != strings.ToLower(workerVendor) {
				break
			}
		}
	}
	return c
}

// ModelVendor is who makes the model, lower case, or "" if c does not say.
func (c Config) ModelVendor() string {
	// A known endpoint says who made the model; vendor cannot override it.
	switch host := hostOf(c.BaseURL); {
	case host == "api.anthropic.com":
		return "anthropic"
	case host == "api.openai.com":
		return "openai"
	case host == "openrouter.ai":
		return vendorOf(c.Model)
	case c.Vendor != "":
		return strings.ToLower(c.Vendor)
	case c.Type == "anthropic":
		return "anthropic"
	}
	return ""
}

func vendorOf(model string) string {
	v, _, ok := strings.Cut(model, "/")
	if !ok {
		return ""
	}
	return strings.ToLower(v)
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// Digest identifies the reviewer c describes.
func (c Config) Digest() string {
	b, _ := json.Marshal(c) // plain strings: always marshals
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// String names the reviewer for people: type, model and config digest.
func (c Config) String() string {
	model := c.Model
	if c.Type == "exec" { // name what runs
		model = strings.Join(c.Command, " ")
	}
	return fmt.Sprintf("%s %s (config %.12s)", c.Type, model, c.Digest())
}

// New returns the provider c describes, resolved. Keys come from getenv. An
// exec command runs as cred when it is set, in an empty HOME of its own.
func New(c Config, getenv func(string) string, cred *syscall.Credential) (Func, error) {
	switch c.Type {
	case "openaicompat", "anthropic":
		key, err := keyFor(c.BaseURL, getenv)
		if err != nil {
			return nil, err
		}
		if c.Type == "anthropic" {
			return func(ctx context.Context, system, user string) (string, error) {
				return anthropicCall(ctx, c, key, system, user)
			}, nil
		}
		return func(ctx context.Context, system, user string) (string, error) {
			return openaiCall(ctx, c, key, system, user)
		}, nil
	case "exec":
		return func(ctx context.Context, system, user string) (string, error) {
			return execCall(ctx, c.Command, getenv("PATH"), cred, system+"\n\n"+user)
		}, nil
	}
	return nil, fmt.Errorf("provider: unknown type %q", c.Type)
}

func keyFor(base string, getenv func(string) string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	env, ok := keys[strings.ToLower(u.Hostname())]
	if !ok {
		return "", nil // a local or self-hosted endpoint: no key
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("provider: %s takes a key, so its base_url must be https", u.Hostname())
	}
	key := getenv(env)
	if key == "" {
		return "", fmt.Errorf("provider: set %s for %s", env, u.Hostname())
	}
	return key, nil
}

func openaiCall(ctx context.Context, c Config, key, system, user string) (string, error) {
	in := map[string]any{
		"model":       c.Model,
		"temperature": 0,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	h := http.Header{}
	if key != "" {
		h.Set("Authorization", "Bearer "+key)
	}
	if err := post(ctx, c.BaseURL+"/chat/completions", h, in, &out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", errors.New("provider: the answer has no choices")
	}
	return out.Choices[0].Message.Content, nil
}

func anthropicCall(ctx context.Context, c Config, key, system, user string) (string, error) {
	in := map[string]any{
		"model":       c.Model,
		"max_tokens":  8192,
		"temperature": 0,
		"system":      system,
		"messages":    []map[string]string{{"role": "user", "content": user}},
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	h := http.Header{}
	h.Set("anthropic-version", "2023-06-01")
	if key != "" {
		h.Set("x-api-key", key)
	}
	if err := post(ctx, c.BaseURL+"/v1/messages", h, in, &out); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, p := range out.Content {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String(), nil
}

func post(ctx context.Context, url string, h http.Header, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header = h
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("provider: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return fmt.Errorf("provider: %w", err)
	}
	if len(b) > maxAnswer {
		return errors.New("provider: the answer is over 4 MiB")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("provider: %s answered %s: %.500s", url, resp.Status, b)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("provider: the answer is not the expected JSON: %w", err)
	}
	return nil
}

// capped keeps the first maxAnswer bytes written to it.
type capped struct {
	bytes.Buffer
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if c.Len()+len(p) > maxAnswer {
		c.over = true
		return len(p), nil
	}
	return c.Buffer.Write(p)
}

func execCall(ctx context.Context, argv []string, path string, cred *syscall.Credential, prompt string) (string, error) {
	home, err := os.MkdirTemp("", "tardis-provider-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(home)
	if cred != nil {
		if err := os.Chown(home, int(cred.Uid), int(cred.Gid)); err != nil {
			return "", err
		}
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = home
	cmd.Env = []string{"PATH=" + path, "HOME=" + home} // none of the caller's keys
	cmd.Stdin = strings.NewReader(prompt)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: cred}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr capped
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("provider: %s: %v: %.500s", argv[0], err, strings.TrimSpace(stderr.String()))
	}
	if stdout.over {
		return "", errors.New("provider: the answer is over 4 MiB")
	}
	return stdout.String(), nil
}
