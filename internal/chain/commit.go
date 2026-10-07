package chain

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Vendors is the fixed vocabulary for the first segment of Ship-Check-Tool.
var Vendors = []string{"anthropic", "openai", "google", "local", "other"}

// CheckTool validates a Ship-Check-Tool value: <vendor>/<tool>/<model>.
func CheckTool(tool string) error {
	parts := strings.Split(tool, "/")
	if len(parts) != 3 || parts[1] == "" || parts[2] == "" || strings.ContainsAny(tool, " \t\r\n") {
		return fmt.Errorf("tool %q: want <vendor>/<tool>/<model>", tool)
	}
	for _, v := range Vendors {
		if parts[0] == v {
			return nil
		}
	}
	return fmt.Errorf("tool %q: vendor must be one of %s", tool, strings.Join(Vendors, ", "))
}

// CheckGateName reports whether gate can name a check commit.
func CheckGateName(gate string) error {
	// Commas would split the name in `chain verify --gates a,b`; whitespace
	// and control characters would not survive the trailer round trip.
	bad := func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }
	if gate == "" || strings.ContainsAny(gate, "/:,") || strings.IndexFunc(gate, bad) >= 0 {
		return fmt.Errorf("gate %q: must be a non-empty name without spaces, commas, slashes or colons", gate)
	}
	return nil
}

// Commit writes an empty check commit for gate on HEAD of the repo in dir.
// Ship-Check-Of is the nearest code commit at or before HEAD. The commit is
// built with commit-tree (no hooks, no message cleanup, parent's exact tree),
// judged exactly as Verify would judge it, and only then published with a
// compare-and-swap on HEAD, so a failure or a concurrent commit changes nothing.
func Commit(dir, gate, summary, tool string) (string, error) {
	if err := CheckGateName(gate); err != nil {
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
	// Outside a repository, git diff falls back to --no-index and the HEAD
	// checks below would read as "detached"; say what is wrong instead.
	if _, err := git(dir, "rev-parse", "--git-dir"); err != nil {
		return "", err
	}
	// The check commit never includes the index, but staged work next to a
	// check is almost always a mistake; say so rather than leave it behind.
	if staged, err := gitRaw(dir, "diff", "--cached", "--name-only", "-z"); err != nil {
		return "", err
	} else if staged != "" {
		return "", errors.New("staged changes present; commit or unstage them first")
	}
	// Publish to the branch HEAD names now, so a checkout while the check is
	// written cannot move it onto another branch. A detached HEAD is refused:
	// a concurrent checkout could reattach it between the read and the CAS.
	ref, err := git(dir, "symbolic-ref", "-q", "HEAD")
	if err != nil || !strings.HasPrefix(ref, "refs/heads/") {
		return "", errors.New("HEAD is not on a local branch; check out the branch under review first")
	}
	before, err := git(dir, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	of, err := lastCode(dir, before)
	if err != nil {
		return "", err
	}
	// The trailers get their own paragraph so they never merge with a
	// trailer-like last line of the summary.
	trailers := TrailerCheck + ": " + gate + "\n" + TrailerOf + ": " + of + "\n" + TrailerTool + ": " + tool
	// UTF-8 pinned: another commit encoding would add a header that makes git
	// recode the subject but not the raw trailers it is checked against.
	sha, err := git(dir, "-c", "i18n.commitEncoding=UTF-8", "commit-tree", before+"^{tree}", "-p", before,
		"-m", SubjectPrefix+gate, "-m", summary, "-m", trailers)
	if err != nil {
		return "", err
	}
	if s, err := asVerified(dir, sha, before, gate, of); err != nil {
		return "", err
	} else if s != Valid {
		return "", fmt.Errorf("the check commit would be %s; rewrite the summary and retry", s)
	}
	if _, err := git(dir, "update-ref", "-m", "tardis: check commit "+gate, ref, sha, before); err != nil {
		return "", fmt.Errorf("%s moved while the check commit was written; nothing changed: %v", ref, err)
	}
	return sha, nil
}

// asVerified judges commit sha exactly as Verify would on top of parent.
func asVerified(dir, sha, parent, gate, of string) (string, error) {
	cs, err := history(dir, "-1", sha)
	if err != nil {
		return "", err
	}
	c := cs[0]
	if g, ok := c.checkGate(); !ok || g != gate {
		return broken("subject-mismatch"), nil
	}
	if len(c.parents) != 1 || c.parents[0] != parent {
		return broken("not-single-parent"), nil
	}
	if empty, err := isEmpty(dir, c, map[string]string{}); err != nil {
		return "", err
	} else if !empty {
		return broken("not-empty"), nil
	}
	body, err := message(dir, sha)
	if err != nil {
		return "", err
	}
	return judge(body, gate, of), nil
}

// maxCheckRun bounds the walk back over stacked check commits.
// ponytail: fixed window; page through history if a branch ever stacks more.
const maxCheckRun = 500

// lastCode returns the nearest first-parent ancestor of start (start included)
// that is not an empty, single-parent check attempt.
func lastCode(dir, start string) (string, error) {
	commits, err := history(dir, "--first-parent", fmt.Sprintf("--max-count=%d", maxCheckRun+1), start)
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
	return "", errors.New("no code commit found before " + start)
}
