---
name: create-release-note-doc-pr
description: Generate or update the documentation-site release note for a given tag, commit it on a branch, push it to a writable remote, and open a GitHub PR to the docs branch.
---

# Release Note Docs PR

## Overview

Turn a release tag such as `v1.35.7` into an updated docs-site release note and
open a PR targeting the documentation branch.

## Quick Start

Replace `<SKILL_DIR>` with the path of this skill directory.

```bash
bash <SKILL_DIR>/scripts/create_release_note_doc_pr.sh --tag v1.35.7
```

## Requirements

- Clean git working tree
- `gh` CLI installed and authenticated
- GitHub token available through `GITHUB_TOKEN`, `GH_TOKEN`, or `gh auth token`

Default remote behavior:

- Base remote defaults to `upstream` when it has the docs branch; otherwise
  `origin`
- Push remote defaults to `origin`
- PR target repo and head owner are derived from the selected remotes unless
  overridden

## What It Does

1. Resolves remotes, repo, and owner defaults for a fork-to-upstream workflow
2. Resolves GitHub credentials from environment variables or `gh auth token`
3. Checks whether an open PR already exists for the branch
4. Fetches the base branch and creates `doc/release-note-<tag>` from it
5. Runs `./hack/generate-release-note.sh <tag> <temp-output> true` by default
6. Validates the generated docs content before commit
7. Commits only `content/en/blog/releases/<tag>.md`, pushes the branch, and
   opens a PR with label `kind/documentation`

## Script Usage

```bash
bash <SKILL_DIR>/scripts/create_release_note_doc_pr.sh \
  --tag <vX.Y.Z> [--no-generate] [--dry-run] [--force-push] \
  [--base-remote <name>] [--push-remote <name>] [--base-branch <name>] \
  [--target-repo <owner/repo>] [--head-owner <owner>]
```

Common flags:

- `--no-generate`: Skip `hack/generate-release-note.sh` when the file is already
  updated
- `--dry-run`: Print commands without executing them
- `--force-push`: Push with `--force-with-lease`
- `--base-remote`: Override the remote used for the base branch
- `--push-remote`: Override the remote used to push the branch
- `--base-branch`: Override the docs base branch, default `documentation`
- `--target-repo`: Set the PR target repo explicitly
- `--head-owner`: Set the PR head owner explicitly

## Troubleshooting

- If repo or owner detection is wrong, pass `--target-repo` and `--head-owner`
  explicitly
- If token resolution fails, export `GITHUB_TOKEN` or `GH_TOKEN`, or run
  `gh auth login`
- If the generated file is empty or malformed, rerun without `--no-generate`
