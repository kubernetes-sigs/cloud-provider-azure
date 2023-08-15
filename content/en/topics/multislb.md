---
title: "Multiple Standard LoadBalancers"
linkTitle: "Multiple Standard LoadBalancers"
type: docs
description: Multiple Standard LoadBalancers.
---

# Multiple Standard LoadBalancers

## Backgrounds

There will be only a single Standard Load Balancer and a single Internal Load Balancer (if required) per cluster by default. This imposes a number of limits on clusters based on [Azure Load Balancer limits](https://learn.microsoft.com/en-us/azure/azure-resource-manager/management/azure-subscription-service-limits#load-balancer), the largest being based on the 300 rules per NIC limitation. Any IP:port combination in a frontEndIPConfiguration that maps to a member of a backend pool counts as one of the 300 rules for that node. This limits any AKS cluster to a maximum of 300 LoadBalancer service IP:port combinations (so a maximum of 300 services with one port, or fewer if services have multiple ports). Load balancers are also limited to no more than 8 private link services targeting a given load balancer. 

## Configuration

Introduce a new cloud configuration option `multipleStandardLoadBalancerConfigurations`. Example:

```json
{
  ...
  "loadBalancerBackendPoolConfigurationType": "nodeIP",
  "multipleStandardLoadBalancerConfigurations": [
    {
      "name": "<clusterName>",
      "autoPlaceServices": true
    },
    {
      "name": "lb-2",
      "autoPlaceServices": false,
      "serviceNamespaceSelector": [
        "matchExpressions": [
          {
            "key": "key1",
            "operator": "In",
            "values": [
              "val1"
            ]
          }
        ]
      ],
      "nodeSelector": {
        "matchLabels": {
          "key1": "val1"
        }
      },
      "primaryVMSet": "vmss-1"
    }
  ]
}
```

> To enable the multiple standard load balancers, set `loadBalancerSKU` to `Standard`, `loadBalancerBackendPoolConfigurationType` to `nodeIP` and at least one `multipleStandardLoadBalancerConfiguration`. If one or more conditions are not met, the cloud provider will either throw an error or fall back to the single standard load balancer.

### default lbs
The default lb `<clustername>` is required in `loadBalancerProfiles`. The cloud provider will check if there is an lb config named `<clustername>`. If not, an error will be reported in the service event.

### internal lbs
The behavior of internal lbs remains the same as is. It shares the same config as its public counterpart and will be automatically created if needed with the name `<external-lb-name>-internal`. Internal lbs are not required in the `loadBalancerProfiles`, all lb names in it are considered public ones.


## Service selection

In the cases of basic lb and the previous revision of multiple slb design, we use service annotation `service.beta.kubernetes.io/azure-load-balancer-mode` to decide which lb the service should be attached to. It can be set to an agent pool name, and the service will be attached to the lb belongs to that agent pool. If set to `__auto__`, we pick an lb with the fewest number of lb rules for the service. This selection logic will be replaced by the following:

1. New service annotation `service.beta.kubernetes.io/azure-load-balancer-configurations: <lb-config-name1>,<lb-config-name2>` will replace the old annotation `service.beta.kubernetes.io/azure-load-balancer-mode` which will only be useful for basic SKU load balancers. If all selected lbs are not eligible, an error will be reported in the service events. If multiple eligible lbs are provided, choose one with the lowest number of rules.

2. `AllowServicePlacement`
This load balancer can have services placed on it. Defaults to true, can be set to false to drain and eventually remove a load balancer. This will not impact existing services on the load balancer.

3. `ServiceNamespaceSelector`
Only services created in namespaces that match the selector will be allowed to select that load balancer, either manually or automatically. If not supplied, services created in any namespaces can be created on that load balancer. If the value is changed, all services on this slb will be moved onto another one with the public/internal IP addresses unchanged. If the services have no place to go, an error should be thrown in the service event.

4. `ServiceLabelSelector`
Similar to `ServiceNamespaceSelector`. Services must match this selector to be placed on this load balancer.

## Node selection

When the cluster is initially migrated to or created with multiple standard load balancers, each node will be evaluated to see what load balancer it should be placed into.

Valid placement targets will be determined as follows (rules match from top to bottom, first match wins):

1. If this node is in an agent pool that is selected as a primary agent pool for a load balancer, that load balancer will be the only potential placement target.
2. If the nodeSelectors on any load balancer configurations match this node, then all load balancer configurations that match it will be potential placement targets.
3. If no nodeSelectors on any load balancer configurations match this node, then all load balancers that do not have any nodeSelectors will be potential placement targets.

After the list of potential placement targets has been calculated, the node will be placed into the kubernetes backend pool of the load balancer with the fewest number of nodes already assigned. 

## Service with `ExternalTrafficPolicy=Local`

Each local service owns a backend pool named after the service name. The backend pool will be created in the service reconciliation loop when the service is created or updated from external traffic policy cluster. It will be deleted in the service reconciliation loop when: 1, the service is deleted; 2, the service is changed to etp cluster; 3, the cluster is migrated from multi-slb to single-slb; and 4, the service is moved to another load balancer.

Besides the service reconciliation loop, an endpointslice informer is also responsible for updating the dedicated backend pools. It watches all endpointslices of local services, monitors any updating events, and updates the corresponding backend pool when service endpoints changes. Considering local services may churn quickly, the informer will send backend pool updating operations to a buffer queue. The queue merges operations targeting to the same backend pool, and updates them every 30s by default. The updating interval can be adjusted by changing `loadBalancerBackendPoolUpdateIntervalInSeconds` in cloud configurations.

> Local service dedicated backend pool and `<clusterName>` backend pool cannot be reconciled in one loop. Hence, the operation triggered by the update of local service or its endpoints will not affect `<clusterName>` backend pool.
