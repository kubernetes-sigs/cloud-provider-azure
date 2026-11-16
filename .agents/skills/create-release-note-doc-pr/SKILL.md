---
name: create-release-note-doc-pr
description: Generate or update the documentation-site release note for a given tag, commit it on a branch, push it to a writable remote, and open a GitHub PR to the docs branch. Use when the user wants to create or update release notes, generate a changelog for a tag, or mentions release notes, docs PR, or documentation for a release.
---

# Release Note Docs PR

## Overview

Turn a release tag such as `v1.35.7` into an updated docs-site release note and
open a PR targeting the documentation branch. Release note URLs use
`https://cloud-provider-azure.sigs.k8s.io/blog/<tag>/`, without a date segment.

## Quick Start

Replace `<SKILL_DIR>` with the path of this skill directory.

```bash
python3 <SKILL_DIR>/scripts/create_release_note_doc_pr.py --tag v1.35.7
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
6. Validates the generated docs content and sets `url: "/blog/<tag>/"` in its
   YAML front matter before commit, including with `--no-generate`. Keeps the
   `date` metadata for display and sorting; it no longer determines the URL
7. Commits only `content/en/blog/releases/<tag>.md`, pushes the branch, and
   opens a PR with labels `kind/documentation` and `release-note-none`
8. Restores the original branch after completion

The Python helper sets the URL after generation because the shell generator
runs from the checked-out docs branch. It does not change Hugo's site-wide
permalink rule or bulk-migrate historical notes; migrating published URLs and
adding redirects is a separate task.

## Script Usage

```bash
python3 <SKILL_DIR>/scripts/create_release_note_doc_pr.py \
  --tag <vX.Y.Z> [--no-generate] [--dry-run] [--force-push] \
  [--base-remote <name>] [--push-remote <name>] [--base-branch <name>] \
  [--target-repo <owner/repo>] [--head-owner <owner>]
```

Common flags:

- `--no-generate`: Skip `hack/generate-release-note.sh` when the file is already
  updated; still validate the content and normalize its URL
- `--dry-run`: Print commands without executing them
- `--force-push`: Push with `--force-with-lease`
- `--base-remote`: Override the remote used for the base branch
- `--push-remote`: Override the remote used to push the branch
- `--base-branch`: Override the docs base branch, default `documentation`
- `--target-repo`: Set the PR target repo explicitly
- `--head-owner`: Set the PR head owner explicitly

## Local Validation

Run the bundled tests without GitHub credentials or remote writes. Use `-B`
to avoid bytecode caches that would fail the helper's clean-worktree check:

```bash
python3 -B -m unittest discover -s <SKILL_DIR>/scripts -p 'test_*.py' -v
```

When Hugo is installed, the tests also check that the explicit URL overrides
its dated permalink rule for both the main site and the GitHub Pages base path.
Otherwise, that integration test is skipped.

## Error Handling

If the script or any command fails, **do not make up new scripts to work around
the error** and **do not fabricate or guess at content**. Instead, report the
failure and exact error message to the user and let them decide how to proceed.

## Troubleshooting

- If repo or owner detection is wrong, pass `--target-repo` and `--head-owner`
  explicitly
- If token resolution fails, export `GITHUB_TOKEN` or `GH_TOKEN`, or run
  `gh auth login`
- If the generated file is empty or malformed, rerun without `--no-generate`
