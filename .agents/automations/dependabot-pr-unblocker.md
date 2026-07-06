Run the repository skill at `.agents/skills/unblock-dependabot-pr/` for
currently open Dependabot pull requests with failed CI.

Read `.agents/skills/unblock-dependabot-pr/SKILL.md` first and follow it as the
source of truth. Do not duplicate or override the skill's workflow. List open
Dependabot PRs in `kubernetes-sigs/cloud-provider-azure`, identify the ones
with failed checks, and apply the skill to each such PR by PR number or URL.

If the skill requires user judgment, permission, or conflict resolution, stop
for that PR and report the blocker instead of guessing. If no open Dependabot
PRs have failed checks, say so briefly.

Output a concise Markdown table:
| PR | Failed checks | Action | Result | Follow-up |
Use Markdown links for PRs, and include pending checks or residual risks in
`Follow-up`.
