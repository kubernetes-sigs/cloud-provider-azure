---
name: build-images
description: Build cloud-provider-azure container images through the repo Makefile with explicit IMAGE_TAG and IMAGE_REGISTRY inputs, optional make flag overrides, and opt-in bounded Docker or Podman retries. Use when the user wants to build CCM, CNM, health-probe-proxy, CCM e2e, or root CCM/CNM aggregate images, or mentions build-ccm-image, build-node-image-linux, build images, image registry, or image tag.
---

# Build Images

## Workflow

Use this skill to build cloud-provider-azure images from the repository
Makefiles. Ask for `IMAGE_TAG` and `IMAGE_REGISTRY` when either value is
missing.

Replace `<SKILL_DIR>` with the path to this skill directory.

Dry-run the default CCM command:

```bash
python3 <SKILL_DIR>/scripts/build_image.py \
  --image ccm \
  --tag <tag> \
  --registry <registry> \
  --dry-run
```

Build the default CCM image:

```bash
python3 <SKILL_DIR>/scripts/build_image.py \
  --image ccm \
  --tag <tag> \
  --registry <registry>
```

The default CCM command is:

```bash
IMAGE_TAG=<tag> IMAGE_REGISTRY=<registry> GOEXPERIMENT=nosystemcrypto ENABLE_GIT_COMMAND=false make build-ccm-image
```

## Image Aliases

Pass one of these values to `--image`:

| Alias | Make target | Directory |
|-------|-------------|-----------|
| `ccm` | `build-ccm-image` | repo root |
| `ccm-all` | `build-all-ccm-images` | repo root |
| `cnm`, `cnm-linux` | `build-node-image-linux` | repo root |
| `cnm-windows` | `build-node-image-windows` | repo root |
| `cnm-windows-hpc` | `build-node-image-windows-hpc` | repo root |
| `cnm-all` | `build-all-node-images` | repo root |
| `ccm-e2e` | `build-ccm-e2e-test-image` | repo root |
| `hpp` | `build-health-probe-proxy-image` | `health-probe-proxy/` |
| `hpp-windows` | `build-health-probe-proxy-image-windows` | `health-probe-proxy/` |
| `all` | `image` | repo root |

`all` maps to the root `make image` target. It builds the root CCM/CNM
aggregate only; it does not build `hpp`, `hpp-windows`, or `ccm-e2e`.

`cnm-all` maps to the raw root `build-all-node-images` aggregate. Because that
aggregate includes Windows image targets with host-side Go builds, the helper
does not default `GOEXPERIMENT` for `cnm-all`.

The helper invokes health-probe-proxy builds with `make -B` so each `hpp` or
`hpp-windows` image rebuilds its host binary before invoking Buildx. This
prevents a binary left by an earlier branch or remediation cycle from being
reused in a verification image, including when the target checkout has an older
Makefile.

Do not use this skill for acr-credential-provider images. This repo exposes the
acr-credential-provider as a binary build, not an image build target.

## Flags

The helper always sets `IMAGE_TAG`, `IMAGE_REGISTRY`, and
`ENABLE_GIT_COMMAND=false` unless a default is explicitly removed with
`--unset`.

The helper defaults `GOEXPERIMENT=nosystemcrypto` only for `ccm`, `ccm-all`,
`cnm`, and `cnm-linux`. For other aliases, pass it explicitly when desired:

```bash
python3 <SKILL_DIR>/scripts/build_image.py \
  --image hpp \
  --tag <tag> \
  --registry <registry> \
  --set GOEXPERIMENT=nosystemcrypto
```

Use repeated `--set KEY=VALUE` arguments to add or override make variables:

```bash
python3 <SKILL_DIR>/scripts/build_image.py \
  --image cnm \
  --tag <tag> \
  --registry <registry> \
  --set ARCH=arm64 \
  --set BUILDX_EXTRA_FLAGS=--no-cache
```

Use repeated `--unset KEY` arguments to remove default flags:

```bash
python3 <SKILL_DIR>/scripts/build_image.py \
  --image ccm \
  --tag <tag> \
  --registry <registry> \
  --unset GOEXPERIMENT \
  --unset ENABLE_GIT_COMMAND
```

`IMAGE_TAG` and `IMAGE_REGISTRY` are required inputs and cannot be unset.
They also cannot be overridden with `--set`; use `--tag` and `--registry`.
Passing the same key to both `--set` and `--unset` is rejected.

For a deterministic local Linux amd64 verification build, explicitly set
`ARCH=amd64` and `OUTPUT_TYPE=docker`, then unset inherited `OUTPUT_FLAG` and
`BUILDX_EXTRA_FLAGS`. This prevents caller environment from selecting another
architecture, registry output, or push-oriented Buildx flags.

Use `--repo` when the target checkout differs from the checkout containing the
skill. This is required when a caller snapshots the skill before switching the
target worktree to another branch.

Make control variables that can override command-line or environment values are
reserved. Do not pass `MAKEFLAGS`, `MFLAGS`, `GNUMAKEFLAGS`, `MAKEOVERRIDES`,
or `MAKEFILES` with `--set`; the helper rejects those keys and removes inherited
values before running `make`.

## Transient Runtime Retries

Use `--retry-transient-runtime-errors` only when the caller wants the helper to
retry one recognized transient runtime failure. This option requires an
explicit `--set CONTAINER_CLI=<path-to-docker-or-podman>` so classification and
the retry use the same selected runtime:

```bash
python3 <SKILL_DIR>/scripts/build_image.py \
  --image ccm \
  --tag <tag> \
  --registry <registry> \
  --set CONTAINER_CLI=/absolute/path/to/podman \
  --retry-transient-runtime-errors
```

The helper streams build output while retaining only the last 50 stderr lines
for classification. It waits five seconds, then retries the identical working
directory, command, and environment once in these cases:

- Docker Buildx setup failed because concurrent builder creation raced.
- Podman failed during registry access or image transfer with a temporary DNS
  error, connection timeout or reset, TLS handshake timeout, or registry HTTP
  429 or 5xx response. Before retrying, the helper requires the same Podman
  executable's `info` check to succeed within ten seconds.

The helper does not retry authentication or authorization, missing manifests
or digests, disk-capacity, compilation, Dockerfile or Makefile,
builder-configuration, interruption, or unclassified failures. A failed retry
is returned directly; there is no third attempt. `--dry-run` never builds,
checks runtime health, sleeps, or includes the retry option in the resolved
Make command.

## Validation

Use `--dry-run` when the user asks for the command, when checking flag changes,
or before running an expensive build. The script prints the resolved working
directory and shell-quoted command. When the helper removes a default or
sanitizes inherited managed environment, dry-runs show that as `env -u KEY`.
Without `--dry-run`, it prints the same resolved working directory and command.
The default path runs `make` once via `subprocess.run` without `shell=True`; the
opt-in retry path streams stderr via `subprocess.Popen` without `shell=True` so
it can classify and replay bounded failure evidence.
