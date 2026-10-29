# Tests, Builds, and File Headers

## Unit Tests

```sh
make test-unit                        # Run all unit tests
go test -v ./pkg/provider/...         # Run tests for a specific folder
```

If changes are restricted to one folder, prefer `go test -v ./<folder>/...`
over `make test-unit` for faster feedback.

## Testing Expectations

- Test public functions.
- Test edge cases and error conditions.
- Mock external dependencies.
- Match the existing test style in the file being edited.

### azclient Recording Cleanup

Private DNS zones may still report deleted virtual-network links briefly after
the link deletion poller completes. Both affected recording suites use
`recording.DeletePrivateDNSZone`, which retries only HTTP 409
`CannotDeleteResource`, including deletion-poller errors, for at most five
minutes or the caller's deadline. Other errors fail immediately.

Offline cleanup and public-IP ETag regression tests do not require Azure
credentials. Live suites need fresh recordings after their request sequences
change. Preserve successful fixtures and re-record only affected package/cloud
pairs; a fixture saved by a failed test is not a successful recording.

### Service Gateway in Azure China

The generated Service Gateway `TestClient` explicitly skips when
`AZURE_CLOUD=AZURECHINACLOUD`, before running Ginkgo setup or cleanup. Its
required `Microsoft.Network/AllowServiceGateways` feature cannot currently be
self-service registered in the available China test subscriptions. This is a
test-environment limitation, not a claim that the service is unavailable
throughout Azure China. Public-Azure recording tests and offline Service Gateway
unit tests remain enabled. The exception lives in the client generator so
regeneration preserves it; revisit it when an enabled China subscription is
available.

## Building Images

Before building images, ask the user for `IMAGE_REGISTRY` and `IMAGE_TAG`. For
the reusable agent workflow, use `.agents/skills/build-images/`.

```sh
IMAGE_REGISTRY=<registry> IMAGE_TAG=<tag> make build-ccm-image
IMAGE_REGISTRY=<registry> IMAGE_TAG=<tag> make build-node-image-linux
```

The CCM and CNM Dockerfiles use Microsoft Go. Starting with version 1.27,
`systemcrypto` is no longer configured through `GOEXPERIMENT`. For non-FIPS
development images, use `MS_GO_NOSYSTEMCRYPTO=1 make build-ccm-image`. This
produces a non-FIPS-compliant binary and must not be used for production or
FIPS validation.

## File Headers

Newly created tracked source files must carry the standard Kubernetes Apache 2.0
boilerplate header using the appropriate comment style for that file type.

Run `hack/verify-boilerplate.sh` or `make test-check` after adding new tracked
files when there is any doubt about header compliance.

Markdown files do not need the boilerplate header. For shared-skill docs under
`.agents/skills/`, also follow [shared skill authoring](../skills/authoring.md).
