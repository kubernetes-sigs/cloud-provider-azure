# Dependabot Failure Pattern Catalog

This catalog is the pluggable pattern list for the
[`unblock-dependabot-pr`](../SKILL.md) engine. The engine never hard-codes a
pattern; it walks the table below by the staged algorithm in `SKILL.md`.
Normative match criteria, preconditions, exclusions, and actions live in the
linked guard or act pattern reference.

Adding a newly discovered failure pattern requires a catalog row plus a
matching `## Details: <name>` subsection in the stage-appropriate reference.
Details subsections are **normative** — read the linked Details before matching
or acting on a row.

## Row schema

| Column | Purpose |
|--------|---------|
| **Priority** | Gap-numbered (10, 20, 30…). Within a stage, the engine acts on matched rows low→high. New patterns slot into gaps without renumbering. |
| **Stage** | `guard` (metadata/diff/comment-history only) or `act` (CI/log/artifact-based action). After the guard stage, the engine processes failed required jobs one at a time instead of classifying every failure up front. |
| **Pattern** | Short human name. |
| **Signal** | Compact routing hint: the diff marker, failing job name, or log fingerprint. A guard signal must be computable before CI/log I/O. |
| **Autonomy** | `close` / `auto-fix` / `escalate` / `bot-rebase`. Governs whether the agent acts alone (see Autonomy values below). |
| **Stop** | `yes` = short-circuit; end triage after this row is handled. |
| **Details** | Link to the pattern's `## Details: <name>` subsection in the stage-appropriate reference. Details are **normative** and contain the full match criteria, preconditions, exclusions, and action. |

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
  invalidates a stale CI run anyway. The directive consumes one shared unblock
  attempt and carries its attempt stamp in the same comment.

## Catalog

| Pri | Stage | Pattern | Signal | Autonomy | Stop | Details |
|-----|-------|---------|--------|----------|------|---------|
| 10 | guard | K8s minor-version bump | `k8s.io/*` `go.mod` minor-family change | close | yes | [Details: K8s minor-version guard](guard-patterns.md#details-k8s-minor-version-guard) |
| 13 | guard | Retry budget exhausted | Highest `Unblock attempt: N` is `>= 3` | escalate | yes | [Details: Retry budget exhausted](guard-patterns.md#details-retry-budget-exhausted) |
| 15 | guard | Needs rebase | `needs-rebase` label or `mergeable` = CONFLICTING | bot-rebase | yes | [Details: Needs rebase](guard-patterns.md#details-needs-rebase) |
| 20 | act | go-mod-consistency failed | `go-mod-consistency` failed | auto-fix | no | [Details: go-mod-consistency](act-patterns.md#details-go-mod-consistency) |
| 30 | act | Public-IP quota e2e flake | Public-IP quota marker in e2e log | auto-fix | no | [Details: Public-IP quota e2e](act-patterns.md#details-public-ip-quota-e2e) |
| 35 | act | Image-build registry flake e2e | Registry 5xx during pre-test image build | auto-fix | no | [Details: Image-build registry flake](act-patterns.md#details-image-build-registry-flake) |
| 37 | act | Cluster-provisioning node-readiness timeout e2e | Node readiness timeout during pre-test cluster provisioning | auto-fix | no | [Details: Cluster-provisioning node-readiness timeout](act-patterns.md#details-cluster-provisioning-node-readiness-timeout) |
| 39 | act | Prow job did not start | Prow job never reaches entrypoint | auto-fix | no | [Details: Prow job did not start](act-patterns.md#details-prow-job-did-not-start) |
| 40 | act | Only Tide pending | No failed checks; only `tide` pending | auto-fix | no | [Details: Only Tide pending](act-patterns.md#details-only-tide-pending) |
| 45 | act | GitHub Actions transient failure | Failed GitHub Actions `CheckRun` with runner/service transient evidence | auto-fix | no | [Details: GitHub Actions transient failure](act-patterns.md#details-github-actions-transient-failure) |
| 50 | act | Toolchain / SDK / policy blocker | Toolchain, typecheck, SDK-major, or dependency-policy blocker | escalate | yes | [Details: Toolchain / SDK / policy](act-patterns.md#details-toolchain--sdk--policy) |

The **Details** cell links to the pattern's `## Details: <name>` subsection in
`guard-patterns.md` or `act-patterns.md` (a GitHub-style slug of the heading).
Appending a pattern adds a row plus a subsection in the appropriate file and
points the row at the new slug. Keep the table short: put full match criteria,
preconditions, exclusions, and action steps in Details, not in routing columns.
