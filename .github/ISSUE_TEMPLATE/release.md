---
name: New version release
about: Track preparation and tasks for a new release
title: "Release v<major>.<minor>.0"
type: Task
labels: ""
assignees: ""

---

#### Release details

- New release version:

#### Checklist

- [ ] Review and merge pending PRs.
- [ ] Update Go modules and vendor dependencies.
- [ ] Update vendor licenses for all maintained branches.
- [ ] Create the new release branch.
- [ ] Create Dependabot jobs.
- [ ] Create test-infra jobs and dashboard.
- [ ] Update documentation: README, example files, and release notes.

##### After the release is published

- [ ] Update the chart.

#### Notes

<!-- Add blockers, related PRs, and release-specific changes. -->
