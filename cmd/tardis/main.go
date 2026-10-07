// Command tardis takes a code diff through an ordered list of reviews.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"github.com/leighstillard/tardis-gate/internal/chain"
	"github.com/leighstillard/tardis-gate/internal/manifest"
)

const usage = `usage:
  tardis check commit <gate> --summary-file <file> --tool <vendor/tool/model> [--repo <dir>]
  tardis chain verify <base> <head> --gates <a,b,c> [--repo <dir>]
  tardis manifest lint [--repo <dir>]
  tardis manifest resolve <base> <head> [--repo <dir>]
  tardis version
`

// Exit codes: 0 ok, 1 chain not valid, 2 usage or runtime error.
func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	cmd := strings.Join(args[:min(2, len(args))], " ")
	switch {
	case cmd == "check commit":
		return checkCommit(args[2:], stdout, stderr)
	case cmd == "chain verify":
		return chainVerify(args[2:], stdout, stderr)
	case cmd == "manifest lint":
		return manifestLint(args[2:], stdout, stderr)
	case cmd == "manifest resolve":
		return manifestResolve(args[2:], stdout, stderr)
	case len(args) == 1 && args[0] == "version":
		fmt.Fprintln(stdout, version())
		return 0
	case len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help"):
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprint(stderr, usage)
	return 2
}

// parse parses flags before, between or after positional arguments, as in
// `tardis chain verify --repo r main HEAD --gates a,b`. On failure it returns
// the exit code: 0 for -h, 2 for a usage error.
func parse(fs *flag.FlagSet, synopsis string, args []string, npos int, stderr io.Writer) ([]string, int, bool) {
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: tardis %s\n", synopsis)
		fs.PrintDefaults()
	}
	var pos []string
	for {
		if err := fs.Parse(args); err == flag.ErrHelp {
			return nil, 0, false
		} else if err != nil {
			return nil, 2, false
		}
		// Parse stops at the first positional; take it and parse on.
		if args = fs.Args(); len(args) == 0 {
			break
		}
		pos, args = append(pos, args[0]), args[1:]
	}
	if len(pos) != npos {
		fmt.Fprintf(stderr, "%s: want %d argument(s), got %d\n", fs.Name(), npos, len(pos))
		fs.Usage()
		return nil, 2, false
	}
	return pos, 0, true
}

func checkCommit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check commit", flag.ContinueOnError)
	summaryFile := fs.String("summary-file", "", "file holding the review summary (≤40 lines)")
	tool := fs.String("tool", "", "<vendor>/<tool>/<model> that ran the review")
	repo := fs.String("repo", ".", "repository directory")
	pos, code, ok := parse(fs, "check commit <gate> --summary-file <file> --tool <vendor/tool/model>", args, 1, stderr)
	if !ok {
		return code
	}
	if *summaryFile == "" || *tool == "" {
		fmt.Fprintln(stderr, "check commit: --summary-file and --tool are required")
		return 2
	}
	summary, err := os.ReadFile(*summaryFile)
	if err != nil {
		fmt.Fprintln(stderr, "check commit:", err)
		return 2
	}
	sha, err := chain.Commit(*repo, pos[0], string(summary), *tool)
	if err != nil {
		fmt.Fprintln(stderr, "check commit:", err)
		return 2
	}
	fmt.Fprintln(stdout, sha)
	return 0
}

func chainVerify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chain verify", flag.ContinueOnError)
	gates := fs.String("gates", "", "gates that must be valid: a,b,c or the JSON array from manifest resolve")
	repo := fs.String("repo", ".", "repository directory")
	pos, code, ok := parse(fs, "chain verify <base> <head> --gates <a,b,c>", args, 2, stderr)
	if !ok {
		return code
	}
	var list []string
	if v := strings.TrimSpace(*gates); strings.HasPrefix(v, "[") {
		if err := json.Unmarshal([]byte(v), &list); err != nil {
			fmt.Fprintln(stderr, "chain verify: --gates:", err)
			return 2
		}
	} else {
		for _, g := range strings.Split(v, ",") {
			if g = strings.TrimSpace(g); g != "" {
				list = append(list, g)
			}
		}
	}
	if len(list) == 0 {
		fmt.Fprintln(stderr, "chain verify: --gates is required")
		return 2
	}
	statuses, err := chain.Verify(*repo, pos[0], pos[1], list)
	if err != nil {
		fmt.Fprintln(stderr, "chain verify:", err)
		return 2
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(statuses); err != nil {
		fmt.Fprintln(stderr, "chain verify:", err)
		return 2
	}
	if !chain.AllValid(statuses) {
		return 1
	}
	return 0
}

func manifestLint(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("manifest lint", flag.ContinueOnError)
	repo := fs.String("repo", ".", "repository directory")
	if _, code, ok := parse(fs, "manifest lint", args, 0, stderr); !ok {
		return code
	}
	m, err := manifest.Load(*repo)
	if err != nil {
		for _, line := range strings.Split(err.Error(), "\n") {
			fmt.Fprintln(stderr, "manifest lint:", line)
		}
		return 1
	}
	names := make([]string, len(m.Gates))
	for i, g := range m.Gates {
		names[i] = g.Name
	}
	fmt.Fprintf(stdout, "ok: %d gates in order: %s\n", len(names), strings.Join(names, ", "))
	return 0
}

func manifestResolve(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("manifest resolve", flag.ContinueOnError)
	repo := fs.String("repo", ".", "repository directory")
	pos, code, ok := parse(fs, "manifest resolve <base> <head>", args, 2, stderr)
	if !ok {
		return code
	}
	// Resolve both names once, so policy and diff come from the same revisions.
	var ids [2]string
	for i, rev := range pos {
		id, err := manifest.CommitID(*repo, rev)
		if err != nil {
			fmt.Fprintln(stderr, "manifest resolve:", err)
			return 2
		}
		ids[i] = id
	}
	// The policy comes from base, so a branch cannot drop the gates it must pass.
	m, err := manifest.LoadRev(*repo, ids[0])
	if err != nil {
		fmt.Fprintln(stderr, "manifest resolve:", strings.ReplaceAll(err.Error(), "\n", "; "))
		return 1
	}
	changed, err := manifest.Changed(*repo, ids[0], ids[1])
	if err != nil {
		fmt.Fprintln(stderr, "manifest resolve:", err)
		return 2
	}
	names := []string{}
	for _, g := range m.Resolve(changed) {
		names = append(names, g.Name)
	}
	out, _ := json.Marshal(names) // a []string always marshals
	fmt.Fprintln(stdout, string(out))
	return 0
}

// releaseVersion is stamped by the release workflow with -ldflags -X.
var releaseVersion string

func version() string {
	if releaseVersion != "" {
		return releaseVersion
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "(devel)"
}
