# tardis-gate: auditor (attest + re-run)       appetite: 3 Codex windows / 2 Claude reviews
Build 4/5. Needs: -evidence, -manifest, -workflow. Feeds: -pr.

## Problem
A check commit proves nothing; its worker wrote it. Fixed when an off-machine process
with its own GitHub App identity verifies each claim, re-runs the review with fresh
context and a different vendor, and only its `tardis/<gate>` check run counts.

## Solution sketch
- Activity `Attest(gate, sha)`: fresh clone, `chain.Verify` → valid | missing | broken.
- Activity `Rerun(gate, sha)`: blind review via the configured provider → JSON findings
  `{path, line, severity, claim}`; then a judge call over (worker body, findings) →
  `{missed, disputed}`. For `must_differ_from` gates: refuse before any call if the
  trailer vendor == provider vendor or is not in `worker_vendors`.
- Verdict: reject with reason if `missed` or `disputed` is non-empty, else pass.
- Post `tardis/<gate>` check run on the SHA as the App: success, or failure with reason
  and findings in the check-run output. Heartbeats throughout (numbers in -workflow).
- `tardis review <gate>`: the reference gates' `run`; the same blind review via the
  configured provider, from the worker's side. A stub that exits 2 until this build.
- `tardis auditor`: hosts queue `runner`; reads App key, provider key, Temporal env.
- Touches: `internal/auditor`, `internal/provider`, `internal/ghapp`, `cmd/tardis auditor`.
  Does not touch: branch contents (read-only clone), worker tokens, chain rules.

## Acceptance checks
- `go test ./internal/auditor/... ./internal/provider/... ./internal/ghapp/...` → pass,
  no network (httptest fakes for GitHub and providers)
- fixture SHA lacking `verify` check → `tardis/verify: failure`, summary "missing check"
- forged `Ship-Check-Of` → `tardis/<gate>: failure`, summary "of-mismatch"
- trailer `anthropic/…`, provider `anthropic`, gate `review` → "same vendor"; 0 calls made
- planted-bug fixture → `tardis/review: failure`, output text contains the finding;
  fix commit + new checks → `tardis/review: success`
- `kill -9 tardis auditor` during Rerun → check run failure + worker notified (-workflow)
- `provider: exec` with a fake `codex` on PATH → prompt on stdin, stdout parsed
- judge returns only `disputed` → `tardis/<gate>: failure`, summary "disputed: …"
- config without `provider` → `openaicompat` at openrouter.ai, model ends in `:free`
- check-run output names tardis version, reference-gate digest and reviewer (provider,
  model, config digest); changing the model re-runs a gate that had passed; `tardis
  review` warns when its reference-gate digest differs from the auditor's

## No-gos
- Auditor never writes to the branch, never pushes, never holds the worker's token.
- The re-run never sees the auditor's keys: it runs as its own unprivileged user in a
  sandbox with an empty HOME and only the provider endpoint reachable.
- No provider call for a `must_differ_from` gate when vendors match or are unlisted.
- Check runs are posted only by the App; `tardis` on a dev machine has no path to do it.
- The blind review never sees the worker's summary; only the judge call does.

## Decisions
- App: checks:write, pull_requests:write, contents:write, metadata:read; no webhooks, no
  admin; findings go in check-run `output.text`, not comments (expert-go-aws, 0.85)
- Provider = `Complete(ctx, system, user) (string, error)`; impls `openaicompat`
  (openrouter, openai, ollama/vllm), `anthropic`, `exec` (stdin prompt; codex =
  `codex exec -`); no SDKs (expert-general, 0.85)
- Compare = LLM-as-judge, blind then judge — worker body is prose (expert-general, 0.75)
- Worker vendor = trailer, cross-checked against `worker_vendors` (expert-general, 0.7)
- App token: hand-rolled RS256 JWT + cached 1 h installation token (expert-go-aws, 0.9)
- Any disagreement rejects: `missed` or `disputed` non-empty ⇒ reject (Owner)
- Default provider = OpenRouter free tier via `openaicompat`, `OPENROUTER_API_KEY`, a
  `:free` model whose vendor ≠ the worker's (Owner)

## Open questions
- (none)
