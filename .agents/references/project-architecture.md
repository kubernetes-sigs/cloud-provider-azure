# Project Architecture

This repository is the Kubernetes out-of-tree cloud provider for Azure,
implementing the cloud provider interface. It produces three binaries:

- **cloud-controller-manager**: manages Azure resources for a Kubernetes cluster
- **cloud-node-manager**: runs on each node and manages node-level cloud operations
- **acr-credential-provider**: provides Azure Container Registry credentials

## Layout

```text
cmd/
  cloud-controller-manager/     Entry point for CCM
  cloud-node-manager/           Entry point for CNM
  acr-credential-provider/      Entry point for ACR credential provider

pkg/
  provider/                     Core cloud provider implementation
    azure_loadbalancer*.go        Load balancer reconciliation
    azure_standard.go             Standard single-VM support
    azure_vmss*.go                VMSS scale set support
    azure_vmssflex*.go            VMSS Flex support
    azure_routes.go               Route table management
    azure_zones.go                Availability zone logic
    azure_instances_v1.go         Instance metadata v1
    azure_instances_v2.go         Instance metadata v2
    azure_controller_*.go         Disk attach/detach for Standard, VMSS, VMSSFlex
    azure_backoff.go              Backoff and retry logic
    azure_lock.go                 Concurrent access locking
    azure_*_repo.go               Data access and repository pattern
    servicegateway/              SGW runtime, informer wiring, and difftracker
    azure_fakes.go                Manual test fakes
    azure_mock_*.go               Generated mocks
  azclient/                     Azure SDK client wrappers; do not call Azure SDK directly
  consts/                       Shared constants
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

SGW NIC source policies live in `servicegateway/difftracker/nsg_*.go`. The runtime
supplies existing node/compute clients; classic NSG and NIC helpers are unchanged.
VM and NIC reads use the compute wrappers directly so cancellation is preserved.
NIC policy functions use the existing DiffTracker and its lock; there is no
separate NSG reconciler/client-wrapper object. Compute and node-cache dependencies
are constructor arguments stored directly alongside DiffTracker's other clients.
LocationsUpdater calls the plain functions in `nsg_reconcile.go`: prepare
rules/association, publish addresses, then prune after acknowledgement. A
dirty-location set coalesces changes; each pass returns unfinished work through
one finalization path. There is
no NSG worker, workqueue, lifecycle or independent retry scheduler. Direct
NSG/NIC/VM I/O remains outside the lock and uses the LocationsUpdater attempt
context/backoff. Shared VMSS cache refill retains its existing background context.
New protected bindings wait for acknowledged rules/association; confirmed and
uncertain SGW publications retain their protection until withdrawal is confirmed.
Owned orphan NSGs are swept during initialization. There is no periodic NSG
inventory or orphan polling; interrupted cleanup is repeated by the next startup.
Accepted policies and LB-port obligations belong to the shared Service operation
record. Active requests use its existing `InFlightConfig`; only recovered or
retired-attempt ports need separate retention. Detailed Node/NIC models, current
rules, and protection confirmations are pass-local. Publication owner identity
and its drain obligation belong to the existing location record, not another
Node or alias index. A replacement owner cannot inherit publication or LB-update
permission merely because its IP strings match. The common owner-retirement
transition invalidates affected Service permissions, including changes discovered
through live reads. The feature's Node entry delegates endpoint relocation to the
unchanged baseline `ReconcileNodeIPChange` implementation.
Successful NIC cleanup retires its current location keys, not another NIC's
retry key that happens to match a historical location.
An LB update cannot introduce a restricted shared tuple merely because that
Service lacks the annotation. If live-port reads or rule planning fail before a
write, a validated current-policy observation can certify requested LB targets
that are already protected as requested or are effectively unrestricted.
Only acknowledged/possibly published bindings participate in that check;
unpublished desired backends cannot block repair of an already-protected target.
The existing prepared-NIC record marks this evidence `lbOnly`; it cannot publish
new memberships, prune rules or retire work. The original error still retries,
and the existing all-NIC gate is the only publisher of `NSGPrepared`.
Both full and LB-only coverage must match any retained publication owner; a new
NIC's confirmation cannot release an old owner's unacknowledged withdrawal.
The original location calculator and public diagnostic getters remain unchanged.
The actual publisher selects `getSyncNICPolicyLocationsLocked` when NIC-policy
clients are configured; that separate calculation handles eligible deltas,
affected-location corrections after unknown outcomes, and startup recovery.
Baseline diagnostic output is not NIC publication permission. The journal
retains unresolved scopes and possible additions beyond the ACK baseline;
default/outbound references are included in corrections. No per-address removal
journal or second desired-state model is maintained.
The NIC-policy path uses canonical inbound `<UID>-lb` and outbound
`<label>-natgw` identities, preserving dynamic RP-default references through
`isReservedServiceLocked`. Kubernetes warning objects use the decoded raw UID,
not the canonical SGW name. BYO selection, conditional link/unlink and managed
resource ownership checks remain in the existing outbound path; NIC preparation
does not adopt or delete the owner NAT Gateway or its Public IPs.
Pod drain checks include the same uncertain-publication journal as Service drains,
and pending Pod deletions keep terminally rejected corrections retrying. Recovered
unrestricted bindings require a settled live-LB port read even before their first
publication on a policy-managed NIC. Protected new bindings still wait for the
existing applied-LB handshake.
Node Ready-only changes do not request NSG work.
Unexpected custom rules on an owned NSG are rejected, not merged or overwritten.
Service policy retirement uses the existing `DeleteService` path and startup
recovery, not a separate Service informer handler.
No additional feature flag controls source filtering; an absent annotation adds
no SGW restriction. Initial work is queued before the existing workers start.
Generic startup Service reconciliation and ownership/admission diagnostics retain
their baseline implementations. A feature extension seeds registered inbound
Services through ordinary `AddService`, fills operation metadata, and reuses the
already fetched LB models as retained port observations. While LastAppliedConfig is
unknown, a context-bound live LB read in `Succeeded` provisioning state supplies
the actual baseline once per Service per pass; unfinished responses, historical
rules or attempted ports do not replace that read.
Node-cache absence or mismatch is checked against the live Node API before
publication ownership is retired. Unresolved lookup failures preserve existing
memberships and pending cleanup work. A dirty location with neither a cached
current Node nor retained ownership uses one live Node-list fallback for that
pass; there is no separate discovery loop. An initialization withdrawal ACK also
re-drives any LB update whose last preparation obligation disappeared, without
running NSG I/O during startup. Node address movement still uses the existing
Node event handler. If an unresolved owner moves before its last membership ACK
retires its identity, cleanup can wait for that Node observation or startup;
location-only work cannot reconstruct a discarded owner-to-address relationship.
Source-policy recovery acknowledges a full membership snapshot before pruning
observed-only rules. Per-Service NSG work follows ordinary initialization so it
cannot block the controllers that supply corrected intent or Node identities.
New protected membership still waits for confirmed NSG and LB configuration.
