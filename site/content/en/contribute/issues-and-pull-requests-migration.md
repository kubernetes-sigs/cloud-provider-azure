---
title: "Issues and pull requests migration"
linkTitle: "Issues and PRs"
type: docs
weight: 2
description: >
    Developer guidance for how to contribute using issues and PRs.
---

*NOTE* This page only applies after Azure cloud provider implementation code has been moved to this repository.


There are some ongoing issues and pull requests addressing the Azure cloud provider in Kubernetes repository.

When we turned to use the standalone cloud provider in this repository, those issues and pull requests should also be moved.

Here are some notes for issues and pull requests migration.

## Issue migration

If issue applies only to Azure cloud provider, please close it and create a new one in this repository.

If issue also involves other component, leave it there, but do create a new issue in this repository to track counterpater in Azure cloud provider.

In both cases, leave a link to the new created issue in the old issue.

## Pull request migration

Basically we have migrated code from `k8s.io/legacy-cloud-providers/azure/` to `github.com/sigs.k8s.io/cloud-provider-azure/pkg/provider`.

The following steps describe how to port an existing PR from kubernetes repository to this repository.

1. Generate pull request patch

In your kubernetes repository, run following to generate a patch for your PR.
- PR_ID: Pull Request ID in kubernetes repository
- UPSTREAM_BRANCH: Branch name pointing to upstream, basically the branch with url `https://github.com/kubernetes/kubernetes.git` or `https://k8s.io/kubernetes`

```shell script
PR_ID=
UPSTREAM_BRANCH=origin
PR_BRANCH_LOCAL=PR$PR_ID

git fetch $UPSTREAM_BRANCH pull/$PR_ID/head:$PR_BRANCH_LOCAL
MERGE_BASE=$(git merge-base $UPSTREAM_BRANCH/master $PR_BRANCH_LOCAL)
PATCH_FILE=/tmp/${PR_ID}.patch
git diff $MERGE_BASE $PR_BRANCH_LOCAL > $PATCH_FILE
git branch -D $PR_BRANCH_LOCAL
```

2. Transform the patch and apply

Switch to kubernetes-azure-cloud-controller-manager repo.
Apply the patch:
```
hack/transform-patch.pl $PATCH_FILE | git apply
```

If any of file in the patch does not fall under Azure cloud provider directory, the transform script will prompt a warning.
