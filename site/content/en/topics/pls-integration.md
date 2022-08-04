---
title: "Azure Private Link Service Integration"
linkTitle: "Azure Private Link Service Integration"
type: docs
description: >
    Connect Azure Private Link service to Azure Standard Load Balancer.
---

Azure Private Link Service (PLS) is an infrastructure component that allows users to privately connect via a Private Endpoint (PE) in a VNET in Azure and a Frontend IP Configuration associated with an Azure Load Balancer (ALB). With Private Link, users as service providers can securely provide their services to consumers who can connect from within Azure or on-premises without data exfiltration risks.

Before Private Link Service integration, users who wanted private connectivity from on-premises or other VNETs to their services in the Azure Kubernetes cluster were required to create a Private Link Service (PLS) to reference the Azure LoadBalancer. The user would then create a Private Endpoint (PE) to connect to the PLS to enable private connectivity. With this feature, a managed PLS to the LB would be created automatically, and the user would only be required to create PE connections to it for private connectivity.

Currently, private link service connectivity is broken with Azure external Standard Load Balancer and floating ip enabled (default). To use managed private link service, users can either create an internal service by setting annotation `service.beta.kubernetes.io/azure-load-balancer-internal` to `true` or disable floating ip by setting annotation `service.beta.kubernetes.io/service.beta.kubernetes.io/azure-disable-load-balancer-floating-ip` to `true` ([more details here](../loadbalancer)).

## PrivateLinkService annotations

Below is a list of annotations supported for Kubernetes services with Azure PLS created:

| Annotation | Value | Description | Required | Default |
| ------------------------------------------------------------------------ | ---------------------------------- | ------------------------------------------------------------ |------|------|
| `service.beta.kubernetes.io/azure-pls-create`                            | `"true"`                           | Boolean indicating whether a PLS needs to be created. | Required | |
| `service.beta.kubernetes.io/azure-pls-name`                              | `<PLS name>`                       | String specifying the name of the PLS resource to be created. | Optional | `"pls-<LB frontend config name>"` |
| `service.beta.kubernetes.io/azure-pls-ip-configuration-subnet`           |`<Subnet name>`                     | String indicating the subnet to which the PLS will be deployed. This subnet must exist in the same VNET as the backend pool. PLS NAT IPs are allocated within this subnet. | Optional | If `service.beta.kubernetes.io/azure-load-balancer-internal-subnet`, this ILB subnet is used. Otherwise, the default subnet from config file is used. |
| `service.beta.kubernetes.io/azure-pls-ip-configuration-ip-address-count` | `[1-8]`                            | Total number of private NAT IPs to allocate. | Optional | 1 |
| `service.beta.kubernetes.io/azure-pls-ip-configuration-ip-address`       | `"10.0.0.7 ... 10.0.0.10"`         | A space separated list of static **IPv4** IPs to be allocated. (IPv6 is not supported right now.) Total number of IPs should not be greater than the ip count specified in `service.beta.kubernetes.io/azure-pls-ip-configuration-ip-address-count`. If there are fewer IPs specified, the rest are dynamically allocated. The first IP in the list is set as `Primary`. |  Optional | All IPs are dynamically allocated. |
| `service.beta.kubernetes.io/azure-pls-fqdns`                             | `"fqdn1 fqdn2"`                    | A space separated list of fqdns associated with the PLS. | Optional | `[]` |
| `service.beta.kubernetes.io/azure-pls-proxy-protocol`                    | `"true"` or `"false"`              | Boolean indicating whether the TCP PROXY protocol should be enabled on the PLS to pass through connection information, including the link ID and source IP address. Note that the backend service MUST support the PROXY protocol or the connections will fail. | Optional | `false` |
| `service.beta.kubernetes.io/azure-pls-visibility`                        | `"sub1 sub2 sub3 … subN"` or `"*"` | A space separated list of Azure subscription ids for which the private link service is visible. Use `"*"` to expose the PLS to all subs (Least restrictive). | Optional | Empty list `[]` indicating role-based access control only: This private link service will only be available to individuals with role-based access control permissions within your directory. (Most restrictive) |
| `service.beta.kubernetes.io/azure-pls-auto-approval`                     | `"sub1 sub2 sub3 … subN"`          | A space separated list of Azure subscription ids. This allows PE connection requests from the subscriptions listed to the PLS to be automatically approved. This only works when visibility is set to "*". |  Optional | `[]` |

For more details about each configuration, please refer to [Azure Private Link Service Documentation](https://docs.microsoft.com/en-us/cli/azure/network/private-link-service?view=azure-cli-latest#az-network-private-link-service-create).


## Design Details

### Creating managed PrivateLinkService

When a `LoadBalancer` typed service is created without the `loadBalancerIP` field specified, an LB frontend IP configuration is created with a dynamically generated IP. If the service has `loadBalancerIP` in its spec, an existing LB frontend IP configuration may be reused if one exists; otherwise a static configuration is created with the specified IP. When a service is created with annotation `service.beta.kubernetes.io/azure-pls-create` set to `true` or updated later with the annotation added, a PLS resource attached to the LB frontend is created in the default resource group or the resource group user set in config file with key `PrivateLinkServiceResourceGroup`.

The Kubernetes service creating the PLS is assigned as the owner of the resource. Azure cloud provider tags the PLS with cluster name and service name `kubernetes-owner-service: <namespace>/<service name>`. Only the owner service can later update the properties of the PLS resource.

If there's a managed PLS already created for the LB frontend, the same PLS is reused automatically since each LB frontend can be referenced by only one PLS. If the LB frontend is attached to a user defined PLS, service creation should fail with proper error logged.

For now, Azure cloud provider does not manage any [Private Link Endpoint](https://docs.microsoft.com/en-us/azure/private-link/private-endpoint-overview) resources. Once a PLS is created, users can create their own PEs to connect to the PLS.

### Deleting managed PrivateLinkService

Once a PLS is created, it shares the lifetime of the LB frontend IP configuration and is deleted only when its corresponding LB frontend gets deleted. As a result, a PLS may still exist even when its owner service is deleted. This is out of the consideration that multiple Kubernetes services can share the same LB frontend IP configuration and thus share the PLS automatically. More details are discussed in [next section](#sharing-managed-privatelinkservice).

If there are active PE connections to the PLS, all connections are removed and the PEs become obsolete. Users are responsible for cleaning up the PE resources.

### Sharing managed PrivateLinkService

Multiple Kubernetes services can share the same LB frontend by specifying the same `loadBalancerIP` (for more details, please refer to [Multiple Services Sharing One IP Address](../shared-ip)). Once a PLS is attached to the LB frontend, these services automatically share the PLS. Users can access these services via the same PE but different ports.

Azure cloud provider tags the service creating the PLS as the owner (`kubernetes-owner-service: <namespace>/<service name>`) and only allows that service to update the configurations of the PLS. If the owner service is deleted or if user wants some other service to take control, user can modify the tag value to a new service in `<namespace>/<service name>` pattern.

PLS is only automatically deleted when the LB frontend IP configuration is deleted. One can delete a service while preserving the PLS by creating a temporary service referring to the same LB frontend.

### Managed PrivateLinkService Creation example

Below we provide an example for creating a Kubernetes service object with Azure ILB and PLS created:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: myService
  annotations:
    service.beta.kubernetes.io/azure-load-balancer-internal: "true" # Use an internal LB with PLS
    service.beta.kubernetes.io/azure-pls-create: "true"
    service.beta.kubernetes.io/azure-pls-name: myServicePLS
    service.beta.kubernetes.io/azure-pls-ip-configuration-subnet: pls-subnet
    service.beta.kubernetes.io/azure-pls-ip-configuration-ip-address-count: "1"
    service.beta.kubernetes.io/azure-pls-ip-configuration-ip-address: 10.240.0.9 # Must be available in pls-subnet
    service.beta.kubernetes.io/azure-pls-fqdns: "fqdn1 fqdn2"
    service.beta.kubernetes.io/azure-pls-proxy-protocol: "false"
    service.beta.kubernetes.io/azure-pls-visibility: "*"
    service.beta.kubernetes.io/azure-pls-auto-approval: "subId1"
spec:
  type: LoadBalancer
  selector:
    app: myApp
  ports:
    - name: myAppPort
      protocol: TCP
      port: 80
      targetPort: 80
```
## Restrictions

* PLS does not support basic Load Balancer or IP-based Load Balancer.
* PLS connectivity is broken with Azure external Standard Load Balancer and floating ip enabled (default). Users can either use an internal Load Balancer or disable floating ip on loadbalancing rules (described above).
* At this moment, PLS connectivity is broken if [AzureCNI V2](https://docs.microsoft.com/en-us/azure/aks/configure-azure-cni#dynamic-allocation-of-ips-and-enhanced-subnet-support) is used in cluster. Azure networking team is working on supporting the feature.
* Due to limitation of [kubernetes#95555](https://github.com/kubernetes/kubernetes/issues/95555), when the service's externalTrafficPolicy set to Local, PLS need to use a different subnet from Pod's subnet. If the same subnet is required, then the service should use Cluster externalTrafficPolicy.
