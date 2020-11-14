---
title: "Custom load balancer health probe"
linkTitle: "Custom load balancer health probe"
weight: 9
type: docs
---

Currently, the protocol of the health probe is defined as below:

1. for local services, HTTP and /healthz would be used;
1. for cluster TCP services, TCP would be used;
1. for cluster UDP services, no health probes.

Since v1.20, two service annotations `service.beta.kubernetes.io/azure-load-balancer-health-probe-protocol` and `service.beta.kubernetes.io/azure-load-balancer-health-probe-request-path` are introduced, which determine the new health probe behavior. If the `service.beta.kubernetes.io/azure-load-balancer-health-probe-protocol` is set, both local and cluster TCP services would use the specified health probe protocol. If the `service.beta.kubernetes.io/azure-load-balancer-health-probe-request-path` is set, the specified request path would be used instead of `/healthz`. Note that the request path would be ignored when using TCP or the `service.beta.kubernetes.io/azure-load-balancer-health-probe-protocol` is empty. More specifically:

| `externalTrafficPolicy` | `service.beta.kubernetes.io/azure-load-balancer-health-probe-protocol` | `service.beta.kubernetes.io/azure-load-balancer-health-probe-request-path` | protocol | request path |
| ------------------------------------------------------------ | ---------------------------- | ------------------------------------------------------------ |------| ----- |
| local |  | (ignored) | http | `/healthz` |
| local | tcp | (ignored) | tcp | null |
| local | http/https | | http/https | `/healthz` |
| local | http/https | `/custom-path` | http/https | `/custom-path` |
| cluster |  | (ignored) | tcp | null |
| cluster | tcp | (ignored) | tcp | null |
| cluster | http/https | | http/https | `/healthz` |
| cluster | http/https | `/custom-path` | http/https | `/custom-path` |

