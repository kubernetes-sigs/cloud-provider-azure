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
* The cluster name set for `kube-controller-manager --cluster-name=<cluster-name>` MUST not end with `-internal`.

After the cluster is provisioned, cloud provider Azure MAY update the following Azure resources based on workloads:

* New routes would be added for each node if `--configure-cloud-routes` is enabled.
* New LoadBalancer (including external and internal) would be created if they're not existing yet.
* Virtual machines and virtual machine scale sets would be added to LoadBalancer backend address pools if they're not added yet.
* New public IPs and NSG rules would be added when LoadBalancer typed services are created.
