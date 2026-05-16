---
name: unblock-dependabot-pr
description: Diagnose and unblock failed Dependabot pull requests in cloud-provider-azure by classifying CI failures, syncing Go modules, retesting quota-flaked e2e jobs, and updating PR status. Use when a Dependabot PR fails go-mod-consistency, pull-cloud-provider-azure-e2e jobs, or dependency/toolchain CI.
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

1. Inspect the current PR state before changing files:

```bash
gh pr view <pr> --json number,title,headRefName,headRepositoryOwner,headRefOid,baseRefName,author,labels,mergeable,statusCheckRollup
gh pr checks <pr>
```

2. If the local checkout is needed, fetch and check out the PR head, then check
   for unrelated work:

```bash
gh pr checkout <pr>
git status --short
```

Stop and report a conflict if unrelated uncommitted changes exist. Do not stage
or overwrite unrelated files.

3. Classify every failing required job. Do not treat a PR as unblocked until all
   non-pending failures have a clear action.

## go-mod-consistency Failed

Use the shared `sync-go-modules` skill from this repo.

1. Read `.agents/skills/sync-go-modules/SKILL.md`.
2. Run the helper from the PR checkout. If the Dependabot branch is already
   dirty only because of the dependency bump you are fixing, pass
   `--allow-dirty` as that skill describes.
3. Inspect the diff and stage only files produced by the sync:

```bash
git diff --stat
git status --short
git add <specific go.mod/go.sum/vendor files>
git diff --cached --stat
```

4. Commit and push the fix to the PR branch:

```bash
git commit -m "Update Go modules"
git push
```

5. After the push succeeds, comment `/lgtm` on the PR:

```bash
gh pr comment <pr> --body "/lgtm"
```

The push reruns CI. Do not add `/retest` just for the old failed
`go-mod-consistency` run after pushing a new commit.

## Azure Public IP Quota e2e Failures

Use this path only when the failed jobs are exclusively
`pull-cloud-provider-azure-e2e-*` jobs and the failing logs show one of:

- `PublicIPCountLimitReached`
- `PublicIPPrefixCountLimitReached`

Confirm the failure text from the job logs before retesting. If any non-e2e job
also failed, handle that failure separately before blanket retesting.

Retest options:

```bash
gh pr comment <pr> --body "/test <job-name>"
```

Use one `/test <job-name>` comment per failed e2e job when you want a narrow
rerun. Use `/retest` only when all current failed jobs are safe to rerun:

```bash
gh pr comment <pr> --body "/retest"
```

If you are already pushing a commit for another fix, prefer the push-triggered
CI rerun instead of adding a separate retest comment.

## Other Common Dependabot Blockers

Discuss these with the user before changing code or CI policy:

- `golangci-lint` or `Analyze (go)` fails with a Go toolchain mismatch, such as
  `panic: file requires newer Go version ... (application built with ...)`
- `golangci-lint` `typecheck` failures after an Azure SDK or Kubernetes module
  bump
- Mixed Azure SDK major versions, such as `armcompute/v6` consumers in a PR
  that updated packages to `armcompute/v7`
- Mixed Kubernetes module families, such as `k8s.io/api`,
  `k8s.io/apimachinery`, and `k8s.io/client-go` moving ahead of the rest of the
  Kubernetes dependency set
- `dependency-review`, vulnerability, or license failures where the resolution
  may require accepting risk, excluding a finding, or changing the dependency
  version

For these cases, collect the failing job names, the smallest representative log
snippets, current dependency versions, and a proposed narrow fix. Ask before
changing linter policy, broadening a dependency bump, or editing generated
Dependabot PR metadata.

## PR Update Rules

- Preserve the generated Dependabot PR body. If a manual compatibility fix was
  pushed, append a concise reviewer-facing note instead of replacing the body.
- Use specific staging commands, never `git add .`.
- Push only the current task's files.
- Use `/lgtm` after a successful module-sync push when the PR is otherwise ready
  for review.
- Use `/retest` only for failures already classified as transient or safe to
  rerun.
- Report pending jobs and any residual risk clearly instead of claiming the PR
  is green before CI finishes.
