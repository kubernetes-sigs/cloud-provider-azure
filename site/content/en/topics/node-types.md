---
title: "Support Multiple Node Types"
linkTitle: "Node Types"
weight: 4
type: docs
description: >
    Node type description in provider azure.
---

Kubernetes v1.26 adds support for using [Azure VMSS Flex VMs](https://learn.microsoft.com/en-us/azure/virtual-machine-scale-sets/virtual-machine-scale-sets-orchestration-modes#scale-sets-with-flexible-orchestration) as the cluster nodes. Besides, mixing up different VM types in the same cluster is also supported. There is no API change expected from end users' perspective when manipulating the Kubernetes cluster, however, users can choose to specify the VM type when configuring the Cloud Provider to further optimize the API calls in Cloud Controller Manager. Below are the configurations suggested based on the cluster modes.

|Node Type|Configurations|Comments|
|---|---|---|
|Standalone VMs or AvailabilitySet VMs|vmType == standard|This will bypass the node type check and assume all the nodes in the cluster are standalone VMs / AvailabilitySet VMs. This should only be used for pure standalone VM / AvailabilitySet VM clusters. |
|VMSS Uniform VMs|vmType==vmss && DisableAvailabilitySetNodes==true && EnbleVmssFlexNodes==false|This will bypass the node type check and assume all the nodes in the cluster are VMSS Uniform VMs. This should only be used for pure VMSS Uniform VM clusters.|
|VMSS Flex VMs|vmType==vmssflex|This will bypass the node type check and assume all the nodes in the cluster are VMSS Flex VMs. This should only be used for pure VMSS Flex VM clusters (since v1.26.0).|
|Standalone VMs, AvailabilitySet VMs, VMSS Uniform VMs and VMSS Flex VMs|vmType==vmss && (DisableAvailabilitySetNodes==false \|\| EnbleVmssFlexNodes==true)|This should be used the clusters of which the nodes are mixed from standalone VMs, AvailabilitySet VMs, VMSS Flex VMs (since v1.26.0) and VMSS Uniform VMs. Node type will be checked and corresponding cloud provider API will be called based on the ndoe type.|
