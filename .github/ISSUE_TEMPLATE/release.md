---
name: New version release
about: Track preparation and tasks for a new release
title: "Release v<major>.<minor>.0"
labels: ""
assignees: ""

---

#### Release details

- New release version:
- Version entering LTS:
- Retiring version:

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

##### Version entering LTS

- [ ] Remove periodic test-infra jobs.
- [ ] Replace `latest-xx` Kubernetes version references with pinned versions in test-infra jobs.
- [ ] Disable Dependabot jobs.

##### Retiring version

- [ ] Remove test-infra jobs.
- [ ] Set RP versioning manifest `imageTagUpdateStrategy` to `"no-update"`.

#### Notes

<!-- Add blockers, related PRs, and release-specific changes here. -->
