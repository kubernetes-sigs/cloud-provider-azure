# Dependabot Failure Pattern Catalog

This catalog is the pluggable pattern list for the
[`unblock-dependabot-pr`](../SKILL.md) engine. The engine never hard-codes a
pattern; it walks the table below by the phased algorithm in `SKILL.md`. Adding
a newly discovered failure pattern is a single-file edit here: append a row to
the table and add a matching `## Details: <name>` subsection.

Details subsections are **normative** — read the linked Details before matching
or acting on a row.

## Row schema

| Column | Purpose |
|--------|---------|
| **Priority** | Gap-numbered (10, 20, 30…). Within a phase, the engine acts on matched rows low→high. New patterns slot into gaps without renumbering. |
| **Phase** | `0` (guard, metadata/diff only) / `1` (classify) / `2` (act). Binds the row to an engine phase so guard rows are resolved before any CI/log I/O. |
| **Pattern** | Short human name. |
| **Detection signal** | What to match — the diff grep, the failing job name, or the log marker. A Phase-0 row's signal must be computable from metadata and the `go.mod` diff alone. |
| **Preconditions / Exclusions** | Negative and dependency guards that must hold for the Action to be valid (e.g. "only e2e jobs failed", "no non-Tide check pending", "after other blockers resolved"). Never dropped into prose only. |
| **Autonomy** | `close` / `auto-fix` / `escalate` / `bot-rebase`. Governs whether the agent acts alone (see Autonomy values below). |
| **Action** | One-line summary of the fix or Prow comment. |
| **Stop** | `yes` = short-circuit; end triage after this row is handled. |
| **Details** | Markdown anchor link to the pattern's `## Details: <name>` subsection in the same file. Details are **normative** — the agent must read them before matching/acting. `—` only when the Action and Preconditions cells are complete on their own. |

## Autonomy values

- `close` — comment `/close` and stop (disallowed change).
- `auto-fix` — the agent may act alone (push, `/lgtm`, `/test <job-name>`)
  because the fix is deterministic and low-risk.
- `escalate` — the agent makes no automated change: it posts no PR comment, runs
  no checks, syncs no modules, and does not touch code, CI policy, or dependency
  versions. It stops working the PR and reports it as needing human review in its
  final output. Used both as a Phase-0 guard when the automated retry budget is
  spent (Retry budget exhausted) and as a Phase-2 flag when a blocker needs a
  human policy or toolchain decision (Toolchain / SDK / policy).
- `bot-rebase` — post a bot directive (`@dependabot rebase`) that regenerates
  the branch, then stop; distinct from `escalate` because it directs another
  bot to regenerate the branch rather than handing the PR to a human reviewer.
  The needs-rebase row uses this as a Phase-0 guard: a conflicting branch is
  detected from metadata and handed to Dependabot before any CI/log I/O, since a
  rebase invalidates a stale CI run anyway.

## Catalog

| Pri | Phase | Pattern | Detection signal | Preconditions / Exclusions | Autonomy | Action | Stop | Details |
|-----|-------|---------|------------------|----------------------------|----------|--------|------|---------|
| 10 | 0 | K8s minor-version bump | `go.mod` diff moves a `k8s.io/*` module across minor families (guard rules) | Computed from metadata + diff only, before any CI/log read | close | Comment `/close` + reason | yes | [Details: K8s minor-version guard](#details-k8s-minor-version-guard) |
| 15 | 0 | Needs rebase | `needs-rebase` label or `mergeable` = CONFLICTING | Computed from metadata only, before any CI/log read; evaluate after the K8s close-guard | bot-rebase | Comment `@dependabot rebase` | yes | [Details: Needs rebase](#details-needs-rebase) |
| 17 | 0 | Retry budget exhausted | Highest `Unblock attempt: N` stamp in PR comment history is `>= 3` (a 4th attempt would exceed the budget of 3) | Computed from comment metadata only, before any CI/log read; evaluate after the needs-rebase guard | escalate | Do nothing; report the PR as needing human review in the final output | yes | [Details: Retry budget exhausted](#details-retry-budget-exhausted) |
| 20 | 2 | go-mod-consistency failed | `go-mod-consistency` job failed | Clean tree, or dirty only because of the dependency bump; stage only sync output | auto-fix | Run `sync-go-modules`, push, `/lgtm` | no | [Details: go-mod-consistency](#details-go-mod-consistency) |
| 30 | 2 | Public-IP quota e2e flake | Failing log has `PublicIPCountLimitReached` / `PublicIPPrefixCountLimitReached` | Only `pull-*-e2e-*` failed; no non-e2e failure remains; skip if a push this triage already reruns the job | auto-fix | `/test <job>` per failed job | no | [Details: Public-IP quota e2e](#details-public-ip-quota-e2e) |
| 35 | 2 | Image-build registry flake e2e | Failing e2e log has a container-registry 5xx during image build before any test runs, e.g. `502 Bad Gateway` from `registry-1.docker.io` resolving the BuildKit `docker/dockerfile` frontend | Only `pull-*-e2e-*` failed and the failure is in the image-build phase (no Ginkgo spec ran); no non-flake failure remains; skip if a push this triage already reruns the job | auto-fix | `/test <job>` per failed job | no | [Details: Image-build registry flake](#details-image-build-registry-flake) |
| 37 | 2 | Cluster-provisioning node-readiness timeout e2e | Failing e2e log has `timed out waiting for the condition on nodes/<name>` during CAPZ cluster bring-up and ends with `EXIT_VALUE=124`, before any Ginkgo spec ran | Only `pull-*-e2e-*` failed and the timeout is in the cluster-provisioning phase (no Ginkgo spec ran); no non-flake failure remains; skip if a push this triage already reruns the job | auto-fix | `/test <job>` per failed job | no | [Details: Cluster-provisioning node-readiness timeout](#details-cluster-provisioning-node-readiness-timeout) |
| 40 | 2 | Only Tide pending | No failed checks; only `tide` pending | PR does not already have `lgtm` label; no non-Tide check pending or failed | auto-fix | `/lgtm` | no | [Details: Only Tide pending](#details-only-tide-pending) |
| 50 | 2 | Toolchain / SDK / policy blocker | Go-version panic, typecheck, mixed SDK majors, or dependency-review/license failure | Needs a human policy, toolchain, or dependency-version decision the agent must not make autonomously | escalate | Do nothing; report the PR as needing human review in the final output | no | [Details: Toolchain / SDK / policy](#details-toolchain--sdk--policy) |

The **Details** cell links to the pattern's `## Details: <name>` subsection
below (a GitHub-style slug of the heading), or is `—` when the Action cell is a
complete instruction. Appending a pattern adds a row plus a subsection and
points the row at the new slug; no existing links move.

## Details: K8s minor-version guard

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

Shared rule for any pattern whose Action pushes a commit to the PR branch
(today: [go-mod-consistency](#details-go-mod-consistency)). Link here from a new
push-based pattern instead of copying the `/lgtm` recipe.

Readiness gate — post `/lgtm` only when the push leaves the PR otherwise ready
for review:

- Every other failing required job this triage is already resolved or maps to a
  matched row whose Action has been taken.
- No `escalate` blocker (e.g. the Pri 50
  [Toolchain / SDK / policy](#details-toolchain--sdk--policy) row) matched this
  triage. If one did, the PR needs human review and must not be approved — a
  push that fixes one job must not `/lgtm` a PR that still needs a policy or
  toolchain decision.

When the gate holds, put `/lgtm` on the first line so Prow can parse it, then
give a Reason naming the pushed commit, the changed files, and the validation
that passed. Because this Action pushes a commit and leaves the PR for another
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

Shared rule for any Phase-2 non-final action — a `Stop=no` row whose Action
posts a comment or push that leaves the PR for another CI round rather than
terminating triage. Today that is [go-mod-consistency](#details-go-mod-consistency)
(push + `/lgtm`), [Public-IP quota e2e](#details-public-ip-quota-e2e),
[Image-build registry flake](#details-image-build-registry-flake), and
[Cluster-provisioning node-readiness timeout](#details-cluster-provisioning-node-readiness-timeout)
(each `/test <job-name>`). Link here from a new non-final pattern instead of
copying the stamp recipe.

The skill keeps no state between runs, so the attempt count lives in the PR's
own comment history. Count triage **rounds**, not comments: one triage may push
a module-sync fix and rerun three e2e jobs, but that is a single attempt.

Compute the next attempt number once, at the start of Phase 2, before posting
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
after the existing Reason text:

```
Unblock attempt: <N+1>
```

The stamp is what the Phase-0 [Retry budget exhausted](#details-retry-budget-exhausted)
guard reads on the next run. Terminal actions do not stamp: `/close`
(K8s guard), `@dependabot rebase` (needs-rebase), the `escalate`
[Toolchain / SDK / policy](#details-toolchain--sdk--policy) and
[Retry budget exhausted](#details-retry-budget-exhausted) handoffs (which post no
comment at all), and the [Only Tide pending](#details-only-tide-pending) `/lgtm`
carry no `Unblock attempt:` line, because none of them hand the PR to another
automated retry round.

## Details: Public-IP quota e2e

Use this path only when the failed jobs are exclusively
`pull-cloud-provider-azure-e2e-*` jobs and the failing logs show one of:

- `PublicIPCountLimitReached`
- `PublicIPPrefixCountLimitReached`

Confirm the failure text from the job logs before retesting. If any non-e2e job
also failed, handle that failure separately before blanket retesting.

Rerun each failed e2e job with its own `/test <job-name>` comment. Put
`/test <job-name>` on the first line, then include the quota marker found in that
job's current Prow log:

```bash
gh pr comment <pr> --body-file - <<'EOF'
/test <job-name>

Reason: rerunning this failed e2e job because its current Prow log contains <PublicIPCountLimitReached or PublicIPPrefixCountLimitReached> and no non-e2e failure remains.
Unblock attempt: <N+1>
EOF
```

Post one such comment per failed e2e job. Do not use `/retest`; rerun each
failed job by name so a still-broken required job is never blanket-rerun.

If you are already pushing a commit for another fix, prefer the push-triggered
CI rerun instead of adding a separate `/test` comment.

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

Rerun each failed e2e job with its own `/test <job-name>` comment. Put
`/test <job-name>` on the first line, then include the registry error and the
image-build phase marker found in that job's current Prow log:

```bash
gh pr comment <pr> --body-file - <<'EOF'
/test <job-name>

Reason: rerunning this failed e2e job because its current Prow log shows a transient container-registry 5xx during the image-build phase (before any test ran) and no non-flake failure remains.
Unblock attempt: <N+1>
EOF
```

Post one such comment per failed e2e job. Do not use `/retest`; rerun each
failed job by name so a still-broken required job is never blanket-rerun.

If you are already pushing a commit for another fix, prefer the push-triggered
CI rerun instead of adding a separate `/test` comment.

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

Rerun each failed e2e job with its own `/test <job-name>` comment. Put
`/test <job-name>` on the first line, then include the node-timeout marker and
the provisioning-phase evidence found in that job's current Prow log:

```bash
gh pr comment <pr> --body-file - <<'EOF'
/test <job-name>

Reason: rerunning this failed e2e job because its current Prow log shows a cluster-provisioning node-readiness timeout (`timed out waiting for the condition on nodes/...`, EXIT_VALUE=124, before any test ran) and no non-flake failure remains.
Unblock attempt: <N+1>
EOF
```

Post one such comment per failed e2e job. Do not use `/retest`; rerun each
failed job by name so a still-broken required job is never blanket-rerun.

If you are already pushing a commit for another fix, prefer the push-triggered
CI rerun instead of adding a separate `/test` comment.

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

## Details: Toolchain / SDK / policy

These blockers need a human policy, toolchain, or dependency-version decision the
agent must not make on its own:

- `golangci-lint` or `Analyze (go)` fails with a Go toolchain mismatch, such as
  `panic: file requires newer Go version ... (application built with ...)`
- `golangci-lint` `typecheck` failures after an Azure SDK or Kubernetes module
  bump
- Mixed Azure SDK major versions, such as `armcompute/v6` consumers in a PR
  that updated packages to `armcompute/v7`
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

Evaluate this guard from PR comment metadata as a Phase-0 step, right after the
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
