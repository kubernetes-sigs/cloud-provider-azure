---
title: "Out-of-tree Node IPAM Controller"
linkTitle: "node IPAM controller"
type: docs
weight: 10
description: >
Support Node IPAM allocator in the out-of-tree provider Azure.
---

## Background

The in-tree [Node IPAM controller](https://github.com/kubernetes/kubernetes/tree/master/pkg/controller/nodeipam) only supports a fixed node CIDR mask size for all nodes, while in multiple node pool (VMSS) scenarios, different mask sizes are required for different node pools. There is a GCE-specific cloud CIDR allocator for a similar scenario, but that is not exposed in cloud provider API and it is planned to be moved out-of-tree. 

Hence this docs proposes an out-of-tree node IPAM controller. Specifically, allocate different pod CIDRs based on different CIDR mask size for different node pools (VMSS or VMAS).

## User experience

There are two kinds of CIDR allocator in the node IPAM controller, which are `RangeAllocator` and `CloudAllocator`.
The `RangeAllocator` is the default one which allocates the pod CIDR for every node in the range of the cluster CIDR.
The `CloudAllocator` allocates the pod CIDR for every node in the range of the CIDR on the corresponding VMSS or VMAS.

The CIDR on the VMSS or VMAS is set by a specific tag `{"kubernetesNodeCIDRMaskSize": "24"}` and every node would get a 
pod CIDR within the range. Note that the mask size in the VMSS or VMAS must be within the cluster CIDR, or an error would be 
thrown.

When the above tag doesn't exist on VMSS/VMAS, the values from `--node-cidr-mask-size` would be used as default mask size.

## Configurations

### kube-controller-manager

kube-controller-manager would be configured with option `--allocate-node-cidrs=false` to indicate it won't allocate Node CIDRs.

### cloud-controller-manager

The following configurations from cloud-controller-manager would be used as default options:

| name | type | default | description |
| ----- | -----| ----- | ----- |
| allocate-node-cidrs | bool | true | Should CIDRs for Pods be allocated and set on the cloud provider. |
| cluster-cidr | string | "10.244.0.0/16" | CIDR Range for Pods in cluster. Requires --allocate-node-cidrs to be true |
| service-cluster-ip-range | string | "" | CIDR Range for Services in cluster. Requires --allocate-node-cidrs to be true |
| node-cidr-mask-size | int | 24 | Mask size for node cidr in cluster. Default is 24 for IPv4 and 64 for IPv6. |
| node-cidr-mask-size-ipv4 | int | 24 | Mask size for IPv4 node cidr in dual-stack cluster. Default is 24. |
| node-cidr-mask-size-ipv6 | int | 64 | Mask size for IPv6 node cidr in dual-stack cluster. Default is 64. |
| cidr-allocator-type | string | "RangeAllocator" | The CIDR allocator type. "RangeAllocator" or "CloudAllocator" |

For each node pool (VMSS or VMAS), tag `{"kubernetesNodeCIDRMaskSize": "24"}` could be added by demand and its values would be used for new node's CIDR allocation.

## Open Questions

Q: what would happen if the tag value gets changed after some time?
A: In this case, existing node's nodeCIDR won't be changed, while new nodes' CIDR would be allocated with a new mask size.
