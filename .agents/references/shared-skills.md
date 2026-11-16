# Shared Agent Skills

Shared reusable skills live under `.agents/skills/` and are the only repo-tracked
skill source.

## Routing

| Topic | Doc | When to check |
|-------|-----|---------------|
| Shared skill catalog and layout | [../skills/README.md](../skills/README.md) | Discovering existing shared skills, checking the expected skill directory layout, or confirming how agents consume repo-owned skills |
| Shared skill authoring | [../skills/authoring.md](../skills/authoring.md) | Adding or updating shared skills, editing skill metadata, adding support files, writing bundled scripts, or creating PRs with shared-skill changes |

## Rules

- Do not commit `.codex/`, `.claude/`, `.github/skills`, or other local
  agent-specific skill folders.
- Shared skills are intended to be consumed directly from `.agents/skills/` by
  agents that support the shared `.agents` convention.
- Use Python for bundled shared-skill scripts by default.
- Only introduce a different script language when Python is genuinely
  insufficient for the job, and document the exception in the skill.
