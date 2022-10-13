---
title: "Node IPAM controller"
linkTitle: "Node IPAM controller"
type: docs
weight: 10
description: "Usage of out-of-tree Node IPAM allocator."
---

> This feature is supported since v1.21.0.

## Background

The in-tree [Node IPAM controller](https://github.com/kubernetes/kubernetes/tree/master/pkg/controller/nodeipam) only 
supports a fixed node CIDR mask size for all nodes, while in multiple node pool (VMSS) scenarios, different mask sizes 
are required for different node pools. There is a GCE-specific cloud CIDR allocator for a similar scenario, but that is 
not exposed in cloud provider API and it is planned to be moved out-of-tree. 

Hence this docs proposes an out-of-tree node IPAM controller. Specifically, allocate different pod CIDRs based on 
different CIDR mask size for different node pools (VMSS or VMAS).

## Usage

There are two kinds of CIDR allocator in the node IPAM controller, which are `RangeAllocator` and `CloudAllocator`.
The `RangeAllocator` is the default one which allocates the pod CIDR for every node in the range of the cluster CIDR.
The `CloudAllocator` allocates the pod CIDR for every node in the range of the CIDR on the corresponding VMSS or VMAS.

The pod CIDR mask size of each node that belongs to a specific VMSS or VMAS is set by a specific tag 
`{"kubernetesNodeCIDRMaskIPV4": "24"}` or `{"kubernetesNodeCIDRMaskIPV6": "64"}`. Note that the mask size tagging on 
the VMSS or VMAS must be within the cluster CIDR, or an error would be thrown.

When the above tag doesn't exist on VMSS/VMAS, the default mask size (24 for ipv4 and 64 for ipv6) would be used.

To turn on the out-of-tree node IPAM controller:
1. Disable the in-tree node IPAM controller by setting `--allocate-node-cidrs=false` in kube-controller-manager.
1. Enable the out-of-tree counterpart by setting `--allocate-node-cidrs=true` in cloud-controller-manager.
1. To use `RangeAllocator`:
    * configure the `--cluster-cidr`, `--service-cluster-ip-range` and `--node-cidr-mask-size`;
    * if you enable the ipv6 dualstack, setting `--node-cidr-mask-size-ipv4` and `--node-cidr-mask-size-ipv6` instead of 
      `--node-cidr-mask-size`. An error would be reported if `--node-cidr-mask-size` and `--node-cidr-mask-size-ipv4` 
      (or `--node-cidr-mask-size-ipv6`) are set to non-zero values at a time. If only `--node-cidr-mask-size` is set, 
      which is not recommended, the `--node-cidr-mask-size-ipv4` and `--node-cidr-mask-size-ipv6` would be set to this
      value by default.
1. To use `CloudAllocator`:
    * set the `--cidr-allocator-type=CloudAllocator`;
    * configure mask sizes of each VMSS/VMAS by tagging `{"kubernetesNodeCIDRMaskIPV4": "custom-mask-size"}` and
      `{"kubernetesNodeCIDRMaskIPV4": "custom-mask-size"}` if necessary.

## Configurations

### kube-controller-manager

kube-controller-manager would be configured with option `--allocate-node-cidrs=false` to disable the in-tree node IPAM controller.

### cloud-controller-manager

The following configurations from cloud-controller-manager would be used as default options:

| name | type | default | description |
| ----- | -----| ----- | ----- |
| allocate-node-cidrs | bool | true | Should CIDRs for Pods be allocated and set on the cloud provider. |
| cluster-cidr | string | "10.244.0.0/16" | CIDR Range for Pods in cluster. Requires --allocate-node-cidrs to be true. It will be ignored when enabling dualstack. |
| service-cluster-ip-range | string | "" | CIDR Range for Services in cluster, this would get excluded from the allocatable range. Requires --allocate-node-cidrs to be true. |
| node-cidr-mask-size | int | 24 | Mask size for node cidr in cluster. Default is 24 for IPv4 and 64 for IPv6. |
| node-cidr-mask-size-ipv4 | int | 24 | Mask size for IPv4 node cidr in dual-stack cluster. Default is 24. |
| node-cidr-mask-size-ipv6 | int | 64 | Mask size for IPv6 node cidr in dual-stack cluster. Default is 64. |
| cidr-allocator-type | string | "RangeAllocator" | The CIDR allocator type. "RangeAllocator" or "CloudAllocator". |

## Limitations

1. We plan to integrate out-of-tree node ipam controller with [cluster-api-provider-azure](https://github.com/kubernetes-sigs/cluster-api-provider-azure) to provider a better experience. Before that, 
the manual configuration is required.
1. It is not supported to change the custom mask size value on the tag once it is set.
1. For now, there is no e2e test covering this feature, so there can be potential bugs. It is not recommended enabling
it in the production environment.
