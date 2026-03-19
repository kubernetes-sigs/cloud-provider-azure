# Authoring Shared Skills

Shared skills are written once under `ai/skills/` and consumed from local
agent-specific skill folders through symlinks.

## Rules

- Create each shared skill at `ai/skills/<skill-name>/`.
- Keep `SKILL.md` agent-agnostic. Do not put Codex-only, Claude-only, or
  Copilot-only metadata in the shared skill body.
- Do not commit `.codex/`, `.claude/`, `.github/skills`, or other local agent
  folders.
- Keep `SKILL.md` concise. Move bulky details into `references/`, `examples/`,
  or `scripts/`.
- Prefer bundled scripts for deterministic workflows instead of long procedural
  prose.

## Suggested Process

1. Copy `ai/skills/templates/skill/` to `ai/skills/<skill-name>/`.
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
6. Validate onboarding with the bootstrap script in a temporary local skills
   directory before relying on the new skill from an agent.

## Design Guidance

- Use lowercase hyphenated names.
- Keep skill folder contents intentional. Avoid extra helper docs inside the
  skill itself unless they are part of the agent workflow.
- Make scripts accept explicit inputs rather than assuming a specific local
  agent path.
- If a user asks an agent to onboard skills, the agent should use
  `onboard-local-skills` or its bundled script instead of creating ad hoc
  symlinks.
