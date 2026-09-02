# Dependabot Guard Patterns

This reference contains the normative guard details for the
[`unblock-dependabot-pr`](../SKILL.md) engine's
[failure pattern catalog](failure-patterns.md). Evaluate these sections in
ascending catalog priority before any CI or log I/O. Read this file at most once
per triage.

## Details: K8s minor-version guard

Run this guard for every Dependabot Go module PR before inspecting CI or taking
an unblock action. The goal is to close Kubernetes minor-version bumps early
instead of spending automation time on checks that should not unblock the PR.

Inspect the PR diff for `go.mod` changes to Kubernetes modules:

```bash
gh pr diff <pr> --patch | rg '^[+-][[:space:]]+(require[[:space:]]+)?(k8s\.io/[[:alnum:]_.\/-]+)[[:space:]]+v0\.[0-9]+\.[0-9]+' || true
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
within the same minor family, this guard does not match; continue with the
remaining guard rows in ascending catalog priority.

## Details: Retry budget exhausted

Evaluate this guard from PR comment metadata as a guard-stage step immediately
after the K8s minor-version guard and before the
[Needs rebase](#details-needs-rebase) guard or any CI/log I/O. Its purpose is to
cap automated churn: once this skill has already tried to unblock a PR three
times without success, a fourth automated attempt is unlikely to help, so the
PR goes to human reviewers instead of burning more CI on the same failing jobs.

The count comes from the one `Unblock attempt: N` stamp that each completed
retry-budgeted triage leaves on the PR: the rebase directive carries its own
stamp, while budgeted act-stage actions use one summary (see
[Attempt stamp](shared-actions.md#details-attempt-stamp)). Public-IP quota
reruns do not write this stamp. Read the highest stamp already present:

```bash
gh pr view <pr> --json comments \
  --jq '[.comments[].body | capture("Unblock attempt: (?<n>[0-9]+)"; "g").n | tonumber] | max // 0'
```

Let `N` be that maximum. The budget is three attempts, so:

- `N < 3` — budget remains. Do not escalate; continue triage. If the
  [Needs rebase](#details-needs-rebase) guard matches, its directive carries
  `Unblock attempt: <N+1>`; otherwise, if the act stage completes one or more
  retry-budgeted non-final actions, post one summary with that stamp per the
  [Attempt stamp](shared-actions.md#details-attempt-stamp) rule. Quota-only
  reruns post no summary and leave `N` unchanged.
- `N >= 3` — the budget is spent (a fourth attempt would exceed three). Stop
  working the PR: make no automated change, including no rebase directive, and
  report it as needing human review in the final output.

Escalation makes no change to the PR — no comment, no checks, no module sync, no
push, no `/lgtm`. Because it posts nothing, it leaves no `Unblock attempt:` stamp
and is safe to re-evaluate on every later run: once `N >= 3` the guard simply
keeps reporting the PR as needing human review until a human resolves it. Report
the PR in the final output with the retry-budget blocker and current attempt
count. Include failing jobs only when they are already known from the caller's
input or guard-stage metadata; do not fetch CI status or logs solely to populate
this report.

## Details: Needs rebase

Evaluate this guard from PR metadata after the
[Retry budget exhausted](#details-retry-budget-exhausted) guard and before
reading CI status or any Prow log. If the PR carries the `needs-rebase` label or
`gh pr view` reports `mergeable` = `CONFLICTING`, the branch is out of date and
`@dependabot rebase` will regenerate it — so classifying CI, syncing modules, or
pushing a local fix first would be wasted work against a stale branch.

Reuse `N`, the highest attempt stamp read by the retry-budget guard. Ask
Dependabot to rebase the branch instead of manually rewriting the generated PR
branch. Follow the guard-stage rebase form in the shared
[Attempt stamp](shared-actions.md#details-attempt-stamp) rule so the directive
and its `N + 1` accounting remain atomic.

After posting the comment, stop triage for this PR. Do not inspect CI, sync
modules, retest, comment `/lgtm`, or report no-action; Dependabot will push a
rebased branch and a fresh CI run to triage next time. The rebase directive is
one automated unblock attempt; do not post a separate attempt-summary comment.
