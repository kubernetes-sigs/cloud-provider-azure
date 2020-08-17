---
title: "AzureDisk CSI Driver Known Issues"
linkTitle: "AzureDisk"
type: docs
---

<!-- TOC -->

- [azure disk plugin known issues](#azure-disk-plugin-known-issues)
    - [Recommended stable version for azure disk](#recommended-stable-version-for-azure-disk)
    - [1. disk attach error](#1-disk-attach-error)
    - [2. disk unavailable after attach/detach a data disk on a node](#2-disk-unavailable-after-attachdetach-a-data-disk-on-a-node)
    - [3. Azure disk support on Sovereign Cloud](#3-azure-disk-support-on-sovereign-cloud)
    - [4. Time cost for Azure Disk PVC mount](#4-time-cost-for-azure-disk-pvc-mount)
    - [5. Azure disk PVC `Multi-Attach error`, makes disk mount very slow or mount failure forever](#5-azure-disk-pvc-multi-attach-error-makes-disk-mount-very-slow-or-mount-failure-forever)
    - [6. WaitForAttach failed for azure disk: parsing "/dev/disk/azure/scsi1/lun1": invalid syntax](#6-waitforattach-failed-for-azure-disk-parsing-devdiskazurescsi1lun1-invalid-syntax)
    - [7. `uid` and `gid` setting in azure disk](#7-uid-and-gid-setting-in-azure-disk)
    - [8. `Addition of a blob based disk to VM with managed disks is not supported`](#8-addition-of-a-blob-based-disk-to-vm-with-managed-disks-is-not-supported)
    - [9. dynamic azure disk PVC try to access wrong storage account (of other resource group)](#9-dynamic-azure-disk-pvc-try-to-access-wrong-storage-account-of-other-resource-group)
    - [10. data loss if using existing azure disk with partitions in disk mount](#10-data-loss-if-using-existing-azure-disk-with-partitions-in-disk-mount)
    - [11. Delete azure disk PVC which is already in use by a pod](#11-delete-azure-disk-pvc-which-is-already-in-use-by-a-pod)
    - [12. create azure disk PVC failed due to account creation failure](#12-create-azure-disk-pvc-failed-due-to-account-creation-failure)
    - [13. cannot find Lun for disk](#13-cannot-find-Lun-for-disk)
    - [14. azure disk attach/detach failure, mount issue, i/o error](#14-azure-disk-attachdetach-failure-mount-issue-io-error)

<!-- /TOC -->

## Recommended stable version for azure disk

| k8s version | stable version |
| ---- | ---- |
| v1.7 | 1.7.14 or later |
| v1.8 | 1.8.13 or later |
| v1.9 | 1.9.7 or later (1.9.6 on AKS) |
| v1.10 | 1.10.12 or later |
| v1.11 | 1.11.6 or later |
| v1.12 | 1.12.4 or later |
| v1.13 | 1.13.0 |

## 1. disk attach error

**Issue details**:

In some corner case(detaching multiple disks on a node simultaneously), when scheduling a pod with azure disk mount from one node to another, there could be lots of disk attach error(no recovery) due to the disk not being released in time from the previous node. This issue is due to lack of lock before DetachDisk operation, actually there should be a central lock for both AttachDisk and DetachDisk operations, only one AttachDisk or DetachDisk operation is allowed at one time.

The disk attach error could be like following:

```sh
Cannot attach data disk 'cdb-dynamic-pvc-92972088-11b9-11e8-888f-000d3a018174' to VM 'kn-edge-0' because the disk is currently being detached or the last detach operation failed. Please wait until the disk is completely detached and then try again or delete/detach the disk explicitly again.
```

**Related issues**

- [Azure Disk Detach are not working with multiple disk detach on the same Node](https://github.com/kubernetes/kubernetes/issues/60101)
- [Azure disk fails to attach and mount, causing rescheduled pod to stall following node disruption](https://github.com/kubernetes/kubernetes/issues/46421)
- [Since Intel CPU Azure update, new Azure Disks are not mounting, very critical... ](https://github.com/Azure/acs-engine/issues/2002)
- [Busy azure-disk regularly fail to mount causing K8S Pod deployments to halt](https://github.com/Azure/ACS/issues/12)


**Mitigation**:

- option#1: Update every agent node that has attached or detached the disk in problem

In Azure cloud shell, run

```sh
$vm = Get-AzureRMVM -ResourceGroupName $rg -Name $vmname
Update-AzureRmVM -ResourceGroupName $rg -VM $vm -verbose -debug
```

In Azure cli, run

```sh
az vm update -g <group> -n <name>
```

- option#2:

1) ```kubectl cordon node``` #make sure no scheduling on this node
2) ```kubectl drain node```  #schedule pod in current node to other node
3) restart the Azure VM for node via the API or portal, wait until VM is "Running"
4) ```kubectl uncordon node```

**Fix**

- PR [fix race condition issue when detaching azure disk](https://github.com/kubernetes/kubernetes/pull/60183) has fixed this issue by add a lock before DetachDisk

| k8s version | fixed version |
| ---- | ---- |
| v1.6 | no fix since v1.6 does not accept any cherry-pick |
| v1.7 | 1.7.14 |
| v1.8 | 1.8.9 |
| v1.9 | 1.9.5 |
| v1.10 | 1.10.0 |

## 2. disk unavailable after attach/detach a data disk on a node
> 💡 NOTE: Azure platform has fixed the host cache issue, the suggested host cache setting of data disk is `ReadOnly` now, more details about [azure disk cache setting](https://docs.microsoft.com/en-us/azure/virtual-machines/windows/premium-storage-performance#disk-caching)
**Issue details**:

From k8s v1.7, default host cache setting changed from `None` to `ReadWrite`, this change would lead to device name change after attach multiple disks on a node, finally lead to disk unavailable from pod. When access data disk inside a pod, will get following error:

```sh
[root@admin-0 /]# ls /datadisk
ls: reading directory .: Input/output error
```

In my testing on Ubuntu 16.04 D2_V2 VM, when attaching the 6th data disk will cause device name change on agent node, e.g. following lun0 disk should be `sdc` other than `sdk`.

```sh
azureuser@k8s-agentpool2-40588258-0:~$ tree /dev/disk/azure
...
â””â”€â”€ scsi1
    â”œâ”€â”€ lun0 -> ../../../sdk
    â”œâ”€â”€ lun1 -> ../../../sdj
    â”œâ”€â”€ lun2 -> ../../../sde
    â”œâ”€â”€ lun3 -> ../../../sdf
    â”œâ”€â”€ lun4 -> ../../../sdg
    â”œâ”€â”€ lun5 -> ../../../sdh
    â””â”€â”€ lun6 -> ../../../sdi
```

**Related issues**

- [device name change due to azure disk host cache setting](https://github.com/kubernetes/kubernetes/issues/60344)
- [unable to use azure disk in StatefulSet since /dev/sd* changed after detach/attach disk](https://github.com/kubernetes/kubernetes/issues/57444)
- [Disk error when pods are mounting a certain amount of volumes on a node](https://github.com/Azure/AKS/issues/201)
- [unable to use azure disk in StatefulSet since /dev/sd* changed after detach/attach disk](https://github.com/Azure/acs-engine/issues/1918)
- [Input/output error when accessing PV](https://github.com/Azure/AKS/issues/297)
- [PersistentVolumeClaims changing to Read-only file system suddenly](https://github.com/Azure/ACS/issues/113)

**Workaround**:

- add `cachingmode: None` in azure disk storage class(default is `ReadWrite`), e.g.

```yaml
kind: StorageClass
apiVersion: storage.k8s.io/v1
metadata:
  name: hdd
provisioner: kubernetes.io/azure-disk
parameters:
  skuname: Standard_LRS
  kind: Managed
  cachingmode: None
```

**Fix**

- PR [fix device name change issue for azure disk](https://github.com/kubernetes/kubernetes/pull/60346) could fix this issue too, it will change default `cachingmode` value from `ReadWrite` to `None`.

| k8s version | fixed version |
| ---- | ---- |
| v1.6 | no such issue as `cachingmode` is already `None` by default |
| v1.7 | 1.7.14 |
| v1.8 | 1.8.11 |
| v1.9 | 1.9.4 |
| v1.10 | 1.10.0 |

## 3. Azure disk support on Sovereign Cloud

**Fix**

- PR [Azure disk on Sovereign Cloud](https://github.com/kubernetes/kubernetes/pull/50673) fixed this issue

| k8s version | fixed version |
| ---- | ---- |
| v1.7 | 1.7.9 |
| v1.8 | 1.8.3 |
| v1.9 | 1.9.0 |
| v1.10 | 1.10.0 |

## 4. Time cost for Azure Disk PVC mount

Original time cost for Azure Disk PVC mount on a standard node size(e.g. Standard_D2_V2) is around 1 minute, `podAttachAndMountTimeout` is [2 minutes](https://github.com/kubernetes/kubernetes/blob/release-1.7/pkg/kubelet/volumemanager/volume_manager.go#L76), total `waitForAttachTimeout` is [10 minutes](https://github.com/kubernetes/kubernetes/blob/release-1.7/pkg/kubelet/volumemanager/volume_manager.go#L88), so a disk remount(detach and attach in sequential) would possibly cost more than 2min, thus may fail.

> Note: for some smaller VM size which has only 1 CPU core, time cost would be much bigger(e.g. > 10min) since container is hard to get CPU slot.

**Related issues**

- ['timeout expired waiting for volumes to attach/mount for pod when cluster' when node-vm-size is Standard_B1s](https://github.com/Azure/AKS/issues/166)

**Fix**

- PR [using cache fix](https://github.com/kubernetes/kubernetes/pull/57432) fixed this issue, which could reduce the mount time cost to around 30s.

| k8s version | fixed version |
| ---- | ---- |
| v1.8 | no fix |
| v1.9 | 1.9.2 |
| v1.10 | 1.10.0 |

## 5. Azure disk PVC `Multi-Attach error`, makes disk mount very slow or mount failure forever

**Issue details**:

When schedule a pod with azure disk volume from one node to another, total time cost of detach & attach is around 1 min from v1.9.2, while in v1.9.x, there is an [UnmountDevice failure issue in containerized kubelet](https://github.com/kubernetes/kubernetes/issues/62282) which makes disk mount very slow or mount failure forever, this issue only exists in v1.9.x due to PR [Refactor nsenter](https://github.com/kubernetes/kubernetes/pull/51771), v1.10.0 won't have this issue since `devicePath` is updated in [v1.10 code](https://github.com/kubernetes/kubernetes/blob/release-1.10/pkg/volume/util/operationexecutor/operation_generator.go#L1130-L1131)

**error logs**:

- `kubectl describe po POD-NAME`

```sh
Events:
  Type     Reason                 Age   From                               Message
  ----     ------                 ----  ----                               -------
  Normal   Scheduled              3m    default-scheduler                  Successfully assigned deployment-azuredisk1-6cd8bc7945-kbkvz to k8s-agentpool-88970029-0
  Warning  FailedAttachVolume     3m    attachdetach-controller            Multi-Attach error for volume "pvc-6f2d0788-3b0b-11e8-a378-000d3afe2762" Volume is already exclusively attached to one node and can't be attached to another
  Normal   SuccessfulMountVolume  3m    kubelet, k8s-agentpool-88970029-0  MountVolume.SetUp succeeded for volume "default-token-qt7h6"
  Warning  FailedMount            1m    kubelet, k8s-agentpool-88970029-0  Unable to mount volumes for pod "deployment-azuredisk1-6cd8bc7945-kbkvz_default(5346c040-3e4c-11e8-a378-000d3afe2762)": timeout expired waiting for volumes to attach/mount for pod "default"/"deployment-azuredisk1-6cd8bc7945-kbkvz". list of unattached/unmounted volumes=[azuredisk]
```

- kubelet logs from the new node

```sh
E0412 20:08:10.920284    7602 nestedpendingoperations.go:263] Operation for "\"kubernetes.io/azure-disk//subscriptions/xxx/resourceGroups/MC_xxx_eastus/providers/Microsoft.Compute/disks/kubernetes-dynamic-pvc-11035a31-3e8d-11e8-82ec-0a58ac1f04cf\"" failed. No retries permitted until 2018-04-12 20:08:12.920234762 +0000 UTC m=+1467.278612421 (durationBeforeRetry 2s). Error: "Volume has not been added to the list of VolumesInUse in the node's volume status for volume \"pvc-11035a31-3e8d-11e8-82ec-0a58ac1f04cf\" (UniqueName: \"kubernetes.io/azure-disk//subscriptions/xxx/resourceGroups/MC_xxx_eastus/providers/Microsoft.Compute/disks/kubernetes-dynamic-pvc-11035a31-3e8d-11e8-82ec-0a58ac1f04cf\") pod \"symbiont-node-consul-0\" (UID: \"11043b12-3e8d-11e8-82ec-0a58ac1f04cf\") "
```

**Related issues**

- [UnmountDevice would fail in containerized kubelet](https://github.com/kubernetes/kubernetes/issues/62282)
- [upgrade k8s process is broke](https://github.com/Azure/acs-engine/issues/2022)

**Mitigation**:

If azure disk PVC mount successfully in the end, there is no action, while if it could not be mounted for more than 20min, following actions could be taken:

- check whether `volumesInUse` list has unmounted azure disks, run:
```
kubectl get no NODE-NAME -o yaml > node.log
```
all volumes in `volumesInUse` should be also in `volumesAttached`, otherwise there would be issue
- restart kubelet on the original node would solve this issue: `sudo kubectl kubelet restart`

**Fix**

- PR [fix nsenter GetFileType issue in containerized kubelet](https://github.com/kubernetes/kubernetes/pull/62467) fixed this issue

| k8s version | fixed version |
| ---- | ---- |
| v1.8 | no such issue |
| v1.9 | v1.9.7 |
| v1.10 | no such issue |

After fix in v1.9.7, it took about 1 minute for scheduling one azure disk mount from one node to another, you could find details [here](https://github.com/kubernetes/kubernetes/issues/62282#issuecomment-380794459).

Since azure disk attach/detach operation on a VM cannot be parallel, scheduling 3 azure disk mounts from one node to another would cost about 3 minutes.

## 6. WaitForAttach failed for azure disk: parsing "/dev/disk/azure/scsi1/lun1": invalid syntax

**Issue details**:
MountVolume.WaitForAttach may fail in the azure disk remount

**error logs**:

in v1.10.0 & v1.10.1, `MountVolume.WaitForAttach` will fail in the azure disk remount, error logs would be like following:

- incorrect `DevicePath` format on Linux
```
MountVolume.WaitForAttach failed for volume "pvc-f1562ecb-3e5f-11e8-ab6b-000d3af9f967" : azureDisk - Wait for attach expect device path as a lun number, instead got: /dev/disk/azure/scsi1/lun1 (strconv.Atoi: parsing "/dev/disk/azure/scsi1/lun1": invalid syntax)
  Warning  FailedMount             1m (x10 over 21m)   kubelet, k8s-agentpool-66825246-0  Unable to mount volumes for pod
```
- wrong `DevicePath`(LUN) number on Windows
```
  Warning  FailedMount             1m    kubelet, 15282k8s9010    MountVolume.WaitForAttach failed for volume "disk01" : azureDisk - WaitForAttach failed within timeout node (15282k8s9010) diskId:(andy-mghyb
1102-dynamic-pvc-6c526c51-4a18-11e8-ab5c-000d3af7b38e) lun:(4)
```

**Related issues**

- [WaitForAttach failed for azure disk: parsing "/dev/disk/azure/scsi1/lun1": invalid syntax](https://github.com/kubernetes/kubernetes/issues/62540)
- [Pod unable to attach PV after being deleted (Wait for attach expect device path as a lun number, instead got: /dev/disk/azure/scsi1/lun0 (strconv.Atoi: parsing "/dev/disk/azure/scsi1/lun0": invalid syntax)](https://github.com/Azure/acs-engine/issues/2906)

**Fix**

- PR [fix WaitForAttach failure issue for azure disk](https://github.com/kubernetes/kubernetes/pull/62612) fixed this issue

| k8s version | fixed version |
| ---- | ---- |
| v1.8 | no such issue |
| v1.9 | no such issue |
| v1.10 | 1.10.2 |

## 7. `uid` and `gid` setting in azure disk

**Issue details**:
Unlike azure file mountOptions, you will get following failure if set `mountOptions` like `uid=999,gid=999` in azure disk mount:

```
azureDisk - mountDevice:FormatAndMount failed with exit status 32
```

That's because azureDisk use ext4 file system by default, mountOptions like [uid=x,gid=x] could not be set in mount time.

**Related issues**

- [Timeout expired waiting for volumes to attach](https://github.com/kubernetes/kubernetes/issues/67014)

**Solution**:

 - option#1: Set uid in `runAsUser` and gid in `fsGroup` for pod: [security context for a Pod](https://kubernetes.io/docs/tasks/configure-pod-container/security-context/)

e.g. Following setting will set pod run as root, make it accessible to any file:
```yaml
apiVersion: v1
kind: Pod
metadata:
  name: security-context-demo
spec:
  securityContext:
    runAsUser: 0
    fsGroup: 0
```

> Note: Since gid & uid is mounted as 0(root) by default, if set as non-root(e.g. 1000), k8s will use chown to change all dir/files under that disk, this is a time consuming job, which would make mount device very slow, in this issue: [Timeout expired waiting for volumes to attach](https://github.com/kubernetes/kubernetes/issues/67014#issuecomment-413546283), it costs about 10 min for chown operation complete.

 - option#2: use `chown` in `initContainers`
```
initContainers:
- name: volume-mount
  image: busybox
  command: ["sh", "-c", "chown -R 100:100 /data"]
  volumeMounts:
  - name: <your data volume>
    mountPath: /data
```

## 8. `Addition of a blob based disk to VM with managed disks is not supported`

**Issue details**:

Following error may occur if attach a blob based(unmanaged) disk to VM with managed disks:

```sh
  Warning  FailedMount            42s (x2 over 1m)  attachdetach                    AttachVolume.Attach failed for volume "pvc-f17e5e77-474e-11e8-a2ea-000d3a10df6d" : Attach volume "holo-k8s-dev-dynamic-pvc-f17e5e77-474e-11e8-a2ea-000d3a10df6d" to instance "k8s-master-92699158-0" failed with compute.VirtualMachinesClient#CreateOrUpdate: Failure responding to request: StatusCode=409 -- Original Error: autorest/azure: Service returned an error. Status=409 Code="OperationNotAllowed" Message="Addition of a blob based disk to VM with managed disks is not supported."
```

This issue is by design as in Azure, there are two kinds of disks, blob based(unmanaged) disk and managed disk, an Azure VM could not attach both of these two kinds of disks.

**Solution**:

Use `default` azure disk storage class in aks-engine, as `default` will always be identical to the agent pool, that is, if VM is managed, it will be managed azure disk class, if unmanaged, then it's unmanaged disk class.

## 9. dynamic azure disk PVC try to access wrong storage account (of other resource group)

**Issue details**:

In a k8s cluster with **blob based** VMs(won't happen in AKS since AKS only use managed disk), create dynamic azure disk PVC may fail, error logs is like following:

```sh
Failed to provision volume with StorageClass "default": azureDisk - account ds6c822a4d484211eXXXXXX does not exist while trying to create/ensure default container
```

**Related issues**

- [Multiple clusters - dynamic PVCs try to access wrong storage account (of other resource group)](https://github.com/Azure/acs-engine/issues/2768)

**Fix**

- PR [fix storage account not found issue: use ListByResourceGroup instead of List()](https://github.com/kubernetes/kubernetes/pull/56474) fixed this issue

| k8s version | fixed version |
| ---- | ---- |
| v1.8 | 1.8.13 |
| v1.9 | 1.9.9 |
| v1.10 | no such issue |

**Work around**:

this bug only exists in blob based VM in v1.8.x, v1.9.x, so if specify `ManagedDisks` when creating k8s cluster in aks-engine(AKS is using managed disk by default), it won't have this issue:

```json
    "agentPoolProfiles": [
      {
        ...
        "storageProfile" : "ManagedDisks",
        ...
      }
```

## 10. data loss if using existing azure disk with partitions in disk mount

**Issue details**:

When use an existing azure disk(also called [static provisioning](https://github.com/andyzhangx/demo/tree/master/linux/azuredisk#static-provisioning-for-azure-disk)) in pod, if that disk has partitions, the disk will be formatted in the pod mounting process, actually k8s volume don't support mount disk with partitions, disk mount would fail finally. While for mounting existing **azure** disk that has partitions, data will be lost since it will format that disk first. This issue happens only on **Linux**.

**Related issues**

- [data loss if using existing azure disk with partitions in disk mount](https://github.com/kubernetes/kubernetes/issues/63235)

**Fix**

- PR [fix data loss issue if using existing azure disk with partitions in disk mount](https://github.com/kubernetes/kubernetes/pull/63270) will let azure provider return error when mounting existing azure disk that has partitions

| k8s version | fixed version |
| ---- | ---- |
| v1.8 | 1.8.15 |
| v1.9 | 1.9.11 |
| v1.10 | 1.10.5 |
| v1.11 | 1.11.0 |

**Work around**:

Don't use existing azure disk that has partitions, e.g. following disk in LUN 0 that has one partition:

```sh
azureuser@aks-nodepool1-28371372-0:/$ ls -l /dev/disk/azure/scsi1/
total 0
lrwxrwxrwx 1 root root 12 Apr 27 08:04 lun0 -> ../../../sdc
lrwxrwxrwx 1 root root 13 Apr 27 08:04 lun0-part1 -> ../../../sdc1
```

## 11. Delete azure disk PVC which is already in use by a pod

**Issue details**:

Following error may occur if delete azure disk PVC which is already in use by a pod:

```sh
kubectl describe pv pvc-d8eebc1d-74d3-11e8-902b-e22b71bb1c06
...
Message:         disk.DisksClient#Delete: Failure responding to request: StatusCode=409 -- Original Error: autorest/azure: Service returned an error. Status=409 Code="OperationNotAllowed" Message="Disk kubernetes-dynamic-pvc-d8eebc1d-74d3-11e8-902b-e22b71bb1c06 is attached to VM /subscriptions/{subs-id}/resourceGroups/MC_markito-aks-pvc_markito-aks-pvc_westus/providers/Microsoft.Compute/virtualMachines/aks-agentpool-25259074-0."
```

**Fix**:

This is a common k8s issue, other cloud provider would also has this issue. There is a [PVC protection](https://kubernetes.io/docs/tasks/administer-cluster/pvc-protection/) feature to prevent this, it's alpha in v1.9, and beta(enabled by default) in v1.10

**Work around**:
delete pod first and then delete azure disk pvc after a few minutes

## 12. create azure disk PVC failed due to account creation failure

> please note this issue only happens on **unmanaged** k8s cluster

**Issue details**: User may get `Account property kind is invalid for the request` error when trying to create a new **unmanaged** azure disk PVC, error would be like following:

```sh
azureuser@k8s-master-17140924-0:/tmp$ kubectl describe pvc
Name:          pvc-azuredisk
Namespace:     default
StorageClass:  hdd
Status:        Bound
...
Events:
  Type     Reason                 Age                From                         Message
  ----     ------                 ----               ----                         -------
  Warning  ProvisioningFailed     31m                persistentvolume-controller  Failed to provision volume with StorageClass "hdd": Create Storage Account: ds10e15ed89c5811e8a0a70, error: storage.AccountsClient#Create: Failure sending request: StatusCode=400 -- Original Error: Code="AccountPropertyIsInvalid" Message="Account property kind is invalid for the request."
```

**Fix**

- PR [fix azure disk create failure due to sdk upgrade](https://github.com/kubernetes/kubernetes/pull/67236) fixed this issue

| k8s version | fixed version |
| ---- | ---- |
| v1.9 | no such issue |
| v1.10 | no such issue |
| v1.11 | 1.11.3 |
| v1.12 | no such issue |

**Work around**:

- create a storage account and specify that account in azure disk storage class, e.g.

```yaml
kind: StorageClass
apiVersion: storage.k8s.io/v1beta1
metadata:
  name: ssd
provisioner: kubernetes.io/azure-disk
parameters:
  skuname: Premium_LRS
  storageAccount: customerstorageaccount
  kind: Dedicated
 ```

## 13. cannot find Lun for disk

**Issue details**:

Following error may occur if attach a disk to a node:
```
MountVolume.WaitForAttach failed for volume "pvc-12b458f4-c23f-11e8-8d27-46799c22b7c6" : Cannot find Lun for disk kubernetes-dynamic-pvc-12b458f4-c23f-11e8-8d27-46799c22b7c6
```

**Related issues**

- [GetAzureDiskLun sometimes costs 1 min which is too long time](https://github.com/kubernetes/kubernetes/issues/69262)

**Fix**

- PR [fix azure disk attachment error on Linux](https://github.com/kubernetes/kubernetes/pull/70002) will extract the LUN num from device path **only on Linux**

| k8s version | fixed version |
| ---- | ---- |
| v1.9 | no such issue |
| v1.10 | 1.10.10 |
| v1.11 | 1.11.5 |
| v1.12 | 1.12.3 |
| v1.13 | no such issue |

**Work around**:

wait for a few more minutes should work

## 14. azure disk attach/detach failure, mount issue, i/o error

**Issue details**:

We found a disk attach/detach issue due to [dirty vm cache PR](https://github.com/kubernetes/kubernetes/pull/58313) introduced from v1.9.2, it would lead to following disk issues:
 - disk attach/detach failure for a long time
 - disk I/O error
 - unexpected disk detachment from VM
 - VM running into failed state due to attaching non-existing disk

> Note: above error may **only** happen when there are multiple disk attach/detach operations in parallel and it's not easy to repro since it happens on a little possibility.


**Related issues**

- [Azure Disks volume attach still times out on Kubernetes 1.10](https://github.com/kubernetes/kubernetes/issues/71344)
- [Azure Disks occasionally mounted in a way leading to I/O errors](https://github.com/kubernetes/kubernetes/issues/71453)

**Fix**

We changed the azure disk attach/detach retry logic in k8s v1.13, switch to use k8s attach-detach controller to do attach/detach disk retry and clean vm cache after every disk operation, this issue is proved to be fixed in our disk attach/detach stress test and also verified in customer env:
- PR [remove retry operation on attach/detach azure disk in azure cloud provider](https://github.com/kubernetes/kubernetes/pull/70568)
- PR [fix azure disk attach/detach failed forever issue](https://github.com/kubernetes/kubernetes/pull/71377)
- PR [fix detach azure disk issue due to dirty cache](https://github.com/kubernetes/kubernetes/pull/71495)


| k8s version | fixed version |
| ---- | ---- |
| v1.9 | issue introduced in v1.9.2, no cherry-pick fix allowed|
| v1.10 | 1.10.12 |
| v1.11 | 1.11.6 |
| v1.12 | 1.12.4 |
| v1.13 | no such issue |

**Work around**:
 - if there is attach disk failure for long time, restart controller manager may work
 - if there is disk not detached for long time, detach that disk manually

