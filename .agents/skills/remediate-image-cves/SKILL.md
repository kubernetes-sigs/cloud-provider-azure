---
name: remediate-image-cves
description: Orchestrate end-to-end CVE remediation for the Linux CCM, CNM, and health-probe-proxy images on cloud-provider-azure master or a release-X.Y branch, including builds, repeated Trivy verification, checkpoint commits, cleanup, validation, push, and pull-request creation. Use when the user wants to verify or fix CVEs on master or a release branch, run the three-image CVE workflow, or open a CVE remediation PR for either supported target branch.
---

# Remediate Image CVEs

Remediate actionable fixable vulnerabilities in the Linux CCM, CNM, and
health-probe-proxy images for one input target branch.

Require the caller to provide an input branch that is exactly `master` or
matches `^release-[0-9]+\.[0-9]+$`. Operate only on the current checkout;
callers may run this skill concurrently in separate worktrees for separate
input branches.

## Sources of Truth

Read these repository skills before taking any action:

- `.agents/skills/build-images/SKILL.md`
- `.agents/skills/fix-image-cves/SKILL.md`
- `.agents/skills/sync-go-modules/SKILL.md`

Follow those skills as the sources of truth for Makefile image builds and
Trivy-based CVE remediation and for final repository-wide Go module
consistency. Do not duplicate, override, or extend their commands. This skill
supplies orchestration, lifecycle, Git, validation, cleanup, and pull-request
policy only. If a dependency is missing or conflicts with this skill, stop and
report the blocker.

Before checking out the input branch, copy this complete skill directory and
all three dependency skill directories into a unique run-scoped temporary
directory outside the repository. Use that exact tooling snapshot for the
entire run; do not depend on skill files present on the target branch.
Invoke both image helpers and the module-sync helper with their explicit
`--repo` input pointing at the current worktree. Remove the temporary tooling
snapshot during final cleanup.

## Preconditions

1. Require a clean worktree before changing branches. Preserve all pre-existing
   work; never reset, discard, or overwrite it.
2. Fail fast if Git, GitHub CLI, Trivy, Docker with Buildx or Podman with native
   build support, or a local Go version compatible with the checked-out branch
   is unavailable. Do not install or upgrade host software.
3. Require both `git var GIT_AUTHOR_IDENT` and
   `git var GIT_COMMITTER_IDENT` to succeed before any build so checkpoint
   commits cannot fail after remediation has started.
4. Require `upstream` to contain the input branch and verify, before any build,
   that the current authenticated account can push to `origin` and open a pull
   request against `upstream`. Check repository permissions rather than
   hard-coding classic-token scope names.
5. Check for an existing open CVE-remediation pull request targeting the input
   branch. If one exists, stop and report its URL instead of creating a
   duplicate.
6. If this worktree already has unfinished `fix-image-cves` state, stop and
   report it instead of deleting evidence from an earlier run.

## Branch and Image Identity

Validate that the input branch is exactly `master` or matches
`^release-[0-9]+\.[0-9]+$`. Fetch and check out its current tip from `upstream`,
using only a fast-forward update if a local branch already exists. Never
hard-reset a local branch; stop if it cannot be fast-forwarded safely. Then
create a branch named
`cve-fix-<input-branch>-<UTC timestamp>`, for example
`cve-fix-master-20260721T013000Z` or
`cve-fix-release-1.36-20260721T013000Z`.

Use the build skill with:

- image registry: `local`
- image tag: `cve-<short-sha>-<UTC timestamp>-<run-id>`
- explicit make inputs: `ARCH=amd64` and `OUTPUT_TYPE=docker`
- inherited make inputs removed: `OUTPUT_FLAG` and `BUILDX_EXTRA_FLAGS`

Generate `<run-id>` once as a lowercase UUID without hyphens and retain it for
the complete run. The tag must be unique to the run so concurrent worktrees
cannot overwrite, scan, or delete one another's verification images. Pass
those make inputs through the build skill's `--set` and `--unset` options on
every build, including rebuilds and final verification builds. These images
are disposable and must never be pushed.

Select one container runtime for the complete run. Respect an explicit
`CONTAINER_CLI`; otherwise use the Makefile preference of Podman when available
and Docker otherwise. Resolve and record its absolute path, then pass it through
the build skill as `--set CONTAINER_CLI=<path>` on every build. Do not build with
one runtime and scan or clean up with another.

After each build, use the selected runtime to resolve and record the canonical
local image reference. Use that exact reference for Trivy scans, rescans, and
cleanup. Podman may normalize `local/...` to `localhost/local/...`; do not
assume the Makefile reference is also the canonical runtime reference.

## Verification Set

Use this order and identity in every phase:

| Image | Build alias | Makefile image reference | Module root | Dockerfile |
|---|---|---|---|---|
| CCM | `ccm` | `local/azure-cloud-controller-manager:<tag>` | `.` | `Dockerfile` |
| CNM | `cnm` | `local/azure-cloud-node-manager:<tag>-linux-amd64` | `.` | `cloud-node-manager.Dockerfile` |
| health-probe-proxy | `hpp` | `local/health-probe-proxy:<tag>` | `health-probe-proxy` | `health-probe-proxy/Dockerfile` |

For every build, allow concurrent builds from other worktrees. Docker builds
share a host-level Buildx builder; Podman uses its native builder. For Docker
only, if a build fails specifically because Buildx setup raced, retry the exact
build once; otherwise do not retry it.

### Baseline Phase

Establish the three-image baseline before mutating source:

1. Build all three images from the untouched input-branch source before running
   any `fix-image-cves apply` command.
2. For each baseline image, run `scan` and `plan` with its canonical image
   reference, module root, and Dockerfile. Record the complete plan, actionable
   findings, unsupported fixable findings, and residual risks outside the
   helper state before scanning the next image.
3. Treat Go toolchain findings and findings without a `FixedVersion` as
   residual risks. Treat any OTHER finding with a non-empty `FixedVersion` as
   an unsupported fixable finding and stop without applying changes or opening
   a PR, preserving that image's state.
4. The helper has one worktree-local state file. When continuing, run its
   `clean` command after recording each successful baseline result so the next
   image starts with empty state. If a baseline scan or plan fails, preserve
   that image's state and stop.

If all three baseline plans contain zero Go-module actions, zero base-image
actions, and zero unsupported fixable findings, exit successfully without
committing, pushing, or opening a PR. Only residual findings may remain on this
no-change path.

### Remediation Phase

After the complete baseline is recorded, process images sequentially in the
table order:

1. Rebuild the image from the current source and resolve its canonical runtime
   reference. Run a fresh `scan` and `plan`; do not apply the saved baseline
   plan because an earlier image may already have changed a shared module.
2. Review every fresh plan. Apply Go-module fixes through `fix-image-cves`. For
   a fixable runtime base-image finding, automatically replace only the digest
   of the existing registry, repository, and tag. If remediation would change
   the image family or tag, stop for review. Stop and report any other
   permission, judgment, or conflict-resolution requirement instead of
   guessing.
3. If the fresh plan contains no Go-module or base-image actions and no
   unsupported fixable findings, record its residual risks, run `clean`, and
   continue to the next image. Skip `apply`, file verification, remediation
   rebuild, and planned-key rescan for this zero-action plan.
4. If the fresh plan has actions, run `apply`, then file verification. Rebuild
   the image and run `verify --rescan` to prove the planned vulnerability keys
   disappeared.
5. Before starting a new scan, record the apply state's exact modified-file
   list and verification results; `scan` replaces prior apply and verify state.
   Then run a fresh `scan` and `plan` of the rebuilt image to detect newly
   introduced actionable or unsupported fixable findings.
6. After the fresh plan is reviewed, commit only the recorded modified files as
   the checkpoint for that cycle. Do not commit immediately after file
   verification. The fresh plan need not be empty for an intermediate cycle.
   Never reset or revert an earlier checkpoint, and keep all checkpoint commits
   in the final PR.
7. If the fresh plan has no remaining actions or unsupported fixable findings,
   record residual risks, run `clean`, and continue to the next image. If it has
   more actions, use that current plan for the next cycle. Allow at most three
   apply/verify/fresh-plan cycles per image.
8. If verification fails, an unsupported fixable finding appears, or actions
   remain after the third cycle, mark the run incomplete. Stop without pushing
   or opening a PR and preserve the current helper state, source changes, and
   local checkpoint commits for diagnosis.

## Final Gate

After all remediation cycles:

1. From the clean checkpointed source, run `sync-go-modules` once to normalize
   every tracked module and the root vendor tree exactly as CI does. If the
   vendor dependency set changes, rerun it with `--allow-dirty` and
   `--update-vendor-licenses`, then run it once more with `--allow-dirty` to
   normalize the root module after license generation. Inspect the resulting
   diff and commit only expected `go.mod`, `go.sum`, `vendor/`, and `LICENSES/`
   paths as a final module-consistency checkpoint. Never absorb unrelated
   files.
2. Run `sync-go-modules --check-clean`. Do not continue unless it produces no
   diff. If vendor-license generation was needed, normalize modules again after
   that generation before running `--check-clean` because the upstream license
   tooling executes `go list -m all`.
3. Run the full repository unit-test suite with `make test-unit`.
4. Run `git diff --check` against the input branch.
5. Rebuild all three images from the final committed source.
6. Freshly scan and plan all three rebuilt images in table order. Record each
   result before scanning the next image; a new `scan` replaces the previous
   state, so no intermediate `clean` is required. Stop immediately on failure
   so the failing image's current state remains available. Verification is
   complete only when none has remaining Go-module or base-image actions and
   none has an unsupported fixable OTHER finding. Preserve findings without a
   fixed version and Go toolchain findings as residual risks.
7. Require a clean worktree containing only the checkpoint commits relative to
   the input branch.

If any gate fails or actionable or unsupported fixable vulnerabilities remain,
do not push or open a PR. Preserve the local branch, checkpoint commits, current
CVE state, and a precise failure summary.

## Pull Request

When source changes exist and every final gate passes:

1. Push the CVE-fix branch to `origin`.
2. Open a ready-for-review PR in `kubernetes-sigs/cloud-provider-azure`
   targeting the input branch.
3. Treat `.github/PULL_REQUEST_TEMPLATE.md` in the target checkout as
   authoritative. Start from its exact contents and preserve every section
   heading, the issue field, the fenced `release-note` and `docs` blocks, and
   any additional sections it contains. HTML guidance comments may be removed
   only after following their instructions. Never replace or drop repository
   template structure to match the skill asset; stop if the two cannot be
   merged without losing required content.
4. Use title `chore: fix cves for <input-branch>`. Read
   `assets/pull-request-template.md` from the run-scoped snapshot of this skill,
   and replace every placeholder. Under `What this PR does / why we need it`,
   use `Remediates actionable fixable vulnerabilities in the Linux CCM, CNM,
   and health-probe-proxy images for <input-branch>.`, followed by the populated
   results table and validation block from the asset. Under `Special notes for
   your reviewer`, use only the populated residual-risk details block from the
   asset. Do not use the asset as the complete PR body or copy its content into
   any other section.
5. Preserve the repository template's heading text and order. Before opening
   the PR, verify that every `####` heading from the repository template appears
   exactly once in the same order and that its `release-note` and `docs` fences
   remain. Use `/kind cleanup`, no issue closure unless the caller supplied one,
   and `NONE` in the release-note block. Replace the issue placeholder with
   `NONE` when no issue was supplied. Count vulnerability findings rather than
   grouped remediation actions in the baseline column. Use `None` instead of
   omitting an empty fix or residual-risk entry. For a shared root-module fix,
   identify the checkpoint that covers both CCM and CNM.

## Cleanup and Output

In a final cleanup step on both success and failure, use the selected runtime to
remove only the three recorded canonical references for this run's uniquely
tagged local verification images. Ignore an image that was never created. Never
run a system prune, remove shared base layers, clear Buildx cache, or delete
images belonging to another run.

Clean `fix-image-cves` state after complete success. Preserve it after failure.
On the no-change path, clean the state and remove the unused local CVE-fix
branch.

Output:

| Image | Baseline findings | Applied fixes | Final verification | Residual risks |
|---|---|---|---|---|

Then report the module-consistency result, unit-test result, checkpoint commits,
cleanup result, and PR URL, or the precise reason no PR was created.
