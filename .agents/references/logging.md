# Logging Conventions

Kubernetes uses `klog` for logging configuration, but this project is migrating
to `logr` as its logging interface. New code should not use direct `klog.Infof`,
`klog.Errorf`, or `klog.InfoS` calls.

This codebase has a logger wrapper in `pkg/log/` that provides `logr.Logger`
instances backed by `klog`. New code should use this wrapper.

## Obtaining a Logger

Use `pkg/log` to get a `logr.Logger` from context:

```go
import "sigs.k8s.io/cloud-provider-azure/pkg/log"

logger := log.FromContextOrBackground(ctx)
logger = logger.WithName("ReconcileLB").WithValues("service", serviceName)
ctx = log.NewContext(ctx, logger)
```

## How to Log

- Use `logger.V(level).Info(msg, keysAndValues...)` for info logging.
- Always use a V-level for info logging.
- Use `logger.Error(err, msg, keysAndValues...)` only when admins must take action.
- If there is no `error` instance for `logger.Error`, pass `nil`.
- Do not emit an error log before returning an error; the caller may handle it gracefully.
- Return informative errors with `fmt.Errorf` and `%w`.
- Avoid `"failed to"` as a wrapping prefix because it quickly becomes repetitive.
- For diagnostic errors before returning, use an info log at least at `V(4)` and put
  the error under the `"err"` key.

## Shared Libraries

Shared libraries should not log errors themselves. They should return `error`
so clients, including CLI UIs, can control output.

## Passing Loggers to Components

For helper types that need logging, accept `logr.Logger` as a parameter and
enrich with `.WithName()`:

```go
func NewAccessControl(logger logr.Logger, svc *v1.Service, sg *armnetwork.SecurityGroup) {
    logger = logger.WithName("AccessControl").WithValues("security-group", ptr.To(sg.Name))
    // ...
}
```

## Message Style

- Start messages with a capital letter.
- Do not end messages with a period.
- Use active voice.
- Use complete sentences when there is an acting subject, such as
  `"A could not do B"`.
- Omit the subject if it would be the program itself, such as `"Could not do B"`.
- Use past tense, such as `"Could not delete B"` instead of `"Cannot delete B"`.
- When referring to an object, state the object type, such as `"Deleted pod"`.

## V-Level Conventions

| Level | Usage |
|-------|-------|
| V(0) | Always visible: programmer errors, logging extra info about a panic, CLI argument handling |
| V(1) | Reasonable default: config info, errors that repeat frequently relating to correctable conditions |
| V(2) | Recommended default: HTTP requests/exit codes, system state changes, controller state changes, scheduler log messages |
| V(3) | Extended information about changes |
| V(4) | Debug level for thorny code paths |
| V(5) | Trace level for context leading to errors and warnings |

The practical default level is `V(2)`. Dev and QE environments may run at
`V(3)` or `V(4)`.

## Logging Formats

Text format is the default. It is human-readable, maintains backward
compatibility with `klog` format, marshals objects using `fmt.Stringer`, and
supports multi-line strings with actual line breaks.

JSON format is available via `--logging-format=json`. It is machine-readable
and optimized for log consumption, processing, and querying. Special keys are
`ts` for Unix timestamp, `v` for verbosity, `err` for optional error string,
and `msg` for message string.

## Example

```go
logger := log.FromContextOrBackground(ctx).WithName(Operation).WithValues("service", svcName)
ctx = log.NewContext(ctx, logger)

logger.V(2).Info("Start reconciling Service", "lb", lbName)
logger.Error(err, "Could not reconcile LoadBalancer")
logger.V(4).Info("Could not fetch instance metadata", "err", err, "node", nodeName)
```
