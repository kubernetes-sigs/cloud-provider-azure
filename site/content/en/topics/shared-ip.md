---
title: "Multiple Services Sharing One IP Address"
linkTitle: "Multiple Services Sharing One IP Address"
weight: 6
type: docs
description: >
    Bind one IP address to multiple services.
---

> Note that this feature is supported since v1.20.0.

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

Note that the `loadBalancerIP` is not set, or Azure would find a pre-allocated public IP with the address. After obtaining the IP address of the service, you could create other services using this address.

```yaml
apiVersion: v1
kind: Service
metadata:
  name: https
  namespace: default
spec:
  loadBalancerIP: 1.2.3.4 # the IP address could be the same as it is of `nginx` service
  ports:
    - port: 443
      protocol: TCP
      targetPort: 443
  selector:
    app: https
  type: LoadBalancer
```

Note that if you specify the `loadBalancerIP` but there is no corresponding public IP pre-allocated, an error would be reported.
