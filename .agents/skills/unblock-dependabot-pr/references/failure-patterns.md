# Dependabot Failure Pattern Catalog

This catalog is the pluggable pattern list for the
[`unblock-dependabot-pr`](../SKILL.md) engine. The engine never hard-codes a
pattern; it walks the table below by the staged algorithm in `SKILL.md`. Adding
a newly discovered failure pattern is a single-file edit here: append a row to
the table and add a matching `## Details: <name>` subsection.

Details subsections are **normative** — read the linked Details before matching
or acting on a row.

## Row schema

| Column | Purpose |
|--------|---------|
| **Priority** | Gap-numbered (10, 20, 30…). Within a stage, the engine acts on matched rows low→high. New patterns slot into gaps without renumbering. |
| **Stage** | `guard` (metadata/diff/comment-history only) or `act` (CI/log/artifact-based action). Classification is an engine gate between these stages, not a catalog row. |
| **Pattern** | Short human name. |
| **Signal** | Compact routing hint: the diff marker, failing job name, or log fingerprint. A guard signal must be computable before CI/log I/O. |
| **Autonomy** | `close` / `auto-fix` / `escalate` / `bot-rebase`. Governs whether the agent acts alone (see Autonomy values below). |
| **Stop** | `yes` = short-circuit; end triage after this row is handled. |
| **Details** | Markdown anchor link to the pattern's `## Details: <name>` subsection in the same file. Details are **normative** and contain the full match criteria, preconditions, exclusions, and action. |

## Autonomy values

- `close` — comment `/close` and stop (disallowed change).
- `auto-fix` — the agent may act alone (push, `/lgtm`, `/test <job-name>`,
  `gh run rerun`)
  because the fix is deterministic and low-risk.
- `escalate` — the agent makes no automated change: it posts no PR comment, runs
  no checks, syncs no modules, and does not touch code, CI policy, or dependency
  versions. It stops working the PR and reports it as needing human review in its
  final output. Used both as a guard when the automated retry budget is spent
  (Retry budget exhausted) and as an act-stage flag when a blocker needs a
  human policy or toolchain decision (Toolchain / SDK / policy).
- `bot-rebase` — post a bot directive (`@dependabot rebase`) that regenerates
  the branch, then stop; distinct from `escalate` because it directs another
  bot to regenerate the branch rather than handing the PR to a human reviewer.
  The needs-rebase row uses this as a guard: a conflicting branch is detected
  from metadata and handed to Dependabot before any CI/log I/O, since a rebase
  invalidates a stale CI run anyway.

## Catalog

| Pri | Stage | Pattern | Signal | Autonomy | Stop | Details |
|-----|-------|---------|--------|----------|------|---------|
| 10 | guard | K8s minor-version bump | `k8s.io/*` `go.mod` minor-family change | close | yes | [Details: K8s minor-version guard](#details-k8s-minor-version-guard) |
| 15 | guard | Needs rebase | `needs-rebase` label or `mergeable` = CONFLICTING | bot-rebase | yes | [Details: Needs rebase](#details-needs-rebase) |
| 17 | guard | Retry budget exhausted | Highest `Unblock attempt: N` is `>= 3` | escalate | yes | [Details: Retry budget exhausted](#details-retry-budget-exhausted) |
| 20 | act | go-mod-consistency failed | `go-mod-consistency` failed | auto-fix | no | [Details: go-mod-consistency](#details-go-mod-consistency) |
| 30 | act | Public-IP quota e2e flake | Public-IP quota marker in e2e log | auto-fix | no | [Details: Public-IP quota e2e](#details-public-ip-quota-e2e) |
| 35 | act | Image-build registry flake e2e | Registry 5xx during pre-test image build | auto-fix | no | [Details: Image-build registry flake](#details-image-build-registry-flake) |
| 37 | act | Cluster-provisioning node-readiness timeout e2e | Node readiness timeout during pre-test cluster provisioning | auto-fix | no | [Details: Cluster-provisioning node-readiness timeout](#details-cluster-provisioning-node-readiness-timeout) |
| 39 | act | Prow pod scheduling timeout e2e | Prow job pod never schedules | auto-fix | no | [Details: Prow pod scheduling timeout](#details-prow-pod-scheduling-timeout) |
| 40 | act | Only Tide pending | No failed checks; only `tide` pending | auto-fix | no | [Details: Only Tide pending](#details-only-tide-pending) |
| 45 | act | GitHub Actions transient failure | Failed GitHub Actions `CheckRun` with runner/service transient evidence | auto-fix | no | [Details: GitHub Actions transient failure](#details-github-actions-transient-failure) |
| 47 | act | Deterministic test / spec failure | Current logs name a failing test, spec, or assertion after test execution started | escalate | yes | [Details: Deterministic test / spec failure](#details-deterministic-test--spec-failure) |
| 50 | act | Toolchain / SDK / policy blocker | Toolchain, typecheck, SDK-major, or dependency-policy blocker | escalate | yes | [Details: Toolchain / SDK / policy](#details-toolchain--sdk--policy) |

The **Details** cell links to the pattern's `## Details: <name>` subsection
below (a GitHub-style slug of the heading). Appending a pattern adds a row plus
a subsection and points the row at the new slug; no existing links move. Keep
the table short: put full match criteria, preconditions, exclusions, and action
steps in Details, not in routing columns.

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
within the same minor family, continue with the matching workflow below.

## Details: go-mod-consistency

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

5. After the push succeeds, post `/lgtm` following the shared
   [Post-push /lgtm](#details-post-push-lgtm) rule. Fill its Reason with the
   go-mod-consistency context: the pushed commit, the changed
   `go.mod`/`go.sum`/vendor files, and the `sync-go-modules` check that passed.

## Details: Post-push /lgtm

Shared rule for any pattern whose action pushes a commit to the PR branch
(today: [go-mod-consistency](#details-go-mod-consistency)). Link here from a new
push-based pattern instead of copying the `/lgtm` recipe.

Readiness gate — post `/lgtm` only when the push leaves the PR otherwise ready
for review:

- Every other failing required job this triage is already resolved or maps to a
  matched row whose action has been taken.
- No `escalate` blocker (e.g. the Pri 50
  [Toolchain / SDK / policy](#details-toolchain--sdk--policy) row) matched this
  triage. If one did, the PR needs human review and must not be approved — a
  push that fixes one job must not `/lgtm` a PR that still needs a policy or
  toolchain decision.

When the gate holds, put `/lgtm` on the first line so Prow can parse it, then
give a Reason naming the pushed commit, the changed files, and the validation
that passed. Because this action pushes a commit and leaves the PR for another
CI round, it is a non-final action: stamp it with this round's attempt number per
the [Attempt stamp](#details-attempt-stamp) rule (`Unblock attempt: <N+1>`,
using the same `N + 1` as any other non-final comment this round):

```bash
gh pr comment <pr> --body-file - <<'EOF'
/lgtm

Reason: pushed a fix for this Dependabot PR and the PR is otherwise ready for review.
Commit: <sha>
Files: <files changed by the push>
Validation: <check command> passed.
Unblock attempt: <N+1>
EOF
```

The push reruns CI. Do not add a `/test <job-name>` comment just for the old
failed run after pushing a new commit; the push-triggered rerun supersedes it.

## Details: Attempt stamp

Shared rule for any act-stage non-final action — a `Stop=no` row whose action
posts a comment, pushes, or triggers a CI rerun that leaves the PR for another
CI round rather than terminating triage. Today that is
[go-mod-consistency](#details-go-mod-consistency) (push + `/lgtm`),
the [Shared e2e flake rerun](#details-shared-e2e-flake-rerun) rule used by
[Public-IP quota e2e](#details-public-ip-quota-e2e),
[Image-build registry flake](#details-image-build-registry-flake),
[Cluster-provisioning node-readiness timeout](#details-cluster-provisioning-node-readiness-timeout),
and [Prow pod scheduling timeout](#details-prow-pod-scheduling-timeout)
(each `/test <job-name>`), and
[GitHub Actions transient failure](#details-github-actions-transient-failure)
(`gh run rerun`). Link here from a new non-final pattern instead of copying the
stamp recipe.

The skill keeps no state between runs, so the attempt count lives in the PR's
own comment history. Count triage **rounds**, not comments: one triage may push
a module-sync fix and rerun three e2e jobs, but that is a single attempt.

Compute the next attempt number once, at the start of the act stage, before posting
any non-final comment this round:

```bash
# Highest existing "Unblock attempt: N" stamp across all PR comments; 0 if none.
gh pr view <pr> --json comments \
  --jq '[.comments[].body | capture("Unblock attempt: (?<n>[0-9]+)"; "g").n | tonumber] | max // 0'
```

Let `N` be that maximum. This round's attempt number is `N + 1`. Use the **same**
`N + 1` for every non-final comment posted this round, so a round that pushes a
fix and reruns three jobs stamps `Unblock attempt: <N+1>` on all four comments
rather than incrementing per comment.

Add the stamp as its own line in the `Reason` block of each non-final comment,
after the existing Reason text. GitHub Actions reruns are triggered by API rather
than by PR slash command; after the rerun succeeds, record the attempt with a
plain informational comment that has no slash command.

```
Unblock attempt: <N+1>
```

The stamp is what the guard-stage [Retry budget exhausted](#details-retry-budget-exhausted)
row reads on the next run. Terminal actions do not stamp: `/close`
(K8s guard), `@dependabot rebase` (needs-rebase), the `escalate`
[Toolchain / SDK / policy](#details-toolchain--sdk--policy) and
[Retry budget exhausted](#details-retry-budget-exhausted) handoffs (which post no
comment at all), and the [Only Tide pending](#details-only-tide-pending) `/lgtm`
carry no `Unblock attempt:` line, because none of them hand the PR to another
automated retry round.

## Details: Shared e2e flake rerun

Shared action for act-stage e2e rows that are classified as safe transient
failures. Pattern-specific Details must supply the fingerprint evidence and any
extra exclusions before using this rule.

Before rerunning:

- The failed jobs are exclusively `pull-*-e2e-*`; no non-e2e failure remains.
- Each job being rerun has the pattern-specific fingerprint in current Prow
  evidence: build log, `prowjob.json`, `podinfo.json`, or another listed
  artifact.
- The pattern-specific exclusions do not apply.
- A push in this triage has not already rerun the same job; prefer the
  push-triggered CI rerun when there is one.

Rerun each failed e2e job with its own `/test <job-name>` comment. Put
`/test <job-name>` on the first line, then include the fingerprint evidence from
that job's current Prow artifacts:

```bash
gh pr comment <pr> --body-file - <<'EOF'
/test <job-name>

Reason: rerunning this failed e2e job because its current Prow artifacts show <pattern-specific evidence> and no non-flake failure remains.
Unblock attempt: <N+1>
EOF
```

Post one such comment per failed e2e job. Do not use `/retest`; rerun each
failed job by name so a still-broken required job is never blanket-rerun.

## Details: Public-IP quota e2e

Use this path only when the failed jobs are exclusively
`pull-cloud-provider-azure-e2e-*` jobs and the failing logs show one of:

- `PublicIPCountLimitReached`
- `PublicIPPrefixCountLimitReached`

Confirm the failure text from each job's current Prow log before retesting. If
the row matches, follow [Shared e2e flake rerun](#details-shared-e2e-flake-rerun)
and fill the evidence with the quota marker found in that job's log.

## Details: Image-build registry flake

Use this path only when the failed jobs are exclusively
`pull-cloud-provider-azure-e2e-*` jobs and the failure happened in the
pre-test image-build phase — before any Ginkgo spec ran — because a container
registry returned a transient 5xx while BuildKit resolved a frontend or pulled
a base image. The canonical fingerprint is a `502 Bad Gateway` from
`registry-1.docker.io` while resolving the `docker/dockerfile` frontend, ending
the build with a non-zero `make` exit (for example `make ... Error 2`,
`EXIT_VALUE=2`) and no test output.

Confirm from the job log before retesting:

- The 5xx / registry error appears during image build (BuildKit, `docker build`,
  or `make ... image`), not inside a running test.
- No Ginkgo spec started — there is no `Running Suite` / `[It]` / spec summary,
  so the failure cannot be a real test regression.

If any Ginkgo spec ran and failed, or any non-e2e job failed, handle that
failure separately instead of retesting.

If the row matches, follow [Shared e2e flake rerun](#details-shared-e2e-flake-rerun)
and fill the evidence with the registry 5xx plus the image-build phase marker.

## Details: Cluster-provisioning node-readiness timeout

Use this path only when the failed jobs are exclusively
`pull-cloud-provider-azure-e2e-*` jobs and the failure is a CAPZ
cluster-provisioning timeout — the harness gave up waiting for workload nodes to
become Ready before any test ran. The canonical fingerprint is one or more
`timed out waiting for the condition on nodes/<node-name>` lines during cluster
bring-up, with the run ending in `EXIT_VALUE=124` (the `timeout` wrapper killed
the wait) and no test output.

Confirm from the job log before retesting:

- One or more `timed out waiting for the condition on nodes/...` lines appear
  during cluster provisioning (after `kubectl wait --for=condition=Ready`), not
  inside a running test.
- The run ends with `EXIT_VALUE=124` (or an equivalent `timeout`-driven
  non-zero exit), which marks a watchdog timeout rather than a test assertion.
- No Ginkgo spec started — there is no `Running Suite` / `[It]` / spec summary,
  so the failure cannot be a real test regression.

If any Ginkgo spec ran and failed, or any non-e2e job failed, handle that
failure separately instead of retesting.

If the row matches, follow [Shared e2e flake rerun](#details-shared-e2e-flake-rerun)
and fill the evidence with the node-timeout marker, `EXIT_VALUE=124`, and the
fact that no test ran.

## Details: Prow pod scheduling timeout

Use this path only when a failed `pull-*-e2e-*` job never starts because Prow
cannot schedule the job pod. This is a Prow infrastructure/capacity failure, not
a cloud-provider-azure test failure.

Confirm from the current Prow artifacts before retesting:

- `prowjob.json` has `status.state` = `error` and `status.description` =
  `Pod scheduling timeout.`
- The artifact root has no `build-log.txt`, or the build log never reaches the
  job entrypoint or any Ginkgo output.
- `finished.json` has `result` = `error`.
- `podinfo.json` shows the job pod stayed `Pending`, with `PodScheduled=False`
  and `reason=Unschedulable`, or `FailedScheduling` events such as
  `Insufficient cpu`, `Insufficient memory`, `No preemption victims`, or
  `all available instance types exceed limits`.

If the job pod starts and the build log reaches cluster provisioning or test
execution, use a more specific pattern instead. If any non-e2e job failed,
handle that failure separately instead of retesting.

If the row matches, follow [Shared e2e flake rerun](#details-shared-e2e-flake-rerun)
and fill the evidence with `Pod scheduling timeout.` plus the `podinfo.json`
Unschedulable / FailedScheduling capacity marker.

## Details: Only Tide pending

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

## Details: GitHub Actions transient failure

Use this path only for failed GitHub Actions `CheckRun` entries, not Prow
`StatusContext` jobs. The failure must be retryable infrastructure or GitHub
Actions service noise, not a deterministic repository command failure.

Confirm from `gh pr view --json statusCheckRollup`, `gh run view`, and the
current GitHub Actions logs before rerunning:

- The failed item is a GitHub Actions `CheckRun` with a `detailsUrl` under
  `https://github.com/.../actions/runs/...`.
- The failed job log shows runner, service, download, cache, or network evidence
  that is outside the repository's code path, such as a GitHub Actions service
  error, runner shutdown, action download failure, cache service 5xx, or
  transient network failure before the checked command produced a meaningful
  project error.
- No failed step contains a deterministic compile, test, lint, module, license,
  vulnerability, or policy failure. Those must use a more specific row or the
  [Toolchain / SDK / policy](#details-toolchain--sdk--policy) escalation row.
- Every other failing required job either matches this row or another act row
  whose action has been taken. Do not rerun one GitHub Actions job while leaving
  another failing required job unclassified.

Rerun through GitHub Actions, not through a PR slash command:

```bash
# Rerun every failed job in the workflow run when all failed jobs are retryable.
gh run rerun <run-id> --failed
```

If only one failed job in the workflow is retryable, rerun that specific job.
Use the job's `databaseId`, not the numeric job id from the browser URL:

```bash
gh run view <run-id> --json jobs --jq '.jobs[] | {name, databaseId, conclusion}'
gh run rerun <run-id> --job <databaseId>
```

Do not comment `/retest`, `/test <job-name>`, or any other Prow directive for a
GitHub Actions check. Record the retry-budget attempt with a plain informational
comment after `gh run rerun` succeeds:

```bash
gh pr comment <pr> --body-file - <<'EOF'
Reason: reran GitHub Actions check <check-name> because its current logs show <transient evidence>.
Run: <run-url>
Unblock attempt: <N+1>
EOF
```

Report the rerun command, workflow run id, and target job(s) in the final output.

## Details: Deterministic test / spec failure

Use this path when a failed required job reached real test execution and the
current logs show a named failing test, spec, or assertion that does not match
an earlier safe auto-fix row.

Typical evidence includes:

- Ginkgo or test-suite summaries such as `Summarizing 1 Failure:`, `[FAIL]`,
  `FAIL! --`, `--- FAIL:`, or `Unexpected error:` with a concrete spec name,
  test name, or source file/line
- JUnit or test output that identifies a specific failing case rather than
  infrastructure noise
- Prow or GitHub Actions logs showing tests actually ran (`Running Suite`,
  `Ran <n> of <m> Specs`, individual test names, or equivalent)

Do not use this path when an earlier catalog row already matched, such as a
quota flake, pre-test image-build registry 5xx, cluster-provisioning node
readiness timeout, pod scheduling timeout, or transient GitHub Actions runner
failure. Those rows are the only safe retry paths.

When this row matches, make no automated change: do not rerun the job, do not
comment `/test`, do not push code, and do not `/lgtm`. Stop working the PR and
report it as needing human review in the final output, naming the failing job(s)
and the concrete failing test/spec evidence so a reviewer knows where to look.

## Details: Toolchain / SDK / policy

These blockers need a human policy, toolchain, or dependency-version decision the
agent must not make on its own:

- `golangci-lint` or `Analyze (go)` fails with a Go toolchain mismatch, such as
  `panic: file requires newer Go version ... (application built with ...)`
- `golangci-lint` `typecheck` failures after an Azure SDK or Kubernetes module
  bump
- Mixed Azure SDK major versions, such as `armcompute/v6` consumers in a PR
  that updated packages to `armcompute/v7`
- GitHub Actions failures where the failed step reached a deterministic
  repository command error instead of matching
  [GitHub Actions transient failure](#details-github-actions-transient-failure)
- `dependency-review`, vulnerability, or license failures where the resolution
  may require accepting risk, excluding a finding, or changing the dependency
  version

When one of these matches, make no automated change: do not push code, do not
change linter policy, do not broaden a dependency bump, do not edit generated
Dependabot PR metadata, and do not `/lgtm`. Stop working the PR and report it as
needing human review in the final output, naming the failing job(s) and the
blocker type so a reviewer knows where to look.

## Details: Needs rebase

Evaluate this guard from PR metadata right after the K8s minor-version guard,
before reading CI status or any Prow log. If the PR carries the `needs-rebase`
label or `gh pr view` reports `mergeable` = `CONFLICTING`, the branch is out of
date and `@dependabot rebase` will regenerate it — so classifying CI, syncing
modules, or pushing a local fix first would be wasted work against a stale
branch.

Ask Dependabot to rebase the branch instead of manually rewriting the generated
PR branch. Put `@dependabot rebase` on the first line, then say the branch is in
a needs-rebase / conflicting state and must be rebased before Tide can merge it:

```bash
gh pr comment <pr> --body-file - <<'EOF'
@dependabot rebase

Reason: the PR is in a needs-rebase or conflicting state and must be rebased before Tide can merge it.
EOF
```

After posting the comment, stop triage for this PR. Do not inspect CI, sync
modules, retest, comment `/lgtm`, or report no-action; Dependabot will push a
rebased branch and a fresh CI run to triage next time.

## Details: Retry budget exhausted

Evaluate this guard from PR comment metadata as a guard-stage step, right after the
[Needs rebase](#details-needs-rebase) guard and before reading CI status or any
Prow log. Its purpose is to cap automated churn: once this skill has already
tried to unblock a PR three times without success, a fourth automated attempt is
unlikely to help, so the PR goes to human reviewers instead of burning more
CI on the same failing jobs.

The count comes from the `Unblock attempt: N` stamps that non-final actions
leave on the PR (see [Attempt stamp](#details-attempt-stamp)). Read the highest
stamp already present:

```bash
gh pr view <pr> --json comments \
  --jq '[.comments[].body | capture("Unblock attempt: (?<n>[0-9]+)"; "g").n | tonumber] | max // 0'
```

Let `N` be that maximum. The budget is three attempts, so:

- `N < 3` — budget remains. Do not escalate; continue triage. A new non-final
  action this run will stamp `Unblock attempt: <N+1>` per the
  [Attempt stamp](#details-attempt-stamp) rule.
- `N >= 3` — the budget is spent (a fourth attempt would exceed three). Stop
  working the PR: make no automated change and report it as needing human review
  in the final output.

Escalation makes no change to the PR — no comment, no checks, no module sync, no
push, no `/lgtm`. Because it posts nothing, it leaves no `Unblock attempt:` stamp
and is safe to re-evaluate on every later run: once `N >= 3` the guard simply
keeps reporting the PR as needing human review until a human resolves it. Report
the PR in the final output with its remaining failing jobs so a reviewer knows
where to look.
