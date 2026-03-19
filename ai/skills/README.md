# Shared AI Skills

This directory is the repo-owned source of truth for reusable AI agent skills.
Only content under `ai/skills/` is committed. Agent-specific skill folders such
as `.codex/skills`, `.claude/skills`, and `.github/skills` are local config and
must stay untracked.

## Layout

```text
ai/skills/
  README.md
  authoring.md
  onboarding.md
  templates/
    skill/
      SKILL.md
  onboard-local-skills/
    SKILL.md
    scripts/
      onboard_local_skills.sh
```

Each shared skill lives in `ai/skills/<skill-name>/` and should keep the top
level small:

- `SKILL.md` for the core instructions
- `references/` for detailed material the agent can load on demand
- `examples/` for short examples that do not belong in `SKILL.md`
- `scripts/` for deterministic or repetitive operations

## Current Shared Skills

- `onboard-local-skills`: bootstrap skill for linking all or selected shared
  skills into a local agent-specific skills directory

## How To Use

- Read [`authoring.md`](authoring.md) to add or update a shared skill.
- Read [`onboarding.md`](onboarding.md) to bootstrap shared skills into a local
  agent configuration.
