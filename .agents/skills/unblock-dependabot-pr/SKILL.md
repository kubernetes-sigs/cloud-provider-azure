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
hard-codes a pattern — it walks the catalog by the phased algorithm below.

Fetch PR metadata and changed dependencies first; these feed Phase 0 before any
CI or log I/O:

```bash
gh pr view <pr> --json number,title,headRefName,headRepositoryOwner,headRefOid,baseRefName,author,labels,mergeable
```

Then walk the catalog as an explicit phased algorithm:

> **Phase 0 — Guard (metadata/diff only).** Before running `gh pr checks`,
> reading `statusCheckRollup`, or fetching any Prow log, evaluate every Phase-0
> row whose Detection signal is computable from PR metadata and the `go.mod`
> diff alone. If a Phase-0 row matches and is marked Stop, take its Action
> (e.g. `/close`) and **end triage immediately** — do not inspect CI, sync
> modules, retest, `/lgtm`, or report no-action.
>
> **Phase 1 — Classify.** Only if no Phase-0 stop fired: fetch CI status and
> checkout as needed, then classify every failing required job. A PR is not
> "unblocked" until every failing job maps to a matched row.
>
> **Phase 2 — Act.** Walk matched Phase-2 rows by ascending Priority, honoring
> each row's Preconditions/Exclusions. Track the actions already taken this
> triage: if an action reruns CI (a push), skip any later row whose only effect
> would be to retest jobs that the push will rerun. Prefer the push-triggered
> rerun. A Phase-2 row marked Stop ends triage after it is handled.

Phase 1 inspects CI only after no Phase-0 stop fired:

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
- Resolve Phase-0 guard rows from PR metadata and the `go.mod` diff before any
  CI or log I/O. When a Phase-0 row marked Stop matches, take its Action and end
  triage immediately — do not inspect CI, sync modules, retest, comment `/lgtm`,
  or report that no action is needed.
- Act on matched rows in ascending Priority within each phase, and take a row's
  Action only when its Preconditions/Exclusions hold.
- When a row's Action reruns CI (a push), skip any later row whose only effect
  would be to retest the jobs that push will rerun; prefer the push-triggered
  rerun.
- Stamp every non-final action (a Phase-2 `Stop=no` comment or push that leaves
  the PR for another CI round) with this triage's attempt number, counted from
  the PR's own comment history. Once the automated retry budget is spent, an
  `escalate` row makes no change to the PR — no comment, no checks, no push — and
  the PR is reported as needing human review in the final output. All of this is
  catalog-driven: the Phase-0 retry-budget guard reads the stamps before any
  CI/log I/O, and the shared attempt-stamp rule writes them. Do not invent a
  separate counter.
- A row marked Stop ends triage after it is handled.
- Rerun a failed job only with a per-job `/test <job-name>` comment, and only
  for failures already classified as transient or safe to rerun. Do not use
  `/retest`; rerun each failed job by name so a still-broken required job is
  never blanket-rerun.
- Report pending jobs, `tide` status, and any residual risk clearly instead of
  claiming the PR is green before CI finishes. When an `escalate` row matched
  (retry budget spent, or a toolchain / SDK / policy blocker), report the PR as
  needing human review, naming the failing jobs and the blocker, rather than
  claiming it was unblocked.
