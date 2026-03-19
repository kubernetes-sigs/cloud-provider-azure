# Onboarding Shared Skills Locally

Shared skills are committed under `ai/skills/`. Agents can only use them after
they are linked into a local agent-specific skills directory.

## One-Time Bootstrap

Before any agent can use the bootstrap skill, manually symlink
`ai/skills/onboard-local-skills` into the local skills directory for that
agent.

Examples from the repo root:

```bash
mkdir -p .codex/skills
ln -s ../../ai/skills/onboard-local-skills .codex/skills/onboard-local-skills
```

```bash
mkdir -p .claude/skills
ln -s ../../ai/skills/onboard-local-skills .claude/skills/onboard-local-skills
```

After that initial step, ask the agent to use `$onboard-local-skills` to link
all or selected shared skills into the same target directory.

## Agent-Driven Onboarding

Once the bootstrap skill is available locally, the agent should call the
bundled script with an explicit target:

```bash
bash ai/skills/onboard-local-skills/scripts/onboard_local_skills.sh \
  --target .codex/skills \
  --all
```

```bash
bash ai/skills/onboard-local-skills/scripts/onboard_local_skills.sh \
  --target .claude/skills \
  --skill onboard-local-skills
```

The script creates relative symlinks by default.

## Makefile Shortcut

Use the repo wrapper when you want the same behavior without invoking the script
directly:

```bash
make link-ai-skills TARGET_DIR=.codex/skills
make link-ai-skills TARGET_DIR=.claude/skills SKILLS="onboard-local-skills"
```

Supported variables:

- `TARGET_DIR`: required target directory for local agent skills
- `SKILLS`: optional space-separated list of shared skill names
- `FORCE=1`: replace conflicting paths
- `DRY_RUN=1`: print planned symlink operations without changing anything

If `SKILLS` is omitted, the target links all shared skills.

## Local Ignore Guidance

If local agent folders live inside the repo worktree, keep them out of Git with
local ignore rules such as `.git/info/exclude` or a global gitignore. Do not
commit agent-specific skill folders.
