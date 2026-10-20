# tardis-gate

tardis-gate sits between a finished code change and its pull request. It takes the diff
through an ordered list of reviews, and it only opens the PR once an independent auditor is
satisfied that every review happened and that each one held up.

It is a framework rather than a fixed policy. It ships four reference reviews, and each
repository that enrols chooses which to run and can add its own. It runs on Temporal Cloud
or on a local `temporal server start-dev`.

**Status:** builds 1 to 5 are in: check-commit evidence (released as v0.1.0), the gate
manifest, the ship workflow on Temporal, the auditor's independent re-run and the PR gate.
A traced tier (build 6), which ties each review to the worker's session telemetry, is
specified below and being built in public.

## The problem

Coding agents are now good enough to write most of a change. They are also asked to review
it: simplify the diff, prove it works, check the UI, get a second opinion. Those reviews
mostly happen inside a chat session. When the session ends, the only record is the
agent's own account of what it did.

That account is the weak point. A review can be skipped and still reported as done. It can
run against an earlier version of the code than the one being merged. It can be done by the
same model that wrote the code, which tends to share the same blind spots. Branch protection
doesn't help here. It can require CI to pass, but it has no idea whether a review happened.

tardis-gate makes each review leave evidence in the repository. Something other than the
worker then checks that evidence before the change can become a PR.

## Terms

- **Worker**: the agent session that writes the code and runs the reviews on the
  developer's machine.
- **Auditor**: the off-machine process, with its own GitHub App, that checks the worker's
  claims and re-runs the reviews.
- **Operator**: the person who enrols a repository, holds the auditor's keys and applies
  the branch rules.
- **Gate**: one named review step, defined by a `gate.yml`.
- **Claim**: the worker's check commit, saying gate X ran on code Y with tool Z.
- **Trace**: the worker's telemetry for one gate, tied to its claim (traced tier).
- **Blind review**: the auditor's own re-run of a gate, without the worker's summary.
- **Judge**: the auditor's comparison of the blind review with the claim.
- **Verdict**: the auditor's `tardis/<gate>` check run. The only proof that counts.
- **Round**: one pass through the gates from the first, on one head commit.
- **Run**: one ship workflow for a branch, from `tardis request` to merge.

## How a run works

1. The worker, usually a Claude Code session, finishes a change and runs
   `tardis request <sha>`.
2. A Temporal workflow takes the repository's list of reviews, for example simplify, then
   verify, then design, then an adversarial review.
3. For each review in turn, the worker runs it and records the result as an empty
   *check commit* on the branch. The commit carries a short summary and three trailers
   naming the review, the code commit it reviewed, and the tool that ran it.
4. The auditor then handles that review. It runs off the worker's machine under its own
   GitHub App identity. It checks that the check commit exists, sits in the right place in
   history and is well formed. Then it re-runs the review itself, with fresh context and a
   model from a different vendor, and compares its findings with the worker's summary.
5. If they agree, the auditor posts a passing `tardis/<review>` check run on that commit.
   If not, it posts a failing check with the reason. The workflow sends that reason back to
   the worker, who fixes the code, pushes, and the reviews start again from the top.
6. When every review has passed on the head commit, the auditor's App opens the PR. Later,
   once CI is green on the same commit, it can merge.
7. The worker's session is told about each step: rejections, the PR being opened, the CI
   result and the merge.

## Design ideas

### A claim is not proof

A check commit is a claim, and it is forgeable by design. Anyone who can push to the branch
can write one. Rather than fight that, tardis-gate keeps claims cheap and readable and
moves trust somewhere else. Only a check run posted by the auditor's GitHub App counts as
proof. The base branch's rules accept only that App's checks, and only that App can merge.

This split lets the worker's tools stay simple and fast while the part that matters is
hard to fake.

### Evidence lives in git, next to the code

Each check commit records which code commit it reviewed. Its position in history then says
exactly what was reviewed. A code commit that lands after a check commit makes that check,
and every check before it, stale. The next run starts again from the first review.

Keeping evidence in git means it travels with the branch, can be read offline with plain
`git log`, and doesn't depend on any one tool's database. A reviewer can see what was
checked without trusting anything but the history.

### The checker must not be the worker

The auditor holds the only credentials that can post evidence or merge: the GitHub App key,
its model provider key and the Temporal key for its side. The developer's machine holds
none of them. It can push code and nothing else. (`tardis review` on the developer's
machine uses the developer's own provider key, if any; that key vouches for nothing,
because the auditor re-runs the review with its own.)

So an agent on the developer's machine cannot approve its own work, however it behaves.
The worst it can do is write false claims, and the auditor exists to catch those.

### The worker can't talk the auditor round

The worker and the auditor never talk to each other. There is no chat, no agent-to-agent
protocol such as A2A, and no request the auditor answers. Everything passes through git
history, GitHub check runs and the Temporal workflow, as data that code checks.

This is deliberate. Agents can be argued with. A worker that could message the auditor
could plead, explain away a finding or slip instructions into the conversation. The only
text from the worker the auditor reads is the claim's summary, capped at 40 lines, and
only the judge sees it. Telemetry is never put in front of a model at all.

### Independent re-runs, with a different vendor

Running the same review twice with the same model mostly reproduces the same answer. The
auditor re-runs each review from scratch with fresh context. For the adversarial review it
must use a model from a different vendor than the worker.

The re-run happens in two steps. First the auditor reviews the diff blind, without seeing
the worker's summary, so it isn't anchored by it. Then a second call compares the two sets
of findings and lists what the worker missed and what the auditor disputes. Any
disagreement rejects the review. That is strict on purpose: a rerun is cheaper than a
missed bug.

### Reviews run in order

No review starts before the one ahead of it has a verdict. Order matters because the
reviews change the code. Simplification comes first so that verification and the
adversarial review look at the final shape of the change, not a draft of it. Running them
in parallel would be faster, but later reviews would be checking code that was about to
change.

### A durable driver, with failures that show

A run can span several commits, several rejections and many minutes of model time. A shell
script loses its place when a laptop sleeps. Temporal keeps the run's state on a server,
so it survives crashes, restarts and network drops.

Temporal also enforces liveness. Every long step sends a heartbeat. If the auditor dies
mid-review, the server notices within a minute and the step fails. The worker is told,
and a failing check is posted when the auditor comes back. A review is never left showing
"running" forever.

### The PR is the last step, not the first

Usually a PR is opened early and checks pile up on it. Here, a PR opened by tardis-gate
means every review already passed. A PR opened any other way gets a failing
`tardis/complete` check marked "incomplete", and the branch rules refuse to merge it. A
new push to an open PR makes the check stale until the reviews pass again on the new
commit.

### Framework, not policy

Each review is a directory with a `gate.yml`. It gives the review's name, which paths it
applies to, the command to run, a timeout and a retry count. The framework ships four
reference reviews: simplify, verify, design and an adversarial review. A repository opts in
with `.tardis/config.yml`. That file chooses which reviews run and in what order, which
paths count as user-facing for the design review, and where the runbook for verification
lives. Nothing about a particular repository is built into the framework.

## Assurance tiers

Each repository picks how much evidence it wants in `.tardis/config.yml`:

- **basic**: claims in git, the auditor's blind review and judge. Builds 1 to 5.
- **traced**: adds the worker session's OpenTelemetry. Each claim carries a digest of a
  scrubbed trace, and the auditor rejects a trace that doesn't show the claimed model
  reading the diff during that gate. Build 6.
- **brokered**: the worker's model calls go through a model gateway the auditor reads,
  so the auditor sees the real requests. Planned once an open-source gateway is chosen.

Telemetry comes from the worker's machine, so the traced tier makes forging much harder,
not impossible: a forger has to fabricate a whole consistent trace. The brokered tier is
where faking a review costs about as much as doing one.

## What it does not protect against

- **A local Temporal server is weaker.** On `temporal server start-dev`, anything on the
  machine can talk to the workflow without logging in. The GitHub check runs and branch
  rules still decide what merges, but the workflow's own state can be tampered with.
  Temporal Cloud ties every action to an API key.
- **The worker's tool is self-reported.** The auditor learns which vendor wrote the code
  from the check commit. It checks that against a list the repository owner sets, but it
  cannot prove which model actually ran.
- **One working copy per branch.** A run hands its worker steps to whichever `tardis`
  is attached to that branch. If two clones of the same branch attach at once, either may
  get a step; the wrong one fails it and the worker has to ask again.
- **One auditor per Temporal namespace.** An auditor records the re-runs it passed in its
  own work directory, and auditors share one task queue. A second auditor could pick up a
  run's later steps without the first one's record and refuse them.
- **One tardis version per Temporal namespace.** Each attached `tardis` also runs
  workflow tasks for every run in the namespace. During an upgrade, an older binary can
  pick up a run and drive it with older logic. Upgrade everyone together, or give each
  release its own namespace.
- **Admins still have the keys.** A repository admin can change the branch rules.
  tardis-gate makes skipping a review visible and deliberate; it doesn't take control
  away from the repository owner.

## Specs, in build order

Each spec is one page. Its acceptance checks are commands you can run.

1. [Check-commit evidence and chain](specs/tardis-gate-evidence.md): the commit format and
   the `tardis chain verify` command that judges a branch.
2. [Gate manifest and enrolment](specs/tardis-gate-manifest.md): `gate.yml`,
   `.tardis/config.yml`, and working out which reviews apply to a diff.
3. [Ship workflow on Temporal](specs/tardis-gate-workflow.md): ordering, rejection and
   rerun, heartbeats, and events back to the worker.
4. [Auditor: attest and independent re-run](specs/tardis-gate-auditor.md): the GitHub App,
   model providers, and the comparison that decides pass or reject.
5. [PR gate, ruleset and worker events](specs/tardis-gate-pr.md): opening the PR, marking
   other PRs incomplete, the branch rules, and merging.
6. [Traced tier](specs/tardis-gate-trace.md): binding each claim to the worker's
   telemetry, and the auditor's checks on it.

## Install

Use a tagged release, never a checkout. On a developer machine:

```bash
go install github.com/leighstillard/tardis-gate/cmd/tardis@v0.1.0
```

On an auditor host, download the release binary and check its build provenance first:

```bash
gh release download v0.1.0 -R leighstillard/tardis-gate -p 'tardis_linux_amd64'
gh attestation verify tardis_linux_amd64 -R leighstillard/tardis-gate
```

## Choosing the reviewer

The auditor's re-run and `tardis review` both use the model named in the base branch's
`.tardis/config.yml`:

```yaml
provider:
  type: openaicompat          # openaicompat (default), anthropic or exec
  base_url: https://openrouter.ai/api/v1
  model: qwen/qwen3-coder:free
worker_vendors: [anthropic]   # whose claims a must_differ_from gate accepts
```

With no `provider` section the reviewer is a free OpenRouter model from a vendor other
than the worker's. That list of free models goes stale, so pin `model` for anything that
matters. A gate with `must_differ_from: worker` is refused, before any model call, if the
claim's vendor is not in `worker_vendors`, is `other`, or is the reviewer's own vendor.
The reviewer's vendor comes from its endpoint (api.anthropic.com, api.openai.com, or the
model prefix on OpenRouter); `provider.vendor` names it only for any other endpoint.

API keys never come from the config. They come from the environment, and each key goes
only to its own host, over https: `OPENROUTER_API_KEY` to openrouter.ai, `OPENAI_API_KEY`
to api.openai.com, `ANTHROPIC_API_KEY` to api.anthropic.com. Any other `base_url`, such
as a local model, gets no key.

The reference gates run `tardis review <gate>` on the worker's machine. It is the same
blind review the auditor makes, so a worker sees what the auditor will look for. It warns
when its reference gates differ from the auditor's tardis version. When the model call
fails it exits 75, and any gate that exits 75 gets its `retry` attempts; any other
non-zero exit rejects the change.

## Running the auditor

```bash
tardis auditor --repo https://github.com/you/repo.git \
  --app-id 123456 --app-key /etc/tardis/app.pem
```

The auditor posts a `tardis/<gate>` check run on each gate's head, as the GitHub App.
Without `--app-id` and `--app-key` (or `TARDIS_APP_ID` and `TARDIS_APP_KEY`) the check
runs are only logged. Each check names the tardis version, its reference gates' digest
and, on a pass, the reviewer.

A provider of `type: exec` runs a local command, which is code. The auditor refuses one
unless it is given `--rerun-user <user>`, which must be neither root nor the auditor's
own user, and whose group is not root. The command then runs as that user, with an
empty `HOME` and none of the auditor's environment. Limit that user's network to the
provider with a firewall rule, for example with nftables:

```
meta skuid tardis-rerun ip daddr != <provider address> drop
```

## The PR gate

The App needs checks, pull requests and contents (write) and metadata (read). It never
gets administration. Once per repository, the operator writes the base's two rulesets
with their own `gh` login:

```bash
tardis ruleset apply --app-id 123456
```

`tardis-gates` makes every change to the base a PR that carries the App's
`tardis/complete` success, with no bypass. `tardis-merge-only` lets only the App update
the base. Running it again prints `unchanged`, or puts back anything edited since.

When every gate has passed, the auditor opens the PR as the App and posts
`tardis/complete`. It then watches CI on the head. With `auto_merge: true` in the base's
`.tardis/config.yml`, it merges once CI is green, and only at the head the gates passed
on. A PR opened any other way is marked incomplete within a poll (30 seconds).

`tardis hook install` adds a `gh` wrapper to your shell rc file so that `gh pr create`
refuses a branch whose gates have not passed. `tardis hook uninstall` removes it. The
hook only saves a round trip; the rulesets are what enforce the gate.

## Secrets

Nothing secret belongs in this repository. The auditor reads its keys from its own
environment on its own host. The developer's machine holds no credential that can post a
check run or merge. The `.gitignore` blocks key files, env files and local databases.

## Licence

MIT. See [LICENSE](LICENSE).
