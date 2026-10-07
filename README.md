# tardis-gate

tardis-gate sits between a finished code change and its pull request. It takes the diff
through an ordered list of reviews, and it only opens the PR once an independent runner is
satisfied that every review happened and that each one held up.

It is a framework rather than a fixed policy. It ships four reference reviews, and each
repository that enrols chooses which to run and can add its own. It runs on Temporal Cloud
or on a local `temporal server start-dev`.

**Status:** builds 1 and 2 of 5 are in: check-commit evidence (released as v0.1.0) and
the gate manifest. The rest is specified below and being built in public.

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
author then checks that evidence before the change can become a PR.

## How a run works

1. The author, usually a Claude Code session, finishes a change and runs
   `tardis request <sha>`.
2. A Temporal workflow takes the repository's list of reviews, for example simplify, then
   verify, then design, then an adversarial review.
3. For each review in turn, the author runs it and records the result as an empty
   *check commit* on the branch. The commit carries a short summary and three trailers
   naming the review, the code commit it reviewed, and the tool that ran it.
4. The runner then handles that review. It runs off the author's machine under its own
   GitHub App identity. It checks that the check commit exists, sits in the right place in
   history and is well formed. Then it re-runs the review itself, with fresh context and a
   model from a different vendor, and compares its findings with the author's summary.
5. If they agree, the runner posts a passing `tardis/<review>` check run on that commit.
   If not, it posts a failing check with the reason. The workflow sends that reason back to
   the author, who fixes the code, pushes, and the reviews start again from the top.
6. When every review has passed on the head commit, the runner's App opens the PR. Later,
   once CI is green on the same commit, it can merge.
7. The author's session is told about each step: rejections, the PR being opened, the CI
   result and the merge.

## Design ideas

### A claim is not proof

A check commit is a claim, and it is forgeable by design. Anyone who can push to the branch
can write one. Rather than fight that, tardis-gate keeps claims cheap and readable and
moves trust somewhere else. Only a check run posted by the runner's GitHub App counts as
proof. The base branch's rules accept only that App's checks, and only that App can merge.

This split lets the author's tools stay simple and fast while the part that matters is
hard to fake.

### Evidence lives in git, next to the code

Each check commit records which code commit it reviewed. Its position in history then says
exactly what was reviewed. A code commit that lands after a check commit makes that check,
and every check before it, stale. The next run starts again from the first review.

Keeping evidence in git means it travels with the branch, can be read offline with plain
`git log`, and doesn't depend on any one tool's database. A reviewer can see what was
checked without trusting anything but the history.

### The checker must not be the author

The runner holds the only credentials that can post evidence or merge: the GitHub App key,
the model provider key and the Temporal key for its side. The developer's machine holds
none of them. It can push code and nothing else.

So an agent on the developer's machine cannot approve its own work, however it behaves.
The worst it can do is write false claims, and the runner exists to catch those.

### Independent re-runs, with a different vendor

Running the same review twice with the same model mostly reproduces the same answer. The
runner re-runs each review from scratch with fresh context. For the adversarial review it
must use a model from a different vendor than the author.

The re-run happens in two steps. First the runner reviews the diff blind, without seeing
the author's summary, so it isn't anchored by it. Then a second call compares the two sets
of findings and lists what the author missed and what the runner disputes. Any
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

Temporal also enforces liveness. Every long step sends a heartbeat. If the runner dies
mid-review, the server notices within a minute and the step fails. The author is told,
and a failing check is posted when the runner comes back. A review is never left showing
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

## What it does not protect against

- **A local Temporal server is weaker.** On `temporal server start-dev`, anything on the
  machine can talk to the workflow without logging in. The GitHub check runs and branch
  rules still decide what merges, but the workflow's own state can be tampered with.
  Temporal Cloud ties every action to an API key.
- **The author's tool is self-reported.** The runner learns which vendor wrote the code
  from the check commit. It checks that against a list the repository owner sets, but it
  cannot prove which model actually ran.
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
   rerun, heartbeats, and events back to the author.
4. [Runner: attest and independent re-run](specs/tardis-gate-runner.md): the GitHub App,
   model providers, and the comparison that decides pass or reject.
5. [PR gate, ruleset and author events](specs/tardis-gate-pr.md): opening the PR, marking
   other PRs incomplete, the branch rules, and merging.

## Install

Use a tagged release, never a checkout. On a developer machine:

```bash
go install github.com/leighstillard/tardis-gate/cmd/tardis@v0.1.0
```

On a runner host, download the release binary and check its build provenance first:

```bash
gh release download v0.1.0 -R leighstillard/tardis-gate -p 'tardis_linux_amd64'
gh attestation verify tardis_linux_amd64 -R leighstillard/tardis-gate
```

## Secrets

Nothing secret belongs in this repository. The runner reads its keys from its own
environment on its own host. The developer's machine holds no credential that can post a
check run or merge. The `.gitignore` blocks key files, env files and local databases.

## Licence

MIT. See [LICENSE](LICENSE).
