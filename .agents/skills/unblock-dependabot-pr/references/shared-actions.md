# Dependabot Shared Actions

This reference contains reusable actions linked by matched workflows in the
[`unblock-dependabot-pr`](../SKILL.md) engine's
[failure pattern catalog](failure-patterns.md). Read it only when a matched
guard or act detail links here, and at most once per triage.

## Details: Post-push /lgtm

Shared rule for any pattern whose action pushes a commit to the PR branch
(today: [go-mod-consistency](act-patterns.md#details-go-mod-consistency)). Link
here from a new push-based pattern instead of copying the `/lgtm` recipe.

Readiness gate — post `/lgtm` only when the push leaves the PR otherwise ready
for review:

- Every other failing required job this triage is already resolved or maps to a
  matched row whose action has been taken.
- No `escalate` blocker (e.g. the Pri 50
  [Toolchain / SDK / policy](act-patterns.md#details-toolchain--sdk--policy) row)
  matched this triage. If one did, the PR needs human review and must not be
  approved — a push that fixes one job must not `/lgtm` a PR that still needs a
  policy or toolchain decision.

When the gate holds, put `/lgtm` on the first line so Prow can parse it, then
give a Reason naming the pushed commit, the changed files, and the validation
that passed. Do not add an attempt marker here; record this push in the single
end-of-triage [Attempt stamp](#details-attempt-stamp) summary:

```bash
gh pr comment <pr> --body-file - <<'EOF'
/lgtm

Reason: pushed a fix for this Dependabot PR and the PR is otherwise ready for review.
Commit: <sha>
Files: <files changed by the push>
Validation: <check command> passed.
EOF
```

The push reruns CI. Do not add a `/test <job-name>` comment just for the old
failed run after pushing a new commit; the push-triggered rerun supersedes it.

## Details: Attempt stamp

Shared rule for a triage round that takes one or more retry-budgeted automated
unblock actions and leaves the PR for another automated round rather than
terminating it. This includes the guard-stage
[Needs rebase](guard-patterns.md#details-needs-rebase) directive, whose
`Stop=yes` ends the current triage, and budgeted act-stage `Stop=no` actions
that post a comment, push, or trigger a CI rerun. The budgeted act-stage actions
are [go-mod-consistency](act-patterns.md#details-go-mod-consistency) (push +
`/lgtm`), the [Shared e2e flake rerun](#details-shared-e2e-flake-rerun) rule
used by [Image-build registry flake](act-patterns.md#details-image-build-registry-flake)
and [Cluster-provisioning node-readiness timeout](act-patterns.md#details-cluster-provisioning-node-readiness-timeout)
(each `/test <job-name>`),
[Prow job did not start](act-patterns.md#details-prow-job-did-not-start)
(`/test <job-name>`), and
[GitHub Actions transient failure](act-patterns.md#details-github-actions-transient-failure)
(`gh run rerun`). Link here from a new non-final pattern instead of copying the
stamp recipe. [Public-IP quota e2e](act-patterns.md#details-public-ip-quota-e2e)
reruns are explicitly unbudgeted and do not create or increment an attempt
stamp.

The skill keeps no state between runs, so the attempt count lives in the PR's
own comment history. Count retry-budgeted triage **rounds**, not actions or
comments: a rebase directive is one attempt, while one act-stage triage may push
a module-sync fix and rerun three budgeted e2e jobs but is still one attempt
with one summary comment.

Compute the next attempt number once, before taking any retry-budgeted automated
unblock action this round:

```bash
# Highest existing "Unblock attempt: N" stamp across all PR comments; 0 if none.
gh pr view <pr> --json comments \
  --jq '[.comments[].body | capture("Unblock attempt: (?<n>[0-9]+)"; "g").n | tonumber] | max // 0'
```

Let `N` be that maximum. This round's attempt number is `N + 1`. For a
guard-stage rebase, ask Dependabot to rebase the branch instead of manually
rewriting the generated PR branch. Put `@dependabot rebase` on the first line,
explain why it is needed, and put `Unblock attempt: <N+1>` in the same comment
so the action and its accounting are atomic:

```bash
gh pr comment <pr> --body-file - <<'EOF'
@dependabot rebase

Reason: the PR is in a needs-rebase or conflicting state and must be rebased before Tide can merge it.
Unblock attempt: <N+1>
EOF
```

For act-stage actions, do not add the stamp to an individual `/test`, `/lgtm`,
or GitHub Actions action comment. After all retry-budgeted act-stage non-final
actions taken this triage have completed, post exactly one plain informational
comment that summarizes only those budgeted actions:

```bash
gh pr comment <pr> --body-file - <<'EOF'
Reason: completed this triage's automatic unblock actions:
- <action 1>
- <action 2>
Unblock attempt: <N+1>
EOF
```

Post no summary when the triage takes no retry-budgeted act-stage non-final
action. In particular, one or more
[Public-IP quota e2e](act-patterns.md#details-public-ip-quota-e2e) reruns alone
do not consume an attempt; in a mixed triage, summarize only the budgeted
actions. A rebase directive causes no separate summary because its comment
already carries the attempt stamp. Terminal actions do not cause a summary or
consume an attempt: `/close` (K8s guard), the `escalate`
[Toolchain / SDK / policy](act-patterns.md#details-toolchain--sdk--policy) and
[Retry budget exhausted](guard-patterns.md#details-retry-budget-exhausted)
handoffs, and the
[Only Tide pending](act-patterns.md#details-only-tide-pending) `/lgtm`.

## Details: Shared e2e flake rerun

Shared action for act-stage e2e rows that are classified as safe transient
failures. Pattern-specific Details must supply the fingerprint evidence and any
extra exclusions before using this rule.

Before rerunning:

- The job being rerun is a failed `pull-*-e2e-*` job.
- Each job being rerun has the pattern-specific fingerprint in current Prow
  evidence: build log, `prowjob.json`, `podinfo.json`, or another listed
  artifact.
- The pattern-specific exclusions do not apply.
- Other failed required jobs are still processed by the per-failure loop; this
  rerun is not a substitute for examining them.
- A push in this triage has not already rerun the same job; prefer the
  push-triggered CI rerun when there is one.

Rerun each matched failed e2e job with its own `/test <job-name>` comment. Put
`/test <job-name>` on the first line, then include the fingerprint evidence from
that job's current Prow artifacts:

```bash
gh pr comment <pr> --body-file - <<'EOF'
/test <job-name>

Reason: rerunning this failed e2e job because its current Prow artifacts show <pattern-specific evidence> and the pattern-specific exclusions do not apply.
EOF
```

Post one such comment per matched failed e2e job. Do not use `/retest`; rerun
each failed job by name so a still-broken required job is never blanket-rerun.
Record budgeted reruns from this triage in the single end-of-triage
[Attempt stamp](#details-attempt-stamp) summary. The linked act pattern may
declare an accounting exception, such as the public-IP quota rerun.
