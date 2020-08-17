---
title: "Release Versioning"
linkTitle: "Release Versioning"
type: docs
weight: 3
description: >
    Introduce rules related to release.
---

## Release source
There are two major code change sources for this project, either may push forward a new release for `Kubernetes azure-cloud-controller-manager`:
1. Changes in [Kubernetes cloud-controller-manager](https://kubernetes.io/docs/concepts/overview/components/#cloud-controller-manager), which happens in [Kubernetes repository](https://github.com/kubernetes/kubernetes)
   Since this project dependes on `Kubernetes cloud-controller-manager`, we'll periodically sync changes from Kubernetes upstream repository. When upstream shipped a new release tag, we may consider publishing a new release

2. Changes in [Azure cloud provider](https://github.com/kubernetes-sigs/cloud-provider-azure), which happens directly in this repository
   Azure cloud provider also accepts new features and bug changes. In cases when a security fix is required or when the changes accumulated to certain amount, we may also consider publishing a new release, even if there is no change from Kubernetes upstream.

## Versioning
This project is a Kubernetes component whereas the functionalities and APIs all go with Kubernetes upstream project, thus we will use same versioning mechanism of Kubernetes, with some subtle differences for `Azure cloud provider` and non-Kubernetes changes.

The basic rule is:
1. Every release version follows `Semantic Versioning`, in the form of `MAJOR.MINOR.PATCH`
2. For `MAJOR.MINOR`, it keeps same value as the Kubernetes upstream
3. For `PATCH`, it is calculated independently:
    - If upstream Kubernetes has a new a [patch release](https://github.com/kubernetes/community/blob/master/contributors/design-proposals/release/versioning.md#patch-releases), which introduces change in `cloud-controller-manager` or any component we depend on, then sync the change and increase the `PATCH` number.
    - If any code change happens in [Azure cloud provider](https://github.com/kubernetes-sigs/cloud-provider-azure) or other dependency projects, which becomes eligible for a new release, then increase the `PATCH` number.

References:
- [Kubernetes Release Versioning](https://github.com/kubernetes/community/blob/master/contributors/design-proposals/release/versioning.md)
- [Semantic Versioning](http://semver.org/)

### Branch and version scheme
This project uses golang's vendoring mechanism for managing dependencies (see [Dependency management](../../development/dependencies) for detail). When talking about 'sync from Kubernetes upstream', it actually means vendoring Kubernetes repository code under the vendor directory.

During each sync from upstream, it is usually fine to sync to latest commit. But if there is a new tagged commit in upstream that we haven't vendored, we should sync to that tagged commit first, and apply a version tag correspondingly if applicable. The version tag mechanism is a bit different on master branch and releasing branch, please see below for detail.

The upstream syncing change should be made in a single Pull Request. If in some case, the upstream change causes a test break, then the pull requests should not be merged until follow up fix commits are added.

For example, if upstream change adds a new cloud provider interface, syncing the upstream change may raise a test break, and we should add the implementation (even no-op) in same pull request.

#### master branch
This is the main development branch for merging pull requests. When upgrading dependencies, it will sync from Kubernetes upstream's `master` branch.

Fixes to releasing branches should be merged in master branch first, and then ported to corresponding release branch.

Version tags:
- X.Y.0-alpha.0
  - This is initial tag for a new release, it will be applied when a release branch is created. See below for detail
- X.Y.0-alpha.W, W > 0
  - Those version tags are periodically created if enough change accumulated. It does not have direct mapping with `X.Y.0-alpha.W` in Kubernetes upstream

#### releasing branch
For release `X.Y`, the branch will have name `release-X.Y`. When upgrading dependencies, it will sync with Kubernetes upstream's `release-X.Y` branch.
Release branch would be created when upstream release branch is created and first `X.Y.0-beta.0` tag is applied.

Version tags:
- X.Y.0-beta.0
  - `X.Y.0-beta.0` would be tagged at first independent commit on release branch, the corresponding separation point commit on master would be tagged `X.Y+1.0-alpha.0`
  - No new feature changes are allowed from this time on
- X.Y.0-beta.W, W > 0
  - Those version tags are periodically created if enough change accumulated. It does not have direct mapping with `X.Y.0-beta.W` in Kubernetes upstream
- X.Y.0
  - This is the final release version. When upstream `X.Y.0` tag rolls out, we will begin prepare `X.Y.0` release
  - After merging upstream `X.Y.0` tag commit, we will run full test cycle to ensure the `Azure cloud provider` works well before release:
    - If any test fails, prepare fixes first. If the fix also applies to master branch, then also apply it to master.
    - Rerun full test cycle till all tests got passed stablely
    - Finally, apply `X.Y.0` to latest commit of releasing branch
  - X.Y.1-beta.0 will be tagged at the same commit
- X.Y.Z, Z > 0
  - Those version tags are periodically created if enough change accumulated. It does not have direct mapping with `X.Y.Z` in Kubernetes upstream
  - Testing and release process follows same rule as `X.Y.0`

### CI and dev version scheme
We use [git-describe](https://git-scm.com/docs/git-describe) as versioning source, please check [version](https://github.com/kubernetes-sigs/cloud-provider-azure/tree/master/pkg/version) for detail.

In this case, for commits that does not have a certain tag, the result version would be something like 'v0.1.0-alpha.0-25-gd7999d10'.
