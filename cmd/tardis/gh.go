package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/leighstillard/tardis-gate/internal/ghapp"
	"github.com/leighstillard/tardis-gate/internal/manifest"
)

// rulesetApply writes the base's two rulesets with the operator's own token:
// the App never holds administration.
func rulesetApply(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ruleset apply", flag.ContinueOnError)
	repo := fs.String("repo", ".", "repository directory, enrolled, with an origin on GitHub")
	appID := fs.String("app-id", os.Getenv("TARDIS_APP_ID"), "the GitHub App's numeric ID (or $TARDIS_APP_ID)")
	if _, code, ok := parse(fs, "ruleset apply --app-id <id> [--repo <dir>]", args, 0, stderr); !ok {
		return code
	}
	id, err := strconv.ParseInt(*appID, 10, 64)
	if err != nil || id <= 0 {
		fmt.Fprintln(stderr, "ruleset apply: --app-id must be the App's numeric ID")
		return 2
	}
	// The base comes from the default branch on origin, never the checkout:
	// a feature branch must not move protection off the real base.
	// set-head follows a renamed default branch, which a fetch does not.
	if _, err := gitLine(*repo, "fetch", "-q", "origin"); err != nil {
		fmt.Fprintln(stderr, "ruleset apply:", err)
		return 2
	}
	if _, err := gitLine(*repo, "remote", "set-head", "origin", "--auto"); err != nil {
		fmt.Fprintln(stderr, "ruleset apply:", err)
		return 2
	}
	m, err := manifest.LoadRev(*repo, "refs/remotes/origin/HEAD")
	if err != nil {
		fmt.Fprintln(stderr, "ruleset apply: manifest on origin's default branch:", err)
		return 2
	}
	origin, err := gitLine(*repo, "remote", "get-url", "origin")
	if err != nil {
		fmt.Fprintln(stderr, "ruleset apply:", err)
		return 2
	}
	gh, ok := ghapp.Repo(origin)
	if !ok {
		fmt.Fprintf(stderr, "ruleset apply: origin %s is not a GitHub repository\n", origin)
		return 2
	}
	token := cmp.Or(os.Getenv("GH_TOKEN"), os.Getenv("GITHUB_TOKEN"))
	if token == "" {
		out, err := exec.Command("gh", "auth", "token").Output()
		if token = strings.TrimSpace(string(out)); err != nil || token == "" {
			fmt.Fprintln(stderr, "ruleset apply: no token: set GH_TOKEN or run gh auth login")
			return 2
		}
	}
	done, err := ghapp.ApplyRulesets(context.Background(), "", token, gh, m.BaseBranch, id)
	for _, line := range done {
		fmt.Fprintln(stdout, line)
	}
	if err != nil {
		fmt.Fprintln(stderr, "ruleset apply:", err)
		return 2
	}
	return 0
}

const (
	hookStart = "# >>> tardis hook >>>"
	hookEnd   = "# <<< tardis hook <<<"
	hookBody  = hookStart + `
# Advisory: gh pr create refuses a branch whose gates have not passed.
# The base's rulesets enforce it.
gh() {
  if [ "$1" = pr ] && [ "$2" = create ]; then
    tardis hook check || return 1
  fi
  command gh "$@"
}
` + hookEnd + "\n"
)

// hook installs or removes the gh wrapper in a shell rc file, or, as hook
// check, is the wrapper's test.
func hook(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "check" {
		return hookCheck(args[1:], stdout, stderr)
	}
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	home, _ := os.UserHomeDir()
	rc := filepath.Join(home, ".bashrc")
	if strings.HasSuffix(os.Getenv("SHELL"), "zsh") {
		rc = filepath.Join(home, ".zshrc")
	}
	fs.StringVar(&rc, "rc", rc, "shell rc file")
	pos, code, ok := parse(fs, "hook install|uninstall [--rc <file>]", args, 1, stderr)
	if !ok {
		return code
	}
	if pos[0] != "install" && pos[0] != "uninstall" {
		fs.Usage()
		return 2
	}
	b, err := os.ReadFile(rc)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(stderr, "hook:", err)
		return 2
	}
	s := string(b)
	if i := strings.Index(s, hookStart); i >= 0 {
		end := strings.Index(s[i:], hookEnd)
		if end < 0 {
			fmt.Fprintf(stderr, "hook: %s has %q without %q; fix it by hand\n", rc, hookStart, hookEnd)
			return 2
		}
		s = s[:i] + strings.TrimPrefix(s[i+end+len(hookEnd):], "\n")
	}
	if pos[0] == "install" {
		if s != "" && !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		s += hookBody
	}
	if err := os.WriteFile(rc, []byte(s), 0o644); err != nil {
		fmt.Fprintln(stderr, "hook:", err)
		return 2
	}
	fmt.Fprintf(stdout, "%sed the gh hook in %s; open a new shell, or source it\n", pos[0], rc)
	return 0
}

// hookCheck verifies HEAD's chain against origin's base, as the base's
// manifest requires. Only a chain that is not valid refuses: anything that
// keeps it from looking is reported and let through, as the hook is advisory.
func hookCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hook check", flag.ContinueOnError)
	repo := fs.String("repo", ".", "repository directory")
	if _, code, ok := parse(fs, "hook check [--repo <dir>]", args, 0, stderr); !ok {
		return code
	}
	if top, err := gitLine(*repo, "rev-parse", "--show-toplevel"); err == nil {
		*repo = top // gh pr create may run in a subdirectory
	}
	m, err := manifest.Load(*repo)
	if errors.Is(err, manifest.ErrNotEnrolled) {
		return 0
	} else if err != nil {
		fmt.Fprintln(stderr, "tardis hook: not checked:", err)
		return 0
	}
	var out bytes.Buffer
	switch chainVerify([]string{"--repo", *repo, "--manifest", "origin/" + m.BaseBranch, "HEAD"}, &out, stderr) {
	case 0:
		return 0
	case 1:
		var status map[string]string
		json.Unmarshal(out.Bytes(), &status)
		var bad []string
		for g, s := range status {
			if s != "valid" {
				bad = append(bad, g+" "+s)
			}
		}
		slices.Sort(bad)
		if len(bad) == 0 { // a policy refusal chainVerify has explained, not an invalid chain
			break
		}
		fmt.Fprintf(stderr, "tardis hook: refusing gh pr create: %s; run tardis request, which opens the PR\n", strings.Join(bad, ", "))
		return 1
	}
	fmt.Fprintln(stderr, "tardis hook: not checked; gh pr create goes ahead")
	return 0
}
