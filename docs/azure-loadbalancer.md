# Azure LoadBalancer

The way azure define LoadBalancer is different with GCE or AWS. Azure's LB can have multiple frontend IP refs. The GCE and AWS can only allow one, if you want more, you'd better to have another LB. Because of the fact, Public IP is not part of the LB in Azure. NSG is not part of LB in Azure either. However, you cannot delete them in parallel, Public IP can only be deleted after LB's frontend IP ref is removed. 

For different Azure Resources, such as LB, Public IP, NSG. They are the same tier Azure resources. We need to make sure there is no connection in their own ensure loops. In another words, They would be eventually reconciled regardless of other resources' state. They should only depend on service state.

By default primary load balancer (with basic SKU) is selected. Services can be annotated to allow auto selection of available load balancers. Service annotations can also be used to provide specific availability sets that host the load balancers. Note that in case of auto selection or specific availability set selection, when the availability set is lost in case of downtime or cluster scale down the services are currently not auto assigned to an available load balancer.

## LoadBalancer annotations

Below is a list of annotations supported for Kubernetes services with type `LoadBalancer`:

| Annotaion                                                    | Value                        | Description                                                  |
| ------------------------------------------------------------ | ---------------------------- | ------------------------------------------------------------ |
| `service.beta.kubernetes.io/azure-load-balancer-internal`    | `true `or `false`            | Specify whether the load balancer should be internal. It’s defaulting to public if not set. |
| `service.beta.kubernetes.io/azure-load-balancer-internal-subnet` | Name of the subnet           | Specify which subnet the internal load balancer should be bound to. It’s defaulting to the subnet configured in cloud config file if not set. |
| `service.beta.kubernetes.io/azure-load-balancer-mode`        | `auto`, `{name1},{name2}`    | Specify the Azure load balancer selection algorithm based on availability sets. There are currently three possible load balancer selection modes : default, auto or "{name1}, {name2}". This is only working for basic LB (see below for how it works) |
| `service.beta.kubernetes.io/azure-dns-label-name`            | Name of the DNS label        | Specify the DNS label name for the service.                  |
| `service.beta.kubernetes.io/azure-shared-securityrule`       | `true` or `false`            | Specify that the service should be exposed using an Azure security rule that may be shared with other service, trading specificity of rules for an increase in the number of services that can be exposed. This relies on the Azure "augmented security rules" feature. |
| `service.beta.kubernetes.io/azure-load-balancer-resource-group` | Name of the resource group   | Specify the resource group of load balancer objects that are not in the same resource group as the cluster. |
| `service.beta.kubernetes.io/azure-allowed-service-tags`      | List of allowed service tags | Specify a list of allowed [service tags](https://docs.microsoft.com/en-us/azure/virtual-network/security-overview#service-tags) separated by comma. |
| `service.beta.kubernetes.io/azure-load-balancer-tcp-idle-timeout` | TCP idle timeouts in minutes | Specify the time, in minutes, for TCP connection idle timeouts to occur on the load balancer. Default and minimum value is 4. Maximum value is 30. Must be an integer. |
| `service.beta.kubernetes.io/azure-load-balancer-mixed-protocols` | `true` or `false`            | Specify whether both TCP and UDP protocols should be created for the service. (This is not allowed from Kubernetes API) |

### Load balancer selection modes

There are currently three possible load balancer selection modes :
  1. Default mode - service has no annotation ("service.beta.kubernetes.io/azure-load-balancer-mode"). In this case the Loadbalancer of the primary Availability set is selected
  2. "__auto__" mode - service is annotated with __auto__ value, this when loadbalancer from any availability set is selected which has the minimum rules associated with it.
  3. "{name1}, {name2}" mode - this is when the load balancer from the specified availability sets is selected that has the minimum rules associated with it.

The selection modes of load balancer only works for basic SKU (see below) because of their difference in backend pool endpoints:

* Standard SKU supports any virtual machine in a single virtual network, including blend of virtual machines, availability sets, virtual machine scale sets. So all the nodes would be added to the same standard LB backend pools for standard SKU (it supports 1000 instances at most).
* Basic SKU only supports virtual machines in a single availability set or virtual machine scale set. So only nodes with same availability set or virtual machine scale set would be added to the basic LB backend pools.

## LoadBalancer SKUs

Azure cloud provider supports both `basic` and `standard` SKU load balancers, which can be set via `loadBalancerSku` option in [cloud config file](cloud-provider-config.md). A list of differences between these two SKUs could be found [here](https://docs.microsoft.com/en-us/azure/load-balancer/load-balancer-standard-overview#why-use-standard-load-balancer).

> Note that the public IPs used in load balancer frontend configurations should be same SKU. That is standard public IP for standard load balancer, while basic public IP for basic load balancer.

Azure doesn’t support a network interface joining load balancers with different SKUs, hence migration dynamically between them is not supported. 

> If you do require migration, please delete all services with type `LoadBalancer` (or change to other type)

### Outbound connectivity

Though there are many differences between two SKUs, the [outbound connectivity](https://docs.microsoft.com/en-us/azure/load-balancer/load-balancer-outbound-connections
) is the one that each provisioning tools should take care:

* For basic SKU, the outbound connectivity is opened by default. If multiple frontends are set, then the outbound IP is selected randomly (and configurable) from them.

* For standard SKU, the outbound connectivity is disabled by default. There are two ways to open the outbound connectivity: use standard public load balancer or define outbound rules.

### Standard LoadBalancer

Because load balancer in Kubernetes cluster is managed by Azure cloud provider and it may change dynamically (e.g. the public load balancer would be deleted if no services defined with type `LoadBalancer`), [outbound rules](https://docs.microsoft.com/en-us/azure/load-balancer/load-balancer-outbound-rules-overview) are recommended way if you want to ensure the outbound connectivity for all nodes.

> Specially notes:
>
> * In the context of outbound connectivity, a single standalone VM, all the VM's in an Availability Set, all the instances in a VMSS behave as a group. This means, if a single VM in an Availability Set is associated with a Standard SKU, all VM instances within this Availability Set now behave by the same rules as if they are associated with Standard SKU, even if an individual instance is not directly associated with it.
>
> * Public IP's used as instance-level public IP are mutually exclusive with outbound rules.  

Here is the recommend way to define the [outbound rules](https://docs.microsoft.com/en-us/azure/load-balancer/load-balancer-outbound-rules-overview) in provisioning tools:

* Create a separate IP (or multiple IPs for scale) in standard SKU for outbound rules. Make use of the [allocatedOutboundPorts](https://docs.microsoft.com/en-us/azure/load-balancer/load-balancer-outbound-rules-overview#snatports) parameter to allocate sufficient ports for your desired scenario scale.
* Create a separate pool definition for outbound, and ensure all virtual machines or VMSS virtual machines are in this pool. Azure cloud provider would manage the load balancer rules with another pool, so that provisioning tools and Azure cloud provider won't affect each other.
* Define inbound with load balancing rules and inbound NAT rules as needed, and set `disableOutboundSNAT` to true on the load balancing rule(s).  Don't rely on the side effect from these rules for outbound connectivity. It makes it messier than it needs to be and limits your options.  Use inbound NAT rules to create port forwarding mappings for SSH access to the VM's rather than burning public IPs per instance.

