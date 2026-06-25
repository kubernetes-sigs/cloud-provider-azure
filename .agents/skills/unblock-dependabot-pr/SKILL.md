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

1. Inspect the PR metadata and changed dependencies before checking CI:

```bash
gh pr view <pr> --json number,title,headRefName,headRepositoryOwner,headRefOid,baseRefName,author,labels,mergeable
```

2. Run the [Kubernetes Minor Version Guard](#kubernetes-minor-version-guard)
   immediately for Dependabot Go module PRs. If the guard finds a disallowed
   minor-family change, close the PR and stop before inspecting CI, syncing
   modules, retesting, commenting `/lgtm`, or reporting that no unblock action
   is needed.

3. Inspect CI only after the minor version guard passes:

```bash
gh pr view <pr> --json number,title,headRefName,headRepositoryOwner,headRefOid,baseRefName,author,labels,mergeable,statusCheckRollup
gh pr checks <pr>
```

4. If the local checkout is needed, fetch and check out the PR head, then check
   for unrelated work:

```bash
gh pr checkout <pr>
git status --short
```

Stop and report a conflict if unrelated uncommitted changes exist. Do not stage
or overwrite unrelated files.

5. Classify every failing required job. Do not treat a PR as unblocked until all
   non-pending failures have a clear action.

6. If no checks are failing and the only pending status is `tide`, follow the
   [Only Tide Pending](#only-tide-pending) path.

## Kubernetes Minor Version Guard

Run this guard for every Dependabot Go module PR before inspecting CI or taking
an unblock action. The goal is to close Kubernetes minor-version bumps early
instead of spending automation time on checks that should not unblock the PR.

Inspect the PR diff for `go.mod` changes to Kubernetes modules:

```bash
gh pr diff <pr> --patch | grep -E '^[+-][[:space:]]+(require[[:space:]]+)?(k8s\.io/[[:alnum:]_.\-/]+)[[:space:]]+v0\.[0-9]+\.[0-9]+'
```

Then apply these rules:

- Compare removed and added lines by module path. A patch-only bump within the
  same minor family is allowed, even when that minor family was already
  mismatched with the release branch before the PR. For example, on
  `release-1.34`, `k8s.io/api v0.35.4` to `v0.35.6` is allowed because the PR
  did not introduce the `v0.35.x` family.
- For any branch, an existing `k8s.io/*` module must not move from one minor
  family to another, such as `v0.36.x` to `v0.37.x`, unless the user explicitly
  asks for that Kubernetes minor bump.
- For a PR targeting `release-1.N`, a newly added `k8s.io/*` module that has no
  removed counterpart in the diff must use the `v0.N.x` family unless the user
  explicitly asks for a different Kubernetes minor family.
- The PR must not introduce a new mixed minor-family set. Existing mixed
  families may receive patch bumps, but a new module or changed module must not
  add a minor family that was not already present in the corresponding removed
  `k8s.io/*` lines.

If the guard finds a disallowed minor-family change, comment `/close` and stop
for that PR. Put `/close` on the first line so Prow can parse it, then include
the base branch, mismatched module lines, and expected minor family:

```bash
gh pr comment <pr> --body-file - <<'EOF'
/close

Reason: closing because this Dependabot PR introduces a disallowed Kubernetes minor-family change.
Base branch: release-1.N; expected new k8s.io modules to use v0.N.x, or existing modules to stay in their removed minor family.
Mismatched modules:
- k8s.io/example v0.X.Y -> v0.Z.W
EOF
```

Do not inspect CI, run module sync, comment `/lgtm`, post `/retest` or `/test`,
or report the PR as requiring no action. Report the `/close` comment plus the
mismatched module lines, the base branch, and the expected minor family. For a
changed existing module, the expected minor family is the module's removed
version; for a newly added module on `release-1.N`, it is `v0.N.x`.

If the guard finds no `k8s.io/*` module changes, or only patch-level updates
within the same minor family, continue with the matching workflow below.

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

5. After the push succeeds, comment `/lgtm` on the PR. Put `/lgtm` on the first
   line so Prow can parse it, then name the pushed commit, changed module files,
   and validation that passed:

```bash
gh pr comment <pr> --body-file - <<'EOF'
/lgtm

Reason: pushed a go-mod-consistency fix for this Dependabot PR.
Commit: <sha>
Files: <go.mod/go.sum/vendor files changed>
Validation: <sync-go-modules check command> passed.
EOF
```

The push reruns CI. Do not add `/retest` just for the old failed
`go-mod-consistency` run after pushing a new commit.

## Only Tide Pending

Use this path when the current status rollup has no failed checks and the only
pending status is `tide`.

Check the PR labels from `gh pr view`. If the PR does not already have the
`lgtm` label, comment `/lgtm` so Tide can re-evaluate the PR. Put `/lgtm` on the
first line, then say that Tide is the only pending status and no checks are
failing:

```bash
gh pr comment <pr> --body-file - <<'EOF'
/lgtm

Reason: no checks are failing, Tide is the only pending status, and the PR does not already have the lgtm label.
EOF
```

If the PR already has the `lgtm` label, do not post a duplicate `/lgtm`
comment. Report that no unblock action was needed and Tide is the only
remaining pending status.

Do not use this path when any non-Tide check is pending or failed. Report those
checks explicitly and handle them through the matching workflow instead.

## Azure Public IP Quota e2e Failures

Use this path only when the failed jobs are exclusively
`pull-cloud-provider-azure-e2e-*` jobs and the failing logs show one of:

- `PublicIPCountLimitReached`
- `PublicIPPrefixCountLimitReached`

Confirm the failure text from the job logs before retesting. If any non-e2e job
also failed, handle that failure separately before blanket retesting.

Retest options:

```bash
gh pr comment <pr> --body-file - <<'EOF'
/test <job-name>

Reason: rerunning this failed e2e job because its current Prow log contains <PublicIPCountLimitReached or PublicIPPrefixCountLimitReached> and no non-e2e failure remains.
EOF
```

Use one `/test <job-name>` comment per failed e2e job when you want a narrow
rerun. Put `/test <job-name>` on the first line, then include the quota marker
found in that job's current Prow log.

Use `/retest` only when all current failed jobs are safe to rerun. Put `/retest`
on the first line, then say every current failure was confirmed as an e2e
public-IP quota flake and no non-e2e failure remains:

```bash
gh pr comment <pr> --body-file - <<'EOF'
/retest

Reason: rerunning all failed jobs because every current failure is an e2e public-IP quota flake confirmed in Prow logs, and no non-e2e failure remains.
EOF
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
- `dependency-review`, vulnerability, or license failures where the resolution
  may require accepting risk, excluding a finding, or changing the dependency
  version
- The PR is labeled `needs-rebase` or `mergeable` reports `CONFLICTING`

For dependency, toolchain, and policy failures, collect the failing job names,
the smallest representative log snippets, current dependency versions, and a
proposed narrow fix. Ask before changing linter policy, broadening a dependency
bump, or editing generated Dependabot PR metadata.

If the PR still needs a rebase after other common blockers have been fixed,
ask Dependabot to rebase the branch instead of manually rewriting the generated
PR branch. Put `@dependabot rebase` on the first line, then say the local
blockers are resolved and the remaining blocker is rebase state:

```bash
gh pr comment <pr> --body-file - <<'EOF'
@dependabot rebase

Reason: local blockers are resolved, but the PR still needs rebase before Tide can merge it.
EOF
```

Do this after local fixes and pushes are complete so Dependabot rebases the
latest branch state.

## PR Update Rules

- Preserve the generated Dependabot PR body. If a manual compatibility fix was
  pushed, append a concise reviewer-facing note instead of replacing the body;
  include why the note is needed and the smallest useful evidence, such as the
  compatibility issue, commit SHA, changed files, and validation result.
- Use specific staging commands, never `git add .`.
- Push only the current task's files.
- Run the Kubernetes minor version guard before inspecting CI, module sync,
  retest comments, `/lgtm` comments, or no-action reports for Dependabot Go
  module PRs.
- If the Kubernetes minor version guard fails, comment `/close` and stop.
- Use `/lgtm` after a successful module-sync push when the PR is otherwise ready
  for review.
- Use `/lgtm` when no checks are failing, `tide` is the only pending status,
  and the PR does not already have the `lgtm` label.
- If the PR needs rebase after other blockers are resolved, comment
  `@dependabot rebase` rather than manually force-pushing a rebase.
- Use `/retest` only for failures already classified as transient or safe to
  rerun.
- Report pending jobs, `tide` status, and any residual risk clearly instead of
  claiming the PR is green before CI finishes.
