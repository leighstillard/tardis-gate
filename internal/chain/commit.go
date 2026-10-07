package chain

import (
	"errors"
	"fmt"
	"strings"
)

// Vendors is the fixed vocabulary for the first segment of Ship-Check-Tool.
var Vendors = []string{"anthropic", "openai", "google", "local", "other"}

// CheckTool validates a Ship-Check-Tool value: <vendor>/<tool>/<model>.
func CheckTool(tool string) error {
	parts := strings.Split(tool, "/")
	if len(parts) != 3 || parts[1] == "" || parts[2] == "" || strings.ContainsAny(tool, " \t") {
		return fmt.Errorf("tool %q: want <vendor>/<tool>/<model>", tool)
	}
	for _, v := range Vendors {
		if parts[0] == v {
			return nil
		}
	}
	return fmt.Errorf("tool %q: vendor must be one of %s", tool, strings.Join(Vendors, ", "))
}

func checkGateName(gate string) error {
	if gate == "" || strings.ContainsAny(gate, " \t\n/:") {
		return fmt.Errorf("gate %q: must be a non-empty name without spaces, slashes or colons", gate)
	}
	return nil
}

// Commit writes an empty check commit for gate on HEAD of the repo in dir.
// Ship-Check-Of is the nearest code commit at or before HEAD. It returns the
// new commit's SHA, and undoes the commit if it would not verify as written.
func Commit(dir, gate, summary, tool string) (string, error) {
	if err := checkGateName(gate); err != nil {
		return "", err
	}
	if err := CheckTool(tool); err != nil {
		return "", err
	}
	summary = strings.TrimRight(summary, "\n")
	if strings.TrimSpace(summary) == "" {
		return "", errors.New("summary is empty")
	}
	if n := len(strings.Split(summary, "\n")); n > MaxBodyLines {
		return "", fmt.Errorf("summary is %d lines; the limit is %d", n, MaxBodyLines)
	}
	// --allow-empty still commits whatever is staged; refuse rather than hide code.
	if _, err := git(dir, "diff", "--cached", "--quiet"); err != nil {
		return "", errors.New("staged changes present; commit or unstage them first")
	}
	of, err := lastCode(dir)
	if err != nil {
		return "", err
	}
	// The trailers get their own paragraph so they never merge with a
	// trailer-like last line of the summary.
	trailers := TrailerCheck + ": " + gate + "\n" + TrailerOf + ": " + of + "\n" + TrailerTool + ": " + tool
	if _, err := git(dir, "commit", "--allow-empty", "--no-verify", "-q", "--cleanup=strip",
		"-m", SubjectPrefix+gate, "-m", summary, "-m", trailers); err != nil {
		return "", err
	}
	head, err := log(dir, "-1", "HEAD")
	if err != nil {
		return "", err
	}
	if s := judge(head[0], gate, of); s != Valid {
		// Only our own empty commit is undone; the index and work tree stay as they were.
		if _, rerr := git(dir, "reset", "-q", "--soft", "HEAD^"); rerr != nil {
			return "", fmt.Errorf("check commit came out %s and could not be undone: %v", s, rerr)
		}
		return "", fmt.Errorf("summary makes the check commit %s; rewrite it and retry", s)
	}
	return head[0].sha, nil
}

// maxCheckRun bounds the walk back over stacked check commits.
// ponytail: fixed window; page through history if a branch ever stacks more.
const maxCheckRun = 500

// lastCode returns the nearest first-parent ancestor of HEAD (HEAD included)
// that is not an empty, single-parent check attempt.
func lastCode(dir string) (string, error) {
	commits, err := log(dir, "--first-parent", fmt.Sprintf("--max-count=%d", maxCheckRun+1), "HEAD")
	if err != nil {
		return "", err
	}
	trees := map[string]string{}
	for _, c := range commits {
		trees[c.sha] = c.tree
	}
	for _, c := range commits { // newest first
		_, isCheck := c.checkGate()
		empty, err := isEmpty(dir, c, trees)
		if err != nil {
			return "", err
		}
		if !isCheck || !empty {
			return c.sha, nil
		}
	}
	return "", errors.New("no code commit found before HEAD")
}
