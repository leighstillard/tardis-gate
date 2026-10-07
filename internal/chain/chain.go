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

// commit is one entry of `git log`. Message text is fetched separately, per
// commit, so nothing in a message can be mistaken for structure.
type commit struct {
	sha, tree string
	parents   []string
	subject   string
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
	// Resolve both names once; every later read uses the object IDs, so a ref
	// that moves mid-run cannot mix two histories.
	baseID, err := git(dir, "rev-parse", "--verify", "--quiet", base+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("base %q is not a commit in %s", base, dir)
	}
	headID, err := git(dir, "rev-parse", "--verify", "--quiet", head+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("head %q is not a commit in %s", head, dir)
	}
	// Starting from the merge base keeps a check from claiming a base commit
	// that head's history doesn't contain.
	mb, err := git(dir, "merge-base", baseID, headID)
	if err != nil {
		return nil, fmt.Errorf("base %q and head %q share no history", base, head)
	}
	commits, err := history(dir, "--reverse", "--topo-order", mb+".."+headID)
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
				body, trailers, err := message(dir, c.sha)
				if err != nil {
					return nil, err
				}
				state[gate] = judge(body, trailers, gate, lastCode)
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

	// The verdict is about headID; refuse to hand it to a name that has moved on.
	if now, err := git(dir, "rev-parse", "--verify", "--quiet", head+"^{commit}"); err != nil || now != headID {
		return nil, fmt.Errorf("head %q moved during verification (was %s); verify again", head, headID)
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

// judge checks an empty, single-parent check commit's message against the
// schema. body is %b and trailers is git's raw %(trailers) block.
func judge(body, trailers, gate, lastCode string) string {
	t, reason := parseTrailers(trailers)
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
	n, ok := summaryLines(body, trailers)
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

// history lists commits for a `git log` range. Structure (commit, tree,
// parents) comes from output that holds only object IDs, one commit per line.
// Subjects come from a second, NUL-separated listing that must match the first
// commit for commit; git refuses NUL in messages, so a mismatch means a forged
// object and fails closed.
func history(dir string, args ...string) ([]commit, error) {
	out, err := gitRaw(dir, append([]string{"log", "--format=%H %T %P"}, args...)...)
	if err != nil {
		return nil, err
	}
	var commits []commit
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return nil, fmt.Errorf("chain: malformed git log line %q", line)
		}
		for _, id := range f {
			if !isOID(id) {
				return nil, fmt.Errorf("chain: malformed object id %q in git log", id)
			}
		}
		commits = append(commits, commit{sha: f[0], tree: f[1], parents: f[2:]})
	}

	out, err = gitRaw(dir, append([]string{"log", "-z", "--format=%H %s"}, args...)...)
	if err != nil {
		return nil, err
	}
	recs := strings.Split(strings.TrimRight(out, "\x00"), "\x00")
	if out == "" {
		recs = nil
	}
	if len(recs) != len(commits) {
		return nil, fmt.Errorf("chain: %d subjects for %d commits; refusing a history that does not parse cleanly", len(recs), len(commits))
	}
	for i, rec := range recs {
		sha, subject, _ := strings.Cut(rec, " ")
		if sha != commits[i].sha {
			return nil, fmt.Errorf("chain: subject listing out of step at %s; refusing", commits[i].sha)
		}
		commits[i].subject = subject
	}
	return commits, nil
}

// message returns one commit's body (%b) and raw trailer block (%(trailers)),
// each from its own git call so neither can be confused with the other.
func message(dir, sha string) (body, trailers string, err error) {
	if body, err = gitRaw(dir, "log", "-1", "--format=%b", sha); err != nil {
		return "", "", err
	}
	// Pin the separator so a user's trailer.separators cannot hide our trailers.
	if trailers, err = gitRaw(dir, "-c", "trailer.separators=:", "log", "-1", "--format=%(trailers)", sha); err != nil {
		return "", "", err
	}
	return body, trailers, nil
}

// isOID reports whether s is a full SHA-1 or SHA-256 object ID in hex.
func isOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func git(dir string, args ...string) (string, error) {
	out, err := gitRaw(dir, args...)
	return strings.TrimSpace(out), err
}

// gitRaw returns git's stdout untouched.
func gitRaw(dir string, args ...string) (string, error) {
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
	return stdout.String(), nil
}
