package ship

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"

	"github.com/leighstillard/tardis-gate/gates"
	"github.com/leighstillard/tardis-gate/internal/ghapp"
	"github.com/leighstillard/tardis-gate/internal/manifest"
)

// PollEvery is how often the auditor looks at PRs and their CI.
const PollEvery = 30 * time.Second

// NoCI is how long a head with no CI check runs waits for one to appear
// before it counts as green.
const NoCI = 2 * time.Minute

// unmovedBase refuses a base that has moved past baseID: it may hold a policy
// the gates never ran under, which only a rebase brings under review.
func unmovedBase(ctx context.Context, dir, base, baseID string) error {
	now, err := gitOut(ctx, dir, "rev-parse", "refs/remotes/origin/"+base)
	if err != nil {
		return err
	}
	if now != baseID {
		return temporal.NewNonRetryableApplicationError(
			base+" is at "+now+", not "+baseID+", where the gates were resolved", "BaseMoved", nil)
	}
	return nil
}

// github is repoURL's owner/repo, if the App can act on it.
func (r *Auditor) github(repoURL string) (string, bool) {
	repo, ok := ghapp.Repo(repoURL)
	return repo, ok && r.App != nil
}

// succeeded reports whether runs hold the App's success for check name.
func succeeded(runs []ghapp.Run, appID int64, name string) bool {
	return slices.ContainsFunc(runs, func(c ghapp.Run) bool {
		return c.App.ID == appID && c.Name == name && c.Status == "completed" && c.Conclusion == "success"
	})
}

// ciState judges CI on a head from its check runs, every one but the App's:
// "" while any is running, or none has appeared and waited is false.
// ponytail: check runs only; read commit statuses too for CI that posts those.
func ciState(runs []ghapp.Run, appID int64, waited bool) (kind, detail string) {
	var pending, failed []string
	n := 0
	for _, c := range runs {
		if c.App.ID == appID {
			continue
		}
		n++
		switch {
		case c.Status != "completed":
			pending = append(pending, c.Name)
		case c.Conclusion != "success" && c.Conclusion != "neutral" && c.Conclusion != "skipped":
			failed = append(failed, c.Name+" "+c.Conclusion)
		}
	}
	switch {
	case len(failed) > 0:
		return "failure", strings.Join(failed, ", ")
	case len(pending) > 0 || n == 0 && !waited:
		return "", ""
	case n == 0:
		return "success", "(no CI check runs)"
	}
	return "success", ""
}

// OpenPR opens the PR as the App, only while the branch still points at the
// head the gates passed on and the base at the commit they were resolved on:
// anything pushed to either since has not been reviewed. Then it posts
// tardis/complete success on the head, the one check the base requires.
func (r *Auditor) OpenPR(ctx context.Context, in OpenPRIn) (PR, error) {
	defer heartbeat(ctx)()
	unlock, err := r.lock(ctx, in.RepoURL)
	if err != nil {
		return PR{}, err
	}
	defer unlock()
	dir, err := r.sync(ctx, in.RepoURL)
	if err != nil {
		return PR{}, err
	}
	if now, err := gitOut(ctx, dir, "rev-parse", "refs/remotes/origin/"+in.Branch); err != nil {
		return PR{}, err
	} else if now != in.Head {
		return PR{}, temporal.NewNonRetryableApplicationError(
			in.Branch+" is at "+now+", not the reviewed "+in.Head, "BranchMoved", nil)
	}
	if err := unmovedBase(ctx, dir, in.Base, in.BaseID); err != nil {
		return PR{}, err
	}
	if err := checkBase(ctx, dir, in.Base, in.BaseID); err != nil {
		return PR{}, err
	}
	// The workflow is not trusted to say the gates passed. Every gate the
	// current base requires must hold a valid check on this head and a pass
	// this auditor recorded on this base: one earned on any other base opens
	// nothing.
	applied, err := gatesFor(dir, in.Base, in.BaseID, in.Head)
	if err != nil {
		return PR{}, err
	}
	var names []string
	for _, g := range applied {
		passed, _, why, err := r.passedHere(dir, in.RepoURL, g.Name, in.BaseID, in.Head)
		if err != nil {
			return PR{}, err
		}
		if !passed {
			return PR{}, temporal.NewNonRetryableApplicationError(fmt.Sprintf(
				"refusing to open a PR: %s on %.12s: %s", g.Name, in.Head, why), "Rejected", nil)
		}
		names = append(names, g.Name)
	}
	repo, ok := r.github(in.RepoURL)
	if !ok {
		fmt.Fprintf(r.Log, "open-pr %s into %s at %s (no GitHub App for %s)\n", in.Branch, in.Base, in.Head, in.RepoURL)
		return PR{URL: "stand-in:" + in.Branch + "@" + in.Head[:min(12, len(in.Head))] + " (no PR opened)"}, nil
	}
	// Nor is this auditor's memory alone: GitHub must hold the App's success
	// for every gate on the head.
	appID, login, err := r.App.Identity(ctx)
	if err != nil {
		return PR{}, err
	}
	runs, err := r.App.Runs(ctx, repo, in.Head)
	if err != nil {
		return PR{}, err
	}
	for _, n := range names {
		if !succeeded(runs, appID, "tardis/"+n) {
			return PR{}, temporal.NewNonRetryableApplicationError(fmt.Sprintf(
				"refusing to open a PR: tardis/%s is not a success on %.12s", n, in.Head), "Rejected", nil)
		}
	}
	// A retry, or a later pass on the same branch, finds the App's PR open.
	pr, err := r.App.FindPR(ctx, repo, in.Branch)
	if err != nil {
		return PR{}, err
	}
	if pr != nil && pr.User.Login != login {
		return PR{}, temporal.NewNonRetryableApplicationError(fmt.Sprintf(
			"PR #%d from %s was opened by %s, and only the App opens PRs; close it and run tardis request again",
			pr.Number, in.Branch, pr.User.Login), "Rejected", nil)
	}
	if pr == nil {
		body := fmt.Sprintf("Opened by tardis-gate: every gate passed on %s.\n\nGates: %s", in.Head, strings.Join(names, ", "))
		if len(names) == 0 {
			body = fmt.Sprintf("Opened by tardis-gate: no gate applies to %s.", in.Head)
		}
		created, err := r.App.CreatePR(ctx, repo, in.Branch, in.Base, in.Branch, body)
		if err != nil {
			return PR{}, err
		}
		pr = &created
	}
	summary := "every gate passed: " + strings.Join(names, ", ")
	if err := r.App.PostCheck(ctx, repo, in.Head, ghapp.CheckRun{Name: ghapp.Check, Conclusion: "success", Title: "complete",
		Summary: summary, Text: summary + fmt.Sprintf("\n\ntardis %s, reference gates %.12s", r.Version, gates.Digest)}); err != nil {
		return PR{}, err
	}
	return PR{URL: pr.URL, Number: pr.Number}, nil
}

// WatchPR polls PR in.Number until CI on in.Head concludes, or the PR is
// merged, closed or moves to another head first.
func (r *Auditor) WatchPR(ctx context.Context, in WatchIn) (CI, error) {
	defer heartbeat(ctx)()
	repo, ok := r.github(in.RepoURL)
	if !ok {
		return CI{}, temporal.NewNonRetryableApplicationError("no GitHub App for "+in.RepoURL, "Malformed", nil)
	}
	appID, _, err := r.App.Identity(ctx)
	if err != nil {
		return CI{}, err
	}
	start := time.Now()
	for {
		pr, err := r.App.GetPR(ctx, repo, in.Number)
		if err != nil {
			return CI{}, err
		}
		switch {
		case pr.Merged:
			return CI{Kind: "merged", Detail: pr.Merge}, nil
		case pr.State != "open":
			return CI{Kind: "closed"}, nil
		case pr.Head.SHA != in.Head:
			return CI{Kind: "moved", Detail: pr.Head.SHA}, nil
		}
		runs, err := r.App.Runs(ctx, repo, in.Head)
		if err != nil {
			return CI{}, err
		}
		if kind, detail := ciState(runs, appID, time.Since(start) > NoCI); kind != "" {
			return CI{Kind: kind, Detail: detail}, nil
		}
		select {
		case <-ctx.Done():
			return CI{}, ctx.Err()
		case <-time.After(PollEvery):
		}
	}
}

// Merge merges PR in.Number at in.Head if the base's manifest asks for
// auto_merge, and returns the merge commit, or "" if it does not. As with
// OpenPR, GitHub's check runs decide, not the workflow: the head must hold the
// App's tardis/complete success and green CI, and the base must not have
// moved. GitHub itself refuses the merge if the head is no longer in.Head.
func (r *Auditor) Merge(ctx context.Context, in MergeIn) (string, error) {
	defer heartbeat(ctx)()
	repo, ok := r.github(in.RepoURL)
	if !ok {
		return "", temporal.NewNonRetryableApplicationError("no GitHub App for "+in.RepoURL, "Malformed", nil)
	}
	unlock, err := r.lock(ctx, in.RepoURL)
	if err != nil {
		return "", err
	}
	defer unlock()
	dir, err := r.sync(ctx, in.RepoURL)
	if err != nil {
		return "", err
	}
	if err := checkBase(ctx, dir, in.Base, in.BaseID); err != nil {
		return "", err
	}
	m, err := manifest.LoadRev(dir, in.BaseID)
	if err != nil {
		return "", fmt.Errorf("manifest on %.12s: %w", in.BaseID, err)
	}
	if !m.AutoMerge {
		return "", nil
	}
	if err := unmovedBase(ctx, dir, in.Base, in.BaseID); err != nil {
		return "", err
	}
	appID, _, err := r.App.Identity(ctx)
	if err != nil {
		return "", err
	}
	runs, err := r.App.Runs(ctx, repo, in.Head)
	if err != nil {
		return "", err
	}
	if !succeeded(runs, appID, ghapp.Check) {
		return "", temporal.NewNonRetryableApplicationError(fmt.Sprintf(
			"refusing to merge: %s is not a success on %.12s", ghapp.Check, in.Head), "Rejected", nil)
	}
	if kind, detail := ciState(runs, appID, true); kind != "success" {
		return "", temporal.NewNonRetryableApplicationError(fmt.Sprintf(
			"refusing to merge: CI is not green on %.12s: %s", in.Head, detail), "Rejected", nil)
	}
	sha, err := r.App.Merge(ctx, repo, in.Number, in.Head)
	if errors.Is(err, ghapp.ErrHeadMoved) {
		return "", temporal.NewNonRetryableApplicationError(err.Error(), "HeadMoved", nil)
	}
	return sha, err
}

// MarkForeign polls each served repository's open PRs every PollEvery until
// ctx ends, and posts tardis/complete failure, "incomplete", on the head of
// every PR into the base that the App did not open: only the App opens a PR,
// and only once every gate has passed. The App's own PRs are left alone, so a
// push to one leaves tardis/complete expected until the gates pass again.
func (r *Auditor) MarkForeign(ctx context.Context) {
	etags := map[string]string{}
	marked := map[string]bool{} // owner/repo#n@sha already posted
	for {
		for url := range r.Repos {
			if err := r.markForeign(ctx, url, etags, marked); err != nil && ctx.Err() == nil {
				fmt.Fprintf(r.Log, "marking foreign PRs on %s: %v\n", url, err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(PollEvery):
		}
	}
}

func (r *Auditor) markForeign(ctx context.Context, repoURL string, etags map[string]string, marked map[string]bool) error {
	repo, ok := r.github(repoURL)
	if !ok {
		return nil
	}
	prs, tag, err := r.App.OpenPRs(ctx, repo, etags[repoURL])
	if errors.Is(err, ghapp.ErrNotModified) {
		return nil
	} else if err != nil {
		return err
	}
	_, login, err := r.App.Identity(ctx)
	if err != nil {
		return err
	}
	base, err := r.baseBranch(ctx, repoURL)
	if err != nil {
		return err
	}
	for _, pr := range prs {
		key := fmt.Sprintf("%s#%d@%s", repo, pr.Number, pr.Head.SHA)
		if pr.Base.Ref != base || pr.User.Login == login || marked[key] {
			continue
		}
		why := fmt.Sprintf("PR #%d was opened by %s, not by tardis-gate. Only the App opens a PR, once every gate has passed: run tardis request on the branch.", pr.Number, pr.User.Login)
		if err := r.App.PostCheck(ctx, repo, pr.Head.SHA, ghapp.CheckRun{Name: ghapp.Check, Conclusion: "failure",
			Title: "incomplete", Summary: why, Text: why}); err != nil {
			return err
		}
		fmt.Fprintf(r.Log, "marked PR #%d on %s incomplete at %s\n", pr.Number, repo, pr.Head.SHA)
		marked[key] = true
	}
	etags[repoURL] = tag // only once all are marked, so a failed post is tried again
	return nil
}

// baseBranch is the base the default branch's manifest names.
func (r *Auditor) baseBranch(ctx context.Context, repoURL string) (string, error) {
	unlock, err := r.lock(ctx, repoURL)
	if err != nil {
		return "", err
	}
	defer unlock()
	dir, err := r.sync(ctx, repoURL)
	if err != nil {
		return "", err
	}
	m, err := manifest.LoadRev(dir, "refs/remotes/origin/HEAD")
	if err != nil {
		return "", fmt.Errorf("manifest on the default branch: %w", err)
	}
	return m.BaseBranch, nil
}
