---
name: create-release-tags
description: Create and optionally push the next Kubernetes-style release tag (vX.Y.Z) from a release-X.Y branch by syncing it with a remote, computing the next patch tag, and creating the tag locally or remotely.
---

# Push Release Tag

## Goal

Cut a new stable release tag for a Kubernetes-style release branch:

- Branch: `release-X.Y`
- Tag: `vX.Y.Z` (patch bump)

## Recommended Workflow

Use `scripts/create_release_tags.py` to:

1. Fetch the chosen remote and tags
2. Fast-forward the local release branch to `<remote>/release-X.Y`
3. Compute the next stable tag if one is not given explicitly
4. Create the tag locally
5. Optionally push the tag to the remote

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
- Use `--force-branch` only when you intentionally want to reset the local
  release branch to the remote branch.

## Manual Fallback

Fetch the remote branch and tags:

```bash
git fetch upstream --prune --tags
```

Check out the release branch and fast-forward it:

```bash
git checkout release-1.34
git merge --ff-only upstream/release-1.34
```

Find the latest stable tag merged into the branch:

```bash
git tag --merged HEAD --list 'v1.34.*' --sort=version:refname | grep -E '^v1\\.34\\.[0-9]+$' | tail -n1
```

Create and push an annotated tag:

```bash
git tag -a v1.34.7 -m "v1.34.7"
git push upstream v1.34.7
```

## Safety Checks

- Ensure the working tree is clean before switching branches.
- Ensure the computed tag does not already exist.
- Push only the intended tag, not every local tag.
