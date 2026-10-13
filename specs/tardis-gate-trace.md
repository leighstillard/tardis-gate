# tardis-gate: traced tier (worker OTel)      appetite: 3 Codex windows / 2 Claude reviews
Build 6. Needs: -evidence, -manifest, -workflow, -runner. Feeds: brokered tier (backlog).

## Problem
A verdict proves the code survives review, not that the worker ran the review it claims.
Fixed when each claim is bound to the worker session's telemetry and the auditor rejects
a trace that doesn't show the claimed model reading the diff in that gate's window, so
forging means fabricating a whole consistent trace, not one commit.

## Solution sketch
- `.tardis/config.yml`: `tier: basic|traced` (default basic), `require_trace` (default
  false). Untraced claim: blind review + judge decide, check-run title says "untraced";
  `require_trace: true` → failure "no trace". `tier: brokered` → lint "not supported yet".
- Workflow mints a nonce per gate attempt for `AuthorReview`; it must appear in the trace
  before the session's first read of a diff path.
- The attached `tardis` hosts an OTLP/HTTP-JSON receiver on 127.0.0.1; `tardis trace env
  --tool claude|codex` prints the env/flags to start the session with.
- `tardis check commit` cuts the gate's records, scrubs, gzips, pushes them to
  `refs/tardis/trace/<claim>`; trailer `Ship-Check-Trace: <session> <nonce> sha256:<d>`.
- Auditor activity `Trace` between `Attest` and `Rerun` rejects: digest-mismatch,
  stale-nonce, out-of-window, model-mismatch, diff-not-read:<path>, gate-cmd-missing,
  seq-gap. Passing `Trace` only lets `Rerun` proceed.
- Touches: `internal/{trace,chain,workflow,runner,manifest}`, `cmd/tardis`. Not: judge.

## Acceptance checks
- `go test ./internal/...` → pass, no network, OTLP fixtures from Claude 2.1, Codex 0.137
- real Claude Code session started via `tardis trace env`, fixture diff, tier `traced` →
  `tardis/<gate>: success`, title contains "traced"
- trace model haiku vs `Ship-Check-Tool` …/sonnet → "model-mismatch"; trace never reads
  diff path `b.go` → "diff-not-read: b.go"
- `require_trace: false`, no trailer → blind review decides, title "untraced"; `true` →
  failure "no trace"
- round-1 bundle on a round-2 claim → "stale-nonce"; one byte changed → "digest-mismatch";
  an `event.sequence` gap → "seq-gap"
- `git cat-file -p refs/tardis/trace/<sha> | gzip -d | grep -cE '@|account_|organization'`
  → 0; same for the fixture's prompt text
- `git config user.email` not `*@users.noreply.github.com` → `tardis trace env` warns,
  naming GitHub's "keep email private" and "block pushes that expose email" settings

## No-gos
- A trace is a claim, never proof: trace checks only reject; blind review + judge still
  decide every pass.
- Trace content never enters a model prompt; checks are code (no injection path).
- No channel where the worker talks to the auditor (no A2A, no chat); data only.
- No email, account/org IDs, prompt, response or tool-output text leaves the worker.
- No inbound port or live OTLP to the auditor, no otelcol; no gateway adapter (backlog).

## Decisions
- Bundle on a git ref — worker already pushes; no inbound port; Temporal 2 MB payload
  cap; Attest's clone fetches it (expert-general, 0.75)
- One trailer for session, nonce, digest — "a fourth earns the field" (-evidence)
- Nonce per gate attempt — stops replaying an old trace (expert-general, 0.75)
- Paths: Claude `tool_result.tool_input` (`OTEL_LOG_TOOL_DETAILS=1`, no beta traces);
  Codex only inside `exec_command` args; neither emits provider IDs or signatures (spike)
- stdlib OTLP/HTTP-JSON receiver, no protobuf — both tools speak `http/json` (spike)
- `require_trace` default false, untraced labelled; scrub at source + email prompt (Owner)
- After build 5; brokered tier backlogged until an OSS gateway target is chosen (Owner)

## Open questions
- (none)
