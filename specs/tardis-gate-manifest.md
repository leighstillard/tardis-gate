# tardis-gate: gate manifest & enrolment       appetite: 2 Codex windows / 1 Claude review
Build 2/5. Needs: tardis-gate-evidence. Feeds: -workflow, -runner, -pr.

## Problem
A user can't add a review, or enable/override the reference set, without editing the
framework. Fixed when a repo opts in with one config file, each review is a directory
with a `gate.yml`, and one command resolves the ordered gate list a given diff must pass.

## Solution sketch
- `gate.yml` per gate dir: `name`, `applies_when` (path globs; empty = always), `run`
  (argv; env `TARDIS_BASE`, `TARDIS_HEAD`, `TARDIS_DIFF_FILE`, `TARDIS_RUNBOOK`,
  `TARDIS_FINDINGS_OUT`), `timeout`, `retry`, `must_differ_from: author` (bool).
- Framework ships `gates/{simplify,verify,design,review}`; `design.applies_when` is empty
  and the enrolment config supplies the user-visible globs; `review.must_differ_from`.
- Enrolment config `.tardis/config.yml`: `base_branch`, `gates:` ordered list with
  `enabled`, `dir` (override), per-gate `applies_when`; `verify_runbook`; `provider`,
  `author_vendors`, `auto_merge`, `merge_method` (consumed by -runner / -pr).
- `tardis manifest lint` → schema/order errors; `tardis manifest resolve <base> <head>`
  → ordered JSON of applicable gates for the diff (feeds -evidence `--gates`, -workflow).
- Touches: `gates/`, `internal/manifest`, `cmd/tardis`. Does not touch: Temporal, GitHub

## Acceptance checks
- `go test ./internal/manifest/...` → pass, no network
- `tardis manifest lint` in `testdata/sample-repo` → exit 0
- duplicate gate name in config → exit 1, message names the gate
- `gates:` entry whose `dir` lacks `gate.yml` → exit 1, message names the path
- `verify_runbook` missing on disk → exit 1
- `resolve` on a diff under `internal/` only → no design; order simplify,verify,review
- `resolve` on diff touching `web/` → design present, between verify and review
- gate with empty `run` → lint exit 1
- `resolve` output piped to `tardis chain verify --gates` → accepted unchanged
- repo without `.tardis/config.yml` → `lint` exit 1, message "not enrolled"

## No-gos
- No repo-specific logic under `gates/`; all repo specifics live in the enrolment config.
- No remote gate sources in v1 (no registry, no URLs, no submodule resolution).
- `manifest` never executes a gate's `run`; it only resolves and validates.
- No per-gate ordering keys; order is list order in the config.

## Decisions
- Default order simplify, verify, design, review; config list order wins
- YAML via `go.yaml.in/yaml/v3` — maintained successor of gopkg.in yaml.v3, same API, no
  transitive deps; users hand-edit `gate.yml`, JSON/TOML would hurt (expert-general, 0.8)
- No `evidence` field in v1 — all gates need the same three trailers (expert-general, 0.7)
- `run` is argv; prompt and diff arrive via env + files — one contract on both sides
- Config at `.tardis/config.yml`; `dir` defaults to `.tardis/gates/<name>` (Owner)

## Open questions
- (none)
