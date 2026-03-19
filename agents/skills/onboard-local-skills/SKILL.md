---
name: onboard-local-skills
description: Link shared skills from agents/skills into a local agent-specific skills directory. Use after the initial manual bootstrap when a user wants to onboard all or selected shared skills into .codex/skills, .claude/skills, or another local skills folder.
---

# Onboard Local Skills

## When To Use

Use this skill when a user wants to make shared repo skills available to a
local agent by linking them into a local skills directory.

This is the bootstrap skill. The user must manually link this skill into a
local agent folder once before the agent can use it.

## Workflow

1. Confirm the target local skills directory, for example `.codex/skills` or
   `.claude/skills`.
2. Decide whether to link all shared skills or only a named subset.
3. Run `scripts/onboard_local_skills.sh` with:
   - `--target <dir>`
   - exactly one of `--all` or one or more `--skill <name>`
   - optional `--dry-run` or `--force` when needed
4. Tell the user what was linked and note that local agent folders should stay
   untracked.

## Commands

Replace `<SKILL_DIR>` with the path of this skill directory.

List shared skills:

```bash
bash <SKILL_DIR>/scripts/onboard_local_skills.sh --list
```

Link all shared skills:

```bash
bash <SKILL_DIR>/scripts/onboard_local_skills.sh \
  --target .codex/skills \
  --all
```

Link selected shared skills:

```bash
bash <SKILL_DIR>/scripts/onboard_local_skills.sh \
  --target .claude/skills \
  --skill onboard-local-skills
```

## Guardrails

- Prefer this script over hand-written symlink commands after the initial
  bootstrap.
- Keep the target explicit. Do not guess an agent-specific local path.
- Do not commit local agent folders created by this workflow.
