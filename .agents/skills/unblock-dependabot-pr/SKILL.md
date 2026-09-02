---
name: unblock-dependabot-pr
description: Diagnose and unblock failed Dependabot pull requests in cloud-provider-azure by closing Kubernetes minor-version dependency bumps, classifying CI failures, syncing Go modules, retesting quota-flaked e2e jobs, and updating PR status. Use when a Dependabot PR fails go-mod-consistency, pull-cloud-provider-azure-e2e jobs, or dependency/toolchain CI.
---

# Unblock Dependabot Pull Requests

## When To Use

Use this skill when a Dependabot PR in `cloud-provider-azure` has failed CI and
the user wants the agent to unblock it with the smallest safe action.

Expected inputs:

- PR URL or number
- Permission to push to the PR branch when a local fix is needed
- A clean or intentionally scoped working tree

## Triage First

Start by reading these references once:

- [`references/failure-patterns.md`](references/failure-patterns.md) — the
  source-of-truth catalog and routing metadata.
- [`references/guard-patterns.md`](references/guard-patterns.md) — the
  normative guard match criteria and actions.

The engine never hard-codes a pattern — it walks the catalog by the staged
algorithm below. Do not read
[`references/act-patterns.md`](references/act-patterns.md) until the act stage.
Read [`references/shared-actions.md`](references/shared-actions.md) only when a
matched guard or act workflow links to it. Read each reference at most once per
triage; following a cross-file link never requires reloading a file already
read.

Fetch the PR metadata shared by guard rows first, before any CI or log I/O:

```bash
gh pr view <pr> --json number,title,headRefName,headRepositoryOwner,headRefOid,baseRefName,author,labels,mergeable
```

When each guard row is reached, its linked Details fetch any extra allowed
input, such as the `go.mod` diff or PR comment history, in priority order.

Then walk the catalog as an explicit staged algorithm:

> **Guard stage — metadata/diff/comment-history only.** Before running
> `gh pr checks`, reading `statusCheckRollup`, or fetching any Prow log,
> evaluate every guard row whose Signal is computable from PR metadata, the
> `go.mod` diff, and PR comment history alone. If a guard row matches and is
> marked Stop, follow its linked Details action
> (e.g. `/close`) and **end triage immediately** — do not inspect CI, sync
> modules, retest, `/lgtm`, or report no-action.
>
> **Classification gate.** Only if no guard Stop fired: fetch CI status and
> checkout as needed, then build the current failed required job list. Do not
> require a full up-front classification before acting.
>
> **Act stage.** Read
> [`references/act-patterns.md`](references/act-patterns.md), then process failed
> required jobs one at a time. For one failed job, walk act rows by ascending
> Priority, inspect only enough current evidence to match a row or escalate,
> take that row's linked Details action, then move to the next failed job. Read
> [`references/shared-actions.md`](references/shared-actions.md) only if that
> matched workflow links to it. Continue until every failed required job is
> examined, resolved, rerun, superseded by a push, or escalated. Track the
> actions already taken this triage: if an action reruns CI (a push), skip any
> later row whose only effect would be to retest jobs that the push will rerun.
> Prefer the push-triggered rerun. An act row marked Stop ends triage after it
> is handled.

Classification inspects CI only after no guard Stop fired:

```bash
gh pr view <pr> --json number,title,headRefName,headRepositoryOwner,headRefOid,baseRefName,author,labels,mergeable,statusCheckRollup
gh pr checks <pr>
```

If the local checkout is needed, fetch and check out the PR head, then check
for unrelated work:

```bash
gh pr checkout <pr>
git status --short
```

Stop and report a conflict if unrelated uncommitted changes exist. Do not stage
or overwrite unrelated files.

## PR Update Rules

- Preserve the generated Dependabot PR body. If a manual compatibility fix was
  pushed, append a concise reviewer-facing note instead of replacing the body;
  include why the note is needed and the smallest useful evidence, such as the
  compatibility issue, commit SHA, changed files, and validation result.
- Use specific staging commands, never `git add .`.
- Push only the current task's files.
- Resolve guard rows from PR metadata, the `go.mod` diff, and PR comment history
  before any CI or log I/O. When a guard row marked Stop matches, follow its
  linked Details action and end triage immediately — do not inspect CI, sync
  modules, retest, comment `/lgtm`, or report that no action is needed.
- After the guard stage, handle failed required jobs one by one. For each failed
  job, walk act rows in ascending Priority and take a row's linked Details
  action only when its Details preconditions and exclusions hold. Do not stop
  after the first fixed or rerun job unless a Stop row fired, a push made the
  remaining failures stale, or every failed required job has been examined.
- When a row's Details action reruns CI (a push), skip any later row whose only
  effect would be to retest the jobs that push will rerun; prefer the
  push-triggered rerun.
- One retry-budgeted automated unblock round consumes one attempt from one
  PR-comment-backed counter. Public-IP quota e2e reruns are unbudgeted: a
  quota-only triage creates no attempt stamp, while a mixed triage summarizes
  only its budgeted actions. Read the counter once before any rebase or CI/log
  I/O and reuse it throughout the triage. Rebase and budgeted act-stage paths
  must never create two attempt stamps in one triage. When the retry budget is
  exhausted, mutate nothing and escalate for human review. Do not invent a
  second counter; the guard and shared-action references own the policy and
  write mechanics.
- A row marked Stop ends triage after it is handled.
- Use the retry mechanism for the CI system that produced the failure, and only
  after the failure is classified as transient or safe to rerun. For Prow jobs,
  rerun with a per-job `/test <job-name>` comment; never use `/retest`. For
  GitHub Actions check runs, rerun through GitHub Actions (`gh run rerun`), not
  through a PR slash command.
- Report pending jobs, `tide` status, and any residual risk clearly instead of
  claiming the PR is green before CI finishes. When an `escalate` row matched
  (retry budget spent, or a toolchain / SDK / policy blocker), report the PR as
  needing human review, naming the blocker and any failing jobs already known
  under that row's I/O rules, rather than claiming it was unblocked.
