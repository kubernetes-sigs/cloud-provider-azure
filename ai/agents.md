# Cloud Provider Azure — AI Agent Guide

## Project Overview

This is the Kubernetes out-of-tree cloud provider for Azure, implementing the
[cloud provider interface](https://github.com/kubernetes/cloud-provider). It
produces three binaries:

- **cloud-controller-manager** — manages Azure resources for a K8s cluster
- **cloud-node-manager** — runs on each node, manages node-level cloud operations
- **acr-credential-provider** — provides Azure Container Registry credentials

## Architecture

```
cmd/
  cloud-controller-manager/     Entry point for CCM
  cloud-node-manager/           Entry point for CNM
  acr-credential-provider/      Entry point for ACR credential provider

pkg/
  provider/                     Core cloud provider implementation
    azure_loadbalancer*.go        Load balancer reconciliation
    azure_standard.go             Standard (single-VM) support
    azure_vmss*.go                VMSS (scale set) support
    azure_vmssflex*.go            VMSS Flex support
    azure_routes.go               Route table management
    azure_zones.go                Availability zone logic
    azure_instances_v1.go         Instance metadata (v1)
    azure_instances_v2.go         Instance metadata (v2)
    azure_controller_*.go         Disk attach/detach (Standard, VMSS, VMSSFlex)
    azure_backoff.go              Backoff/retry logic
    azure_lock.go                 Concurrent access locking
    azure_*_repo.go               Data access / repository pattern
    azure_fakes.go                Test fakes
    azure_mock_*.go               Generated mocks
  azclient/                     Azure SDK client wrappers — don't call Azure SDK directly
  consts/                       Shared constants — all constants go here
  cache/                        Caching layer for Azure API responses
  node/                         Node management helpers
  nodemanager/                  Node lifecycle management
  nodeipam/                     Node IP address management
  credentialprovider/           ACR credential provider logic
  util/                         Shared utilities
  log/                          Logging helpers
  metrics/                      Prometheus metrics
  trace/                        Tracing helpers
  version/                      Version info
```

## Unit Tests

```sh
make test-unit                        # Run all unit tests
go test -v ./pkg/provider/...         # Run tests for a specific folder
```

If your changes are restricted to one folder, prefer `go test -v ./<folder>/...`
over `make test-unit` for faster feedback.

## Building Images

Before building images, ask the user for `IMAGE_REGISTRY` and `IMAGE_TAG`.

```sh
IMAGE_REGISTRY=<registry> IMAGE_TAG=<tag> make build-ccm-image   # Build cloud-controller-manager image
IMAGE_REGISTRY=<registry> IMAGE_TAG=<tag> make build-cnm-image   # Build cloud-node-manager image
```

## Code Conventions

- **Constants**: all constants live in `pkg/consts/` — do not scatter magic strings
- **Azure API calls**: go through `pkg/azclient/` wrappers — never call the Azure SDK directly
- **Mocks**: use existing `azure_mock_*.go` (generated) and `azure_fakes.go` (manual) files
- **Tests**: table-driven tests preferred — match the style in the file you're editing
- **Repository pattern**: `azure_*_repo.go` files abstract data access — follow this pattern

## Logging Conventions

Kubernetes uses [klog](https://github.com/kubernetes/klog) for logging, a permanent fork
of glog. The project is migrating to [logr](https://github.com/go-logr/logr) as its logging
interface. klog is then only used to manage logging configuration — its direct logging calls
(`klog.Infof`, `klog.Errorf`, `klog.InfoS`) should not be used in new code.

This codebase has a logger wrapper in `pkg/log/` that provides `logr.Logger` instances
backed by klog. **New code should use this wrapper, not direct klog calls.**

### Obtaining a Logger

Use `pkg/log` to get a `logr.Logger` from context:

```go
import "sigs.k8s.io/cloud-provider-azure/pkg/log"

// From context (preferred) — returns background logger if none in context
logger := log.FromContextOrBackground(ctx)

// Enrich with name and key/value pairs for tracing
logger = logger.WithName("ReconcileLB").WithValues("service", serviceName)

// Store enriched logger back into context for downstream use
ctx = log.NewContext(ctx, logger)
```

### How to Log

The `logr.Logger` interface provides:

- `logger.V(level).Info(msg, keysAndValues...)` — always use a V-level for info logging
- `logger.Error(err, msg, keysAndValues...)` — when admins must take action

Use `logger.Error` only to inform admins they might need to fix a problem.
If there is no `error` instance, pass `nil`.

Do not emit an error log before returning an error — it is usually uncertain if and how the
caller will handle the returned error. It might handle it gracefully, making an error log
inappropriate. Instead, use `fmt.Errorf` with `%w` to return a more informative error.
Avoid "failed to" as prefix when wrapping errors — it quickly becomes repetitive.

Sometimes it may be useful to log an error for debugging purposes before returning it.
Use an info log with at least `V(4)` and `"err"` as key for the key/value pair.

### When Not to Log

Shared libraries should not log errors themselves but just return `error`, because
client libraries may be used in CLI UIs that wish to control output.

### Passing Loggers to Components

For helper types that need logging, accept `logr.Logger` as a parameter and
enrich with `.WithName()`:

```go
func NewAccessControl(logger logr.Logger, svc *v1.Service, sg *armnetwork.SecurityGroup) {
    logger = logger.WithName("AccessControl").WithValues("security-group", ptr.To(sg.Name))
    // ...
}
```

### Message Style

- Start with a capital letter
- Do not end the message with a period
- Use active voice. Use complete sentences when there is an acting subject
  ("A could not do B") or omit the subject if it would be the program itself ("Could not do B")
- Use past tense ("Could not delete B" instead of "Cannot delete B")
- When referring to an object, state what type of object it is ("Deleted pod" instead of "Deleted")

### V-Level Conventions

| Level | Usage                                                                     |
|-------|---------------------------------------------------------------------------|
| V(0)  | Always visible — programmer errors, logging extra info about a panic, CLI argument handling |
| V(1)  | Reasonable default — config info, errors that repeat frequently relating to correctable conditions |
| V(2)  | Recommended default — HTTP requests/exit codes, system state changes (killing pod), controller state change events, scheduler log messages |
| V(3)  | Extended information about changes                                        |
| V(4)  | Debug level — logging in particularly thorny parts of code                |
| V(5)  | Trace level — context to understand the steps leading up to errors and warnings |

The practical default level is V(2). Dev and QE environments may run at V(3) or V(4).

### Logging Formats

**Text format** (default): human-readable, maintains backward compatibility with klog format.
Marshals objects using `fmt.Stringer` interface (e.g., `pod="kube-system/kube-dns"`).
Supports multi-line strings with actual line breaks.

**JSON format** (via `--logging-format=json`): machine-readable, optimized for log
consumption, processing and querying. Special keys: `ts` (Unix timestamp, float),
`v` (verbosity, int), `err` (error string, optional), `msg` (message string).

### Examples

```go
// Obtain and enrich logger
logger := log.FromContextOrBackground(ctx).WithName(Operation).WithValues("service", svcName)
ctx = log.NewContext(ctx, logger)

// Verbosity-controlled info logging
logger.V(2).Info("Start reconciling Service", "lb", lbName)

// Error logging (only when operator must take action)
logger.Error(err, "Failed to reconcile LoadBalancer")

// Debug-level error for investigation (use Info with "err" key, not Error)
logger.V(4).Info("Could not fetch instance metadata", "err", err, "node", nodeName)
```

## Reference Docs

Check these docs when working in the relevant area. These docs should be updated once the logics have been changed.

| Topic | Doc | When to check |
|-------|-----|---------------|
| ETag & cache invalidation | [ai/references/etag-cache.md](references/etag-cache.md) | Modifying Azure resource create/update/delete operations, or changing cache logic |

## Shared Skills

Shared reusable skills live under `ai/skills/` and are the only repo-tracked
skill source.

- Do not commit `.codex/`, `.claude/`, `.github/skills`, or other local
  agent-specific skill folders.
- To bootstrap local skill usage, manually link
  `ai/skills/onboard-local-skills` into the local agent skills directory once.
- After bootstrap, use the `onboard-local-skills` shared skill or
  `make link-ai-skills TARGET_DIR=<dir>` to link all or selected shared skills.
- When a user asks to onboard skills, prefer the shared onboarding script over
  ad hoc symlink commands.
