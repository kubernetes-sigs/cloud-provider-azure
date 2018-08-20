# Azure LoadBalancer

The way azure define LoadBalancer is different with GCE or AWS. Azure's LB can have multiple frontend IP refs. The GCE and AWS can only allow one, if you want more, you'd better to have another LB. Because of the fact, Public IP is not part of the LB in Azure. NSG is not part of LB in Azure either. However, you cannot delete them in parallel, Public IP can only be deleted after LB's frontend IP ref is removed. 

For different Azure Resources, such as LB, Public IP, NSG. They are the same tier Azure resources. We need to make sure there is no connection in their own ensure loops. In another words, They would be eventually reconciled regardless of other resources' state. They should only depend on service state.

Despite the ideal philosophy above, we have to face the reality. NSG depends on LB's frontend IP to adjust NSG rules. So when we want to reconcile NSG, the LB should contain the corresponding frontend IP config.

And also, For Azure, we cannot afford to have more than 1 worker of service_controller. Because, different services could operate on the same LB, concurrent execution could result in conflict or unexpected result. For AWS and GCE, they apparently doesn't have the problem, they use one LB per service, no such conflict.

There are two load balancers per availability set internal and external. There is a limit on number of services that can be associated with a single load balancer.
By default primary load balancer is selected. Services can be annotated to allow auto selection of available load balancers. Service annotations can also be used to provide specific availability sets that host the load balancers. Note that in case of auto selection or specific availability set selection, when the availability set is lost in case of downtime or cluster scale down the services are currently not auto assigned to an available load balancer.

## LoadBalancer annotations

1. Internal mode
    ```
    metadata:
      name: my-service
      annotations:
        service.beta.kubernetes.io/azure-load-balancer-internal: ""
    ```
    Value type: string

    Supported values: "true", "false"

    Default value: "false"

    Description: Specify whether the load balancer should be internal, by default a public Internet faced load balancer will be created.
1. Internal mode subnet
    ```
    metadata:
      name: my-service
      annotations:
        service.beta.kubernetes.io/azure-load-balancer-internal-subnet: ""
    ```
    Value type: string

    Description: Specify which subnet the internal load balancer should be bound to.
1. Load balancer mode
    ```
    metadata:
      name: my-service
      annotations:
        service.beta.kubernetes.io/azure-load-balancer-mode: ""
    ```
    Value type: string

    Supported values: "__auto__", "{name1},{name2}"

    Description: Specify the Azure load balancer selection based on availability sets. There are currently three possible load balancer selection modes :
      1. Default mode - service has no annotation ("service.beta.kubernetes.io/azure-load-balancer-mode"). In this case the Loadbalancer of the primary Availability set is selected
      2. "__auto__" mode - service is annotated with __auto__ value, this when loadbalancer from any availability set is selected which has the minimum rules associated with it.
      3. "{name1}, {name2}" mode - this is when the load balancer from the specified availability sets is selected that has the minimum rules associated with it.
1. Dns label name
   ```
    metadata:
      name: my-service
      annotations:
        service.beta.kubernetes.io/azure-dns-label-name: ""
    ```
    Value type: string

    Description: Specify the DNS label name for the service.
1. Shared security rule
   ```
    metadata:
      name: my-service
      annotations:
        service.beta.kubernetes.io/azure-shared-securityrule: ""
    ```
    Value type: string

    Supported values: "true", "false"

    Description: Specify that the service should be exposed using an Azure security rule that may be shared with other service, trading specificity of rules for an increase in the number of services that can be exposed. This relies on the Azure "augmented security rules" feature.

1. Load balancer resource group
   ```
    metadata:
      name: my-service
      annotations:
        service.beta.kubernetes.io/azure-load-balancer-resource-group: ""
    ```
    Value type: string

    Description: Specify the resource group of load balancer objects that are not in the same resource group as the cluster.

1. Allowed service tags
   ```
    metadata:
      name: my-service
      annotations:
        service.beta.kubernetes.io/azure-allowed-service-tags: ""
    ```
    Value type: string
    
    Description: Specify a list of allowed service tags separated by comma

    Reference: https://docs.microsoft.com/en-us/azure/virtual-network/security-overview#service-tags

1. Load balancer TCP idle timeout
   ```
    metadata:
      name: my-service
      annotations:
        service.beta.kubernetes.io/azure-load-balancer-tcp-idle-timeout: ""
   ```

    Value type: string

    Description: Specify the time, in minutes, for TCP connection idle timeouts to occur on the load balancer. Default and minimum value is 4. Maximum value is 30. Must be a whole number.

## Introduce Functions

- reconcileLoadBalancer(clusterName string, service *v1.Service, nodes []*v1.Node, wantLb bool) (*network.LoadBalancer, error)
  - Go through lb's properties, update based on wantLb
  - If any change on the lb, no matter if the lb exists or not
    - Call az cloud to CreateOrUpdate on this lb, or Delete if nothing left
  - return lb, err

- reconcileSecurityGroup(clusterName string, service *v1.Service, lbIP *string, wantLb bool) (*network.SecurityGroup, error)
  - Go though NSG' properties, update based on wantLb
    - Use destinationIPAddress as target address if possible
    - Consolidate NSG rules if possible
  - If any change on the NSG, (the NSG should always exist)
    - Call az cloud to CreateOrUpdate on this NSG
  - return sg, err

- reconcilePublicIP(clusterName string, service *v1.Service, wantLb bool) (*network.PublicIPAddress, error)
  - List all the public ip in the resource group
  - Make sure we only touch Public IP resources has tags[service] = "namespace/serviceName"
    - skip for wantLb && !isInternal && pipName == desiredPipName
    - delete other public ip resources if any
  - if !isInternal && wantLb 
    - ensure Public IP with desiredPipName exists

- getServiceLoadBalancer(service *v1.Service, clusterName string, nodes []*v1.Node, wantLb bool) (lb, status, exists, error)
  - gets the loadbalancer for the service if it already exists
  - If wantLb is TRUE then -it selects a new load balancer, the selection helps distribute the services across load balancers
  - In case the selected load balancer does not exist it returns network.LoadBalancer struct with added metadata (such as name, location) and existsLB set to FALSE 
  - By default - cluster default LB is returned

## Define interface behaviors

### GetLoadBalancer

- Get LoadBalancer status, return status, error
  - return the load balancer status for this service
  - it will not create or update or delete any resource

### EnsureLoadBalancer

- Reconcile LB for the flipped service
  - Call reconcileLoadBalancer(clusterName, flipedService, nil, false/* wantLb */)
- Reconcile Public IP
  - Call reconcilePublicIP(cluster, service, true)
- Reconcile LB's related and owned resources, such as FrontEndIPConfig, Rules, Probe.
  - Call reconcileLoadBalancer(clusterName, service, nodes, true /* wantLb */)
- Reconcile NSG rules, it need to be called after reconcileLB
  - Call reconcileSecurityGroup(clusterName, service, lbStatus, true /* wantLb */)

### UpdateLoadBalancer

- Has no difference with EnsureLoadBalancer

### EnsureLoadBalancerDeleted

- Reconcile NSG first, before reconcile LB, because SG need LB to be there
  - Call reconcileSecurityGroup(clusterName, service, nil, false /* wantLb */)
- Reconcile LB's related and owned resources, such as FrontEndIPConfig, Rules, Probe.
  - Call reconcileLoadBalancer(clusterName, service, nodes, false)
- Reconcile Public IP, public IP needs related LB reconciled first
  - Call reconcilePublicIP(cluster, service, false)
