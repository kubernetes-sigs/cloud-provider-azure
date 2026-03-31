# Authoring Shared Skills

Shared skills are written once under `.agents/skills/` and consumed directly by
agents that support the shared `.agents` convention.

## Rules

- Create each shared skill at `.agents/skills/<skill-name>/`.
- Keep `SKILL.md` agent-agnostic. Do not put Codex-only, Claude-only, or
  Copilot-only metadata in the shared skill body.
- Do not commit `.codex/`, `.claude/`, `.github/skills`, or other local agent
  folders.
- Keep `SKILL.md` concise. Move bulky details into `references/`, `examples/`,
  or `scripts/`.
- Prefer bundled scripts for deterministic workflows instead of long procedural
  prose.
- Standardize bundled scripts on Python unless there is a concrete technical
  reason another language is necessary.
- Markdown files (e.g., `SKILL.md`) do not need the Kubernetes Apache 2.0
  boilerplate header. Only source code files (`.py`, `.sh`, `.go`, etc.)
  require it.

## Suggested Process

1. Copy `.agents/skills/templates/skill/` to `.agents/skills/<skill-name>/`.
2. Update `SKILL.md` frontmatter:
   - `name`
   - `description`
3. Replace the template body with:
   - when the skill should be used
   - what inputs it expects
   - the exact workflow the agent should follow
   - which bundled files to open or execute
4. Add supporting files only when they materially reduce ambiguity or repeated
   agent effort.
5. Update [`README.md`](README.md) so the shared skill catalog stays current.
6. Verify any bundled scripts or examples still point at `.agents/skills/`
   paths when they refer to other shared skills.

## Design Guidance

- Use lowercase hyphenated names.
- Keep skill folder contents intentional. Avoid extra helper docs inside the
  skill itself unless they are part of the agent workflow.
- Make scripts accept explicit inputs rather than assuming a specific local
  agent path.
- If you need an exception to the Python-only convention, explain it in
  `SKILL.md` so later contributors do not reintroduce mixed runtimes casually.
