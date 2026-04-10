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
   - `name` — lowercase hyphenated, must match the directory name
   - `description` — see [Description field](#description-field) below
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

## Description Field

The `description` in SKILL.md frontmatter is the primary signal agents use to
decide whether to activate a skill. It is loaded at startup for every skill
(~100 tokens each), so it must be both concise and informative.

Follows the [Agent Skills specification](https://agentskills.io/specification).

### Requirements

- Max 1024 characters.
- Must describe **what** the skill does and **when** to use it.
- Should include specific keywords that help agents match user requests to
  this skill.

### Structure

Write the description in two parts:

1. **What it does** — a concise summary of the skill's capabilities.
2. **When to use it** — trigger phrases starting with "Use when..." that
   list the situations, user requests, or keywords that should activate
   the skill.

### Examples

Good:

```yaml
description: >-
  Extract text and tables from PDF files, fill PDF forms, and merge
  multiple PDFs. Use when working with PDF documents or when the user
  mentions PDFs, forms, or document extraction.
```

```yaml
description: >-
  Parse a Go e2e test from tests/e2e/, translate each step to kubectl
  and az CLI commands, and interactively replay the test against a live
  cluster. Use when the user wants to manually run, debug, or reproduce
  an e2e test case, or when they mention replaying a test, running a
  test against a cluster, or verifying e2e test behavior with kubectl
  and az.
```

Poor — missing when-to-use triggers:

```yaml
description: Helps with e2e tests.
```

Poor — missing what-it-does summary:

```yaml
description: Use when the user asks about e2e tests.
```

### Progressive Disclosure

The description is part of the metadata layer (~100 tokens per skill) that
agents load at startup. The full `SKILL.md` body (< 5000 tokens recommended)
is loaded only when the skill is activated. Files in `references/`, `scripts/`,
and `assets/` are loaded only when needed during execution. Structure your
skill to take advantage of this:

| Layer | Loaded when | Budget |
|-------|-------------|--------|
| `name` + `description` | Startup (all skills) | ~100 tokens |
| `SKILL.md` body | Skill activated | < 5000 tokens |
| `references/`, `scripts/`, `assets/` | On demand | No limit |
