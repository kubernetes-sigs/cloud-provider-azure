# ServiceGateway NIC Source Policies

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
Full NIC preparation records the canonical resolved NIC ARM ID and ResourceGUID
in retained publication ownership. A changed ID or known GUID retires matching
ACK and uncertain owners before subsequent NIC/NSG operations can fail. Node-only
events preserve known physical identity; LB-only observations cannot establish
it. Same-NIC errors, ETag changes and casing-only identity changes do not retire
ownership. Missing GUIDs cannot authorize new full preparation.
Every prepared NIC is re-resolved after the batch's writes, checking incarnation,
settled association and fresh Node identity before accepting its proof. Pruning
uses the same check. These are bounded per-NIC reads, not a cross-resource atomic
fence or a check per backend.
The owned NSG's `k8s-azure-nic-confirmed-guid` tag persists the confirmed generation.
Its conditional metadata PUT follows confirmed protection/association and waits
for conflicting current-location owners' withdrawal ACKs. A deferred stamp yields
no publishable plan, not LB-only authority. Requested tag values are checked in
the write response. First/new confirmation can add a PUT; unchanged generations
do not repeat it.
Startup reuses Node objects from its existing LIST, published memberships and the
owned NSG inventory. Pass-local primary-NIC observations prefer a current confirmed
owner over stale footprints, retire positive replacements before initial sync and
drain unresolved conflicting histories. Ordinary lookup failures preserve
unambiguous history. Legacy unstamped groups establish current ownership only
through settled attachment and exact rule round-trip evidence; an unconfirmed
first policy is not historical publication authority. A matching old NIC
attachment and settled round-tripping legacy rules can identify a retired ARM
target when a different current target is positively known; this grants only
drain authority, not a current GUID or publication permission. Required orphan ownership
metadata survives until retirement, including for rejected serving Services;
superseded physical owners do not supply current published-address policy claims.
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
