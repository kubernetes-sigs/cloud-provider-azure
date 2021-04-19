---
title: "Cluster Provisioning Tools Contract"
linkTitle: "Cluster Provisioning Tools Contract"
weight: 1
type: docs
description: >
    Cloud provider assumptions on Azure resources that provisioning tools should follow.
---

> The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD", "SHOULD NOT", "RECOMMENDED",  "MAY", and "OPTIONAL" in this document are to be interpreted as described in RFC 2119.

Here is a list of Azure resource assumptions that are required for cloud provider Azure:

* All Azure resources MUST be under the same tenant.
* All virtual machine names MUST be the same as their hostname.
* Node LoadBalancer's names SHOULD be following rules (`<clusterName>` is coming from `--cluster-name` configuration, default is `kubernetes`)
  * When `enableMultipleStandardLoadBalancers` is configured to `false`, LoadBalancer's name SHOULD be `<clusterName>` for external type and `<clusterName>-internal` for internal type.
  * When `enableMultipleStandardLoadBalancers` is configured to `true`, multiple standard load balancers SHOULD be provisioned:
    * All the virtual machines MUST be part of either VirtualMachineScaleSet (VMSS) or AvailabilitySet (VMAS).
    * Each VMAS and VMSS SHOULD be put behind a different standard LoadBalancer.
    * The primary LoadBalancer's name SHOULD be `<clusterName>` for external type and `<clusterName>-internal` for internal type. Virtual machines that are part of primary VMAS (set by `primaryAvailabilitySetName`) or primary VMSS (set by `primaryScaleSetName`) SHOULD be added to primary LoadBalancer backend address pool.
    * Other standard LoadBalancer's name SHOULD be same as VMAS or VMSS name.
* The cluster name set for `kube-controller-manager --cluster-name=<cluster-name>` MUST not end with `-internal`.

After the cluster is provisioned, cloud provider Azure MAY update the following Azure resources based on workloads:

* New routes would be added for each node if `--configure-cloud-routes` is enabled.
* New LoadBalancer (including external and internal) would be created if they're not existing yet.
* Virtual machines and virtual machine scale sets would be added to LoadBalancer backend address pools if they're not added yet.
* New public IPs and NSG rules would be added when LoadBalancer typed services are created.
