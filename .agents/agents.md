# Cloud Provider Azure - AI Agent Guide

This is the Kubernetes out-of-tree cloud provider for Azure. It produces the
cloud-controller-manager, cloud-node-manager, and acr-credential-provider
binaries.

Use this file as a routing index. Before working in an area, read the matching
reference doc below instead of loading every project rule into context.

## Reference Docs

Check these docs when working in the relevant area. Update the corresponding
reference doc when the related logic, workflow, or convention changes.

| Topic | Doc | When to check |
|-------|-----|---------------|
| Project layout and architecture | [references/project-architecture.md](references/project-architecture.md) | Locating packages, choosing ownership boundaries, or starting work in an unfamiliar area |
| Tests, builds, and file headers | [references/testing-builds-and-headers.md](references/testing-builds-and-headers.md) | Choosing validation commands, building images, or adding tracked files |
| Code conventions | [references/code-conventions.md](references/code-conventions.md) | Writing or reviewing Go code, constants, Azure clients, mocks, tests, or repository-pattern code |
| Logging conventions | [references/logging.md](references/logging.md) | Adding, changing, or reviewing logs and returned errors |
| Pull requests | [references/pull-requests.md](references/pull-requests.md) | Creating or updating GitHub pull requests |
| Shared agent skills and authoring | [references/shared-skills.md](references/shared-skills.md) | Adding, editing, reviewing, or opening PRs for shared skills under `.agents/skills/` |
| ETag and cache invalidation | [references/etag-cache.md](references/etag-cache.md) | Modifying Azure resource create/update/delete operations, or changing cache logic |
