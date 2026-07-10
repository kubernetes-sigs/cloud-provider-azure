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

Load the catalog at [`references/failure-patterns.md`](references/failure-patterns.md);
it is the source of truth for which failure patterns exist. The engine never
hard-codes a pattern — it walks the catalog by the staged algorithm below.

Fetch PR metadata and changed dependencies first; these feed guard rows before any
CI or log I/O:

```bash
gh pr view <pr> --json number,title,headRefName,headRepositoryOwner,headRefOid,baseRefName,author,labels,mergeable
```

Then walk the catalog as an explicit staged algorithm:

> **Guard stage — metadata/diff only.** Before running `gh pr checks`, reading
> `statusCheckRollup`, or fetching any Prow log, evaluate every guard row whose
> Signal is computable from PR metadata and the `go.mod` diff alone. If a guard
> row matches and is marked Stop, follow its linked Details action
> (e.g. `/close`) and **end triage immediately** — do not inspect CI, sync
> modules, retest, `/lgtm`, or report no-action.
>
> **Classification gate.** Only if no guard Stop fired: fetch CI status and
> checkout as needed, then build the current failed required job list. Do not
> require a full up-front classification before acting.
>
> **Act stage.** Process failed required jobs one at a time. For one failed job,
> walk act rows by ascending Priority, inspect only enough current evidence to
> match a row or escalate, take that row's linked Details action, then move to
> the next failed job. Continue until every failed required job is examined,
> resolved, rerun, superseded by a push, or escalated. Track the actions already
> taken this triage: if an action reruns CI (a push), skip any later row whose
> only effect would be to retest jobs that the push will rerun. Prefer the
> push-triggered rerun. An act row marked Stop ends triage after it is handled.

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
- Resolve guard rows from PR metadata and the `go.mod` diff before any CI or log
  I/O. When a guard row marked Stop matches, follow its linked
  Details action and end triage immediately — do not inspect CI, sync modules,
  retest, comment `/lgtm`, or report that no action is needed.
- After the guard stage, handle failed required jobs one by one. For each failed
  job, walk act rows in ascending Priority and take a row's linked Details
  action only when its Details preconditions and exclusions hold. Do not stop
  after the first fixed or rerun job unless a Stop row fired, a push made the
  remaining failures stale, or every failed required job has been examined.
- When a row's Details action reruns CI (a push), skip any later row whose only
  effect would be to retest the jobs that push will rerun; prefer the
  push-triggered rerun.
- After all non-final actions in a triage are complete, post one plain attempt
  summary comment with this triage's attempt number, counted from the PR's own
  comment history. Do not add an attempt marker to individual action comments.
  Once the automated retry budget is spent, an `escalate` row makes no change
  to the PR — no comment, no checks, no push — and the PR is reported as needing
  human review in the final output. All of this is catalog-driven: the
  retry-budget guard reads the summary before any CI/log I/O, and the shared
  attempt-stamp rule writes it. Do not invent a separate counter.
- A row marked Stop ends triage after it is handled.
- Use the retry mechanism for the CI system that produced the failure, and only
  after the failure is classified as transient or safe to rerun. For Prow jobs,
  rerun with a per-job `/test <job-name>` comment; never use `/retest`. For
  GitHub Actions check runs, rerun through GitHub Actions (`gh run rerun`), not
  through a PR slash command.
- Report pending jobs, `tide` status, and any residual risk clearly instead of
  claiming the PR is green before CI finishes. When an `escalate` row matched
  (retry budget spent, or a toolchain / SDK / policy blocker), report the PR as
  needing human review, naming the failing jobs and the blocker, rather than
  claiming it was unblocked.
