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
input, such as the `go.mod` diff or PR comment/commit history, in priority order.

Then walk the catalog as an explicit staged algorithm:

> **Guard stage — metadata/diff/comment/commit-history only.** Before running
> `gh pr checks`, reading `statusCheckRollup`, or fetching any Prow log,
> evaluate every guard row whose Signal is computable from PR metadata, the
> `go.mod` diff, and PR comment/commit history alone. If a guard row matches
> and is marked Stop, follow its linked Details action
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
> take that row's linked Details action only when its Details preconditions and
> exclusions hold, then move to the next failed job. Read
> [`references/shared-actions.md`](references/shared-actions.md) only if that
> matched workflow links to it. Continue until every failed required job is
> examined, resolved, rerun, or escalated. Track the actions already taken this
> triage: a push reruns CI but does not resolve other failures, so still
> classify each later failed job from its current evidence and skip only an
> action whose sole effect would be to retest a job the push reruns. Prefer the
> push-triggered rerun. An act row marked Stop ends triage after it is handled.
>
> **Finalize.** If no Stop fired and a push recorded a pending approval,
> evaluate [Post-push /lgtm](references/shared-actions.md#details-post-push-lgtm)
> once, never inside the per-failure loop. Then post the single
> [Attempt stamp](references/shared-actions.md#details-attempt-stamp) summary if
> a retry-budgeted act-stage action completed.

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
- Use one PR-comment-backed retry counter. The
  [Retry budget exhausted](references/guard-patterns.md#details-retry-budget-exhausted)
  guard reads `N` once, before any rebase/recreate directive or CI/log I/O;
  reuse it for the
  whole triage and write at most one attempt stamp per triage. When the budget
  is exhausted, mutate nothing and escalate. Public-IP quota reruns are
  unbudgeted; [Attempt stamp](references/shared-actions.md#details-attempt-stamp)
  owns the accounting.
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
