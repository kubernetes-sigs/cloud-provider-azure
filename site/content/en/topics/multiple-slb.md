---
title: "Multiple Standard LoadBalancer per cluster"
linkTitle: "Multiple Standard LoadBalancer per cluster"
weight: 8
type: docs
---

> This feature is supported since v1.20.0.

There is only one external and one internal Standard Load Balancer (SLB) at most per cluster. Set `enableMultipleStandardLoadBalancers=true` in the cloud config if you want to turn on the multiple SLB mode. Similar to the basic LB, there will be a 1:1 mapping between each SLB and VMSS/VMAS. The SLB of the primary VMSS/VMAS will be named after `clusterName` in the cloud config (in AKS, it would be `kubernetes`) while the name of those belonging to non-primary VMSS/VMAS will be the name of the corresponding vmSet.

> If the cluster provisioning tools like ASK-Engine and CAPZ don't proactively create a dedicated SLB for each VMSS/VMAS when enabling multiple SLB, only the primary SLB would be created. You could manually trigger the creation by setting the service annotation `service.beta.kubernetes.io/azure-load-balancer-mode` to bind the service to that VMSS/VMAS. The dedicated SLB would be created once the service reconcile loop is done. Unlike the primary SLB, there is no default outbound rules/IPs for the non-primary SLBs. That means the SLB would be deleted once all the services referencing it are deleted.

## Choose which SLB to use

The service annotation `service.beta.kubernetes.io/azure-load-balancer-mode` will be respected as long as `enableMultipleStandardLoadBalancers=true` when using standard LB, and the usage is the same as it is in the basic LB clusters. Specifically, there are three selection mode: `default` to select the primary SLB; `__auto__` to select the SLB with minimum rules and `vmSetName` to select the dedicated SLB of that VMSS/VMAS.

## Outbound Connections of non-primary VMSS/VMAS

The outbound rules of the non-primary SLB are not managed by cloud provider azure. Instead, it should be managed by cluster provisioning tools. For now, there is no outbound configuration for the non-primary VMSS/VMAS, but we plan to support customized outbound configurations in AKS and CAPZ in the future.

## Sharing the primary SLB with multiple VMSS/VMAS

> This feature is supported since v1.21.0

For each non-primary VMSS/VMAS, one can determine to use dedicated SLB or share the primary SLB. If the VMSS/VMAS names are in the cloud config `nodepoolsWithoutDedicatedSLB`, those would join the backend pool of the primary SLB while the others would remain to have dedicated SLBs. If the VMSS/VMAS supposed to share the primary SLB owns a dedicated SLB, the dedicated one would be deleted, and the VMSS/VMAS would be joint the primary SLB's backend pool.
