---
title: "Multiple Services Sharing One IP Address"
linkTitle: "Multiple Services Sharing One IP Address"
weight: 6
type: docs
description: >
    Bind one IP address to multiple services.
---

> This feature is supported since v1.20.0.

Provider Azure supports sharing one IP address among multiple load balancer typed external or internal services. To share an IP address among multiple public services, a public IP resource is needed. This public IP could be created in advance or let the cloud provider provision it when creating the first external service. Specifically, Azure would create a public IP resource automatically when an external service is discovered.

```yaml
apiVersion: v1
kind: Service
metadata:
  name: nginx
  namespace: default
spec:
  ports:
    - port: 80
      protocol: TCP
      targetPort: 80
  selector:
    app: nginx
  type: LoadBalancer
```

Note that the annotations `service.beta.kubernetes.io/azure-load-balancer-ipv4`, `service.beta.kubernetes.io/azure-load-balancer-ipv6`, field `Service.Spec.LoadBalancerIP` are not set, or Azure would find a pre-allocated public IP with the address. After obtaining the IP address of the service, you could create other services using this address.

```yaml
apiVersion: v1
kind: Service
metadata:
  name: https
  namespace: default
  annotations:
    service.beta.kubernetes.io/azure-load-balancer-ipv4: 1.2.3.4 # the IP address could be the same as it is of `nginx` service
spec:
  ports:
    - port: 443
      protocol: TCP
      targetPort: 443
  selector:
    app: https
  type: LoadBalancer
```

Note that if you specify the annotations `service.beta.kubernetes.io/azure-load-balancer-ipv4`, `service.beta.kubernetes.io/azure-load-balancer-ipv6` or field `Service.Spec.LoadBalancerIP` but there is no corresponding public IP pre-allocated, an error would be reported.

## DNS

Even if multiple services can refer to one public IP, the DNS label cannot be re-used. The public IP would have the label `kubernetes-dns-label-service: <svcName>` to indicate which service is binding to the DNS label. In this case if there is another service sharing this specific IP address trying to refer to the DNS label, an error would be reported. For managed public IPs, this label will be added automatically by the cloud provider. For static public IPs, this label should be added manually.

## Using public IP name instead of IP address to share the public IP

> This feature is supported since v1.24.0.

In addition to using the IP address annotation, you could also use the public IP name to share the public IP. The public IP name could be specified by the annotation `service.beta.kubernetes.io/azure-pip-name`. You can point to a system-created public IP or a static public IP.

```yaml
apiVersion: v1
kind: Service
metadata:
  name: https
  namespace: default
  annotations:
    service.beta.kubernetes.io/azure-pip-name: pip-1
spec:
  ports:
    - port: 443
      protocol: TCP
      targetPort: 443
  selector:
    app: https
  type: LoadBalancer
```


## Restrictions

The cloud provider azure manages the lifecycle of the system-created public IPs. By default, there are two kinds of system managed tags: `kubernetes-cluster-name` and `service` (see the picture below). The controller manager would
add the service name to the `service` if a service is trying to refer to the public IP, and remove the name from the `service` if the service is deleted. The public IP would be deleted if there is no service
in the tag `service`. However, according to the [docs of azure tags](https://docs.microsoft.com/en-us/azure/azure-resource-manager/management/tag-resources#limitations), there are several restrictions:

- Each resource, resource group, and subscription can have a maximum of 50 tag name/value pairs. If you need to apply more tags than the maximum allowed number, use a JSON string for the tag value. The JSON string can contain many values that are applied to a single tag name. A resource group or subscription can contain many resources that each have 50 tag name/value pairs.

- The tag name is limited to 512 characters, and the tag value is limited to 256 characters. For storage accounts, the tag name is limited to 128 characters, and the tag value is limited to 256 characters.

Based to that, we suggest to use static public IPs when there are more than 10 services sharing the IP address.

![tags on the public IP](../images/pip-labels.png)
