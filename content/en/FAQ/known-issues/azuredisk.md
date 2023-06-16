---
title: "AzureDisk CSI Driver Known Issues"
linkTitle: "AzureDisk"
type: docs
---

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
    - [15. azure disk could be not detached forever](#15-azure-disk-could-be-not-detached-forever)
    - [16. potential race condition issue due to detach disk failure retry](#16-potential-race-condition-issue-due-to-detach-disk-failure-retry)
    - [17. very slow disk attach/detach issue when disk num is large](#17-very-slow-disk-attachdetach-issue-when-disk-num-is-large)
    - [18. detach azure disk make VM run into a limbo state](#18-detach-azure-disk-make-vm-run-into-a-limbo-state)
    - [19. disk attach/detach self-healing on VMAS](#19-disk-attachdetach-self-healing-on-vmas)
    - [20. azure disk detach failure if node not exists](#20-azure-disk-detach-failure-if-node-not-exists)
    - [21. invalid disk URI error](#21-invalid-disk-URI-error)
    - [22. vmss dirty cache issue](#22-vmss-dirty-cache-issue)
    - [23. race condition when delete disk right after attach disk](#23-race-condition-when-delete-disk-right-after-attach-disk)
    - [24. attach disk costs 10min](#24-attach-disk-costs-10min)
    - [25. Multi-Attach error](#25-multi-attach-error)
    - [26. attached non-existing disk volume on agent node](#26-attached-non-existing-disk-volume-on-agent-node)
    - [27. failed to get azure instance id for node (not a vmss instance)](#27-failed-to-get-azure-instance-id-for-node-not-a-vmss-instance)

<!-- /TOC -->

## Recommended stable version for azure disk

| k8s version | stable version |
| ---- | ---- |
| v1.15 | 1.15.11+ |
| v1.16 | 1.16.10+ |
| v1.17 | 1.17.6+ |
| v1.18 | 1.18.3+ |
| v1.19 | 1.19.0+ |

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
| v1.6 | no fix |
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

Original time cost for Azure Disk PVC mount on a standard node size(e.g. Standard_D2_V2) is around 1 minute, `podAttachAndMountTimeout` is [2 minutes](https://github.com/kubernetes/kubernetes/blob/b812eaa172804739283e6e8723cbca3ed293e7ff/pkg/kubelet/volumemanager/volume_manager.go#L78), total `waitForAttachTimeout` is [10 minutes](https://github.com/kubernetes/kubernetes/blob/b812eaa172804739283e6e8723cbca3ed293e7ff/pkg/kubelet/volumemanager/volume_manager.go#L86), so a disk remount(detach and attach in sequential) would possibly cost more than 2min, thus may fail.

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
 > 💡 NOTE: AKS and current aks-engine won't have this issue since it's **not** using containerized kubelet

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
Warning  FailedMount             63s                  kubelet, aks-nodepool1-29460110-0  MountVolume.MountDevice failed for volume "pvc-d783d0e4-85a1-11e9-8a90-369885447933" : azureDisk - mountDevice:FormatAndMount failed with mount failed: exit status 32
Mounting command: systemd-run
Mounting arguments: --description=Kubernetes transient mount for /var/lib/kubelet/plugins/kubernetes.io/azure-disk/mounts/m436970985 --scope -- mount -t xfs -o dir_mode=0777,file_mode=0777,uid=1000,gid=1000,defaults /dev/disk/azure/scsi1/lun2 /var/lib/kubelet/plugins/kubernetes.io/azure-disk/mounts/m436970985
Output: Running scope as unit run-rb21966413ab449b3a242ae9b0fbc9398.scope.
mount: wrong fs type, bad option, bad superblock on /dev/sde,
       missing codepage or helper program, or other error
```

That's because azureDisk use ext4,xfs file system by default, mountOptions like [uid=x,gid=x] could not be set in mount time.

**Related issues**
- [Timeout expired waiting for volumes to attach](https://github.com/kubernetes/kubernetes/issues/67014#issuecomment-589915496)
- [Pod failed mounting xfs format volume with mountOptions](https://github.com/Azure/AKS/issues/1030)
- [Allow volume ownership to be only set after fs formatting](https://github.com/kubernetes/kubernetes/issues/69699#issuecomment-558861917)

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

 - new upstream feature to address this: [Allow volume ownership to be only set after fs formatting](https://github.com/kubernetes/kubernetes/issues/69699)

## 8. `Addition of a blob based disk to VM with managed disks is not supported`

**Issue details**:

Following error may occur if attach a blob based(unmanaged) disk to VM with managed disks:

```sh
  Warning  FailedMount            42s (x2 over 1m)  attachdetach                    AttachVolume.Attach failed for volume "pvc-f17e5e77-474e-11e8-a2ea-000d3a10df6d" : Attach volume "holo-k8s-dev-dynamic-pvc-f17e5e77-474e-11e8-a2ea-000d3a10df6d" to instance "k8s-master-92699158-0" failed with compute.VirtualMachinesClient#CreateOrUpdate: Failure responding to request: StatusCode=409 -- Original Error: autorest/azure: Service returned an error. Status=409 Code="OperationNotAllowed" Message="Addition of a blob based disk to VM with managed disks is not supported."
```

This issue is by design as in Azure, there are two kinds of disks, blob based(unmanaged) disk and managed disk, an Azure VM could not attach both of these two kinds of disks.

**Solution**:

Use `default` azure disk storage class in acs-engine, as `default` will always be identical to the agent pool, that is, if VM is managed, it will be managed azure disk class, if unmanaged, then it's unmanaged disk class.

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

this bug only exists in blob based VM in v1.8.x, v1.9.x, so if specify `ManagedDisks` when creating k8s cluster in acs-engine(AKS is using managed disk by default), it won't have this issue:

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

**Related issues**
 - [Multi Attach Error](https://github.com/Azure/AKS/issues/477)

## 15. azure disk could be not detached forever

**Issue details**:

In some condition when first detach azure disk operation failed, it won't retry and the azure disk would be still attached to the original VM node.

Following error may occur when move one disk from one node to another(keyword: `ConflictingUserInput`):
```
[Warning] AttachVolume.Attach failed for volume “pvc-7b7976d7-3a46-11e9-93d5-dee1946e6ce9” : Attach volume “kubernetes-dynamic-pvc-7b7976d7-3a46-11e9-93d5-dee1946e6ce9" to instance “/subscriptions/XXX/resourceGroups/XXX/providers/Microsoft.Compute/virtualMachines/aks-agentpool-57634498-0” failed with compute.VirtualMachinesClient#CreateOrUpdate: Failure sending request: StatusCode=0 -- Original Error: autorest/azure: Service returned an error. Status= Code=“ConflictingUserInput” Message=“Disk ‘/subscriptions/XXX/resourceGroups/XXX/providers/Microsoft.Compute/disks/kubernetes-dynamic-pvc-7b7976d7-3a46-11e9-93d5-dee1946e6ce9’ cannot be attached as the disk is already owned by VM ‘/subscriptions/XXX/resourceGroups/XXX/providers/Microsoft.Compute/virtualMachines/aks-agentpool-57634498-1’.”
```

**Fix**

We added retry logic for detach azure disk:
- PR [add retry for detach azure disk](https://github.com/kubernetes/kubernetes/pull/74398)

| k8s version | fixed version |
| ---- | ---- |
| v1.10 | N/A |
| v1.11 | 1.11.9 |
| v1.12 | 1.12.7 |
| v1.13 | 1.13.4 |
| v1.14 | 1.14.0 |
| v1.15 | 1.15.0 |

**Work around**:
 - if there is disk not detached for long time, detach that disk manually
 
## 16. potential race condition issue due to detach disk failure retry
 
**Issue details**:

In some error condition when detach azure disk failed, azure cloud provider will retry 6 times at most with exponential backoff, it will hold the data disk list for about 3 minutes with a node level lock, and in that time period, if customer update data disk list manually (e.g. need manual operationto attach/detach another disk since there is attach/detach error, ) , the data disk list will be obsolete(dirty data), then weird VM status happens, e.g. attach a non-existing disk, we should split those retry operations, every retry should get a fresh data disk list in the beginning.

**Fix**

Following PR refined detach azure disk retry operation, make every detach azure disk operation in a standalone function
- PR [fix detach azure disk back off issue which has too big lock in failure retry condition](https://github.com/kubernetes/kubernetes/pull/76573)
- PR [fix azure disk list corruption issue](https://github.com/kubernetes/kubernetes/pull/77187)

| k8s version | fixed version |
| ---- | ---- |
| v1.10 | N/A |
| v1.11 | no fix |
| v1.12 | 1.12.9 |
| v1.13 | 1.13.6 |
| v1.14 | 1.14.2 |
| v1.15 | 1.15.0 |

**Work around**:

Detach all the non-existing disks from VM (could do that in azure portal by bulk update)
 > Detaching disk one by one using cli may fail since they are already non-existing disks.

## 17. very slow disk attach/detach issue when disk num is large
 
**Issue details**:

We hit very slow disk attach/detach issue when disk num is large(> 10 disks on one VM)

**Fix**

Azure disk team are fixing this issue.

**Work around**:

No workaround.

## 18. detach azure disk make VM run into a limbo state
 
**Issue details**:

In some corner condition, detach azure disk would sometimes make VM run into a limbo state

**Fix**

Following two PRs would fix this issue by retry update VM if detach disk partially fail:
 - [fix azure retry issue when return 2XX with error](https://github.com/kubernetes/kubernetes/pull/78298)
 - [fix: retry detach azure disk issue](https://github.com/kubernetes/kubernetes/pull/78700)

| k8s version | fixed version |
| ---- | ---- |
| v1.11 | no fix |
| v1.12 | 1.12.10 |
| v1.13 | 1.13.8 |
| v1.14 | 1.14.4 |
| v1.15 | 1.15.0 |

**Work around**:

Update VM status manually would solve the problem:
 - Update Availability Set VM
 ```
 az vm update -n <VM_NAME> -g <RESOURCE_GROUP_NAME>
 ```
 - Update Scale Set VM
 ```
 az vmss update-instances -g <RESOURCE_GROUP_NAME> --name <VMSS_NAME> --instance-id <ID(number)>
 ```

## 19. disk attach/detach self-healing on VMAS

**Issue details**:
There could be disk detach failure due to many reasons(e.g. disk RP busy, controller manager crash, etc.), and it would fail when attach one disk to other node if that disk is still attached to the old node, user needs to manually detach disk in problem in the before, with this fix, azure cloud provider would check and detach this disk if it's already attached to the other node, that's like self-healing. This PR could fix lots of such disk attachment issue.

**Fix**

Following PR would first check whether current disk is already attached to other node, if so, it would trigger a dangling error and k8s controller would detach disk first, and then do the attach volume operation.

This PR would also fix a "disk not found" issue when detach azure disk due to disk URI case sensitive case, error logs are like following(without this PR):
```
azure_controller_standard.go:134] detach azure disk: disk  not found, diskURI: /subscriptions/xxx/resourceGroups/andy-mg1160alpha3/providers/Microsoft.Compute/disks/xxx-dynamic-pvc-41a31580-f5b9-4f08-b0ea-0adcba15b6db
```
**Fix**
 - Fix on VMAS
   - [fix: detach azure disk issue using dangling error](https://github.com/kubernetes/kubernetes/pull/81266)
   - [fix: azure disk name matching issue](https://github.com/kubernetes/kubernetes/pull/81720)

| k8s version | fixed version |
| ---- | ---- |
| v1.12 | no fix |
| v1.13 | 1.13.11 |
| v1.14 | 1.14.7 |
| v1.15 | 1.15.4 |
| v1.15 | 1.16.0 |

 - Fix on VMSS
   - [fix: azure disk dangling attach issue on VMSS which would cause API throttling](https://github.com/kubernetes/kubernetes/pull/90749)

| k8s version | fixed version |
| ---- | ---- |
| v1.15 | no fix |
| v1.16 | 1.16.9 |
| v1.17 | 1.17.6 |
| v1.18 | 1.18.3 |
| v1.19 | 1.19.0 |

**Work around**:

manually detach disk in problem and wait for disk attachment happen automatically

## 20. azure disk detach failure if node not exists

**Issue details**:
If a node with a Azure Disk attached is deleted (before the volume is detached), subsequent attempts by the attach/detach controller to detach it continuously fail, and prevent the controller from attaching the volume to another node.

**Fix**

 - [fix: azure disk detach failure if node not exists](https://github.com/kubernetes/kubernetes/pull/82640)

| k8s version | fixed version |
| ---- | ---- |
| v1.12 | no fix |
| v1.13 | 1.13.9 |
| v1.14 | 1.14.8 |
| v1.15 | 1.15.5 |
| v1.16 | 1.16.1 |
| v1.16 | 1.17.0 |

**Work around**:

Restart kube-controller-manager on master node.

## 21. invalid disk URI error

**Issue details**:

When user use an existing disk in static provisioning, may hit following error:
```
AttachVolume.Attach failed for volume "azure" : invalid disk URI: /subscriptions/xxx/resourcegroups/xxx/providers/Microsoft.Compute/disks/Test_Resize_1/”
```


**Fix**

 - [fix: make azure disk URI as case insensitive](https://github.com/kubernetes/kubernetes/pull/79020)

| k8s version | fixed version |
| ---- | ---- |
| v1.13 | no fix |
| v1.14 | 1.14.9 |
| v1.15 | 1.15.6 |
| v1.16 | 1.16.0 |
| v1.17 | 1.17.0 |

**Work around**:

Use `resourceGroups` instead of `resourcegroups` in disk PV configuration

## 22. vmss dirty cache issue

**Issue details**:

clean vmss cache should happen after disk attach/detach operation, now it's before those operations, which would lead to dirty cache.
since update operation may cost 30s or more, and at that time period, if there is another get vmss operation, it would get the old data disk list

 - [VMSS disk attach/detach issues w/ v1.13.12, v1.14.8, v1.15.5, v1.16.2](https://github.com/Azure/aks-engine/issues/2312)
 - [Disk attachment/mounting problems, all pods with PVCs stuck in ContainerCreating](https://github.com/Azure/AKS/issues/1278) 

**Fix**

 - [fix vmss dirty cache issue](https://github.com/kubernetes/kubernetes/pull/85158)

| k8s version | fixed version | notes |
| ---- | ---- | ---- |
| v1.13 | no fix | regression since 1.13.12 (hotfixed in AKS release) |
| v1.14 | 1.14.10 | regression only in 1.14.8, 1.14.9 (hotfixed in AKS release) |
| v1.15 | 1.15.7 | regression only in 1.15.5, 1.15.6 (hotfixed in AKS release)  |
| v1.16 | 1.16.4 | regression only in 1.16.2, 1.16.3 (hotfixed in AKS release)  |
| v1.17 | 1.17.0 | |

**Work around**:

Detach disk in problem manually

## 23. race condition when delete disk right after attach disk

**Issue details**:

There is condition that attach and delete disk happens in same time, azure CRP don't check such race condition

 - [should not delete an azure disk when that disk is being attached](https://github.com/kubernetes/kubernetes/issues/82714)

**Fix**

 - [fix race condition when delete azure disk right after attach azure disk](https://github.com/kubernetes/kubernetes/pull/84917)

| k8s version | fixed version | notes |
| ---- | ---- | ---- |
| v1.13 | no fix | hotfixed in AKS release since 1.13.12 |
| v1.14 | 1.14.10 | hotfixed in AKS release in 1.14.8, 1.14.9 |
| v1.15 | 1.15.7 | hotfixed in AKS release in 1.15.5, 1.15.6  |
| v1.16 | 1.16.4 | hotfixed in AKS release in 1.16.2, 1.16.3  |
| v1.17 | 1.17.0 | |

**Work around**:

Detach disk in problem manually

## 24. attach disk costs 10min

**Issue details**:

PR [Fix aggressive VM calls for Azure VMSS](https://github.com/kubernetes/kubernetes/pull/83102) change getVMSS cache TTL from 1min to 10min, getVMAS cache TTL from 5min to 10min, that will cause error `WaitForAttach ... Cannot find Lun for disk`, and it would make attach disk operation costs 10min on VMSS and 15min on VMAS, detailed error would be like following:
```
Events:
  Type     Reason                  Age                 From                                        Message
  ----     ------                  ----                ----                                        -------
  Normal   Scheduled               29m                 default-scheduler                           Successfully assigned authentication/authentication-mssql-statefulset-0 to aks-nodepool1-29122124-vmss000004
  Normal   SuccessfulAttachVolume  28m                 attachdetach-controller                     AttachVolume.Attach succeeded for volume "pvc-8d9f0ade-1825-11ea-83a0-22ced17d4a3d"
  Warning  FailedMount             23m (x10 over 27m)  kubelet, aks-nodepool1-29122124-vmss000004  MountVolume.WaitForAttach failed for volume "pvc-8d9f0ade-1825-11ea-83a0-22ced17d4a3d" : Cannot find Lun for disk kubernetes-dynamic-pvc-8d9f0ade-1825-11ea-83a0-22ced17d4a3d
  Warning  FailedMount             23m (x3 over 27m)   kubelet, aks-nodepool1-29122124-vmss000004  Unable to mount volumes for pod "authentication-mssql-statefulset-0_authentication(8df467e7-1825-11ea-83a0-22ced17d4a3d)": timeout expired waiting for volumes to attach or mount for pod "authentication"/"authentication-mssql-statefulset-0". list of unmounted volumes=[authentication-mssql-persistent-data-storage]. list of unattached volumes=[authentication-mssql-persistent-data-storage default-token-b7spv]
  Normal   Pulled                  21m                 kubelet, aks-nodepool1-29122124-vmss000004  Container image "mcr.microsoft.com/mssql/server:2019-CTP3.2-ubuntu" already present on machine
  Normal   Created                 21m                 kubelet, aks-nodepool1-29122124-vmss000004  Created container authentication-mssql
  Normal   Started                 21m                 kubelet, aks-nodepool1-29122124-vmss000004  Started container authentication-mssql
```

This slow disk attachment issue only exists on `1.13.12+`, `1.14.8+`, fortunately, from k8s 1.15.0, this issue won't happen, since getDiskLUN logic has already been refactored (already has PR:[fix azure disk lun error](https://github.com/kubernetes/kubernetes/pull/77912), won't depend on getVMSS operation to get disk LUN.

**Relate issues**:
 - [GetAzureDiskLun sometimes costs 10min which is too long time](https://github.com/kubernetes/kubernetes/issues/69262#issuecomment-562567413)

**Fix**

 - [fix azure disk lun error](https://github.com/kubernetes/kubernetes/pull/77912)

| k8s version | fixed version | notes |
| ---- | ---- | ---- |
| v1.13 | no fix | need to hotfix in AKS release since 1.13.12 (slow disk attachment exists on `1.13.12+`) |
| v1.14 | in cherry-pick | need to hotfix in AKS release in 1.14.8, 1.14.9 (slow disk attachment exists on `1.14.8+`) |
| v1.15 | 1.15.0 | |
| v1.16 | 1.16.0 | |

**Work around**:

Wait for about 10min or 15min, `MountVolume.WaitForAttach` operation would retry and would finally succeed

## 25. Multi-Attach error

**Issue details**:

If two pods on different nodes are using same disk PVC(this issue may also happen when doing rollingUpdate in Deployment using one replica), would probably hit following error:
```
Events:
Warning  FailedAttachVolume  9m                attachdetach-controller                     Multi-Attach error for volume "pvc-fc0bed38-48bf-43f1-a7e4-255eef48ffb9" Volume is already used by pod(s) sqlserver3-5b8449449-5chzx
Warning  FailedMount         42s (x4 over 7m)  kubelet, aks-nodepool1-15915763-vmss000001  Unable to mount volumes for pod "sqlserver3-55754785bb-jjr6d_default(55381f38-9640-43a9-888d-096387cbb780)": timeout expired waiting for volumes to attach or mount for pod "default"/"sqlserver3-55754785bb-jjr6d". list of unmounted volumes=[mssqldb]. list of unattached volumes=[mssqldb default-token-q7cw9]
```

The above issue is upstream issue([detailed error code](https://github.com/kubernetes/kubernetes/blob/20c265fef0741dd71a66480e35bd69f18351daea/pkg/controller/volume/attachdetach/reconciler/reconciler.go#L351)), it could be due to following reasons:
 - two pods are using same disk PVC, this issue could happen even using `Deployment` with one replica(see below workaround)
 - one node is in Shutdown(deallocated) state, this is by design now and there is on-going upstream work to fix this issue
   - [Propose to taint node "shutdown" condition](https://github.com/kubernetes/kubernetes/issues/58635)
   - [add node shutdown KEP](https://github.com/kubernetes/enhancements/pull/1116)   
 >  - workaround: user could use set `terminationGracePeriodSeconds: 0` in deployment or `kubectl delete pod PODNAME --grace-period=0 --force` to delete pod on the deallocated node
 >  - Azure cloud provider solution: delete shutdown node(in `InstanceExistsByProviderID`) like what [other cloud provider does today](https://github.com/kubernetes/kubernetes/blob/d8febccacfc9d51a017be9531247689e0e36df04/staging/src/k8s.io/legacy-cloud-providers/aws/aws.go#L1623-L1627), while it may lead to other problem(e.g. node label loss), see details: [Common handling of stopped instances across cloud providers.
](https://github.com/kubernetes/kubernetes/issues/46442) 

since azure disk PVC could not be attached to one node.

**Relate issues**:
 - [Trouble attaching volume](https://github.com/Azure/AKS/issues/884#issuecomment-571165826)
 
**Work around**:

When using disk PVC config in deployment, `maxSurge: 0` could make sure there would not be no more than two pods in `Running/ContainerCreating` state when doing rollingUpdate:
```
template:
...
  strategy:
    rollingUpdate:
      maxSurge: 0
      maxUnavailable: 1
    type: RollingUpdate
```

Refer to [Rolling Updates with Kubernetes Deployments](https://tachingchen.com/blog/kubernetes-rolling-update-with-deployment/) for more detailed rollingUpdate config, and you could find `maxSurge: 0` setting example [here](https://github.com/andyzhangx/demo/blob/c3199932c4c00ca1095481e845642a0ec4bda598/linux/azuredisk/attach-stress-test/deployment/deployment-azuredisk1.yaml#L45-L49)

**Note**

 - error messages:
   - `Multi-Attach error for volume "pvc-e9b72e86-129a-11ea-9a02-9abdbf393c78" Volume is already used by pod(s)`

two pods are using same disk PVC, this issue could happen even using `Deployment` with one replica, check detailed explanation and workaround here with above explanation

   - `Multi-Attach error for volume "pvc-0d7740b9-3a43-11e9-93d5-dee1946e6ce9" Volume is already exclusively attached to one node and can't be attached to another`

This could be a transient error when move volume from one node to another, use following command to find attached node:
```console  
kubectl get no -o yaml | grep volumesAttached -A 15 | grep pvc-0d7740b9-3a43-11e9-93d5-dee1946e6ce9 -B 10 -A 15
```

related code: [reportMultiAttachError](https://github.com/kubernetes/kubernetes/blob/36e40fb850293076b415ae3d376f5f81dc897105/pkg/controller/volume/attachdetach/reconciler/reconciler.go#L300)

## 26. attached non-existing disk volume on agent node

**Issue details**:

There is little possibility that attach/detach disk and disk deletion happened in same time, that would cause race condition. This PR add remediation when attach/detach disk, if returned 404 error, it will filter out all non-existing disks and try attach/detach operation again.

**Fix**

 - [fix: add remediation in azure disk attach/detach](https://github.com/kubernetes/kubernetes/pull/88444)
 - [fix: azure disk remediation issue](https://github.com/kubernetes/kubernetes/pull/88620)

| k8s version | fixed version |
| ---- | ---- |
| v1.14 | no fix |
| v1.15 | 1.15.11 |
| v1.16 | 1.16.8 |
| v1.17 | 1.17.4 |
| v1.18 | 1.18.0 |

**Work around**:

Detach disk in problem manually

## 27. failed to get azure instance id for node (not a vmss instance)

**Issue details**:

[PR#81266](https://github.com/kubernetes/kubernetes/pull/81266) does not convert the VMSS node name which causes error like this:
```
failed to get azure instance id for node \"k8s-agentpool1-32474172-vmss_1216\" (not a vmss instance)
```
That will make dangling attach return error, and k8s volume attach/detach controller will getVmssInstance, and since the nodeName is in an incorrect format, it will always clean vmss cache if node not found, thus incur a get vmss API call storm.

**Fix**

 - [fix: azure disk dangling attach issue on VMSS which would cause API throttling](https://github.com/kubernetes/kubernetes/pull/90749)

| k8s version | fixed version |
| ---- | ---- |
| v1.14 | only hotfixed with image `mcr.microsoft.com/oss/kubernetes/hyperkube:v1.14.8-hotfix.20200529.1` |
| v1.15 | only hotfixed with image `mcr.microsoft.com/oss/kubernetes/hyperkube:v1.15.11-hotfix.20200529.1`, `mcr.microsoft.com/oss/kubernetes/hyperkube:v1.15.12-hotfix.20200603` |
| v1.16 | 1.16.10 (also hotfixed with image `mcr.microsoft.com/oss/kubernetes/hyperkube:v1.16.9-hotfix.20200529.1`) |
| v1.17 | 1.17.6 |
| v1.18 | 1.18.3 |
| v1.19 | 1.19.0 |

**Work around**:

1.	Stop kube-controller-manager
2.	detach disk in problem from that vmss node manually
```console
az vmss disk detach -g <RESOURCE_GROUP_NAME> --name <VMSS_NAME> --instance-id <ID(number)> --lun number
```

e.g. per below logs, 
```
E0501 11:15:40.981758       1 attacher.go:277] failed to detach azure disk "/subscriptions/xxx/resourceGroups/rg/providers/Microsoft.Compute/disks/rg-dynamic-pvc-dc282131-b669-47db-8d57-cb3b9789ac3e", err failed to get azure instance id for node "k8s-agentpool1-32474172-vmss_1216" (not a vmss instance)
```
 - find lun number of disk `rg-dynamic-pvc-dc282131-b669-47db-8d57-cb3b9789ac3e`:
```console
az vmss show -g rg --name k8s-agentpool1-32474172-vmss --instance-id 1216
```
 - detach vmss disk manually:
```console
az vmss disk detach -g rg --name k8s-agentpool1-32474172-vmss --instance-id 1216 --lun number
```
3.	Start kube-controller-manager
