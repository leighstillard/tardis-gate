package ghapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
)

// Check is the one check the base requires. Per-gate checks are posted but
// not required, so a new gate needs no ruleset change.
const Check = "tardis/complete"

// rulesets are the two the operator applies at enrol. tardis-gates makes
// every change to base a PR carrying the App's tardis/complete success, with
// no bypass at all; tardis-merge-only lets nothing but the App, through a PR,
// update base. mode is the App's bypass mode.
func rulesets(base string, appID int64, mode string) []map[string]any {
	cond := map[string]any{"ref_name": map[string]any{"include": []string{"refs/heads/" + base}, "exclude": []string{}}}
	return []map[string]any{{
		"name": "tardis-gates", "target": "branch", "enforcement": "active", "conditions": cond,
		"bypass_actors": []any{},
		"rules": []any{
			map[string]any{"type": "pull_request", "parameters": map[string]any{
				"required_approving_review_count": 0, "dismiss_stale_reviews_on_push": false,
				"require_code_owner_review": false, "require_last_push_approval": false,
				"required_review_thread_resolution": false,
			}},
			map[string]any{"type": "required_status_checks", "parameters": map[string]any{
				"strict_required_status_checks_policy": false,
				"required_status_checks":               []any{map[string]any{"context": Check, "integration_id": appID}},
			}},
		},
	}, {
		"name": "tardis-merge-only", "target": "branch", "enforcement": "active", "conditions": cond,
		"bypass_actors": []any{map[string]any{"actor_id": appID, "actor_type": "Integration", "bypass_mode": mode}},
		"rules":         []any{map[string]any{"type": "update"}, map[string]any{"type": "deletion"}, map[string]any{"type": "non_fast_forward"}},
	}}
}

// ApplyRulesets writes the two rulesets on repo's base with the operator's
// token, matched by name, and returns "<name> created|updated|unchanged" for
// each. The App bypasses tardis-merge-only for PRs only, or always where
// GitHub refuses that mode.
func ApplyRulesets(ctx context.Context, api, token, repo, base string, appID int64) ([]string, error) {
	c := &App{API: api} // the operator's token, not the App's: the App never gets administration
	auth := "Bearer " + token
	type ruleset struct {
		ID   int64
		Name string
	}
	var have []ruleset
	if err := c.do(ctx, "GET", "/repos/"+repo+"/rulesets?includes_parents=false&per_page=100", auth, nil, &have); err != nil {
		return nil, err
	}
	always := rulesets(base, appID, "always")
	var out []string
	for i, want := range rulesets(base, appID, "pull_request") {
		name := want["name"].(string)
		method, path, did := "POST", "/repos/"+repo+"/rulesets", "created"
		if j := slices.IndexFunc(have, func(h ruleset) bool { return h.Name == name }); j >= 0 {
			method, path, did = "PUT", fmt.Sprintf("%s/%d", path, have[j].ID), "updated"
			var got any
			if err := c.do(ctx, "GET", path, auth, nil, &got); err != nil {
				return out, err
			}
			if matches(want, got) || matches(always[i], got) {
				out = append(out, name+" unchanged")
				continue
			}
		}
		err := c.do(ctx, method, path, auth, want, nil)
		if se := statusError(0); errors.As(err, &se) && se == http.StatusUnprocessableEntity && i == 1 {
			err = c.do(ctx, method, path, auth, always[i], nil) // only tardis-merge-only has a bypass
		}
		if err != nil {
			return out, fmt.Errorf("%s: %w", name, err)
		}
		out = append(out, name+" "+did)
	}
	return out, nil
}

// matches reports whether got, a ruleset as GitHub returns it, holds every
// field of want. GitHub adds fields and may reorder lists, so lists match in
// any order and extra fields are ignored.
func matches(want map[string]any, got any) bool {
	b, _ := json.Marshal(want) // plain values: always marshals
	var w any
	json.Unmarshal(b, &w)
	return subset(w, got)
}

func subset(w, g any) bool {
	switch w := w.(type) {
	case map[string]any:
		gm, ok := g.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range w {
			if !subset(v, gm[k]) {
				return false
			}
		}
		return true
	case []any:
		gs, _ := g.([]any) // an empty list may come back as none
		if len(gs) != len(w) {
			return false
		}
		used := make([]bool, len(gs))
	next:
		for _, v := range w {
			for j, gv := range gs {
				if !used[j] && subset(v, gv) {
					used[j] = true
					continue next
				}
			}
			return false
		}
		return true
	}
	return w == g
}
