---
title: "Use Availability Zones"
linkTitle: "Availability Zones"
weight: 4
type: docs
description: >
    Use availability zones in provider azure.
---

**Feature Status:** Alpha since v1.12.

Kubernetes v1.12 adds support for [Azure availability zones (AZ)](https://azure.microsoft.com/en-us/global-infrastructure/availability-zones/). Nodes in availability zone will be added with label `failure-domain.beta.kubernetes.io/zone=<region>-<AZ>` and topology-aware provisioning is added for Azure managed disks storage class.

**TOC:**

<!-- TOC -->

- [Availability Zones](#availability-zones)
    - [Pre-requirements](#pre-requirements)
    - [Node labels](#node-labels)
    - [Load Balancer](#load-balancer)
    - [Managed Disks](#managed-disks)
        - [StorageClass examples](#storageclass-examples)
        - [PV examples](#pv-examples)
    - [Appendix](#appendix)
    - [Reference](#reference)

<!-- /TOC -->

## Pre-requirements

Because only standard load balancer is supported with AZ, it is a prerequisite to enable AZ for the cluster. It should be configured in Azure cloud provider configure file (e.g. `/etc/kubernetes/cloud-config/azure.json`):

```json
{
    "loadBalancerSku": "standard",
    ...
}
```

If topology-aware provisioning feature is used, feature gate `VolumeScheduling` should be enabled on master components (e.g. kube-apiserver, kube-controller-manager and kube-scheduler).

## Node labels

Both zoned and unzoned nodes are supported, but the value of node label `failure-domain.beta.kubernetes.io/zone` are different:

- For zoned nodes, the value is `<region>-<AZ>`, e.g. `centralus-1`.
- For unzoned nodes, the value is faultDomain, e.g. `0`.

e.g.

```yaml
$ kubectl get nodes --show-labels
NAME                STATUS    AGE   VERSION    LABELS
kubernetes-node12   Ready     6m    v1.11      failure-domain.beta.kubernetes.io/region=centralus,failure-domain.beta.kubernetes.io/zone=centralus-1,...
```

## Load Balancer

`loadBalancerSku` has been set to `standard` in cloud provider configure file, so standard load balancer and standard public IPs will be provisioned automatically for services with type `LoadBalancer`. Both load balancer and public IPs are zone redundant.

## Managed Disks

Zone-aware and topology-aware provisioning are supported for Azure managed disks. To support these features, a few options are added in AzureDisk storage class:

- **zoned**: indicates whether new disks are provisioned with AZ. Default is true.
- **allowedTopologies**: indicates which topologies are allowed for topology-aware provisioning. Only can be set if `zoned` is not false.

### StorageClass examples

An example of zone-aware provisioning storage class is:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  annotations:
  labels:
    kubernetes.io/cluster-service: "true"
  name: managed-premium
parameters:
  kind: Managed
  storageaccounttype: Premium_LRS
  zoned: "true"
provisioner: kubernetes.io/azure-disk
volumeBindingMode: WaitForFirstConsumer
```

Another example of topology-aware provisioning storage class is:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  annotations:
  labels:
    kubernetes.io/cluster-service: "true"
  name: managed-premium
parameters:
  kind: Managed
  storageaccounttype: Premium_LRS
provisioner: kubernetes.io/azure-disk
volumeBindingMode: WaitForFirstConsumer
allowedTopologies:
- matchLabelExpressions:
  - key: failure-domain.beta.kubernetes.io/zone
    values:
    - centralus-1
    - centralus-2
```

### PV examples

When feature gate `VolumeScheduling` disabled, no `NodeAffinity` set for zoned PV:

```shell script
$ kubectl describe pv
Name:              pvc-d30dad05-9ad8-11e8-94f2-000d3a07de8c
Labels:            failure-domain.beta.kubernetes.io/region=southeastasia
                   failure-domain.beta.kubernetes.io/zone=southeastasia-2
Annotations:       pv.kubernetes.io/bound-by-controller=yes
                   pv.kubernetes.io/provisioned-by=kubernetes.io/azure-disk
                   volumehelper.VolumeDynamicallyCreatedByKey=azure-disk-dynamic-provisioner
Finalizers:        [kubernetes.io/pv-protection]
StorageClass:      default
Status:            Bound
Claim:             default/pvc-azuredisk
Reclaim Policy:    Delete
Access Modes:      RWO
Capacity:          5Gi
Node Affinity:
  Required Terms:
    Term 0:        failure-domain.beta.kubernetes.io/region in [southeastasia]
                   failure-domain.beta.kubernetes.io/zone in [southeastasia-2]
Message:
Source:
    Type:         AzureDisk (an Azure Data Disk mount on the host and bind mount to the pod)
    DiskName:     k8s-5b3d7b8f-dynamic-pvc-d30dad05-9ad8-11e8-94f2-000d3a07de8c
    DiskURI:      /subscriptions/<subscription-id>/resourceGroups/<rg-name>/providers/Microsoft.Compute/disks/k8s-5b3d7b8f-dynamic-pvc-d30dad05-9ad8-11e8-94f2-000d3a07de8c
    Kind:         Managed
    FSType:
    CachingMode:  None
    ReadOnly:     false
Events:           <none>
```

When feature gate `VolumeScheduling` enabled, `NodeAffinity` will be populated for zoned PV:

```shell script
$ kubectl describe pv
Name:              pvc-0284337b-9ada-11e8-a7f6-000d3a07de8c
Labels:            failure-domain.beta.kubernetes.io/region=southeastasia
                   failure-domain.beta.kubernetes.io/zone=southeastasia-2
Annotations:       pv.kubernetes.io/bound-by-controller=yes
                   pv.kubernetes.io/provisioned-by=kubernetes.io/azure-disk
                   volumehelper.VolumeDynamicallyCreatedByKey=azure-disk-dynamic-provisioner
Finalizers:        [kubernetes.io/pv-protection]
StorageClass:      default
Status:            Bound
Claim:             default/pvc-azuredisk
Reclaim Policy:    Delete
Access Modes:      RWO
Capacity:          5Gi
Node Affinity:
  Required Terms:
    Term 0:        failure-domain.beta.kubernetes.io/region in [southeastasia]
                   failure-domain.beta.kubernetes.io/zone in [southeastasia-2]
Message:
Source:
    Type:         AzureDisk (an Azure Data Disk mount on the host and bind mount to the pod)
    DiskName:     k8s-5b3d7b8f-dynamic-pvc-0284337b-9ada-11e8-a7f6-000d3a07de8c
    DiskURI:      /subscriptions/<subscription-id>/resourceGroups/<rg-name>/providers/Microsoft.Compute/disks/k8s-5b3d7b8f-dynamic-pvc-0284337b-9ada-11e8-a7f6-000d3a07de8c
    Kind:         Managed
    FSType:
    CachingMode:  None
    ReadOnly:     false
Events:           <none>
```

While unzoned disks are not able to attach in zoned nodes, `NodeAffinity` will also be set for them so that they will only be scheduled to unzoned nodes:

```shell script
$ kubectl describe pv pvc-bdf93a67-9c45-11e8-ba6f-000d3a07de8c
Name:              pvc-bdf93a67-9c45-11e8-ba6f-000d3a07de8c
Labels:            <none>
Annotations:       pv.kubernetes.io/bound-by-controller=yes
                   pv.kubernetes.io/provisioned-by=kubernetes.io/azure-disk
                   volumehelper.VolumeDynamicallyCreatedByKey=azure-disk-dynamic-provisioner
Finalizers:        [kubernetes.io/pv-protection]
StorageClass:      azuredisk-unzoned
Status:            Bound
Claim:             default/unzoned-pvc
Reclaim Policy:    Delete
Access Modes:      RWO
Capacity:          5Gi
Node Affinity:
  Required Terms:
    Term 0:        failure-domain.beta.kubernetes.io/region in [southeastasia]
                   failure-domain.beta.kubernetes.io/zone in [0]
    Term 1:        failure-domain.beta.kubernetes.io/region in [southeastasia]
                   failure-domain.beta.kubernetes.io/zone in [1]
    Term 2:        failure-domain.beta.kubernetes.io/region in [southeastasia]
                   failure-domain.beta.kubernetes.io/zone in [2]
Message:
Source:
    Type:         AzureDisk (an Azure Data Disk mount on the host and bind mount to the pod)
    DiskName:     k8s-5b3d7b8f-dynamic-pvc-bdf93a67-9c45-11e8-ba6f-000d3a07de8c
    DiskURI:      /subscriptions/<subscription>/resourceGroups/<rg-name>/providers/Microsoft.Compute/disks/k8s-5b3d7b8f-dynamic-pvc-bdf93a67-9c45-11e8-ba6f-000d3a07de8c
    Kind:         Managed
    FSType:
    CachingMode:  None
    ReadOnly:     false
Events:           <none>
```

## Appendix

Note that unlike most cases, fault domain and availability zones mean different on Azure:

- A Fault Domain (FD) is essentially a rack of servers. It consumes subsystems like network, power, cooling etc.
- Availability Zones are unique physical locations within an Azure region. Each zone is made up of one or more data centers equipped with independent power, cooling, and networking.

An Availability Zone in an Azure region is a combination of a fault domain, and an update domain (Same like FD, but for updates. When upgrading a deployment, it is carried out one update domain at a time). For example, if you create three or more VMs across three zones in an Azure region, your VMs are effectively distributed across three fault domains and three update domains.

## Reference

See design docs for AZ in [KEP for Azure availability zones](https://github.com/kubernetes/enhancements/tree/master/keps/sig-cloud-provider/azure/586-azure-availability-zones).
