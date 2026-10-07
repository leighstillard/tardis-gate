// Command tardis takes a code diff through an ordered list of reviews.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/leighstillard/tardis-gate/internal/chain"
	"github.com/leighstillard/tardis-gate/internal/manifest"
)

const usage = `usage:
  tardis check commit <gate> --summary-file <file> --tool <vendor/tool/model> [--repo <dir>]
  tardis chain verify <base> <head> (--gates <a,b,c> | --manifest) [--repo <dir>]
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
	gates := fs.String("gates", "", "gates that must be valid: a,b,c, or manifest resolve's output, which also pins base and head")
	fromManifest := fs.Bool("manifest", false, "resolve the gates from base's manifest and verify them against the same commits")
	repo := fs.String("repo", ".", "repository directory")
	pos, code, ok := parse(fs, "chain verify <base> <head> (--gates <a,b,c> | --manifest)", args, 2, stderr)
	if !ok {
		return code
	}
	var list []string
	var pinned [2]string // the commits a resolved gate list belongs to
	bound := false       // whether list is bound to pinned
	if *fromManifest {
		if *gates != "" {
			fmt.Fprintln(stderr, "chain verify: use --gates or --manifest, not both")
			return 2
		}
		ids, names, code, err := resolveGates(*repo, pos[0], pos[1])
		if err != nil {
			fmt.Fprintln(stderr, "chain verify:", err)
			return code
		}
		pinned, list, bound = ids, names, true
	} else if v := strings.TrimSpace(*gates); strings.HasPrefix(v, "{") {
		// manifest resolve's output: the list is only good for the commits it
		// was resolved on, so base and head must still name them.
		r, err := parseResolved(v)
		if err != nil {
			fmt.Fprintln(stderr, "chain verify: --gates: not manifest resolve output:", err)
			return 2
		}
		pinned, list, bound = [2]string{r.Base, r.Head}, r.Gates, true
		if err := unmoved(*repo, pos, pinned); err != nil {
			fmt.Fprintf(stderr, "chain verify: --gates was resolved for other commits (%v); resolve again\n", err)
			return 2
		}
		// Anyone can write this JSON; it counts only if base's manifest
		// resolves the same gates, in the same order, for these commits.
		if _, names, code, err := resolveGates(*repo, r.Base, r.Head); err != nil {
			fmt.Fprintln(stderr, "chain verify:", err)
			return code
		} else if !slices.Equal(names, r.Gates) {
			fmt.Fprintf(stderr, "chain verify: --gates lists %v, but %.12s's manifest resolves %v; resolve again\n", r.Gates, r.Base, names)
			return 2
		}
	} else if strings.HasPrefix(v, "[") {
		fmt.Fprintln(stderr, "chain verify: --gates: pass manifest resolve's output as is, or a,b,c")
		return 2
	} else {
		for _, g := range strings.Split(v, ",") {
			if g = strings.TrimSpace(g); g != "" {
				list = append(list, g)
			}
		}
		if len(list) == 0 {
			fmt.Fprintln(stderr, "chain verify: --gates is required")
			return 2
		}
	}
	base, head := pos[0], pos[1]
	if bound {
		base, head = pinned[0], pinned[1]
	}
	statuses, err := chain.Verify(*repo, base, head, list)
	if err != nil {
		fmt.Fprintln(stderr, "chain verify:", err)
		return 2
	}
	if bound {
		if err := unmoved(*repo, pos, pinned); err != nil {
			fmt.Fprintf(stderr, "chain verify: moved during verification (%v); verify again\n", err)
			return 2
		}
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
	if _, err := os.Stat(*repo); err != nil { // can't look, which is not a bad manifest
		fmt.Fprintln(stderr, "manifest lint:", err)
		return 2
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
	ids, names, code, err := resolveGates(*repo, pos[0], pos[1])
	if err != nil {
		fmt.Fprintln(stderr, "manifest resolve:", err)
		return code
	}
	out, _ := json.Marshal(resolved{Base: ids[0], Head: ids[1], Gates: names}) // always marshals
	fmt.Fprintln(stdout, string(out))
	return 0
}

// resolved is manifest resolve's output: the gates, in order, and the commits
// they were resolved for. chain verify --gates takes it as is.
type resolved struct {
	Base  string   `json:"base"`
	Head  string   `json:"head"`
	Gates []string `json:"gates"`
}

// parseResolved reads manifest resolve's output strictly: a typo or a missing
// gates list must not read as "no gates apply" and pass.
func parseResolved(s string) (resolved, error) {
	var r resolved
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return r, err
	}
	// More would miss a stray ] or }; nothing at all may follow the object.
	if _, err := dec.Token(); err != io.EOF {
		return r, errors.New("trailing data")
	}
	if r.Base == "" || r.Head == "" || r.Gates == nil {
		return r, errors.New("base, head and gates are all required")
	}
	return r, nil
}

// unmoved reports an error if any of refs no longer names the commit pinned
// for it: a verdict on the pinned commits says nothing about a new tip.
func unmoved(repo string, refs []string, pinned [2]string) error {
	for i, rev := range refs {
		if id, err := manifest.CommitID(repo, rev); err != nil {
			return err
		} else if id != pinned[i] {
			return fmt.Errorf("%s is now %.12s, not %.12s", rev, id, pinned[i])
		}
	}
	return nil
}

// resolveGates resolves base and head once, so policy and diff come from the
// same revisions, and returns their IDs and the gates that apply. The policy
// comes from base, so a branch cannot drop the gates it must pass. Head must
// contain base and its own policy must load (manifest.CheckHead). On error,
// code is 1 for a bad manifest or a head to rebase, and 2 otherwise.
func resolveGates(repo, base, head string) (ids [2]string, names []string, code int, err error) {
	for i, rev := range []string{base, head} {
		if ids[i], err = manifest.CommitID(repo, rev); err != nil {
			return ids, nil, 2, err
		}
	}
	m, err := manifest.LoadRev(repo, ids[0])
	if errors.Is(err, manifest.ErrNotEnrolled) {
		return ids, nil, 1, fmt.Errorf("%s is not enrolled: commit %s to it first; enrolment is an operator step that tardis does not gate", base, manifest.ConfigPath)
	} else if err != nil {
		return ids, nil, policyCode(err), fmt.Errorf("%s: %s", base, strings.ReplaceAll(err.Error(), "\n", "; "))
	}
	err = manifest.CheckHead(repo, ids[0], ids[1])
	if errors.Is(err, manifest.ErrBehind) {
		return ids, nil, 1, fmt.Errorf("%s does not contain %s; rebase it onto %s first", head, base, base)
	} else if errors.Is(err, manifest.ErrNotEnrolled) {
		return ids, nil, 1, fmt.Errorf("%s removes %s; unenrolment is an operator step that tardis does not gate", head, manifest.ConfigPath)
	} else if err != nil {
		return ids, nil, policyCode(err), fmt.Errorf("%s: %s", head, strings.ReplaceAll(err.Error(), "\n", "; "))
	}
	changed, err := manifest.Changed(repo, ids[0], ids[1])
	if err != nil {
		return ids, nil, 2, err
	}
	names = []string{}
	for _, g := range m.Resolve(changed) {
		names = append(names, g.Name)
	}
	return ids, names, 0, nil
}

// policyCode is 2 when git could not read the policy, 1 when it is wrong.
func policyCode(err error) int {
	if ge := (*manifest.GitError)(nil); errors.As(err, &ge) {
		return 2
	}
	return 1
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
