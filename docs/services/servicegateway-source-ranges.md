# ServiceGateway source IP restrictions

For ServiceGateway LoadBalancer Services, set the existing source-range annotation:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: example
  annotations:
    service.beta.kubernetes.io/azure-allowed-ip-ranges: "20.0.0.0/8,203.0.113.7"
spec:
  type: LoadBalancer
  selector:
    app: example
  ports:
    - port: 80
      targetPort: 8080
      protocol: TCP
```

Comma-separated IPv4/IPv6 addresses and correctly masked CIDRs are accepted.
Bare addresses become `/32` or `/128`. An absent annotation adds no restriction;
an empty value or any malformed entry rejects the configuration with an
`InvalidAllowedIPRanges` Warning. Invalid updates do not replace an accepted policy.
There is no additional feature flag: within SGW mode, an absent annotation means
allow all sources for this policy.
For each backend address family, CCM's rules allow only matching source prefixes.
A family with no matching source prefix is denied by those rules. Higher-priority
NRMS operational rules retain their normal Azure precedence, as described below.
Existing limitations on Service address families, numeric target ports, and
protocols are unchanged.

## Enforcement

CCM creates one owned NSG per relevant primary node NIC, shared by that NIC's
IPv4 and IPv6 addresses. CCM's rules allow the specified sources, then deny other
sources to the selected **Pod IP, backend port, and protocol**. Under the SGW
packet model, the VIP has already been translated to this backend destination;
no separate VIP-destination rule is added. Direct traffic to that same backend
tuple is also affected. Preserving direct Pod access is not supported.

CCM's rules do not restrict non-target traffic. NRMS rules, subnet NSGs, network
policy, and other platform controls still apply. A matching higher-priority
NRMS allow can permit traffic outside the Service annotation, and a matching
NRMS deny can block traffic that CCM's rules allow. The annotation does not
override NRMS operational policy. The platform must preserve CCM's NIC association,
and subnet rules must permit the intended traffic.
NSGs are stateful: changing a rule does not necessarily terminate existing
connections; use new connections when checking a policy change.

Two Services cannot share a backend tuple when either restricts its source
addresses, even if their restrictive policies are identical. CCM reports
`SourceRangeConflict` rather than merging their policies. Services with no
effective restriction can continue sharing backends.

## Reconciliation and ownership

SGW registrations and Service-owned NSG rule descriptions use the canonical
`<Service UID>-lb` identity; Kubernetes object references and Warnings retain the
raw Service UID. Outbound registrations use `<egress label>-natgw`. NIC source
policies do not change managed/BYO NAT selection: an explicit BYO target never
falls back to a managed gateway, and its owner-controlled NAT Gateway and Public
IPs are not adopted or deleted. Membership correction preserves eligible BYO
references and RP-default services, including defaults discovered by `IsDefault`
rather than a fixed name.

LocationsUpdater performs NSG preparation, waits for confirmed rules and NIC
association, and then publishes eligible backend memberships. After an address
update is acknowledged, the same pass prunes obsolete protection. There is no
independent NSG worker, queue or retry loop. NSG failures retain pending work and
use the existing LocationsUpdater retry path; a pass is not successful while
its NSG work has failed. Affected new inbound memberships can also wait when
their policy is unrestricted but retained NIC rules cannot be confirmed.

Existing memberships remain published during failed policy updates. CCM keeps
the latest desired policy and retries forward; it does not roll back successful
NICs or withdraw existing backends to compensate for an NSG failure. Changes are
not atomic across NICs. Kubernetes Pod readiness is independent of this
controller; the ordering check controls SGW backend publication.

A replacement physical Node/NIC is a new protection obligation even if host and
Pod IP strings are reused. Old-owner membership is drained before the replacement
is allowed to inherit those addresses. This identity transition is separate from
an ordinary policy-update failure on an unchanged owner.

After an unacknowledged membership update, CCM sends the current full membership
of only the affected locations, preserving supported default/outbound references.
Normal successful updates remain incremental. This also restores an endpoint
that returns after an uncertain withdrawal, even if desired state matches the
last locally acknowledged state.
Pod cleanup finalizers also retain potentially published outbound addresses until
the corrective update is acknowledged, including no-IP deletion recovery. A
rejected correction keeps those drains on the existing bounded retry path.

Once the LB configuration is confirmed, source-only changes update the NSG
without changing the LB configuration.
Backend-port transitions retain protection for the previous port until the LB
update is confirmed. Unrestricted Services cannot change ports into another
Service's restriction. A safe unrestricted move away from a conflict can proceed
after CCM verifies the actual NIC policy for the requested LB target; that check
does not authorize new backends or discard old protection before the LB ACK.
Likewise, a recovered failed LB can retry a target already protected by the
settled NIC policy, without accepting its failed response as a complete port
baseline or rewriting NSG rules first.
Removed backends retain protection until their SGW
withdrawal is confirmed. Removing the annotation or Service removes its CCM rules,
preserves NRMS rules, and keeps the NIC NSG while the NIC exists. Node deletion
alone is not proof that its NIC is gone. Runtime retires the departed owner's
membership bookkeeping after its drain acknowledgment and leaves old NSG resource
deletion to initialization.
Node address changes are reconciled through the existing Node handler. If an
owner moves during an unresolved lookup and its last membership is withdrawn,
its NIC cleanup can wait for that Node observation or the next initialization.

Orphan discovery and cleanup run during initialization, not on a periodic timer.
Only owned NSGs with a missing NIC and no NIC/subnet associations are deleted.
If CCM stops during cleanup, the next initialization repeats the sweep; completed
deletions remain completed. Cleanup failures are reported as initialization errors.
A retained NSG whose NIC disappears later is checked
again at the next initialization. A returning Node's current NIC is inspected
for an existing managed NSG, even if it no longer has a source-restricted Service.

On the source-policy recovery path, CCM first acknowledges a full SGW membership
reconciliation while withholding additions that need unconfirmed protection.
Observed old rules remain until that corrective acknowledgment. This is a
startup-only recovery operation, not a periodic resync. Existing default-outbound
references and eligible inbound/outbound memberships are preserved.

Per-Service NSG work does not prevent the normal CCM controllers from starting.
LocationsUpdater performs that work afterward, so Service corrections and Node
initialization can progress through their existing handlers. After a restart,
new protected backends also wait for ServiceUpdater to confirm the LB
configuration; desired ports alone are not proof of the live LB ports.
Until that configuration is confirmed, CCM reads the live LB port baseline once
per relevant Service per reconciliation pass. Only a response in `Succeeded`
provisioning state establishes that baseline; unfinished or missing states retry.
Old NSG rules and retained attempts
remain additional protection obligations, not substitutes for live-port evidence.
An unrestricted recovered Service also needs settled live-port evidence before
its first membership on a policy-managed NIC; an unsettled startup listing cannot
prove that its live tuple avoids another Service's restriction.

The CCM identity needs NSG read/write/delete permissions and the appropriate
NIC or VMSS-instance read/write permissions. CCM can replace an absent NIC NSG
or the configured cluster NSG association; it does not adopt arbitrary customer
NSGs. Rules from the replaced cluster NSG are not copied. The NIC NSG remains
CCM-owned, with NRMS as the supported operational exception. Its name, SGW-owner
tag and NIC-identity tag must agree before it can be managed; a matching name
or generic CCM-managed marker alone is not ownership. Rules named
`NRMS-Rule-<priority>` (case-insensitive), whose numeric suffix matches their
priority in the range 100-499, are preserved without changing their configuration
or priority. They are not classified as CCM-owned. CCM's priorities remain
500-4096, so there is no priority relocation or overlap between these ranges.
Other unexpected custom rules, incomplete rules, and NRMS-shaped rules outside
that layout cause an explicit reconciliation error; they are neither adopted
nor overwritten. Name/layout recognition does not authenticate a writer; Azure
RBAC and the operational ownership contract still apply. Azure's built-in default
rules are separate from these custom rules.

The planner checks limits of 2,000 custom rules, 6,000 source entries, and 6,000
destination entries per NSG, including preserved NRMS entries. Capacity and Azure
failures produce `SourceRangeReconcileFailed` Warnings and are retried.
Manual modification of managed ownership or NIC associations is unsupported.
Classic, non-SGW LoadBalancer behavior is unchanged.
