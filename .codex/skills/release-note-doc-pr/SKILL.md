---
name: release-note-doc-pr
description: Create or update the cloud-provider-azure documentation-site release note for a given tag and open a GitHub pull request to the `documentation` branch. Use when you need to generate/update `content/en/blog/releases/vX.Y.Z.md`, commit it on a `doc/release-note-vX.Y.Z` branch, and open a PR from a fork to upstream with token fallback via env/gh/1Password.
---

# Release Note Docs PR

## Overview

Turn a release tag (for example `v1.35.7`) into an updated docs-site release note file and a PR targeting the `documentation` branch.

## Quick start

From the repo root:

```bash
bash .codex/skills/release-note-doc-pr/scripts/release_note_doc_pr.sh --tag v1.35.7
```

## Requirements

- Clean git working tree (the script will refuse to run otherwise).
- `gh` CLI available.
- GitHub token available from one of: `GITHUB_TOKEN`, `GH_TOKEN`, `gh auth token`, or 1Password (`op` fallback).

Default remote behavior:
- Base remote defaults to `upstream` when it has `documentation`; otherwise `origin`.
- Push remote defaults to `origin`.
- PR target repo/head owner are derived from selected remotes unless overridden.

## What it does

1. Resolves remotes/repo/owner defaults for fork + upstream workflows.
2. Resolves `GITHUB_TOKEN` in order: `GITHUB_TOKEN` → `GH_TOKEN` → `gh auth token` → `op item get`.
3. Checks if an open PR already exists for `doc/release-note-<tag>` → `documentation` (exits if it does).
4. Fetches only the base branch and creates `doc/release-note-<tag>` from `<base-remote>/<base-branch>`.
5. (Default) Runs `./hack/generate-release-note.sh <tag> release-notes.md true` to update `content/en/blog/releases/<tag>.md`.
6. Validates generated release-note content before commit (`##` sections must exist).
7. Commits only `content/en/blog/releases/<tag>.md`, pushes, and opens a PR with label `kind/documentation`.

## Script usage

```bash
bash .codex/skills/release-note-doc-pr/scripts/release_note_doc_pr.sh \
  --tag <vX.Y.Z> [--no-generate] [--dry-run] [--force-push] \
  [--base-remote <name>] [--push-remote <name>] [--base-branch <name>] \
  [--target-repo <owner/repo>] [--head-owner <owner>] [--op-item-id <id>]
```

Common flags:
- `--no-generate`: Skip running `hack/generate-release-note.sh` (use if you already updated the file manually).
- `--dry-run`: Print commands without executing.
- `--force-push`: Force-update the remote branch with `--force-with-lease`.
- `--base-remote`: Override base remote (default auto-select `upstream`/`origin`).
- `--push-remote`: Override push remote (default `origin`).
- `--base-branch`: Override base branch (default `documentation`).
- `--target-repo`: Explicit PR target repo (`owner/repo`) when remote URL parsing is not desired.
- `--head-owner`: Explicit PR head owner (defaults from push remote URL owner).
- `--op-item-id`: Override 1Password item id for token fallback.

Troubleshooting:
- If PR checks return unexpected matches, pass explicit `--target-repo` and `--head-owner`.
- If token retrieval fails, export `GITHUB_TOKEN` or `GH_TOKEN`, or run `gh auth login`.
- If a remote has conflicting local tags, no extra action is required; this workflow no longer fetches all tags.
