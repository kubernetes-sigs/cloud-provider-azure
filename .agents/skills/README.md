# Shared AI Skills

This directory is the repo-owned source of truth for reusable AI agent skills.
Only content under `.agents/skills/` is committed. Agent-specific skill folders such
as `.codex/skills`, `.claude/skills`, and `.github/skills` are local config and
must stay untracked.

## Layout

```text
.agents/skills/
  README.md
  authoring.md
  templates/
    skill/
      SKILL.md
```

Each shared skill lives in `.agents/skills/<skill-name>/` and should keep the top
level small:

- `SKILL.md` for the core instructions
- `references/` for detailed material the agent can load on demand
- `examples/` for short examples that do not belong in `SKILL.md`
- `scripts/` for deterministic or repetitive operations, using Python by
  default

## Current Shared Skills

- `cherry-pick-pr`: cherry-pick a merged pull request onto a release branch,
  resolve conflicts, run targeted validation, and open a cherry-pick PR
- `create-release-tags`: create and optionally push the next stable release tag
  from a `release-X.Y` branch
- `create-release-note-doc-pr`: generate a documentation-site release note for
  a tag and open a documentation PR
- `fix-image-cves`: scan a built image with Trivy, plan fixable Go-module and
  base-image CVE remediation, apply the source changes, and verify the result
- `remediate-image-cves`: orchestrate builds, repeated CVE remediation and
  verification for CCM, CNM, and health-probe-proxy on `master` or a
  `release-X.Y` branch, then validate, clean up, push, and open the remediation
  PR
- `release`: draft-first manual release orchestrator that reuses the shared tag
  and docs-PR skills while keeping GitHub release publication under explicit
  user control
- `run-e2e-test`: parse a Go e2e test from `tests/e2e/`, translate each step to
  `kubectl` and `az` CLI commands, and interactively replay the test against a
  live cluster
- `sync-go-modules`: unblock `go-mod-consistency` by tidying and verifying all
  tracked Go modules, then refreshing the main module's `vendor/` tree
- `unblock-dependabot-pr`: diagnose failed Dependabot PR CI, reuse the Go module
  sync workflow, close Kubernetes minor-version bumps, retest Azure public-IP
  quota e2e flakes, add `/lgtm` when only Tide is pending, and escalate
  dependency/toolchain blockers for discussion
- `debug-e2e-pipeline`: fetch and analyze Prow e2e pipeline failures by
  downloading build logs, JUnit reports, and node artifacts from GCS, then
  matching errors against known failure patterns
- `build-images`: build cloud-provider-azure CCM, CNM, health-probe-proxy,
  CCM e2e, or root CCM/CNM aggregate images with explicit tag/registry inputs
  and optional make flag overrides

## How To Use

- Read [`authoring.md`](authoring.md) to add or update a shared skill.
- Agents that support the shared `.agents` convention should consume these
  skills directly from `.agents/skills/`.
