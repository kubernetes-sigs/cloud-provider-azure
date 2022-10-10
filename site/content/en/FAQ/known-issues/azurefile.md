---
title: "AzureFile CSI Driver Known Issues"
linkTitle: "AzureFile"
type: docs
---

<!-- TOC -->

- [azure file plugin known issues](#azure-file-plugin-known-issues)
    - [Recommended stable version for azure file](#recommended-stable-version-for-azure-file)
    - [1. azure file mountOptions setting](#1-azure-file-mountoptions-setting)
        - [file/dir mode setting:](#filedir-mode-setting)
        - [other useful `mountOptions` setting:](#other-useful-mountoptions-setting)
    - [2. permission issue of azure file dynamic provision in acs-engine](#2-permission-issue-of-azure-file-dynamic-provision-in-acs-engine)
    - [3. Azure file support on Sovereign Cloud](#3-azure-file-support-on-sovereign-cloud)
    - [4. azure file dynamic provision failed due to cluster name length issue](#4-azure-file-dynamic-provision-failed-due-to-cluster-name-length-issue)
    - [5. azure file dynamic provision failed due to no storage account in current resource group](#5-azure-file-dynamic-provision-failed-due-to-no-storage-account-in-current-resource-group)
    - [6. azure file plugin on Windows does not work after node restart](#6-azure-file-plugin-on-windows-does-not-work-after-node-restart)
    - [7. file permission could not be changed using azure file, e.g. postgresql](#7-file-permission-could-not-be-changed-using-azure-file-eg-postgresql)
    - [8. Could not delete pod with AzureFile volume if storage account key changed](#8-could-not-delete-pod-with-azurefile-volume-if-storage-account-key-changed)
    - [9. Long latency when handling lots of small files](#9-long-latency-compared-to-disk-when-handling-lots-of-small-files)
    - [10. `allow access from selected network` setting on storage account will break azure file dynamic provisioning](#10-allow-access-from-selected-network-setting-on-storage-account-will-break-azure-file-dynamic-provisioning)
    - [11. azure file remount on Windows in same node would fail](#11-azure-file-remount-on-windows-in-same-node-would-fail)
    - [12. update azure file secret if azure storage account key changed](#12-update-azure-file-secret-if-azure-storage-account-key-changed)
    - [13. Create Azure Files PV AuthorizationFailure when using advanced networking](#13-create-azure-files-pv-authorizationfailure-when-using-advanced-networking)
    - [14. initial delay(5s) in mounting azure file](#14-initial-delay5s-in-mounting-azure-file)
<!-- /TOC -->

## Recommended stable version for azure file

| k8s version | stable version |
| ---- | ---- |
| v1.7 | 1.7.14+ |
| v1.8 | 1.8.11+ |
| v1.9 | 1.9.7+ |
| v1.10 | 1.10.2+ |
| v1.11 | 1.11.8+ |
| v1.12 | 1.12.6+ |
| v1.13 | 1.13.4+ |
| v1.14 | 1.14.0+ |

## 1. azure file mountOptions setting

### file/dir mode setting:

**Issue details**:

- `fileMode`, `dirMode` value would be different in different versions, in latest master branch, it's `0755` by default, to set a different value, follow this [mount options support of azure file](https://github.com/andyzhangx/Demo/blob/master/linux/azurefile/azurefile-mountoptions.md) (available from v1.8.5).
- For version v1.8.0-v1.8.4, since [mount options support of azure file](https://github.com/andyzhangx/Demo/blob/master/linux/azurefile/azurefile-mountoptions.md) is not available, as a workaround, [securityContext](https://kubernetes.io/docs/tasks/configure-pod-container/security-context/) could be specified for the pod, [detailed pod example](https://github.com/andyzhangx/Demo/blob/master/linux/azurefile/demo-azurefile-securitycontext.yaml)

```yaml
  securityContext:
    runAsUser: XXX
    fsGroup: XXX
```

| version | `fileMode`, `dirMode` value |
| ---- | ---- |
| v1.6.x, v1.7.x | 0777 |
| v1.8.0 ~ v1.8.5, v1.9.0 | 0700 |
| v1.8.6 or later, v1.9.1 ~ v1.10.9, v1.11.0 ~ v1.11.3, v1.12.0 ~ v.12.1 | 0755 |
| v1.10.10 or later | 0777 |
| v1.11.4 or later | 0777 |
| v1.12.2 or later | 0777 |
| v1.13.x | 0777 |

### other useful `mountOptions` setting:

- `mfsymlinks`: make azure file(cifs) mount supports symbolic link
- `nobrl`: Do not send byte range lock requests to the server. This is necessary for certain applications that break with cifs style mandatory byte range locks (and most cifs servers do not yet support requesting advisory byte range locks). Error message could be like following:

```sh
Error: SQLITE_BUSY: database is locked
```

**Related issues**

- [azureFile volume mode too strict for container with non root user](https://github.com/kubernetes/kubernetes/issues/54610)
- [Unable to connect to SQL-lite db mounted on AzureFile/AzureDisks [SQLITE_BUSY: database is locked]](https://github.com/kubernetes/kubernetes/issues/59755)
- [Allow nobrl parameter like docker to use sqlite over network drive](https://github.com/kubernetes/kubernetes/issues/61767)
- [Error to deploy mongo with azure file storage](https://github.com/kubernetes/kubernetes/issues/58308)

## 2. permission issue of azure file dynamic provision in acs-engine

**Issue details**:

From acs-engine v0.12.0, RBAC is enabled, azure file dynamic provision does not work from this version

**error logs**:

```sh
Events:
  Type     Reason              Age   From                         Message
  ----     ------              ----  ----                         -------
  Warning  ProvisioningFailed  8s    persistentvolume-controller  Failed to provision volume with StorageClass "azurefile": Couldn't create secret secrets is forbidden: User "system:serviceaccount:kube-syste
m:persistent-volume-binder" cannot create secrets in the namespace "default"
  Warning  ProvisioningFailed  8s    persistentvolume-controller  Failed to provision volume with StorageClass "azurefile": failed to find a matching storage account
```

**Related issues**

- [azure file PVC need secrets create permission for persistent-volume-binder](https://github.com/kubernetes/kubernetes/issues/59543)

**Workaround**:

- Add a ClusterRole and ClusterRoleBinding for [azure file dynamic provision](https://github.com/andyzhangx/Demo/tree/master/linux/azurefile#dynamic-provisioning-for-azure-file-in-linux-support-from-v170)

```sh
kubectl create -f https://raw.githubusercontent.com/andyzhangx/Demo/master/aks-engine/rbac/azure-cloud-provider-deployment.yaml
```
- delete the original PVC and recreate PVC

**Fix**

- PR in acs-engine: [fix azure file dynamic provision permission issue](https://github.com/Azure/acs-engine/pull/2238)

## 3. Azure file support on Sovereign Cloud

[Azure file on Sovereign Cloud](https://github.com/kubernetes/kubernetes/pull/48460) is supported from v1.7.11, v1.8.0

## 4. azure file dynamic provision failed due to cluster name length issue

**Issue details**:
k8s cluster name length must be less than 16 characters, otherwise following error will be received when creating dynamic privisioning azure file pvc, this bug exists in [v1.7.0, v1.7.10]:
 > Note: check `cluster-name` by running `grep cluster-name /etc/kubernetes/manifests/kube-controller-manager.yaml` on master node

```sh
persistentvolume-controller    Warning    ProvisioningFailed Failed to provision volume with StorageClass "azurefile": failed to find a matching storage account
```

**Fix**

- PR [Fix share name generation in azure file provisioner](https://github.com/kubernetes/kubernetes/pull/48326)

| k8s version | fixed version |
| ---- | ---- |
| v1.7 | 1.7.11 |
| v1.8 | 1.8.0 |
| v1.9 | 1.9.0 |

## 5. azure file dynamic provision failed due to no storage account in current resource group

**Issue details**:

When create an azure file PVC, there will be error if there is no storage account in current resource group, error info would be like following:

```sh
Events:
  Type     Reason              Age               From                         Message
  ----     ------              ----              ----                         -------
  Warning  ProvisioningFailed  10s (x5 over 1m)  persistentvolume-controller  Failed to provision volume with StorageClass "azurefile-premium": failed to find a matching storage account
```

**Related issues**

- [failed to create azure file pvc if there is no storage account in current resource group](https://github.com/kubernetes/kubernetes/issues/56556)

**Workaround**:
specify a storage account in azure file dynamic provision, you should make sure the specified storage account is in the same resource group as your k8s cluster. In AKS, the specified storage account should be in `shadow resource group`(naming as `MC_+{RESOUCE-GROUP-NAME}+{CLUSTER-NAME}+{REGION}`) which contains all resources of your aks cluster.

**Fix**

- PR [fix the create azure file pvc failure if there is no storage account in current resource group](https://github.com/kubernetes/kubernetes/pull/56557)

| k8s version | fixed version |
| ---- | ---- |
| v1.7 | 1.7.14 |
| v1.8 | 1.8.9 |
| v1.9 | 1.9.4 |
| v1.10 | 1.10.0 |

## 6. azure file plugin on Windows does not work after node restart

**Issue details**:
azure file plugin on Windows does not work after node restart, this is due to `New-SmbGlobalMapping` cmdlet has lost account name/key after reboot

**Related issues**

- [azure file plugin on Windows does not work after node restart](https://github.com/kubernetes/kubernetes/issues/60624)

**Workaround**:

- delete the original pod with azure file mount
- create the pod again

**Fix**

- PR [fix azure file plugin failure issue on Windows after node restart](https://github.com/kubernetes/kubernetes/pull/60625)

| k8s version | fixed version |
| ---- | ---- |
| v1.7 | not support in upstream |
| v1.8 | 1.8.10 |
| v1.9 | 1.9.7 |
| v1.10 | 1.10.0 |

## 7. file permission could not be changed using azure file, e.g. postgresql

**error logs** when running postgresql on azure file plugin:
```
initdb: could not change permissions of directory "/var/lib/postgresql/data": Operation not permitted
fixing permissions on existing directory /var/lib/postgresql/data
```

**Issue details**:
azure file plugin is using cifs/SMB protocol, file/dir permission could not be changed after mounting

**Workaround**:

Use `mountOptions` with `dir_mode`, `file_mode` set as `0777`:

```yaml
kind: StorageClass
apiVersion: storage.k8s.io/v1
metadata:
  name: azurefile
provisioner: kubernetes.io/azure-file
mountOptions:
  - dir_mode=0777
  - file_mode=0777
```
> follow detailed config [here](../linux/azurefile/postgresql)

**Related issues**
[Persistent Volume Claim permissions](https://github.com/Azure/AKS/issues/225)

## 8. Could not delete pod with AzureFile volume if storage account key changed

**Issue details**:

- kubelet fails to umount azurefile volume when there is azure file connection, below is an easy repro:
   - create a pod with azure file mount
   - regenerate the account key of the storage account
   - delete the pod, and the pod will never be deleted due to `UnmountVolume.TearDown` error

**error logs**

```sh
nestedpendingoperations.go:263] Operation for "\"kubernetes.io/azure-file/cc5c86cd-422a-11e8-91d7-000d3a03ee84-myvolume\" (\"cc5c86cd-422a-11e8-91d7-000d3a03ee84\")" failed. No retries permitted until 2018-04-17 10:35:40.240272223 +0000 UTC m=+1185722.391925424 (durationBeforeRetry 500ms). Error: "UnmountVolume.TearDown failed for volume \"myvolume\" (UniqueName: \"kubernetes.io/azure-file/cc5c86cd-422a-11e8-91d7-000d3a03ee84-myvolume\") pod \"cc5c86cd-422a-11e8-91d7-000d3a03ee84\" (UID: \"cc5c86cd-422a-11e8-91d7-000d3a03ee84\") : Error checking if path exists: stat /var/lib/kubelet/pods/cc5c86cd-422a-11e8-91d7-000d3a03ee84/volumes/kubernetes.io~azure-file/myvolume: resource temporarily unavailable
...
kubelet_volumes.go:128] Orphaned pod "380b02f3-422b-11e8-91d7-000d3a03ee84" found, but volume paths are still present on disk
```

**Workaround**:

manually umount the azure file mount path on the agent node and then the pod will be deleted right after that

```sh
sudo umount /var/lib/kubelet/pods/cc5c86cd-422a-11e8-91d7-000d3a03ee84/volumes/kubernetes.io~azure-file/myvolume
```

**Fix**

- PR [Fix bug:Kubelet failure to umount mount points](https://github.com/kubernetes/kubernetes/pull/52324)

| k8s version | fixed version |
| ---- | ---- |
| v1.7 | no fix(no cherry-pick fix is allowed) |
| v1.8 | 1.8.8 |
| v1.9 | 1.9.7 |
| v1.10 | 1.10.0 |

**Related issues**

- [UnmountVolume.TearDown fails for AzureFile volume, locks up node](https://github.com/kubernetes/kubernetes/issues/62824)
- [Kubelet failure to umount glusterfs mount points](https://github.com/kubernetes/kubernetes/issues/41141)

## 9. Long latency compared to disk when handling lots of small files

**Related issues**
 - [`azurefile` is very slow](https://github.com/Azure/AKS/issues/223)
 - [Can't roll out Wordpress chart with PV on AzureFile](https://github.com/helm/charts/issues/5751)
 
 
 ## 10. `allow access from selected network` setting on storage account will break azure file dynamic provisioning
 When set `allow access from selected network` on storage account and will get following error when creating a file share by k8s:
 ```
 persistentvolume-controller (combined from similar events): Failed to provision volume with StorageClass "azurefile": failed to create share kubernetes-dynamic-pvc-xxx in account xxx: failed to create file share, err: storage: service returned error: StatusCode=403, ErrorCode=AuthorizationFailure, ErrorMessage=This request is not authorized to perform this operation.
 ```

That's because k8s `persistentvolume-controller` is on master node which is not in the selected network, and that's why it could not create file share on that storage account.

**Workaround**:

use azure file static provisioning instead
 - create azure file share in advance, and then provide storage account and file share name in k8s, here is an [example](https://docs.microsoft.com/en-us/azure/aks/azure-files-volume)
 
 **Related issues**
  - [Azure Files PV AuthorizationFailure when using advanced networking ](https://github.com/Azure/AKS/issues/804)

## 11. azure file remount on Windows in same node would fail

**Issue details**:

If user delete a pod with azure file mount in deployment and it would probably schedule a pod on same node, azure file mount will fail since `New-SmbGlobalMapping` command would fail if file share is already mounted on the node.

**error logs**

Error logs would be like following:
```
E0118 08:15:52.041014    2112 nestedpendingoperations.go:267] Operation for "\"kubernetes.io/azure-file/42c0ea39-1af9-11e9-8941-000d3af95268-pvc-d7e1b5f9-1af3-11e9-8941-000d3af95268\" (\"42c0ea39-1af9-11e9-8941-000d3af95268\")" failed. No retries permitted until 2019-01-18 08:15:53.0410149 +0000 GMT m=+732.446642701 (durationBeforeRetry 1s). Error: "MountVolume.SetUp failed for volume \"pvc-d7e1b5f9-1af3-11e9-8941-000d3af95268\" (UniqueName: \"kubernetes.io/azure-file/42c0ea39-1af9-11e9-8941-000d3af95268-pvc-d7e1b5f9-1af3-11e9-8941-000d3af95268\") pod \"deployment-azurefile-697f98d559-6zrlf\" (UID: \"42c0ea39-1af9-11e9-8941-000d3af95268\") : azureMount: SmbGlobalMapping failed: exit status 1, only SMB mount is supported now, output: \"New-SmbGlobalMapping : Generic failure \\r\\nAt line:1 char:190\\r\\n+ ... , $PWord;New-SmbGlobalMapping -RemotePath $Env:smbremotepath -Cred ...\\r\\n+                 ~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~\\r\\n    + CategoryInfo          : NotSpecified: (MSFT_SmbGlobalMapping:ROOT/Microsoft/...mbGlobalMapping) [New-SmbGlobalMa \\r\\n   pping], CimException\\r\\n    + FullyQualifiedErrorId : HRESULT 0x80041001,New-SmbGlobalMapping\\r\\n \\r\\n\""
```

**Fix**

- PR [fix smb remount issue on Windows](https://github.com/kubernetes/kubernetes/pull/73661)

| k8s version | fixed version |
| ---- | ---- |
| v1.10 | no fix |
| v1.11 | 1.11.8 |
| v1.12 | 1.12.6 |
| v1.13 | 1.13.4 |
| v1.14 | 1.14.0 |

**Related issues**

- [azure file remount on Windows in same node would fail](https://github.com/kubernetes/kubernetes/issues/73087)
- [Mounting volume to pods fails randomly](https://github.com/Azure/aks-engine/issues/327)

## 12. update azure file secret if azure storage account key changed

**Issue details**: 
There would be azure file mount failure if azure storage account key changed

**Workaround**:
User needs to update `azurestorageaccountkey` field manually in azure file secret(secret name format: `azure-storage-account-{storage-account-name}-secret` in `default` namespace):
```
kubectl delete secret azure-storage-account-{storage-account-name}-secret
kubectl create secret generic azure-storage-account-{storage-account-name}-secret --from-literal azurestorageaccountname=... --from-literal azurestorageaccountkey="..." --type=Opaque
```
 > make sure there is no `\r` in the account name and key, here is a [failed case](https://github.com/MicrosoftDocs/azure-docs/issues/61650#issuecomment-683274588)
 - delete original pod(may use `--force --grace-period=0`) and wait a few minutes for new pod retry azure file mount
 
## 13. Create Azure Files PV AuthorizationFailure when using advanced networking

**Issue details**: 

When create an azure file PV using advanced networking, user may hit following error:
```
err: storage: service returned error: StatusCode=403, ErrorCode=AuthorizationFailure, ErrorMessage=This request is not authorized to perform this operation
```

Before api-version `2019-06-01`, create file share action is considered as data-path operation, since `2019-06-01`, it would be considered as control-path operation, not blocked by advanced networking any more.

**Related issues**
 - [Azure Files PV AuthorizationFailure when using advanced networking](https://github.com/Azure/AKS/issues/804)
 - [Azure Files PV AuthorizationFailure when using advanced networking](https://github.com/kubernetes/kubernetes/issues/85354)

 **Fix**

- PR [Switch to use AzureFile management SDK](https://github.com/kubernetes/kubernetes/pull/90350)

| k8s version | fixed version |
| ---- | ---- |
| v1.18 | no fix |
| v1.19 | 1.19.0 |

**Workaround**:

Shut down the advanced networking when create azure file PV.
 
## 14. initial delay(5s) in mounting azure file

**Issue details**: 

When starting pods with AFS volumes, there is an initial delay of five seconds until the pod is transitioning from the "Scheduled" state. The reason for this is that currently the volume mounting happens inside a wait.Poll which will initially wait a specified interval(currently 5 seconds) before execution. This issue is introduced by PR [fix: azure file mount timeout issue](https://github.com/kubernetes/kubernetes/pull/88610) with v1.15.11+, v1.16.8+, v1.17.4+, v1.18.0+

 **Fix**
 - [initial delay(5s) when starting Pods with Azure File volumes](https://github.com/kubernetes/kubernetes/issues/93025)

 **Fix**

- PR [fix: initial delay in mounting azure disk & file](https://github.com/kubernetes/kubernetes/pull/93052)

| k8s version | fixed version |
| ---- | ---- |
| v1.15 | no fix |
| v1.16 | 1.16.14 |
| v1.17 | 1.17.10 |
| v1.18 | 1.18.7 |
| v1.19 | 1.19.0 |
