# Health Probe Proxy for Kubernetes

This project provides a daemonset that is used to forward traffic to the kube-proxy when using `shared` health probe mode in a Kubernetes cluster.

## Overview

When a service is integrated with a private link service and uses the proxy protocol, the health check requests to the kube-proxy will fail. This daemonset will read these requests, parse the proxy protocol header, and forward the request to the kube-proxy. If the proxy protocol is not used, this daemonset will forward the request to the kube-proxy without any modification.

## Getting Started

### Prerequisites

- cloud-provider-azure v1.28.5 or higher

### Configuration

1. Set `--health-check-port` to the port that is configured in the cloud provider config `clusterServiceSharedLoadBalancerHealthProbePort`.
2. Set `--target-port` to the kube-proxy health check port.

### Building

To build the binary for the health probe proxy, navigate to the root directory of the project and run:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o health-probe-proxy-bin .
```