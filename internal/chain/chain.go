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

// commit is one entry of `git log` over base..head.
type commit struct {
	sha, tree, parent string // parent is the first parent, "" for a root commit
	subject           string
	bodyLines         int // summary lines, trailers excluded
	gate, of, tool    string
}

// isCheck reports whether c is shaped like a check commit. Anything else is code.
func (c commit) isCheck() bool {
	return strings.HasPrefix(c.subject, SubjectPrefix) && c.gate != "" && c.of != "" && c.tool != ""
}

// Verify walks base..head oldest first and returns a status per requested gate.
// A later check for the same gate replaces an earlier one; any commit that
// changes the tree supersedes every valid check before it.
func Verify(dir, base, head string, gates []string) (map[string]string, error) {
	baseSHA, err := git(dir, "rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		return nil, err
	}
	commits, err := log(dir, "--reverse", "--topo-order", base+".."+head)
	if err != nil {
		return nil, err
	}
	trees := map[string]string{}
	for _, c := range commits {
		trees[c.sha] = c.tree
	}

	state := map[string]string{}
	lastCode := baseSHA
	for _, c := range commits {
		empty, err := isEmpty(dir, c, trees)
		if err != nil {
			return nil, err
		}
		if c.isCheck() {
			switch {
			case !empty:
				state[c.gate] = broken("not-empty")
			case c.of != lastCode:
				state[c.gate] = broken("of-mismatch")
			case c.bodyLines > MaxBodyLines:
				state[c.gate] = broken("body-too-long")
			default:
				state[c.gate] = Valid
			}
			if empty {
				continue
			}
		}
		// A code commit, or a check commit that smuggled in a tree change.
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

// AllValid reports whether every status is Valid.
func AllValid(statuses map[string]string) bool {
	for _, s := range statuses {
		if s != Valid {
			return false
		}
	}
	return true
}

func isEmpty(dir string, c commit, trees map[string]string) (bool, error) {
	if c.parent == "" {
		return false, nil // a root commit adds the whole tree
	}
	pt, ok := trees[c.parent]
	if !ok {
		var err error
		if pt, err = git(dir, "rev-parse", c.parent+"^{tree}"); err != nil {
			return false, err
		}
		trees[c.parent] = pt
	}
	return pt == c.tree, nil
}

// Field and record separators that cannot appear in commit metadata we read.
const (
	fs = "\x1f"
	rs = "\x1e"
	vs = "\x1d" // separates repeated trailer values
)

var logFormat = strings.Join([]string{
	"%H", "%T", "%P", "%s", "%b",
	"%(trailers:key=" + TrailerCheck + ",valueonly,separator=%x1d)",
	"%(trailers:key=" + TrailerOf + ",valueonly,separator=%x1d)",
	"%(trailers:key=" + TrailerTool + ",valueonly,separator=%x1d)",
	"%(trailers:only,unfold)",
}, "%x1f") + "%x1e"

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
		if len(f) != 9 {
			return nil, fmt.Errorf("chain: unexpected git log record with %d fields", len(f))
		}
		parent, _, _ := strings.Cut(f[2], " ")
		commits = append(commits, commit{
			sha: f[0], tree: f[1], parent: parent, subject: f[3],
			bodyLines: summaryLines(f[4], f[8]),
			gate:      last(f[5]), of: last(f[6]), tool: last(f[7]),
		})
	}
	return commits, nil
}

// last returns the final value of a possibly repeated trailer, so a repeated
// trailer behaves like git's own "last one wins" reading.
func last(v string) string {
	v = strings.TrimSpace(v)
	if i := strings.LastIndex(v, vs); i >= 0 {
		v = v[i+len(vs):]
	}
	return strings.TrimSpace(v)
}

// summaryLines counts body lines that are not part of the trailer block.
func summaryLines(body, trailers string) int {
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if strings.TrimSpace(body) == "" {
		return 0
	}
	if t := strings.TrimRight(trailers, "\n"); t != "" {
		lines = lines[:max(0, len(lines)-len(strings.Split(t, "\n")))]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return len(lines)
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
