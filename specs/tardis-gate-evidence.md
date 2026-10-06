# tardis-gate: check-commit evidence & chain   appetite: 2 Codex windows / 1 Claude review
Build 1/5. Needs: nothing. Feeds: tardis-gate-manifest, -workflow, -runner, -pr.

## Problem
Nothing on a branch records that review X ran against code Y; a reviewer's word lives in
a chat log and dies with it. Fixed when a branch carries machine-parsable claims anyone
can order and judge valid / missing / broken with one command and no network.

## Solution sketch
- Package `internal/chain`: walk `base..head`. A commit is a *check* iff its subject is
  `ship-check: <gate>` and it has trailers `Ship-Check`, `Ship-Check-Of`,
  `Ship-Check-Tool`; anything else is *code*.
- Check is valid iff: tree == parent tree, `Ship-Check-Of` == nearest preceding code SHA,
  body ≤ 40 lines, no code commit between that SHA and the check. Any code commit after
  a check breaks every check before it. Per gate: valid | missing | broken(<reason>).
- `tardis check commit <gate> --summary-file f --tool <vendor/tool/model>` writes it.
- `tardis chain verify <base> <head> --gates a,b,c` → JSON `{gate: status}`, exit 0 iff
  all valid. `chain.Verify()` is what -runner's Attest calls.
- Touches: `internal/chain`, `cmd/tardis`. Does not touch: Temporal, GitHub, providers.

## Acceptance checks
- `go test ./internal/chain/...` → pass, no network (git fixtures built in TempDir)
- fixture A, chk(simplify,Of=A), chk(verify,Of=A); `tardis chain verify` → exit 0
- fixture A, chk(simplify,Of=A), code B → exit 1; JSON `simplify: broken(superseded)`
- fixture chk(simplify, Of=<other sha>) → exit 1; `simplify: broken(of-mismatch)`
- fixture check commit with a tree change → exit 1; `broken(not-empty)`
- fixture body of 41 lines → exit 1; `broken(body-too-long)`
- `--gates simplify,verify`, only simplify present → exit 1; `verify: missing`
- two chk(simplify) on the same SHA, first malformed → exit 0 (later one supersedes)
- `tardis check commit simplify --summary-file s.md --tool anthropic/claude-code/x`, then
  `git log -1 --format='%(trailers:key=Ship-Check-Of,valueonly)'` → prior code SHA
- `time tardis chain verify` on a 200-commit fixture → < 1 s

## No-gos
- No signing or GPG; a check commit is a claim; proof is the App's check run (-runner).
- Never rewrites history; broken checks stay in place and are ignored.
- No body schema beyond the 40-line cap and the three trailers; no JSON in the body.
- Does not read `gate.yml`; the gate list is an argument (-manifest resolves it).

## Decisions
- Write with `git commit --allow-empty --trailer`, read with `git interpret-trailers
  --parse` — both work on git 2.53 (spike)
- Emptiness = `git rev-parse <c>^{tree} <c>^^{tree}` equal — `diff-tree` needs `--root`
  and would be a second code path (spike)
- `Ship-Check` = gate name — machine-stable; subject stays human (expert-general, 0.7)
- `Ship-Check-Tool` = `<vendor>/<tool>/<model>`; vendor ∈ {anthropic, openai, google,
  local, other} — the only field -runner reads; prefix check (expert-general, 0.8)
- Three trailers and the 40-line cap hardcoded, no `evidence` schema — every gate needs
  the same three; a fourth earns the field (expert-general, 0.7)
- CLI = stdlib `flag`, one FlagSet per subcommand — ~10 commands; cobra buys nothing
  (expert-general, 0.85)
- `Ship-Check-Of` = nearest preceding code commit, not merge-base — unambiguous ordering

## Open questions
- (none)
