// Package chain reads and judges ship-check commits: empty commits that claim a
// review ran against the nearest preceding code commit.
package chain

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

const (
	SubjectPrefix = "ship-check: "
	TrailerCheck  = "Ship-Check"
	TrailerOf     = "Ship-Check-Of"
	TrailerTool   = "Ship-Check-Tool"
	MaxBodyLines  = 40
)

// Status values. Broken statuses are "broken(<reason>)".
const (
	Valid   = "valid"
	Missing = "missing"
)

func broken(reason string) string { return "broken(" + reason + ")" }

// commit is one entry of `git log`.
type commit struct {
	sha, tree string
	parents   []string
	subject   string
	body      string // %b: everything after the subject, trailer block included
	trailers  string // %(trailers): git's raw trailer block, non-trailer lines included
}

// checkGate returns the gate named by a check-commit subject.
func (c commit) checkGate() (string, bool) {
	return strings.CutPrefix(c.subject, SubjectPrefix)
}

// Verify walks merge-base(base, head)..head oldest first and returns a status
// per requested gate. A commit whose subject starts "ship-check: " is a check
// attempt; it counts as a check only if it has one parent and changes nothing,
// and anything else is code. A later check for a gate replaces an earlier one,
// and a code commit supersedes every valid check before it.
func Verify(dir, base, head string, gates []string) (map[string]string, error) {
	if _, err := git(dir, "rev-parse", "--verify", "--quiet", base+"^{commit}"); err != nil {
		return nil, fmt.Errorf("base %q is not a commit in %s", base, dir)
	}
	if _, err := git(dir, "rev-parse", "--verify", "--quiet", head+"^{commit}"); err != nil {
		return nil, fmt.Errorf("head %q is not a commit in %s", head, dir)
	}
	// Starting from the merge base keeps a check from claiming a base commit
	// that head's history doesn't contain.
	mb, err := git(dir, "merge-base", base, head)
	if err != nil {
		return nil, fmt.Errorf("base %q and head %q share no history", base, head)
	}
	commits, err := log(dir, "--reverse", "--topo-order", mb+".."+head)
	if err != nil {
		return nil, err
	}
	trees := map[string]string{}
	for _, c := range commits {
		trees[c.sha] = c.tree
	}

	state := map[string]string{}
	lastCode := mb
	for _, c := range commits {
		if gate, ok := c.checkGate(); ok {
			if len(c.parents) != 1 {
				state[gate] = broken("not-single-parent")
			} else if empty, err := isEmpty(dir, c, trees); err != nil {
				return nil, err
			} else if !empty {
				state[gate] = broken("not-empty")
			} else {
				state[gate] = judge(c, gate, lastCode)
				continue
			}
		}
		// A code commit, or a check attempt that is a merge or changes the tree.
		for g, s := range state {
			if s == Valid {
				state[g] = broken("superseded")
			}
		}
		lastCode = c.sha
	}

	out := make(map[string]string, len(gates))
	for _, g := range gates {
		if s, ok := state[g]; ok {
			out[g] = s
		} else {
			out[g] = Missing
		}
	}
	return out, nil
}

// judge checks an empty, single-parent check commit against the schema.
func judge(c commit, gate, lastCode string) string {
	t, reason := parseTrailers(c.trailers)
	switch {
	case reason != "":
		return broken(reason)
	case checkGateName(gate) != nil:
		return broken("bad-gate")
	case t[TrailerCheck] != gate:
		return broken("subject-mismatch")
	case CheckTool(t[TrailerTool]) != nil:
		return broken("bad-tool")
	case t[TrailerOf] != lastCode:
		return broken("of-mismatch")
	}
	n, ok := summaryLines(c.body, c.trailers)
	switch {
	case !ok:
		return broken("unexpected-trailers")
	case n > MaxBodyLines:
		return broken("body-too-long")
	}
	return Valid
}

// parseTrailers requires the raw trailer block to be exactly the three
// Ship-Check trailers, once each, unfolded, and nothing else.
func parseTrailers(block string) (map[string]string, string) {
	block = strings.TrimRight(block, "\n")
	if block == "" {
		return nil, "missing-trailers"
	}
	t := map[string]string{}
	for _, line := range strings.Split(block, "\n") {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			return nil, "folded-trailer"
		}
		key, val, ok := strings.Cut(line, ": ")
		if !ok || (key != TrailerCheck && key != TrailerOf && key != TrailerTool) {
			return nil, "unexpected-trailers"
		}
		if _, dup := t[key]; dup {
			return nil, "duplicate-trailer"
		}
		if val = strings.TrimSpace(val); val == "" {
			return nil, "missing-trailers"
		}
		t[key] = val
	}
	if len(t) != 3 {
		return nil, "missing-trailers"
	}
	return t, ""
}

// summaryLines counts the raw body lines before the trailer block. It reports
// false if the body does not end with that block.
func summaryLines(body, block string) (int, bool) {
	body = strings.TrimRight(body, "\n")
	block = strings.TrimRight(block, "\n")
	if !strings.HasSuffix(body, block) {
		return 0, false
	}
	summary := strings.TrimRight(strings.TrimSuffix(body, block), "\n")
	if strings.TrimSpace(summary) == "" {
		return 0, true
	}
	return len(strings.Split(summary, "\n")), true
}

// AllValid reports whether every status is Valid.
func AllValid(statuses map[string]string) bool {
	for _, s := range statuses {
		if s != Valid {
			return false
		}
	}
	return true
}

// isEmpty reports whether a single-parent commit leaves its parent's tree as is.
func isEmpty(dir string, c commit, trees map[string]string) (bool, error) {
	if len(c.parents) != 1 {
		return false, nil
	}
	p := c.parents[0]
	pt, ok := trees[p]
	if !ok {
		var err error
		if pt, err = git(dir, "rev-parse", p+"^{tree}"); err != nil {
			return false, err
		}
		trees[p] = pt
	}
	return pt == c.tree, nil
}

// Field and record separators that cannot appear in commit metadata we read.
const (
	fs = "\x1f"
	rs = "\x1e"
)

const logFormat = "%H%x1f%T%x1f%P%x1f%s%x1f%b%x1f%(trailers)%x1e"

func log(dir string, args ...string) ([]commit, error) {
	out, err := git(dir, append([]string{"log", "--format=" + logFormat}, args...)...)
	if err != nil {
		return nil, err
	}
	var commits []commit
	for _, rec := range strings.Split(out, rs) {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.Split(rec, fs)
		if len(f) != 6 {
			return nil, fmt.Errorf("chain: unexpected git log record with %d fields", len(f))
		}
		commits = append(commits, commit{
			sha: f[0], tree: f[1], parents: strings.Fields(f[2]),
			subject: f[3], body: f[4], trailers: f[5],
		})
	}
	return commits, nil
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New("git " + args[0] + ": " + msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}
