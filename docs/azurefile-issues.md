# azure file plugin known issues
### Recommended stable version for azure file
| k8s version | stable version |
| ---- | ---- |
| v1.7 | 1.7.14 or later |
| v1.8 | 1.8.11 or later |
| v1.9 | 1.9.7 or later |
| v1.10 | 1.10.2 or later|

### 1. azure file mountOptions setting
#### file/dir mode setting:
**Issue details**:

 - `fileMode`, `dirMode` value would be different in different versions, in latest master branch, it's `0755` by default, to set a different value, follow this [mount options support of azure file](https://github.com/andyzhangx/Demo/blob/master/linux/azurefile/azurefile-mountoptions.md) (available from v1.8.5). 
   - For version v1.8.0-v1.8.4, since [mount options support of azure file](https://github.com/andyzhangx/Demo/blob/master/linux/azurefile/azurefile-mountoptions.md) is not available, as a workaround, [securityContext](https://kubernetes.io/docs/tasks/configure-pod-container/security-context/) could be specified for the pod, [detailed pod example](https://github.com/andyzhangx/Demo/blob/master/linux/azurefile/demo-azurefile-securitycontext.yaml)
```
  securityContext:
    runAsUser: XXX
    fsGroup: XXX
```

| version | `fileMode`, `dirMode` value |
| ---- | ---- |
| v1.6.x, v1.7.x | 0777 |
| v1.8.0-v1.8.5 | 0700 |
| v1.8.6 or above | 0755 |
| v1.9.0 | 0700 |
| v1.9.1 or above | 0755 |
| v1.10.0| 0755 |

#### other useful `mountOptions` setting:
 - `mfsymlinks`:
make azure file(cifs) mount supports symbolic link
 - `nobrl`:
Do not send byte range lock requests to the server. This is necessary for certain applications that break with cifs style mandatory byte range locks (and most cifs servers do not yet support requesting advisory byte range locks). Error message could be like following:
```
Error: SQLITE_BUSY: database is locked
```
 
**Related issues**
  - [azureFile volume mode too strict for container with non root user](https://github.com/kubernetes/kubernetes/issues/54610)
  - [Unable to connect to SQL-lite db mounted on AzureFile/AzureDisks [SQLITE_BUSY: database is locked]](https://github.com/kubernetes/kubernetes/issues/59755)
  - [Allow nobrl parameter like docker to use sqlite over network drive](https://github.com/kubernetes/kubernetes/issues/61767)
  - [Error to deploy mongo with azure file storage](https://github.com/kubernetes/kubernetes/issues/58308)

### 2. permission issue of azure file dynamic provision in acs-engine
**Issue details**:

From acs-engine v0.12.0, RBAC is enabled, azure file dynamic provision does not work from this version

**error logs**:
```
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
 - Add a ClusterRole and ClusterRoleBinding for [azure file dynamic privision](https://github.com/andyzhangx/Demo/tree/master/linux/azurefile#dynamic-provisioning-for-azure-file-in-linux-support-from-v170)
```
kubectl create -f https://raw.githubusercontent.com/andyzhangx/Demo/master/acs-engine/rbac/azure-cloud-provider-deployment.yaml
```

**Fix**
 - PR in acs-engine: [fix azure file dynamic provision permission issue](https://github.com/Azure/acs-engine/pull/2238)
 
### 3. Azure file support on Sovereign Cloud
[Azure file on Sovereign Cloud](https://github.com/kubernetes/kubernetes/pull/48460) is supported from v1.7.11, v1.8.0

### 4. azure file dynamic provision failed due to cluster name length issue
**Issue details**:
k8s cluster name length must be less than 16 characters, otherwise following error will be received when creating dynamic privisioning azure file pvc, this bug exists in [v1.7.0, v1.7.10]:
 > Note: check `cluster-name` by running `grep cluster-name /etc/kubernetes/manifests/kube-controller-manager.yaml` on master node

```
persistentvolume-controller    Warning    ProvisioningFailed Failed to provision volume with StorageClass "azurefile": failed to find a matching storage account
```
**Fix**
 - PR [Fix share name generation in azure file provisioner](https://github.com/kubernetes/kubernetes/pull/48326)

| k8s version | fixed version |
| ---- | ---- |
| v1.7 | 1.7.11 |
| v1.8 | 1.8.0 |
| v1.9 | 1.9.0 |

### 5. azure file dynamic provision failed due to no storage account in current resource group
**Issue details**:
When create an azure file PVC, there will be error if there is no storage account in current resource group, error info would be like following:
```
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

### 6. azure file plugin on Windows does not work after node restart
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

### 7. file permission could not be changed using azure file, e.g. postgresql
**error logs** when running postgresql on azure file plugin:
```
initdb: could not change permissions of directory "/var/lib/postgresql/data": Operation not permitted
fixing permissions on existing directory /var/lib/postgresql/data 
```

**Issue details**:
azure file plugin is using cifs/SMB protocol, file/dir permission could not be changed after mounting

**Workaround**:
Use subPath together with azure disk plugin

**Related issues**
[Persistent Volume Claim permissions](https://github.com/Azure/AKS/issues/225)

### 8. Could not delete pod with AzureFile volume if storage account key changed
**Issue details**:
 - kubelet fails to umount azurefile volume when there is azure file connection, below is an easy repro:
   - create a pod with azure file mount
   - regenerate the account key of the storage account
   - delete the pod, and the pod will never be deleted due to `UnmountVolume.TearDown` error

**error logs**
```
nestedpendingoperations.go:263] Operation for "\"kubernetes.io/azure-file/cc5c86cd-422a-11e8-91d7-000d3a03ee84-myvolume\" (\"cc5c86cd-422a-11e8-91d7-000d3a03ee84\")" failed. No retries permitted until 2018-04-17 10:35:40.240272223 +0000 UTC m=+1185722.391925424 (durationBeforeRetry 500ms). Error: "UnmountVolume.TearDown failed for volume \"myvolume\" (UniqueName: \"kubernetes.io/azure-file/cc5c86cd-422a-11e8-91d7-000d3a03ee84-myvolume\") pod \"cc5c86cd-422a-11e8-91d7-000d3a03ee84\" (UID: \"cc5c86cd-422a-11e8-91d7-000d3a03ee84\") : Error checking if path exists: stat /var/lib/kubelet/pods/cc5c86cd-422a-11e8-91d7-000d3a03ee84/volumes/kubernetes.io~azure-file/myvolume: resource temporarily unavailable
...
kubelet_volumes.go:128] Orphaned pod "380b02f3-422b-11e8-91d7-000d3a03ee84" found, but volume paths are still present on disk
```
**Workaround**:
manually umount the azure file mount path on the agent node and then the pod will be deleted right after that
```
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
