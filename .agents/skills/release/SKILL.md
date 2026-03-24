---
name: release
description: Prepare and publish a stable release in two phases by reusing the shared release-tag and release-note-doc-pr skills, dispatching the GitHub release workflow manually, validating release assets, and controlling latest-release status explicitly.
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
   workflow manually, wait for the workflow to finish, and verify that the
   draft release contains the expected assets.
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

- `prepare` is the phase that pushes the tag. After the repo no longer
  auto-triggers releases on tag push, it manually dispatches `.github/workflows/release.yaml`.
- `publish` keeps latest-release handling explicit. `--latest auto` marks the
  release as latest only when its `major.minor` series is the highest stable
  series currently published.
- The release description comes from the same `hack/generate-release-note.sh`
  flow used by the release workflow and the documentation PR workflow.
- The publish phase calls the shared `create-release-note-doc-pr` script after
  the GitHub release is published.
