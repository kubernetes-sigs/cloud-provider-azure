---
title: "Support node IPAM controller"
linkTitle: "node IPAM controller"
type: docs
weight: 10
description: >
Support node IPAM controller in the out-of-tree provider Azure.
---

## Background

The in-tree node IPAM controller would be moved to the GCE staging repo in the future, 
[PR](https://github.com/kubernetes/kubernetes/pull/97925). Hence, we need to copy the code to the out-of-tree repo and 
implement the azure-specific logic here. Specifically, we want to allocate the CIDR for each VMSS and VMAS so every
instance would get a pod CIDR within the range of the corresponding VMSS or VMAS, instead of the cluster CIDR.

## User experience

There are two kinds of CIDR allocator in the node IPAM controller, which are `RangeAllocator` and `CloudAllocator`.
The `RangeAllocator` is the default one which allocates the pod CIDR for every node in the range of the cluster CIDR.
The `CloudAllocator` allocates the pod CIDR for every node in the range of the CIDR on the corresponding VMSS or VMAS.
The CIDR on the VMSS or VMAS is set by a specific tag `{"VMSet-CIDR": "10.244.1.0/24"}` and every node would get a 
pod CIDR within the range. Note that the range in the VMSS or VMAS must be within the cluster CIDR, or an error would be 
thrown. The mask of each pod CIDR could be modified by setting `--node-cidr-mask-size`. For example, 
if the `--node-cidr-mask-size` is 24 the first node would be allocated with a pod CIDR of "10.244.1.0/24", and the second
would be allocated "10.244.2.0/24", etc.

### Configurations

| name | type | default | description |
| ----- | -----| ----- | ----- |
| allocate-node-cidrs | bool | true | Should CIDRs for Pods be allocated and set on the cloud provider. |
| cluster-cidr | string | "10.244.0.0/16" | CIDR Range for Pods in cluster. Requires --allocate-node-cidrs to be true |
| service-cluster-ip-range | string | "" | CIDR Range for Services in cluster. Requires --allocate-node-cidrs to be true |
| node-cidr-mask-size | int | 24 | Mask size for node cidr in cluster. Default is 24 for IPv4 and 64 for IPv6. |
| node-cidr-mask-size-ipv4 | int | 24 | Mask size for IPv4 node cidr in dual-stack cluster. Default is 24. |
| node-cidr-mask-size-ipv6 | int | 64 | Mask size for IPv6 node cidr in dual-stack cluster. Default is 64. |
| cidr-allocator-type | string | "RangeAllocator" | The CIDR allocator type. "RangeAllocator" or "CloudAllocator" |
