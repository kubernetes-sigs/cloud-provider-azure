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

## Building Images

Before building images, ask the user for `IMAGE_REGISTRY` and `IMAGE_TAG`. For
the reusable agent workflow, use `.agents/skills/build-images/`.

```sh
IMAGE_REGISTRY=<registry> IMAGE_TAG=<tag> make build-ccm-image
IMAGE_REGISTRY=<registry> IMAGE_TAG=<tag> make build-node-image-linux
```

## File Headers

Newly created tracked source files must carry the standard Kubernetes Apache 2.0
boilerplate header using the appropriate comment style for that file type.

Run `hack/verify-boilerplate.sh` or `make test-check` after adding new tracked
files when there is any doubt about header compliance.

Markdown files do not need the boilerplate header. For shared-skill docs under
`.agents/skills/`, also follow [shared skill authoring](../skills/authoring.md).
