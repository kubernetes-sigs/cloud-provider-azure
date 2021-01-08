---
title: "Multiple Standard LoadBalancer per cluster"
linkTitle: "Multiple Standard LoadBalancer per cluster"
weight: 8
type: docs
---

> This feature is supported since v1.20.0.

## Scenarios

Currently, there are three ways to determine which basic LB should be used by a load balancer typed service: default mode, auto mode, and `vmSetName` mode, ref: https://kubernetes-sigs.github.io/cloud-provider-azure/topics/loadbalancer/. However, the standard LB doesn't support the mode selection annotation. The reason is that every vm in the vnet could be added to the backend pool of the SLB, including vms from different node pools (VMAS/VMSS). Hence, for SLB the selection is not needed.

However, there are scenarios that we need multiple SLBs in the cluster. For example, users could set different outbound IPs for different  VMAS or VMSS. In addition, more services are supported with multiple SLBs because there is a 4Mb restriction in an ARM request body.

Basically, we would like to support the mode selection annotation (`service.beta.kubernetes.io/azure-load-balancer-mode`) with SLB, similar to what it is with basic LB.

## User experience

Currently, only one SLB is allowed per cluster. If users set the mode selection annotation when using the SLB, they would get an error. After this feature implemented, the users could choose if they want a single SLB (current behavior), or they want multiple SLBs, with one SLB binding to one VMAS/VMSS. If the annotation is not set, everything is unchanged: every agent node in the cluster would join the backend pool of the SLB and only one SLB is allowed. If the annotation is set to `__auto__` or `vmSetName`, there would be a 1: 1 mapping between the SLBs and the VMAS/VMSS. Specifically, for `__auto__` mode, the SLB with the minimum load balancing rules would be selected to serve the newly created service; for `vmSetName` mode, the SLB of the given  VMAS/VMSS would be selected. In this way, users could set different outbound rules for different VMAS/VMSS. If there is no SLB of the given VMAS/VMSS, a new SLB would be created.

## Implementation

There are three selection mode of the SLB: `default`, `__auto__` and `vmSetName`. If the annotation `service.beta.kubernetes.io/azure-load-balancer-mode` is not set for the SLB cluster, the behavior would be the same as it is now. If it is set to `__auto__`, new services should choose the SLB with minimum rules. If it is set to `vmSetName`, new services should choose the SLB  of the given VMAS or VMSS.

The following are the implementations in detail.

1. Introduce a new cloud provider config `enableMultipleStandardLoadBalancers`, default to `false`. When `enableMultipleStandardLoadBalancers=false`, the mode selection annotation `service.beta.kubernetes.io/azure-load-balancer-mode` would be ignored when using the SLB.

2. When `enableMultipleStandardLoadBalancers=true`:

    - If the annotation `service.beta.kubernetes.io/azure-load-balancer-mode` is not set, the SLB of the primary VMAS/VMSS would be selected for the services.

    - If `service.beta.kubernetes.io/azure-load-balancer-mode = __auto__`, the SLB with minimum rules would be selected for the services.

    - If `service.beta.kubernetes.io/azure-load-balancer-mode = vmSetName`, the SLB of the given VMAS/VMSS would be selected for the services. If there is no corresponding SLB, the cloud provider would start to create one with the naming format `vmSetname` or `vmSetName-internal`.

The behaviors of other load balancing resources, e.g., load balancing rules, health probes, frontend IP configurations, etc., would keep unchanged.
