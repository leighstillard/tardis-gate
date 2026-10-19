package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultIsAFreeOpenRouterModelFromAnotherVendor(t *testing.T) {
	c := Config{}.Resolve("anthropic")
	if c.Type != "openaicompat" || c.BaseURL != OpenRouter || !strings.HasSuffix(c.Model, ":free") {
		t.Fatalf("default = %+v", c)
	}
	v := c.ModelVendor()
	if v == "" || v == "anthropic" {
		t.Errorf("default vendor %q", v)
	}
	if other := (Config{}).Resolve(v); other.ModelVendor() == v {
		t.Errorf("a %s worker got a %s default", v, other.ModelVendor())
	}
	if _, err := New(c, func(string) string { return "" }, nil); err == nil || !strings.Contains(err.Error(), "OPENROUTER_API_KEY") {
		t.Errorf("no key: err = %v", err)
	}
}

func TestCheck(t *testing.T) {
	for _, c := range []Config{
		{Type: "gemini"},
		{Type: "exec"},
		{Type: "exec", Command: []string{"codex"}},
		{Type: "anthropic"},
		{BaseURL: "http://localhost:11434/v1"},
		{BaseURL: "ftp://x", Model: "m"},
		{Model: "m", Command: []string{"x"}},
	} {
		if c.Check() == nil {
			t.Errorf("%+v passed Check", c)
		}
	}
	for _, c := range []Config{{}, {Type: "exec", Command: []string{"codex", "exec", "-"}, Vendor: "openai"}, {Type: "anthropic", Model: "claude"}} {
		if err := c.Check(); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
}

func TestKeysGoOnlyToTheirOwnHost(t *testing.T) {
	env := func(k string) string { return "secret-" + k }
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header
		w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer srv.Close()
	f, err := New(Config{Model: "m", BaseURL: srv.URL}.Resolve(""), env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	if got.Get("Authorization") != "" {
		t.Errorf("a key was sent to %s", srv.URL)
	}
	if _, err := keyFor("http://api.openai.com/v1", env); err == nil {
		t.Error("a key would go over plain http")
	}
	if k, _ := keyFor("https://api.anthropic.com", env); k != "secret-ANTHROPIC_API_KEY" {
		t.Errorf("anthropic key = %q", k)
	}
}

func TestOpenAICompatAndAnthropicShapes(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/v1/chat/completions":
			w.Write([]byte(`{"choices":[{"message":{"content":"oa"}}]}`))
		case "/v1/messages":
			if r.Header.Get("anthropic-version") == "" {
				http.Error(w, "no version", 400)
				return
			}
			w.Write([]byte(`{"content":[{"type":"text","text":"an"},{"type":"text","text":"th"}]}`))
		default:
			http.Error(w, "nope", 404)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	oa, _ := New(Config{BaseURL: srv.URL + "/v1/", Model: "m"}.Resolve(""), os.Getenv, nil)
	if out, err := oa(ctx, "sys", "usr"); err != nil || out != "oa" {
		t.Errorf("openaicompat: %q, %v", out, err)
	}
	if msgs := body["messages"].([]any); len(msgs) != 2 || msgs[0].(map[string]any)["content"] != "sys" {
		t.Errorf("openaicompat request: %v", body)
	}
	an, _ := New(Config{Type: "anthropic", BaseURL: srv.URL, Model: "m"}.Resolve(""), os.Getenv, nil)
	if out, err := an(ctx, "sys", "usr"); err != nil || out != "anth" {
		t.Errorf("anthropic: %q, %v", out, err)
	}
	if body["system"] != "sys" {
		t.Errorf("anthropic request: %v", body)
	}
	bad, _ := New(Config{BaseURL: srv.URL + "/nope", Model: "m"}.Resolve(""), os.Getenv, nil)
	if _, err := bad(ctx, "s", "u"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("404: err = %v", err)
	}
}

func TestExecGetsThePromptOnStdinAndNoKeys(t *testing.T) {
	bin := t.TempDir()
	fake := "#!/bin/sh\necho \"home=$HOME key=$OPENAI_API_KEY\"\ncat\n"
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "leaked")
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	f, err := New(Config{Type: "exec", Command: []string{"codex", "exec", "-"}, Vendor: "openai"}.Resolve(""), os.Getenv, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := f(context.Background(), "SYSTEM", "USER")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "SYSTEM\n\nUSER") || strings.Contains(out, "leaked") || strings.Contains(out, "home= ") {
		t.Errorf("exec output %q", out)
	}
}
