# Dependabot Shared Actions

This reference contains reusable actions linked by matched workflows in the
[`unblock-dependabot-pr`](../SKILL.md) engine's
[failure pattern catalog](failure-patterns.md). Read it only when a matched
guard or act detail links here, and at most once per triage.

## Details: Post-push /lgtm

Shared rule for any pattern whose action pushes a commit to the PR branch
(today: [go-mod-consistency](act-patterns.md#details-go-mod-consistency)). Link
here from a new push-based pattern instead of copying the `/lgtm` recipe. The
pushing pattern records a pending approval and does not post `/lgtm` itself.
Evaluate this rule once, after the per-failure loop; skip it if a guard or act
row marked Stop fired.

Readiness gate — post `/lgtm` only when the push leaves the PR otherwise ready
for review:

- The push is a confirmed successful fix push whose validation passed.
- Every other failing required job known this triage, including any failure
  already reported for the pushed SHA, is already resolved or maps to a matched
  row whose action has been taken. A retest skipped because the push reruns the
  job counts only for a failure observed before the push. A job that matched no
  row, or whose evidence was unavailable, blocks approval even though the push
  reruns it. A failure reported for the pushed SHA, even of a check the push
  targeted, is never resolved by that same push.
- No `escalate` blocker (e.g. the Pri 50
  [Toolchain / SDK / policy](act-patterns.md#details-toolchain--sdk--policy) row)
  matched this triage. If one did, the PR needs human review and must not be
  approved — a push that fixes one job must not `/lgtm` a PR that still needs a
  policy or toolchain decision.
- Immediately before posting, a fresh read shows the PR head is the pushed SHA
  (`gh pr view <pr> --json headRefOid --jq .headRefOid`). If it differs, report
  that the head moved after the push.

A visible `lgtm` label does not skip this post: labels are PR-level and may
predate the push. The label check in
[Only Tide pending](act-patterns.md#details-only-tide-pending) applies only to
that path. CI started by the push may still be pending; do not wait for it, and
report it as pending rather than green. When the gate does not hold, post no
`/lgtm` and report which condition failed.

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
are [go-mod-consistency](act-patterns.md#details-go-mod-consistency) (verified
sync push), the [Shared e2e flake rerun](#details-shared-e2e-flake-rerun) rule
used by [Image-build registry flake](act-patterns.md#details-image-build-registry-flake)
and [Cluster-provisioning node-readiness timeout](act-patterns.md#details-cluster-provisioning-node-readiness-timeout)
(each `/test <job-name>`),
[Prow job did not start](act-patterns.md#details-prow-job-did-not-start)
(`/test <job-name>`), and
[GitHub Actions transient failure](act-patterns.md#details-github-actions-transient-failure)
(`gh run rerun`). Link here from a new non-final pattern instead of copying the
stamp recipe.

The skill keeps no state between runs, so the attempt count lives in the PR's
own comment history. Count retry-budgeted triage **rounds**, not actions or
comments: a rebase or recreate directive is one attempt, while one act-stage
triage may push a module-sync fix and rerun three budgeted e2e jobs but is still
one attempt with one summary comment.

Reuse `N`, the highest existing stamp read once by the
[Retry budget exhausted](guard-patterns.md#details-retry-budget-exhausted)
guard; do not query it again. This round's attempt number is `N + 1`. For a
guard-stage refresh, use the directive selected by
[Needs rebase](guard-patterns.md#details-needs-rebase) from commit history.
Put that directive on the first line, explain why it is needed, and put
`Unblock attempt: <N+1>` in the same comment so the action and its accounting
are atomic. Choose only one of these forms.

All commits from Dependabot:

```bash
gh pr comment <pr> --body-file - <<'EOF'
@dependabot rebase

Reason: the PR needs a refresh before Tide can merge it, and all commits are from Dependabot.
Unblock attempt: <N+1>
EOF
```

Manual edits present:

```bash
gh pr comment <pr> --body-file - <<'EOF'
@dependabot recreate

Reason: the PR needs a refresh and contains manual edits, so Dependabot cannot rebase it. Recreate it from scratch, discarding those edits.
Manual commits: <commit SHAs and authors/committers establishing manual edits>
Unblock attempt: <N+1>
EOF
```

For act-stage actions, do not add the stamp to an individual `/test`, `/lgtm`,
or GitHub Actions action comment. After all retry-budgeted act-stage non-final
actions taken this triage have completed, and after the
[Post-push /lgtm](#details-post-push-lgtm) gate was evaluated or skipped, post
exactly one plain informational comment that summarizes only those budgeted
actions. Post it even when the gate withholds `/lgtm` or an act-stage
`escalate` Stop fired after a budgeted action had already completed:

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
actions. A rebase or recreate directive causes no separate summary because its
comment already carries the attempt stamp. Terminal actions do not cause a
summary or consume an attempt by themselves: `/close` (K8s guard), the `escalate`
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
