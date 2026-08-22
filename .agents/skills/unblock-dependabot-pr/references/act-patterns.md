# Dependabot Act Patterns

This reference contains the normative act-stage details for the
[`unblock-dependabot-pr`](../SKILL.md) engine's
[failure pattern catalog](failure-patterns.md). Read it only after no guard Stop
fires and triage reaches the act stage. Process these sections in ascending
catalog priority, and read this file at most once per triage.

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
   [Post-push /lgtm](shared-actions.md#details-post-push-lgtm) rule. Fill its
   Reason with the go-mod-consistency context: the pushed commit, the changed
   `go.mod`/`go.sum`/vendor files, and the `sync-go-modules` check that passed.

## Details: Public-IP quota e2e

Use this path only for a failed `pull-cloud-provider-azure-e2e-*` job whose log
shows one of:

- `PublicIPCountLimitReached`
- `PublicIPPrefixCountLimitReached`
- `IPv4StandardSkuPublicIpCountLimitReached`

Confirm the failure text from the job's current Prow log before retesting. If
the row matches, follow
[Shared e2e flake rerun](shared-actions.md#details-shared-e2e-flake-rerun) and
fill the evidence with the quota marker found in that job's log.

Apply the public-IP quota accounting exception in the shared
[Attempt stamp](shared-actions.md#details-attempt-stamp) rule. This rerun does
not create or increment an `Unblock attempt` stamp.

## Details: Image-build registry flake

Use this path only for a failed `pull-cloud-provider-azure-e2e-*` job whose
failure happened in the pre-test image-build phase — before any Ginkgo spec ran
— because a container registry returned a transient 5xx while BuildKit resolved
a frontend or pulled a base image. The canonical fingerprint is a `502 Bad Gateway` from
`registry-1.docker.io` while resolving the `docker/dockerfile` frontend, ending
the build with a non-zero `make` exit (for example `make ... Error 2`,
`EXIT_VALUE=2`) and no test output.

Confirm from the job log before retesting:

- The 5xx / registry error appears during image build (BuildKit, `docker build`,
  or `make ... image`), not inside a running test.
- No Ginkgo spec started — there is no `Running Suite` / `[It]` / spec summary,
  so the failure cannot be a real test regression.

If any Ginkgo spec ran and failed, handle that failure separately instead of
retesting. Continue the per-failure loop for other failed required jobs.

If the row matches, follow
[Shared e2e flake rerun](shared-actions.md#details-shared-e2e-flake-rerun) and
fill the evidence with the registry 5xx plus the image-build phase marker.

## Details: Cluster-provisioning node-readiness timeout

Use this path only for a failed `pull-cloud-provider-azure-e2e-*` job whose
failure is a CAPZ
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

If any Ginkgo spec ran and failed, handle that failure separately instead of
retesting. Continue the per-failure loop for other failed required jobs.

If the row matches, follow
[Shared e2e flake rerun](shared-actions.md#details-shared-e2e-flake-rerun) and
fill the evidence with the node-timeout marker, `EXIT_VALUE=124`, and the fact
that no test ran.

## Details: Prow job did not start

Use this path when a failed Prow job never reaches the job entrypoint because
Prow infrastructure or capacity prevented the job pod from starting. This covers
pod scheduling timeouts and similar pre-entrypoint failures. It is a Prow
infrastructure/capacity failure, not a cloud-provider-azure test failure.

Confirm from the current Prow artifacts before retesting:

- `prowjob.json` has `status.state` = `error` and a pre-entrypoint
  infrastructure description such as `Pod scheduling timeout.`
- The artifact root has no `build-log.txt`, or the build log never reaches the
  job entrypoint.
- `finished.json` has `result` = `error`.
- `podinfo.json` shows the job pod stayed `Pending`, with `PodScheduled=False`
  and `reason=Unschedulable`, or `FailedScheduling` events such as
  `Insufficient cpu`, `Insufficient memory`, `No preemption victims`, or
  `all available instance types exceed limits`.

If the job pod starts and the build log reaches cluster provisioning or test
execution, use a more specific pattern instead.

If the row matches, rerun that failed Prow job with its own `/test <job-name>`
comment. Put `/test <job-name>` on the first line, then include the artifact
evidence proving the job never reached its entrypoint:

```bash
gh pr comment <pr> --body-file - <<'EOF'
/test <job-name>

Reason: rerunning this failed Prow job because its current artifacts show <pre-entrypoint infrastructure evidence>.
EOF
```

Continue the per-failure loop for other failed required jobs. Do not use
`/retest`; rerun each failed Prow job by name so a still-broken required job is
never blanket-rerun. Record the rerun in the single end-of-triage
[Attempt stamp](shared-actions.md#details-attempt-stamp) summary.

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

Apply the Only Tide accounting exception in the shared
[Attempt stamp](shared-actions.md#details-attempt-stamp) rule.

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
- Continue the per-failure loop after the rerun. Do not report the PR as
  unblocked while another failed required job remains unexamined.

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
GitHub Actions check. After `gh run rerun` succeeds, include the rerun and its
current evidence in the single end-of-triage
[Attempt stamp](shared-actions.md#details-attempt-stamp) summary rather than
posting a separate retry comment.

Report the rerun command, workflow run id, and target job(s) in the final output.

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
