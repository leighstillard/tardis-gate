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
)

const usage = `usage:
  tardis check commit <gate> --summary-file <file> --tool <vendor/tool/model> [--repo <dir>]
  tardis chain verify <base> <head> --gates <a,b,c> [--repo <dir>]
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
	case len(args) == 1 && args[0] == "version":
		fmt.Fprintln(stdout, version())
		return 0
	}
	fmt.Fprint(stderr, usage)
	return 2
}

// parse parses flags that may follow positional arguments, as in
// `tardis chain verify main HEAD --gates a,b`.
func parse(fs *flag.FlagSet, args []string, npos int, stderr io.Writer) ([]string, bool) {
	fs.SetOutput(stderr)
	var pos []string
	for len(pos) < npos && len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pos, args = append(pos, args[0]), args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return nil, false
	}
	pos = append(pos, fs.Args()...)
	if len(pos) != npos {
		fmt.Fprintf(stderr, "%s: want %d argument(s), got %d\n", fs.Name(), npos, len(pos))
		return nil, false
	}
	return pos, true
}

func checkCommit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check commit", flag.ContinueOnError)
	summaryFile := fs.String("summary-file", "", "file holding the review summary (≤40 lines)")
	tool := fs.String("tool", "", "<vendor>/<tool>/<model> that ran the review")
	repo := fs.String("repo", ".", "repository directory")
	pos, ok := parse(fs, args, 1, stderr)
	if !ok {
		return 2
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
	gates := fs.String("gates", "", "comma-separated gates that must be valid")
	repo := fs.String("repo", ".", "repository directory")
	pos, ok := parse(fs, args, 2, stderr)
	if !ok {
		return 2
	}
	var list []string
	for _, g := range strings.Split(*gates, ",") {
		if g = strings.TrimSpace(g); g != "" {
			list = append(list, g)
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

func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "(devel)"
}
