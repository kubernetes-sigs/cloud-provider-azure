---
name: create-release-tags
description: Create and optionally push the next Kubernetes-style release tag (vX.Y.Z) from a release-X.Y branch by resolving the remote branch tip, computing the next patch tag, and tagging the commit directly without checking out the branch. Use when the user wants to tag a release, create a version tag, or mentions release tagging, cutting a release, or bumping a patch version.
---

# Push Release Tag

## Goal

Cut a new stable release tag for a Kubernetes-style release branch:

- Branch: `release-X.Y`
- Tag: `vX.Y.Z` (patch bump)

## Recommended Workflow

Use `scripts/create_release_tags.py` to:

1. Fetch the chosen remote and tags
2. Resolve the commit at the tip of `<remote>/release-X.Y`
3. Compute the next stable tag if one is not given explicitly
4. Create the tag pointing at that commit (no branch checkout required)
5. Optionally push the tag to the remote

Multiple branches can be tagged in parallel since the script never checks out
or modifies the local working tree.

## Commands

Replace `<SKILL_DIR>` with the path of this skill directory.

Plan only:

```bash
python3 <SKILL_DIR>/scripts/create_release_tags.py --repo . --branch release-1.34
```

Create the tag locally:

```bash
python3 <SKILL_DIR>/scripts/create_release_tags.py --repo . --branch release-1.34 --create
```

Create and push the tag:

```bash
python3 <SKILL_DIR>/scripts/create_release_tags.py --repo . --branch release-1.34 --push
```

## Notes

- Default remote is `upstream`. Override with `--remote <name>`.
- If the branch name does not match `release-X.Y`, pass `--series X.Y`.
- Tags are annotated by default. Use `--sign` for signed tags.
- `--force-branch` is a no-op kept for backward compatibility.

## Manual Fallback

Fetch the remote branch and tags:

```bash
git fetch upstream --prune --tags
```

Find the latest stable tag merged into the branch:

```bash
git tag --merged upstream/release-1.34 --list 'v1.34.*' --sort=version:refname | grep -E '^v1\.34\.[0-9]+$' | tail -n1
```

Create and push an annotated tag at the remote branch tip:

```bash
git tag -a -m "v1.34.7" v1.34.7 upstream/release-1.34
git push upstream v1.34.7
```

## Safety Checks

- Ensure the computed tag does not already exist.
- Push only the intended tag, not every local tag.
