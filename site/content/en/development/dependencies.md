---
title: "Dependency Management"
linkTitle: "Dependency Management"
type: docs
weight: 2
description: >
    Manage Cloud Provider Azure dependencies using go modules.
---

cloud-provider-azure uses [go modules] for Go dependency management.

## Usage

Run `make update-dependencies` whenever vendored dependencies change.
This takes a minute to complete.

Run `make update-mocks` whenever implementations for pkg/azureclients change.

### Updating dependencies

New dependencies causes golang to recompute the minor version used for each major version of each dependency. And
golang automatically removes dependencies that nothing imports any more.

To upgrade to the latest version for all direct and indirect dependencies of the current module:

* run `go get -u <package>` to use the latest minor or patch releases
* run `go get -u=patch <package>` to use the latest patch releases
* run `go get <package>@VERSION` to use the specified version

You can also manually editing `go.mod` and update the versions in `require` and `replace` parts.

Because of staging in Kubernetes, manually `go.mod` updating is required for Kubernetes and
its staging packages. In cloud-provider-azure, their versions are set in `replace` part, e.g.

```go.mod
replace (
    ...
    k8s.io/kubernetes => k8s.io/kubernetes v0.0.0-20190815230911-4e7fd98763aa
)
```

To update their versions, you need switch to `$GOPATH/src/k8s.io/kubernetes`, checkout to
the version you want upgrade to, and finally run the following commands to get the go modules expected version:

```sh
commit=$(TZ=UTC git --no-pager show --quiet --abbrev=12 --date='format-local:%Y%m%d%H%M%S' --format="%cd-%h")
echo "v0.0.0-$commit"
```

After this, replace all kubernetes and staging versions (e.g. `v0.0.0-20190815230911-4e7fd98763aa` in above example) in `go.mod`.

Always run `hack/update-dependencies.sh` after changing `go.mod` by any of these methods (or adding new imports).

See golang's [go.mod], [Using Go Modules] and [Kubernetes Go modules] docs for more details.


[go.mod]: https://github.com/golang/go/wiki/Modules#gomod
[go modules]: https://github.com/golang/go/wiki/Modules
[`hack/update-dependencies.sh`]: https://github.com/kubernetes-sigs/cloud-provider-azure/blob/master/hack/update-dependencies.sh
[Using Go Modules]: https://blog.golang.org/using-go-modules
[Kubernetes Go modules]: https://github.com/kubernetes/enhancements/blob/master/keps/sig-architecture/2019-03-19-go-modules.md

### Updating mocks

mockgen v1.6.0 is used to generate mocks.

```sh
mockgen -copyright_file=<copyright file> -source=<azureclient source> -package=<mock package>
```
