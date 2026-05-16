---
name: sync-go-modules
description: Synchronize all tracked Go modules with the go-mod-consistency CI and refresh the root vendor tree. Use when go-mod-consistency fails, dependency bumps leave go.mod or go.sum stale, or the user asks to unblock module consistency, tidy all modules, or update vendor for the main module.
---

# Sync Go Modules

## When To Use

Use this skill when the `go-mod-consistency` workflow fails or when a dependency
change needs repo-wide `go.mod` / `go.sum` reconciliation. The workflow mirrors
`.github/workflows/go-mod-consistency.yaml` and
`.github/actions/discover-go-modules/action.yml`, then also refreshes the main
module's `vendor/` tree.

## Workflow

Replace `<SKILL_DIR>` with the path to this skill directory.

1. Check for unrelated local work before mutating files:

```bash
git status --short
```

If there are unrelated uncommitted changes, stop and report the conflict. If the
existing changes are the module/dependency edits you are intentionally syncing,
pass `--allow-dirty` to the helper.

2. Run the sync from the repo root:

```bash
python3 <SKILL_DIR>/scripts/sync_go_modules.py --repo .
```

For an intentionally dirty dependency branch:

```bash
python3 <SKILL_DIR>/scripts/sync_go_modules.py --repo . --allow-dirty
```

The helper discovers modules with the same tracked-file pathspec as CI:

```bash
git ls-files 'go.mod' '**/go.mod'
```

It then runs `go mod tidy` and `go mod verify` in every discovered module, with
`GOTOOLCHAIN=local` so local behavior does not drift through automatic toolchain
downloads.

3. Keep the main module vendor tree in sync. This is enabled by default:

```bash
(cd . && go mod vendor)
```

Use `--skip-vendor` only when the user explicitly asks to sync nested modules
without touching root vendored dependencies.

4. Inspect the generated diff:

```bash
git diff --stat
```

Expected changes are scoped to tracked module files (`go.mod` / `go.sum`) and,
when the root module changed, `vendor/` plus `vendor/modules.txt`. If the vendor
dependency set changed and the PR also needs license snapshots refreshed, rerun
the helper with `--update-vendor-licenses` or run `make update-vendor-licenses`.
That license update is adjacent to vendoring, but it is not part of the
`go-mod-consistency` workflow itself.

5. To check a clean branch the same way CI will, run the helper after committing
or otherwise clearing the generated diff:

```bash
python3 <SKILL_DIR>/scripts/sync_go_modules.py --repo . --check-clean
```

`--check-clean` fails if the sync produces any repository diff, matching the CI
"Fail on uncommitted changes" step.

## Manual Fallback

If the helper cannot be used, run the CI-equivalent loop manually:

```bash
mapfile -t go_mods < <(git ls-files 'go.mod' '**/go.mod')
mapfile -t modules < <(for mod in "${go_mods[@]}"; do dirname "$mod"; done | sort -u)
for m in "${modules[@]}"; do
  echo ">> go mod tidy (${m})"
  (cd "${m}" && GOTOOLCHAIN=local go mod tidy)
  echo ">> go mod verify (${m})"
  (cd "${m}" && GOTOOLCHAIN=local go mod verify)
done
GOTOOLCHAIN=local go mod vendor
git diff --stat
```

Do not use `find . -name go.mod` for this repo. Hidden local worktrees and other
untracked directories can contain extra modules that CI does not discover.
