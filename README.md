# tardis-gate

A gate between a code diff and its pull request. A diff goes through an ordered list of
reviews. The author's session records each review as an empty "check commit". An
off-machine runner, with its own GitHub App identity, checks every claim and re-runs every
review with a different model vendor. Only then does it open the PR.

Status: specs only, nothing built yet. Built in public.

## Specs, in build order

1. [Check-commit evidence and chain](specs/tardis-gate-evidence.md)
2. [Gate manifest and enrolment](specs/tardis-gate-manifest.md)
3. [Ship workflow on Temporal](specs/tardis-gate-workflow.md)
4. [Runner: attest and independent re-run](specs/tardis-gate-runner.md)
5. [PR gate, ruleset and author events](specs/tardis-gate-pr.md)

Each spec is one page with acceptance checks you can run as commands.

## Secrets

Nothing secret belongs in this repo. The runner reads its GitHub App key, model provider
key and Temporal key from its own environment on its own host. The dev machine holds no
credential that can post a check run or merge.
