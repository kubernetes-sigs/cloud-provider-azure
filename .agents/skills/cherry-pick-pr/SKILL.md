---
name: cherry-pick-pr
description: Cherry-pick a merged pull request onto a release branch with Prow-style branch naming, manual conflict resolution, targeted validation, and GitHub PR creation. Use when the user wants to backport a PR, cherry-pick a commit to a release branch, or mentions cherry-pick, backport, or porting a fix to an older release.
---

# Cherry-Pick Pull Requests

## When To Use

Use this skill when you need to cherry-pick a merged PR onto a release branch
and want the agent to handle the git and GitHub workflow while keeping conflict
resolution and test decisions in the loop.

## Workflow

Replace `<SKILL_DIR>` with the path to this skill directory.

1. Run a dry-run first:

```bash
python3 <SKILL_DIR>/scripts/cherry_pick_pr.py pick \
  --pr <number> \
  --target-branch <release-X.Y> \
  --dry-run
```

2. Start the cherry-pick from a clean working tree:

```bash
python3 <SKILL_DIR>/scripts/cherry_pick_pr.py pick \
  --pr <number> \
  --target-branch <release-X.Y>
```

3. If `pick` exits with code `3`, inspect the conflicted files from the output,
   read the source diff with `gh pr diff <number>`, resolve the conflicts, stage
   the files with `git add`, then continue:

```bash
python3 <SKILL_DIR>/scripts/cherry_pick_pr.py continue
```

4. Run targeted validation after a clean cherry-pick:

```bash
python3 <SKILL_DIR>/scripts/cherry_pick_pr.py test
```

5. Create the PR only after `test` reports success:

```bash
python3 <SKILL_DIR>/scripts/cherry_pick_pr.py create-pr
```

6. Abandon a failed or stale attempt with:

```bash
python3 <SKILL_DIR>/scripts/cherry_pick_pr.py abort
```

## Notes

- The script refuses to start `pick` from a dirty working tree.
- The script refuses to start a new attempt while `.git/cherry-pick-pr.json`
  exists. Run `abort` first.
- The state file lives under `.git/` intentionally so it stays untracked.
- `continue` runs `git cherry-pick --continue` non-interactively.
- `test` records `passed`, `failed`, or `build_failed` so the agent can tell
  the difference between test failures and target-branch compatibility issues.
- `create-pr` reads `.github/PULL_REQUEST_TEMPLATE.md` at runtime and fails if
  required anchors drift.
- `create-pr` expects the source PR to already carry a `kind/...` label. If it
  does not, fix that first or update the source PR metadata before creating the
  cherry-pick PR.
- Use `--chain <branch>...` on `pick` when you want the resulting PR body to
  append a follow-up `/cherrypick ...` command for later serialized backports.
