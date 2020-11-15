---
title: "Azure LoadBalancer"
linkTitle: "Azure LoadBalancer"
weight: 2
type: docs
description: >
    Azure LoadBalancer basics.
---

The way Azure defines a LoadBalancer is different from GCE or AWS. Azure's LB can have multiple frontend IP refs. GCE and AWS only allow one, if you want more, you would need multiple LBs. Since Public IP's are not part of the LB in Azure, an NSG is not part of the LB in Azure either. However, you cannot delete them in parallel, a Public IP can only be deleted after the LB's frontend IP ref is removed.

The different Azure Resources such as LB, Public IP, and NSG are the same tier of Azure resources and circular dependencies need to be avoided. In other words, they should only depend on service state.

By default the basic SKU is selected for a load balancer. Services can be annotated to allow auto selection of available load balancers. Service annotations can also be used to provide specific availability sets that host the load balancers. Note that in case of auto selection or specific availability set selection, services are currently not auto-reassigned to an available loadbalancer when the availability set is lost in case of downtime or cluster scale down.

## LoadBalancer annotations

Below is a list of annotations supported for Kubernetes services with type `LoadBalancer`:

| Annotation | Value | Description | Kubernetes Version |
| ------------------------------------------------------------ | ---------------------------- | ------------------------------------------------------------ |------|
| `service.beta.kubernetes.io/azure-load-balancer-internal`    | `true` or `false`            | Specify whether the load balancer should be internal. It’s defaulting to public if not set. | v1.10.0 and later |
| `service.beta.kubernetes.io/azure-load-balancer-internal-subnet` | Name of the subnet           | Specify which subnet the internal load balancer should be bound to. It’s defaulting to the subnet configured in cloud config file if not set. | v1.10.0 and later |
| `service.beta.kubernetes.io/azure-load-balancer-mode`        | `auto`, `{name1},{name2}`    | Specify the Azure load balancer selection algorithm based on availability sets. There are currently three possible load balancer selection modes : default, auto or "{name1}, {name2}". This is only working for basic LB (see below for how it works) |  v1.10.0 and later |
| `service.beta.kubernetes.io/azure-dns-label-name`            | Name of the PIP DNS label        | Specify the DNS label name for the service's public IP address (PIP). If it is set to empty string, DNS in PIP would be deleted. Because of a bug, before v1.15.10/v1.16.7/v1.17.3, the DNS label on PIP would also be deleted if the annotation is not specified. | v1.15.0 and later |
| `service.beta.kubernetes.io/azure-shared-securityrule`       | `true` or `false`            | Specify that the service should be exposed using an Azure security rule that may be shared with another service, trading specificity of rules for an increase in the number of services that can be exposed. This relies on the Azure "augmented security rules" feature. | v1.10.0 and later |
| `service.beta.kubernetes.io/azure-load-balancer-resource-group` | Name of the PIP resource group   | Specify the resource group of the service's PIP that are not in the same resource group as the cluster. | v1.10.0 and later |
| `service.beta.kubernetes.io/azure-allowed-service-tags`      | List of allowed service tags | Specify a list of allowed [service tags](https://docs.microsoft.com/en-us/azure/virtual-network/security-overview#service-tags) separated by comma. | v1.11.0 and later |
| `service.beta.kubernetes.io/azure-load-balancer-tcp-idle-timeout` | TCP idle timeouts in minutes | Specify the time, in minutes, for TCP connection idle timeouts to occur on the load balancer. Default and minimum value is 4. Maximum value is 30. Must be an integer. |  v1.11.4, v1.12.0 and later |
| `service.beta.kubernetes.io/azure-pip-name` | Name of PIP | Specify the PIP that will be applied to load balancer | v1.16 and later |
| `service.beta.kubernetes.io/azure-load-balancer-disable-tcp-reset` | `true` | Disable `enableTcpReset` for SLB | v1.16-v1.18. The annotation has been deprecated and would be removed in a future release. |
| `service.beta.kubernetes.io/azure-pip-tags` | Tags of the PIP | Specify the tags of the PIP that will be associated to the load balancer typed service. [Doc](../tagging-resources) | v1.20 and later |
| `service.beta.kubernetes.io/azure-load-balancer-health-probe-protocol` | Health probe protocol of the load balancer typed service. [Doc](../custom-health-probe) | v1.20 and later |
| `service.beta.kubernetes.io/azure-load-balancer-health-probe-request-path` | Request path of the health probe. [Doc](../custom-health-probe) | v1.20 and later |

### Load balancer selection modes

There are currently three possible load balancer selection modes :

1. Default mode - service has no annotation ("service.beta.kubernetes.io/azure-load-balancer-mode"). In this case the Loadbalancer of the primary Availability set is selected
2. "__auto__" mode - service is annotated with __auto__ value, this when loadbalancer from any availability set is selected which has the minimum rules associated with it.
3. "{name1}, {name2}" mode - this is when the load balancer from the specified availability sets is selected that has the minimum rules associated with it.

The selection mode for a load balancer only works for Azure's basic SKU (see below) because of the difference in backend pool endpoints:

* Standard SKU supports any virtual machine in a single virtual network, including a mix of virtual machines, availability sets, and virtual machine scale sets. So all the nodes would be added to the same standard LB backend pool with a max size of 1000.
* Basic SKU only supports virtual machines in a single availability set, or a virtual machine scale set. Only nodes with the same availability set or virtual machine scale set would be added to the basic LB backend pool.

## LoadBalancer SKUs

Azure cloud provider supports both `basic` and `standard` SKU load balancers, which can be set via `loadBalancerSku` option in [cloud config file](../../install/configs). A list of differences between these two SKUs can be found [here](https://docs.microsoft.com/en-us/azure/load-balancer/load-balancer-standard-overview#why-use-standard-load-balancer).

> Note that the public IPs used in load balancer frontend configurations should be the same SKU. That is a standard SKU public IP for standard load balancer and a basic SKU public IP for a basic load balancer.

Azure doesn’t support a network interface joining load balancers with different SKUs, hence migration dynamically between them is not supported.

> If you do require migration, please delete all services with type `LoadBalancer` (or change to other type)

### Outbound connectivity

[Outbound connectivity](https://docs.microsoft.com/en-us/azure/load-balancer/load-balancer-outbound-connections
) is also different between the two load balancer SKUs:

* For the basic SKU, the outbound connectivity is opened by default. If multiple frontends are set, then the outbound IP is selected randomly (and configurable) from them.

* For the standard SKU, the outbound connectivity is disabled by default. There are two ways to open the outbound connectivity: use a standard public IP with the standard load balancer or define outbound rules.

### Standard LoadBalancer

Because the load balancer in a Kubernetes cluster is managed by the Azure cloud provider, and it may change dynamically (e.g. the public load balancer would be deleted if no services defined with type `LoadBalancer`), [outbound rules](https://docs.microsoft.com/en-us/azure/load-balancer/load-balancer-outbound-rules-overview) are the recommended path if you want to ensure the outbound connectivity for all nodes.

> Especially note:
>
> * In the context of outbound connectivity, a single standalone VM, all the VM's in an Availability Set, all the instances in a VMSS behave as a group. This means, if a single VM in an Availability Set is associated with a Standard SKU, all VM instances within this Availability Set now behave by the same rules as if they are associated with Standard SKU, even if an individual instance is not directly associated with it.
>
> * Public IP's used as instance-level public IP are mutually exclusive with outbound rules.

Here is the recommended way to define the [outbound rules](https://docs.microsoft.com/en-us/azure/load-balancer/load-balancer-outbound-rules-overview) when using separate provisioning tools:

* Create a separate IP (or multiple IPs for scale) in a standard SKU for outbound rules. Make use of the [allocatedOutboundPorts](https://docs.microsoft.com/en-us/azure/load-balancer/load-balancer-outbound-rules-overview#snatports) parameter to allocate sufficient ports for your desired scenario scale.
* Create a separate pool definition for outbound, and ensure all virtual machines or VMSS virtual machines are in this pool. Azure cloud provider will manage the load balancer rules with another pool, so that provisioning tools and the Azure cloud provider won't affect each other.
* Define inbound with load balancing rules and inbound NAT rules as needed, and set `disableOutboundSNAT` to true on the load balancing rule(s).  Don't rely on the side effect from these rules for outbound connectivity. It makes it messier than it needs to be and limits your options.  Use inbound NAT rules to create port forwarding mappings for SSH access to the VM's rather than burning public IPs per instance.

## Exclude nodes from the load balancer (v1.20.0)

The kubernetes controller manager supports excluding nodes from the load balancer backend pools by enabling the feature gate `ServiceNodeExclusion`, which is in beta state since v1.19. This PR let users to exclude nodes from the LB by labeling `node.kubernetes.io/exclude-from-external-load-balancers=true` on the nodes. There are several things that need to be mentioned.

1. To use the feature, the feature gate `ServiceNodeExclusion` should be on.

2. The labeled nodes would be excluded from the LB in the next LB reconcile loop, which needs one or more LB typed services to trigger. Basically, users could trigger the update by creating a service. If there are one or more LB typed services existing, no extra operations are needed.

3. To re-include the nodes, just remove the label and the update would be operated in the next LB reconcile loop.