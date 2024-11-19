---
title: "Azure LoadBalancer"
linkTitle: "Azure LoadBalancer"
weight: 2
type: docs
description: Azure LoadBalancer basics.
---

The way Azure defines a LoadBalancer is different from GCE or AWS. Azure's LB can have multiple frontend IP refs. GCE and AWS only allow one, if you want more, you would need multiple LBs. Since Public IP's are not part of the LB in Azure, an NSG is not part of the LB in Azure either. However, you cannot delete them in parallel, a Public IP can only be deleted after the LB's frontend IP ref is removed.

The different Azure Resources such as LB, Public IP, and NSG are the same tier of Azure resources and circular dependencies need to be avoided. In other words, they should only depend on service state.

By default the basic SKU is selected for a load balancer. Services can be annotated to allow auto selection of available load balancers. Service annotations can also be used to provide specific availability sets that host the load balancers. Note that in case of auto selection or specific availability set selection, services are currently not auto-reassigned to an available loadbalancer when the availability set is lost in case of downtime or cluster scale down.

## LoadBalancer annotations

Below is a list of annotations supported for Kubernetes services with type `LoadBalancer`:

| Annotation                                                                      | Value                                                                                                                                  | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                 | Kubernetes Version                               |
| ------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------ |
| `service.beta.kubernetes.io/azure-load-balancer-internal`                       | `true` or `false`                                                                                                                      | Specify whether the load balancer should be internal. It's defaulting to public if not set.                                                                                                                                                                                                                                                                                                                                                                                 | v1.10.0 and later                                |
| `service.beta.kubernetes.io/azure-load-balancer-internal-subnet`                | Name of the subnet                                                                                                                     | Specify which subnet the internal load balancer should be bound to. It's defaulting to the subnet configured in cloud config file if not set.                                                                                                                                                                                                                                                                                                                               | v1.10.0 and later                                |
| `service.beta.kubernetes.io/azure-load-balancer-mode`                           | `auto`, `{vmset-name}`                                                                                                                 | Specify the Azure load balancer selection algorithm based on vm sets (VMSS or VMAS). There are currently three possible load balancer selection modes : default, auto or "{vmset-name}". This is only working for basic LB (see below for how it works)                                                                                                                                                                                             | v1.10.0 and later                                |
| `service.beta.kubernetes.io/azure-dns-label-name`                               | Name of the PIP DNS label                                                                                                              | Specify the DNS label name for the service's public IP address (PIP). If it is set to empty string, DNS in PIP would be deleted. Because of a bug, before v1.15.10/v1.16.7/v1.17.3, the DNS label on PIP would also be deleted if the annotation is not specified.                                                                                                                                                                                                          | v1.15.0 and later                                |
| `service.beta.kubernetes.io/azure-shared-securityrule`                          | `true` or `false`                                                                                                                      | Specify that the service should be exposed using an Azure security rule that may be shared with another service, trading specificity of rules for an increase in the number of services that can be exposed. This relies on the Azure "augmented security rules" feature.                                                                                                                                                                                                   | v1.10.0 and later                                |
| `service.beta.kubernetes.io/azure-load-balancer-resource-group`                 | Name of the PIP resource group                                                                                                         | Specify the resource group of the service's PIP that are not in the same resource group as the cluster.                                                                                                                                                                                                                                                                                                                                                                     | v1.10.0 and later                                |
| `service.beta.kubernetes.io/azure-allowed-service-tags`                         | List of allowed service tags                                                                                                           | Specify a list of allowed [service tags](https://docs.microsoft.com/en-us/azure/virtual-network/security-overview#service-tags) separated by comma.                                                                                                                                                                                                                                                                                                                         | v1.11.0 and later                                |
| `service.beta.kubernetes.io/azure-allowed-ip-ranges`                            | List of allowed IP ranges                                                                                                              | Specify a list of allowed IP ranges separated by comma.                                                                                                                                                                                                                                                                                                                                                                                                                     | v1.29.0 and later                                |
| `service.beta.kubernetes.io/azure-load-balancer-tcp-idle-timeout`               | TCP idle timeouts in minutes                                                                                                           | Specify the time, in minutes, for TCP connection idle timeouts to occur on the load balancer. Default and minimum value is 4. Maximum value is 30. Must be an integer.                                                                                                                                                                                                                                                                                                      | v1.11.4, v1.12.0 and later                       |
| `service.beta.kubernetes.io/azure-pip-name`                                     | Name of PIP                                                                                                                             | Specify the PIP that will be applied to load balancer. It is used for IPv4 or IPv6 in a single stack cluster.                                                                                                                                                                                                                                                                                                                                                                               | v1.16 and later                                  |
| `service.beta.kubernetes.io/azure-pip-name-ipv6`                                | Name of IPv6 PIP                                                                                                                       | After v1.27, specify the IPv6 PIP that will be applied to load balancer in a dual stack cluster. For single stack clusters, this annotation will be ignored.                                                                                                                                                                                                                                                                                                                                                                                                                 | v1.27 and later                                  |
| `service.beta.kubernetes.io/azure-pip-prefix-id`                                | ID of Public IP Prefix                                                                                                                 | Specify the Public IP Prefix that will be applied to load balancer. It is for IPv4 or IPv6 in a single stack cluster.                                                                                                                                                                                                                                                                                                                                                                       | v1.21 and later with out-of-tree cloud provider  |
| `service.beta.kubernetes.io/azure-pip-prefix-id-ipv6`                           | ID of IPv6 Public IP Prefix                                                                                                            | After v1.27, specify the IPv6 Public IP Prefix that will be applied to load balancer in a dual stack cluster. For single stack clusters, this annotation will be ignored.                                                                                                                                                                                                                                                                                                                                                                                                    | v1.27 and later                                  |
| `service.beta.kubernetes.io/azure-pip-tags`                                     | Tags of the PIP                                                                                                                        | Specify the tags of the PIP that will be associated to the load balancer typed service. [Doc](../tagging-resources)                                                                                                                                                                                                                                                                                                                                                         | v1.20 and later                                  |
| `service.beta.kubernetes.io/azure-load-balancer-health-probe-interval`          | Health probe interval                                                                                                                  | Refer to the detailed docs [here](#custom-load-balancer-health-probe)                                                                                                                                                                                                                                                                                                                                                                                                       | v1.21 and later  with out-of-tree cloud provider |
| `service.beta.kubernetes.io/azure-load-balancer-health-probe-num-of-probe`      | The minimum number of unhealthy responses of health probe                                                                              | Refer to the detailed docs [here](#custom-load-balancer-health-probe)                                                                                                                                                                                                                                                                                                                                                                                                       | v1.21 and later  with out-of-tree cloud provider |
| `service.beta.kubernetes.io/azure-load-balancer-health-probe-request-path`      | Request path of the health probe                                                                                                       | Refer to the detailed docs [here](#custom-load-balancer-health-probe)                                                                                                                                                                                                                                                                                                                                                                                                       | v1.20 and later  with out-of-tree cloud provider |
| `service.beta.kubernetes.io/azure-load-balancer-ipv4`                           | Load balancer IPv4 address                                                                                                             | Specify the load balancer IP of IPv4, deprecating Service.spec.loadBalancerIP                                                                                                                                                                                                                                                                                                                                                                                               | v1.21 and later                                  |
| `service.beta.kubernetes.io/azure-load-balancer-ipv6`                           | Load balancer IPv6 address                                                                                                             | Specify the load balancer IP of IPv6, deprecating Service.spec.loadBalancerIP                                                                                                                                                                                                                                                                                                                                                                                               | v1.21 and later                                  |
| `service.beta.kubernetes.io/port_{port}_no_lb_rule`                             | true/false                                                                                                                             | {port} is the port number in the service. When it is set to true, no lb rule and health probe rule for this port will be generated. health check service should not be exposed to the public internet(e.g. istio/envoy health check service)                                                                                                                                                                                                                                | v1.24 and later with out-of-tree cloud provider  |
| `service.beta.kubernetes.io/port_{port}_no_probe_rule`                          | true/false                                                                                                                             | {port} is the port number in the service. When it is set to true, no health probe rule for this port will be generated.                                                                                                                                                                                                                                                                                                                                                     | v1.24 and later with out-of-tree cloud provider  |
| `service.beta.kubernetes.io/port_{port}_health-probe_protocol`                  | Health probe protocol                                                                                                                  | {port} is the port number in the service. Explicit protocol for the health probe for the service port {port}, overriding port.appProtocol if set.  Refer to the detailed docs [here](#custom-load-balancer-health-probe)                                                                                                                                                                                                                                                    | v1.24 and later with out-of-tree cloud provider  |
| `service.beta.kubernetes.io/port_{port}_health-probe_port`                      | port number or port name in service manifest                                                                                           | {port} is the port number in the service. Explicit port for the health probe for the service port {port}, overriding the default value.  Refer to the detailed docs [here](#custom-load-balancer-health-probe)                                                                                                                                                                                                                                                              | v1.24 and later with out-of-tree cloud provider  |
| `service.beta.kubernetes.io/port_{port}_health-probe_interval`                  | Health probe interval                                                                                                                  | {port} is port number of service.  Refer to the detailed docs [here](#custom-load-balancer-health-probe)                                                                                                                                                                                                                                                                                                                                                                    | v1.21 and later  with out-of-tree cloud provider |
| `service.beta.kubernetes.io/port_{port}_health-probe_num-of-probe`              | The minimum number of unhealthy responses of health probe                                                                              | {port} is port number of service. Refer to the detailed docs [here](#custom-load-balancer-health-probe)                                                                                                                                                                                                                                                                                                                                                                     | v1.21 and later with out-of-tree cloud provider  |
| `service.beta.kubernetes.io/port_{port}_health-probe_request-path`              | Request path of the health probe                                                                                                       | {port} is port number of service.  Refer to the detailed docs [here](#custom-load-balancer-health-probe)                                                                                                                                                                                                                                                                                                                                                                    | v1.20 and later with out-of-tree cloud provider  |
| `service.beta.kubernetes.io/azure-load-balancer-enable-high-availability-ports` | Enable [high availability ports](https://docs.microsoft.com/en-us/azure/load-balancer/load-balancer-ha-ports-overview) on internal SLB | HA ports is required when applications require IP fragments                                                                                                                                                                                                                                                                                                                                                                                                                 | v1.20 and later                                  |
| `service.beta.kubernetes.io/azure-deny-all-except-load-balancer-source-ranges`  | `true` or `false`                                                                                                                      | Deny all traffic to the service. This is helpful when the `service.Spec.LoadBalancerSourceRanges` is set to an internal load balancer typed service. When set the loadBalancerSourceRanges field on the service in order to whitelist ip src addresses, although the generated NSG has added the rules for loadBalancerSourceRanges, the default rule (65000) will allow any vnet traffic, basically meaning the whitelist is of no use. This annotation solves this issue. | v1.21 and later                                  |
| `service.beta.kubernetes.io/azure-additional-public-ips`                        | External public IPs besides the service's own public IP                                                                                | It is mainly used for global VIP on Azure cross-region LoadBalancer                                                                                                                                                                                                                                                                                                                                                                                                         | v1.20 and later with out-of-tree cloud provider  |
| `service.beta.kubernetes.io/azure-disable-load-balancer-floating-ip`            | `true` or `false`                                                                                                                      | Disable [Floating IP configuration](https://docs.microsoft.com/en-us/azure/load-balancer/load-balancer-floating-ip) for load balancer                                                                                                                                                                                                                                                                                                                                       | v1.21 and later with out-of-tree cloud provider  |
| `service.beta.kubernetes.io/azure-pip-ip-tags`                                  | comma separated key-value pairs `a=b,c=d`, for example `RoutingPreference=Internet`                                                    | Refer to the [doc](https://learn.microsoft.com/en-us/javascript/api/@azure/arm-network/iptag?view=azure-node-latest)                                                                                                                                                                                                                                                                                                                                                        | v1.21 and later with out-of-tree cloud provider  |

Please note that

* When `loadBalancerSourceRanges` have been set on service spec, `service.beta.kubernetes.io/azure-allowed-service-tags` won't work because of DROP iptables rules from kube-proxy. The CIDRs from service tags should be merged into `loadBalancerSourceRanges` to make it work.
* When `allocateLoadBalancerNodePorts` is set to `false`, ensure the following conditions are met:
  * Set `externalTrafficPolicy` to `Local`.
  * And enable the FloatingIP feature by either not setting annotation `service.beta.kubernetes.io/azure-disable-load-balancer-floating-ip`, or setting its value to false.

### Setting LoadBalancer IP

If you want to specify an IP address for the load balancer, there are two ways:

* Recommended: Set Service annotations `service.beta.kubernetes.io/azure-load-balancer-ipv4` for an IPv4 address and `service.beta.kubernetes.io/azure-load-balancer-ipv6` for an IPv6 address. Dual-stack support will be implemented soon. It is highly recommended for new Services.
* Deprecating: Set Service field: `Service.Spec.LoadbalancerIP`. This field is deprecating following [upstream kubernetes](https://github.com/kubernetes/kubernetes/pull/107235) and it cannot support dual-stack. However, current usage remains the same and existing Services are expected to work without modification.

### Load balancer selection modes

This is only useful for cluster with basic SKU load balancers. There are currently three possible load balancer selection modes:

1. Default mode - service has no annotation ("service.beta.kubernetes.io/azure-load-balancer-mode"). In this case the Loadbalancer of the primary Availability set is selected
2. "__auto__" mode - service is annotated with `__auto__` value. In this case, services would be associated with the Loadbalancer with the minimum number of rules.
3. "{vmset-name}" mode - service is annotated with the name of a VMSS/VMAS. In this case, only load balancers of the specified VMSS/VMAS would be selected, and services would be associated with the one with the minimum number of rules.

> Note that the "__auto__" mode is valid only if the service is newly created. It is not allowed to change the annotation value to `__auto__` of an existed service.

The selection mode for a load balancer only works for basic load balancers. Following is the detailed information of allowed number of VMSS/VMAS in a load balancer.

* Standard SKU supports any virtual machine in a single virtual network, including a mix of virtual machines, availability sets, and virtual machine scale sets. So all the nodes would be added to the same standard LB backend pool with a max size of 1000.
* Basic SKU only supports virtual machines in a single availability set, or a virtual machine scale set. Only nodes with the same availability set or virtual machine scale set would be added to the basic LB backend pool.

## LoadBalancer SKUs

Azure cloud provider supports both `basic` and `standard` SKU load balancers, which can be set via `loadBalancerSku` option in [cloud config file](../../install/configs). A list of differences between these two SKUs can be found [here](https://docs.microsoft.com/en-us/azure/load-balancer/load-balancer-standard-overview#why-use-standard-load-balancer).

> Note that the public IPs used in load balancer frontend configurations should be the same SKU. That is a standard SKU public IP for standard load balancer and a basic SKU public IP for a basic load balancer.

Azure doesn't support a network interface joining load balancers with different SKUs, hence migration dynamically between them is not supported.

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

### Limitations

* Excluding nodes from LoadBalancer is not supported on AKS managed nodes.

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

## Custom Load Balancer health probe

As documented [here](https://docs.microsoft.com/en-us/azure/load-balancer/load-balancer-custom-probe-overview), Tcp, Http and Https are three protocols supported by load balancer service.

Currently, the default protocol of the health probe varies among services with different transport protocols, app protocols, annotations and external traffic policies.

1. for local services, HTTP and /healthz would be used. The health probe will query NodeHealthPort rather than actual backend service
1. for cluster TCP services, TCP would be used.
1. for cluster UDP services, no health probes.

> Note: For local services with PLS integration and PLS proxy protocol enabled, the default HTTP+/healthz health probe does not work. Thus health probe can be customized the same way as cluster services to support this scenario. For more details, please check [PLS Integration Note](../pls-integration).

Since v1.20, service annotation `service.beta.kubernetes.io/azure-load-balancer-health-probe-request-path` is introduced to determine the health probe behavior.

* For clusters <=1.23, `spec.ports.appProtocol` would only be used as probe protocol when `service.beta.kubernetes.io/azure-load-balancer-health-probe-request-path` is also set.
* For clusters >1.24,  `spec.ports.appProtocol` would be used as probe protocol and `/` would be used as default probe request path (`service.beta.kubernetes.io/azure-load-balancer-health-probe-request-path` could be used to change to a different request path).

Note that the request path would be ignored when using TCP or the `spec.ports.appProtocol` is empty. More specifically:

| loadbalancer sku | `externalTrafficPolicy` | spec.ports.Protocol | spec.ports.AppProtocol | `service.beta.kubernetes.io/azure-load-balancer-health-probe-request-path` | LB Probe Protocol                 | LB Probe Request Path       |
| ---------------- | ----------------------- | ------------------- | ---------------------- | -------------------------------------------------------------------------- | --------------------------------- | --------------------------- |
| standard         | local                   | any                 | any                    | any                                                                        | http                              | `/healthz`                  |
| standard         | cluster                 | udp                 | any                    | any                                                                        | null                              | null                        |
| standard         | cluster                 | tcp                 |                        | (ignored)                                                                  | tcp                               | null                        |
| standard         | cluster                 | tcp                 | tcp                    | (ignored)                                                                  | tcp                               | null                        |
| standard         | cluster                 | tcp                 | http/https             |                                                                            | TCP(<=1.23) or http/https(>=1.24) | null(<=1.23) or `/`(>=1.24) |
| standard         | cluster                 | tcp                 | http/https             | `/custom-path`                                                             | http/https                        | `/custom-path`              |
| standard         | cluster                 | tcp                 | unsupported protocol   | `/custom-path`                                                             | tcp                               | null                        |
| basic            | local                   | any                 | any                    | any                                                                        | http                              | `/healthz`                  |
| basic            | cluster                 | tcp                 |                        | (ignored)                                                                  | tcp                               | null                        |
| basic            | cluster                 | tcp                 | tcp                    | (ignored)                                                                  | tcp                               | null                        |
| basic            | cluster                 | tcp                 | http                   |                                                                            | TCP(<=1.23) or http/https(>=1.24) | null(<=1.23) or `/`(>=1.24) |
| basic            | cluster                 | tcp                 | http                   | `/custom-path`                                                             | http                              | `/custom-path`              |
| basic            | cluster                 | tcp                 | unsupported protocol   | `/custom-path`                                                             | tcp                               | null                        |

Since v1.21, two service annotations `service.beta.kubernetes.io/azure-load-balancer-health-probe-interval` and `load-balancer-health-probe-num-of-probe` are introduced, which customize the configuration of health probe. If `service.beta.kubernetes.io/azure-load-balancer-health-probe-interval` is not set, Default value of 5 is applied. If `load-balancer-health-probe-num-of-probe` is not set, Default value of 2 is applied. And total probe should be less than 120 seconds.


### Custom Load Balancer health probe for port
Different ports in a service may require different health probe configurations. This could be because of service design (such as a single health endpoint controlling multiple ports), or Kubernetes features like the [MixedProtocolLBService](https://kubernetes.io/docs/concepts/services-networking/service/#load-balancers-with-mixed-protocol-types).

The following annotations can be used to customize probe configuration per service port.

| port specific annotation                                         | global probe annotation                                                  | Usage                                                                        |
| ---------------------------------------------------------------- | ------------------------------------------------------------------------ | ---------------------------------------------------------------------------- |
| service.beta.kubernetes.io/port_{port}_no_lb_rule                | N/A (no equivalent globally)                                             | if set true, no lb rules and probe rules will be generated                   |
| service.beta.kubernetes.io/port_{port}_no_probe_rule             | N/A (no equivalent globally)                                             | if set true, no probe rules will be generated                                |
| service.beta.kubernetes.io/port_{port}_health-probe_protocol     | N/A (no equivalent globally)                                             | Set the health probe protocol for this service port (e.g. Http, Https, Tcp)  |
| service.beta.kubernetes.io/port_{port}_health-probe_port         | N/A (no equivalent globally)                                             | Sets the health probe port for this service port (e.g. 15021)                |
| service.beta.kubernetes.io/port_{port}_health-probe_request-path | service.beta.kubernetes.io/azure-load-balancer-health-probe-request-path | For Http or Https, sets the health probe request path. Defaults to /         |
| service.beta.kubernetes.io/port_{port}_health-probe_num-of-probe | service.beta.kubernetes.io/azure-load-balancer-health-probe-num-of-probe | Number of consecutive probe failures before the port is considered unhealthy |
| service.beta.kubernetes.io/port_{port}_health-probe_interval     | service.beta.kubernetes.io/azure-load-balancer-health-probe-interval     | The amount of time between probe attempts                                    |

For following manifest, probe rule for port httpsserver is different from the one for httpserver because annotations for port httpsserver are specified.

```yaml
apiVersion: v1
kind: Service
metadata:
  name: appservice
  annotations:
    service.beta.kubernetes.io/azure-load-balancer-health-probe-num-of-probe: "5"
    service.beta.kubernetes.io/port_443_health-probe_num-of-probe: "4"
spec:
  type: LoadBalancer
  selector:
    app: server
  ports:
    - name: httpserver
      protocol: TCP
      port: 80
      targetPort: 30102
    - name: httpsserver
      protocol: TCP
      appProtocol: HTTPS
      port: 443
      targetPort: 30104
```

In this manifest, the https ports use a different node port, an HTTP readiness check at port 10256 on /healthz(healthz endpoint of kube-proxy).
```yaml
apiVersion: v1
kind: Service
metadata:
  name: istio
  annotations:
    service.beta.kubernetes.io/azure-load-balancer-internal: "true"
    service.beta.kubernetes.io/port_443_health-probe_protocol: "http"
    service.beta.kubernetes.io/port_443_health-probe_port: "10256"
    service.beta.kubernetes.io/port_443_health-probe_request-path: "/healthz"
spec:
  ports:
    - name: https
      protocol: TCP
      port: 443
      targetPort: 8443
      nodePort: 30104
      appProtocol: https
  selector:
    app: istio-ingressgateway
    gateway: istio-ingressgateway
    istio: ingressgateway
  type: LoadBalancer
  sessionAffinity: None
  externalTrafficPolicy: Local
  ipFamilies:
    - IPv4
  ipFamilyPolicy: SingleStack
  allocateLoadBalancerNodePorts: true
  internalTrafficPolicy: Cluster
```

In this manifest, the https ports use a different health probe endpoint, an HTTP readiness check at port 30000 on /healthz/ready.
```yaml
apiVersion: v1
kind: Service
metadata:
  name: istio
  annotations:
    service.beta.kubernetes.io/azure-load-balancer-internal: "true"
    service.beta.kubernetes.io/port_443_health-probe_protocol: "http"
    service.beta.kubernetes.io/port_443_health-probe_port: "30000"
    service.beta.kubernetes.io/port_443_health-probe_request-path: "/healthz/ready"
spec:
  ports:
    - name: https
      protocol: TCP
      port: 443
      targetPort: 8443
      appProtocol: https
  selector:
    app: istio-ingressgateway
    gateway: istio-ingressgateway
    istio: ingressgateway
  type: LoadBalancer
  sessionAffinity: None
  externalTrafficPolicy: Local
  ipFamilies:
    - IPv4
  ipFamilyPolicy: SingleStack
  allocateLoadBalancerNodePorts: true
  internalTrafficPolicy: Cluster
```

### Probing kube-proxy with a shared health probe

> This feature is supported since v1.28.5

In `externalTrafficPolicy: Local`, SLB directly probes kube-proxy – the thing it is directing traffic to. If kube-proxy is experiencing an issue on a given node, this will be visible on the healthCheckNodePort and SLB will stop sending traffic to this node.

In `externalTrafficPolicy: Cluster`, the probes are directed to the backend application, and thus SLB can only know kube-proxy's health indirectly – by whether the probes are forwarded to a backend application and answered successfully. This indirection causes confusion and causes problems in multiple different ways.

It is provided since v1.28.5 that a shared health probe can be used to probe kube-proxy. This feature is enabled by setting `clusterServiceLoadBalancerHealthProbeMode: "shared"` in the cloud provider configuration. When this feature is enabled, the health probe will be configured to probe kube-proxy on the healthCheckNodePort. This will allow SLB to directly probe kube-proxy and thus detect kube-proxy issues more quickly and accurately. The customization options are listed as below:

| Configuration                                     | Default           | Description                                                                                                                                                                                      |
|---------------------------------------------------|-------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `clusterServiceLoadBalancerHealthProbeMode`       | `servicenodeport` | Supported values are `shared` and `servicenodeport`. All ETP cluster service will share one health probe if `shared` is set. Otherwise, each ETP cluster service will have its own health probe. |
| `clusterServiceSharedLoadBalancerHealthProbePort` | 10256             | Default to kube-proxy healthCheckNodePort.                                                                                                                                                       |
| `clusterServiceSharedLoadBalancerHealthProbePath` | /healthz          | Default to kube-proxy health check path.                                                                                                                                                         |

When a service is integrated with a private link service and uses the proxy protocol, the health check requests to the kube-proxy will fail. A new `cloud-node-manager` sidecar `health-probe-proxy` is introduced to solve this issue. The sidecar will forward the health check requests to the kube-proxy and return the response to the load balancer. The sidecar will read these requests, parse the proxy protocol header, and forward the request to the kube-proxy. If the proxy protocol is not used, this daemonset will forward the request to the kube-proxy without any modification. To enable `health-probe-proxy` sidecar, turn on `cloudNodeManager.enableHealthProbeProxy` in the helm chart, or deploy it as a daemonset manually. To read more, check [this documentation](https://github.com/kubernetes-sigs/cloud-provider-azure/blob/master/health-probe-proxy/README.md).

## Configure Load Balancer backend

> This feature is supported since v1.23.0

The backend pool type can be configured by specifying `loadBalancerBackendPoolConfigurationType` in the cloud configuration file. There are three possible values:

1. `nodeIPConfiguration` (default). In this case we attach nodes to the LB by calling the VMSS/NIC API to associate the corresponding node IP configuration with the LB backend pool.
2. `nodeIP`. In this case we attach nodes to the LB by calling the LB API to add the node private IP addresses to the LB backend pool.
3. `podIP` (not supported yet). In this case we do not attach nodes to the LB. Instead we directly adding pod IPs to the LB backend pool.

To migrate from one backend pool type to another, just change the value of `loadBalancerBackendPoolConfigurationType` and re-apply the cloud configuration file. There will be downtime during the migration process.

### Migration API from `nodeIPConfiguration` to `nodeIP`

> This feature is supported since v1.24.0

The migration from `nodeIPConfiguration` to `nodeIP` can be done without downtime by configuring `"enableMigrateToIPBasedBackendPoolAPI": true` in the cloud configuration file.

```bash

## Load balancer limits

The limits of the load balancer related resources are listed below:

**Standard Load Balancer**

| Resource                                | Limit                                           |
| --------------------------------------- | ----------------------------------------------- |
| Load balancers                          | 1,000                                           |
| Rules per resource                      | 1,500                                           |
| Rules per NIC (across all IPs on a NIC) | 300                                             |
| Frontend IP configurations              | 600                                             |
| Backend pool size                       | 1,000 IP configurations, single virtual network |
| Backend resources per Load Balancer     | 150                                             |
| High-availability ports                 | 1 per internal frontend                         |
| Outbound rules per Load Balancer        | 600                                             |
| Load Balancers per VM                   | 2 (1 Public and 1 internal)                     |

The limit is up to 150 resources, in any combination of standalone virtual machine resources, availability set resources, and virtual machine scale-set placement groups.

**Basic Load Balancer**

| Resource                                | Limit                                          |
| --------------------------------------- | ---------------------------------------------- |
| Load balancers                          | 1,000                                          |
| Rules per resource                      | 250                                            |
| Rules per NIC (across all IPs on a NIC) | 300                                            |
| Frontend IP configurations              | 200                                            |
| Backend pool size                       | 300 IP configurations, single availability set |
| Availability sets per Load Balancer     | 1                                              |
| Load Balancers per VM                   | 2 (1 Public and 1 internal)                    |
