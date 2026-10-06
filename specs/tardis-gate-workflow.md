# tardis-gate: ship workflow (Temporal)       appetite: 3 Codex windows / 2 Claude reviews
Build 3/5. Needs: -evidence, -manifest. Feeds: -runner, -pr.

## Problem
A multi-step review with feedback loops across SHAs has no durable driver; a dead session
loses its place unnoticed. Fixed when one workflow per request drives the manifest in
order, fails loudly on worker death, routes rejections back, runs on start-dev and Cloud.

## Solution sketch
- `tardis request <sha>` = SignalWithStart `ShipWorkflow`, signal `new-head{sha}`, ID
  `ship/<owner>/<repo>/<branch>`; then hosts the author worker (workflow tasks on queue
  `tardis`, activities on `author/<wfID>`). `tardis wait` re-attaches that worker.
- Per gate in resolved order: `AuthorReview(gate, sha)` on `author/<wfID>` (runs the
  gate's `run` locally or waits for the check commit, pushes, returns its SHA) → `Attest`,
  `Rerun` on queue `runner` (-runner) → verdict. Next gate only after a verdict.
- Reject or `new-head` → cancel the in-flight gate, record the reason, restart at gate 0.
- All green → `OpenPR` then poll/merge (-pr). Every state change is a `Notify(event)`
  activity on `author/<wfID>`; `tardis wait` prints them.
- `Executor.Run(ctx, Job, hb func(resume string)) (Result, error)`: `local` runs argv
  inline; `actions` stub documents dispatch → run-name lookup → `hb(runID)` → poll →
  artifact, returns `ErrNotImplemented`.
- Touches: `internal/workflow`, `internal/executor`, `cmd/tardis {request,wait}`. Does
  not touch: review logic, GitHub API, providers (activities are injected).

## Acceptance checks
- `go test ./internal/workflow/...` (SDK testsuite + replay of recorded history) → pass
- `temporal server start-dev` + `tardis runner` + `tardis request <sha>` on fixture →
  status COMPLETED; fake `OpenPR` counter = 1
- `kill -9 tardis runner` mid-Rerun → `tardis wait` prints `verify failed: runner died`
  ≤ 3 min; restart runner → `tardis/verify: failure` check run posted
- reject on gate 2, push new SHA, `tardis request` → gates 1,2 rerun; gate 3's first
  ACTIVITY_TASK_SCHEDULED is after gate 2's verdict (history assertion)
- `tardis request` twice, same branch → one workflow; query `head` = 2nd sha
- same binary, `TEMPORAL_ADDRESS/NAMESPACE/API_KEY` → Cloud → fixture COMPLETED (manual)

## No-gos
- No activity without heartbeat; no gate before the prior verdict; no cross-gate fan-out.
- Workflow code never sees credentials; activities read them from their worker's env.
- No Actions-lite mode, no Lambda executor in v1; contract only. No custom search attrs.
- No push-triggered runs; `tardis request` or a `Ship-Request` trailer only.

## Decisions
- Author hosts the workflow + `author/<wfID>` worker — author review gets a workflow-
  enforced heartbeat; workflow tasks hold no secrets; a dead runner still fails loudly to
  the author (expert-go-aws, 0.8; spike: runner-only polling left the run RUNNING 90 s)
- Events = `Notify` activities — queries cost one Cloud Action each (expert-go-aws, 0.7)
- HeartbeatTimeout 45 s, ticker 10 s (SDK coalesces at ~80%), ScheduleToStart 2 m,
  StartToClose min(20 m, gate timeout), ScheduleToClose 60 m, retry 10 s ×2, 3 attempts,
  non-retryable Rejected/Malformed — spike: kill→timeout ≈ 6 s at 10 s HB (expert, 0.8)
- Env `TEMPORAL_ADDRESS/NAMESPACE/API_KEY`; key ⇒ TLS + API-key creds, none ⇒ plaintext
  localhost — same names as the `temporal` CLI (expert-go-aws, 0.85)
- Runner v1 host = systemd unit on a $5 VPS, root-only EnvironmentFile, Restart=always;
  + scratch Dockerfile — not internet-facing; Lambda can't long-poll (expert-go-aws, 0.7)
- start-dev: `--db-filename`, namespace from env, CLI ≥ 1.1 (expert-go-aws, 0.8). Lost vs
  Cloud: anyone on localhost:7233 can signal/complete/terminate unauthenticated; README
  says so.

## Open questions
- (none)
