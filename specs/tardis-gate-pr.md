# tardis-gate: PR gate, ruleset & events       appetite: 2 Codex windows / 1 Claude review
Build 5/5. Needs: -workflow, -runner. Feeds: nothing.

## Problem
Anyone can open and merge a PR regardless of review state. Fixed when only the App opens
the PR, only once every gate is green on head; any other PR is marked incomplete and
unmergeable; and the author's session hears of PR creation, CI and merge without polling.

## Solution sketch
- Activity `OpenPR(branch, head)`: re-reads check runs on head from GitHub; if every
  resolved gate is success, the App opens the PR and posts `tardis/complete: success`.
- Foreign PRs: poll open PRs on base every 30 s (ETag); a PR not opened by the App, or
  whose head lacks green gates, gets `tardis/complete: failure` titled "incomplete".
- `tardis ruleset apply`: writes the two rulesets below using the operator's own `gh`
  token at enrol; idempotent on ruleset name.
- `tardis hook install|uninstall`: shell function wrapping `gh`; `gh pr create` runs
  `tardis chain verify` first and refuses on exit 1 (advisory).
- The same poll reads `commits/{sha}/check-runs?filter=latest`; CI + gates green on head
  and `auto_merge: true` → activity `Merge` calls the merge API with `sha: head`.
- pr-created / ci / merged / rejected each become a `Notify` event (-workflow).
- Touches: `internal/ghapp`, workflow activities, `cmd/tardis {ruleset,hook}`. Not: gates.

## Acceptance checks
- `go test ./internal/ghapp/...` → pass, no network (httptest fake)
- sample repo, `gh pr create` with user token → ≤ 60 s later `tardis/complete` failure
  "incomplete" on head; `gh pr merge` → refused by ruleset
- green-chain run → PR exists; `gh pr view --json author` = App bot; `tardis wait` prints
  `pr-created <url>`
- push one commit to the open PR → `gh pr checks` shows `tardis/complete` expected
- `tardis hook install`; branch missing a check; `gh pr create` → exit 1 naming gate;
  `tardis hook uninstall`; `gh pr create` → normal gh behaviour
- `tardis ruleset apply` twice → second prints `unchanged`; `gh api .../rulesets` lists 2
- CI green, `auto_merge: true` → `tardis wait` prints `ci success` then `merged <sha>`;
  merge commit author = App bot
- merge API returns 409 (head moved) → no merge; workflow waits for the new head

## No-gos
- No bypass actor but the App; no admin-override path for humans in either ruleset.
- No PR opened for a head lacking any resolved gate's success check run.
- The hook is advisory; enforcement is the ruleset plus App-only merge.
- App never gets `administration`; rulesets are the operator's one-off enrol step.

## Decisions
- One required check `tardis/complete` pinned via `integration_id` = App ID; per-gate
  checks posted, not required — a new gate needs no ruleset re-apply; absence already
  blocks merge; replaces the `tardis/incomplete` name (expert-go-aws, 0.7)
- Two rulesets: `tardis-gates` (pull_request rule + required check, no bypass) and
  `tardis-merge-only` (update/deletion/non_fast_forward; bypass = App, mode pull_request,
  `always` if the API refuses) — bypass actors are ruleset-wide (expert-go-aws, 0.8)
- ETag polling, no webhooks — no inbound port on home box; 304s free (expert-go-aws, 0.7)
- Merge via REST with `sha` — server-side same-SHA check; auto-merge can't pin a SHA
  (expert-go-aws, 0.85)
- Hook = shell function in rc file — gh refuses `alias set pr` (expert-go-aws, 0.8)
- OpenPR trusts GitHub check runs, not workflow memory — Temporal state is forgeable on
  start-dev (expert-go-aws, 0.8)

## Open questions
- (none)
