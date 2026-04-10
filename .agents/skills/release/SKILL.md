---
name: release
description: Prepare and publish a stable release in two phases by reusing the shared release-tag and release-note-doc-pr skills, building artifacts in GitHub, generating release notes and draft releases locally, validating release assets, and controlling latest-release status explicitly. Use when the user wants to perform a full release, publish a new version, or mentions releasing, shipping a version, or the end-to-end release process.
---

# Manual Release

## Overview

Use this skill when you want a draft-first release flow with explicit control
over tag creation, workflow dispatch, publication, and the follow-up
documentation PR.

This skill reuses the shared scripts from:

- `create-release-tags`
- `create-release-note-doc-pr`

## Workflow

1. `prepare`: create and push the release tag, dispatch the `Release`
   workflow manually, wait for the workflow to finish, download the workflow
   artifacts, generate release notes locally, create or update the draft
   release, upload the assets, and verify that the draft release contains the
   expected assets.
2. `publish`: publish the draft release with an explicit latest-release policy
   and then open the documentation PR for the same tag.

## Quick Start

Replace `<SKILL_DIR>` with the path of this skill directory.

Prepare a draft release from `release-1.35`:

```bash
python3 <SKILL_DIR>/scripts/release.py prepare --branch release-1.35
```

Publish an existing draft release and open the docs PR:

```bash
python3 <SKILL_DIR>/scripts/release.py publish --tag v1.35.7
```

Preview either phase without changing state:

```bash
python3 <SKILL_DIR>/scripts/release.py prepare --branch release-1.35 --tag v1.35.7 --dry-run
python3 <SKILL_DIR>/scripts/release.py publish --tag v1.35.7 --dry-run
```

## Commands

Prepare a draft release:

```bash
python3 <SKILL_DIR>/scripts/release.py prepare \
  --branch <release-X.Y> [--tag <vX.Y.Z>] [--remote <name>] [--workflow release.yaml]
```

Publish a draft release:

```bash
python3 <SKILL_DIR>/scripts/release.py publish \
  --tag <vX.Y.Z> [--latest auto|always|never] [--remote <name>] \
  [--base-remote <name>] [--push-remote <name>] [--base-branch <name>]
```

## Notes

- `.github/workflows/release.yaml` now builds artifacts only. It no longer
  generates release notes or creates/publishes release objects.
- `prepare` pushes the tag, manually dispatches `.github/workflows/release.yaml`,
  waits for the build to finish, downloads the workflow artifacts, generates the
  release notes locally, creates or updates the draft release, uploads the
  assets, and verifies the draft contents.
- `publish` keeps latest-release handling explicit. `--latest auto` marks the
  release as latest only when its `major.minor` series is the highest stable
  series currently published.
- The release description is generated locally by the same
  `hack/generate-release-note.sh` flow used by the docs workflow, so the skill
  is no longer coupled to the release GitHub Action for notes generation.
- The publish phase calls the shared `create-release-note-doc-pr` script after
  the GitHub release is published.
