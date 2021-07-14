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
| `service.beta.kubernetes.io/azure-load-balancer-mode`        | `auto`, `{vmset-name}`    | Specify the Azure load balancer selection algorithm based on vm sets (VMSS or VMAS). There are currently three possible load balancer selection modes : default, auto or "{vmset-name}". This is only working for basic LB or multiple standard LB (see below for how it works) |  v1.10.0 and later |
| `service.beta.kubernetes.io/azure-dns-label-name`            | Name of the PIP DNS label        | Specify the DNS label name for the service's public IP address (PIP). If it is set to empty string, DNS in PIP would be deleted. Because of a bug, before v1.15.10/v1.16.7/v1.17.3, the DNS label on PIP would also be deleted if the annotation is not specified. | v1.15.0 and later |
| `service.beta.kubernetes.io/azure-shared-securityrule`       | `true` or `false`            | Specify that the service should be exposed using an Azure security rule that may be shared with another service, trading specificity of rules for an increase in the number of services that can be exposed. This relies on the Azure "augmented security rules" feature. | v1.10.0 and later |
| `service.beta.kubernetes.io/azure-load-balancer-resource-group` | Name of the PIP resource group   | Specify the resource group of the service's PIP that are not in the same resource group as the cluster. | v1.10.0 and later |
| `service.beta.kubernetes.io/azure-allowed-service-tags`      | List of allowed service tags | Specify a list of allowed [service tags](https://docs.microsoft.com/en-us/azure/virtual-network/security-overview#service-tags) separated by comma. | v1.11.0 and later |
| `service.beta.kubernetes.io/azure-load-balancer-tcp-idle-timeout` | TCP idle timeouts in minutes | Specify the time, in minutes, for TCP connection idle timeouts to occur on the load balancer. Default and minimum value is 4. Maximum value is 30. Must be an integer. |  v1.11.4, v1.12.0 and later |
| `service.beta.kubernetes.io/azure-pip-name` | Name of PIP | Specify the PIP that will be applied to load balancer | v1.16 and later |
| `service.beta.kubernetes.io/azure-load-balancer-disable-tcp-reset` | `true` | Disable `enableTcpReset` for SLB | v1.16-v1.18. The annotation has been deprecated and would be removed in a future release. |
| `service.beta.kubernetes.io/azure-pip-tags` | Tags of the PIP | Specify the tags of the PIP that will be associated to the load balancer typed service. [Doc](../tagging-resources) | v1.20 and later |
| `service.beta.kubernetes.io/azure-load-balancer-health-probe-protocol` | Health probe protocol of the load balancer typed service | Refer the detailed docs [here](../custom-health-probe) | v1.20 and later |
| `service.beta.kubernetes.io/azure-load-balancer-health-probe-request-path` | Request path of the health probe | Refer the detailed docs [here](../custom-health-probe) | v1.20 and later |
| `service.beta.kubernetes.io/azure-load-balancer-enable-high-availability-ports` | Enable [high availability ports](https://docs.microsoft.com/en-us/azure/load-balancer/load-balancer-ha-ports-overview) on internal SLB | HA ports is required when applications require IP fragments | v1.20 and later |
| `service.beta.kubernetes.io/azure-deny-all-except-load-balancer-source-ranges` | `true` or `false` | Deny all traffic to the service. This is helpful when the `service.Spec.LoadBalancerSourceRanges` is set to an internal load balancer typed service. When set the loadBalancerSourceRanges field on the service in order to whitelist ip src addresses, although the generated NSG has added the rules for loadBalancerSourceRanges, the default rule (65000) will allow any vnet traffic, basically meaning the whitelist is of no use. This annotation solves this issue. | v1.21 and later |
| `service.beta.kubernetes.io/azure-additional-public-ips` | External public IPs besides the service's own public IP | It is mainly used for global VIP on Azure cross-region LoadBalancer | v1.20 and later with out-of-tree cloud provider |

Please note that

* When `loadBalancerSourceRanges` have been set on service spec, `service.beta.kubernetes.io/azure-allowed-service-tags` won't work because of DROP iptables rules from kube-proxy. The CIDRs from service tags should be merged into `loadBalancerSourceRanges` to make it work.

### Load balancer selection modes

There are currently three possible load balancer selection modes :

1. Default mode - service has no annotation ("service.beta.kubernetes.io/azure-load-balancer-mode"). In this case the Loadbalancer of the primary Availability set is selected
2. "__auto__" mode - service is annotated with `__auto__` value. In this case, services would be associated with the Loadbalancer with the minimum number of rules.
3. "{vmset-name}" mode - service is annotated with the name of a VMSS/VMAS. In this case, only load balancers of the specified VMSS/VMAS would be selected, and services would be associated with the one with the minimum number of rules.

> Note that the "__auto__" mode is valid only if the service is newly created. It is not allowed to change the annotation value to `__auto__` of an existed service.

The selection mode for a load balancer only works for basic load balancers or multiple standard load balancers. Following is the detailed information of allowed number of VMSS/VMAS in a load balancer.

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

## Exclude nodes from the load balancer

> Excluding nodes from Azure LoadBalancer is supported since v1.20.0.

The kubernetes controller manager supports excluding nodes from the load balancer backend pools by enabling the feature gate `ServiceNodeExclusion`. To exclude nodes from Azure LoadBalancer, label `node.kubernetes.io/exclude-from-external-load-balancers=true` should be added to the nodes.

1. To use the feature, the feature gate `ServiceNodeExclusion` should be on (enabled by default since its beta on v1.19).

2. The labeled nodes would be excluded from the LB in the next LB reconcile loop, which needs one or more LB typed services to trigger. Basically, users could trigger the update by creating a service. If there are one or more LB typed services existing, no extra operations are needed.

3. To re-include the nodes, just remove the label and the update would be operated in the next LB reconcile loop.


## Using SCTP

SCTP protocol services are only supported on internal standard LoadBalancer, hence annotation `service.beta.kubernetes.io/azure-load-balancer-internal: "true"` should be added to SCTP protocol services. See below for an example:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: sctpservice
  annotations:
    service.beta.kubernetes.io/azure-load-balancer-internal: "true"
spec:
  type: LoadBalancer
  selector:
    app: sctpserver
  ports:
    - name: sctpserver
      protocol: SCTP
      port: 30102
      targetPort: 30102
```

## Load balancer limits

The limits of the load balancer related resources are listed below:

**Standard Load Balancer**

| Resource                                | Limit         |
|-----------------------------------------|-------------------------------|
| Load balancers                          | 1,000                         |
| Rules per resource                      | 1,500                         |
| Rules per NIC (across all IPs on a NIC) | 300                           |
| Frontend IP configurations              | 600                           |
| Backend pool size                       | 1,000 IP configurations, single virtual network |
| Backend resources per Load Balancer     | 150                   |
| High-availability ports                 | 1 per internal frontend       |
| Outbound rules per Load Balancer        | 600                           |
| Load Balancers per VM                   | 2 (1 Public and 1 internal)   |

The limit is up to 150 resources, in any combination of standalone virtual machine resources, availability set resources, and virtual machine scale-set placement groups.

**Basic Load Balancer**

| Resource                                | Limit        |
|-----------------------------------------|------------------------------|
| Load balancers                          | 1,000                        |
| Rules per resource                      | 250                          |
| Rules per NIC (across all IPs on a NIC) | 300                          |
| Frontend IP configurations              | 200                          |
| Backend pool size                       | 300 IP configurations, single availability set |
| Availability sets per Load Balancer     | 1                            |
| Load Balancers per VM                   | 2 (1 Public and 1 internal)  |

> There is a restriction of 300 rules per NIC, hence for single SLB mode 300 services are allowed at most. If more services are required, try to enable [multiple SLBs](../multiple-slb).
