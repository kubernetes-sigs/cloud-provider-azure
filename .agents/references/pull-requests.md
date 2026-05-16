# Pull Requests

When creating a GitHub pull request for this repository, read and follow
`.github/PULL_REQUEST_TEMPLATE.md`.

If using `gh pr create`, use the repository template as the starting point for
the PR body instead of replacing it with ad hoc sections.

## Required Template Sections

The PR body must include these sections filled in appropriately:

1. **What type of PR is this?** Add a Prow `/kind` command such as `/kind bug`,
   `/kind cleanup`, `/kind feature`, `/kind documentation`, or `/kind design`.
2. **What this PR does / why we need it**. Provide a concise description of the change.
3. **Which issue(s) this PR fixes**. Use `Fixes #<number>` to auto-close issues on merge.
4. **Special notes for your reviewer**. Include anything reviewers should pay attention to.
5. **Does this PR introduce a user-facing change?** Fill the `release-note` block;
   write `NONE` when there is no user-facing change.
6. **Additional documentation**. Link to KEPs, usage docs, or other references in
   the `docs` block; leave it empty if not applicable.

## PR Rules

- Preserve the template structure, including PR kind, issue reference, reviewer
  notes, `release-note` block, and `docs` block.
- Follow the template instructions for non-applicable sections.
- Use the `gh` CLI for PR interactions when working with GitHub from the agent.
- For PRs with shared-skill changes under `.agents/skills/`, also read
  [shared skill authoring](../skills/authoring.md).
