# cloud-provider-azure

![Version: 1.34.3](https://img.shields.io/badge/Version-1.34.3-informational?style=flat-square)

A Helm chart for installing kubernetes-sigs/cloud-provider-azure components

**Homepage:** <https://raw.githubusercontent.com/kubernetes-sigs/cloud-provider-azure/master/helm/cloud-provider-azure/README.md>

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| Jack Francis | <jackfrancis@gmail.com> |  |
| Zhecheng Li | <zhechengli1995@outlook.com> |  |

## Source Code

* <https://github.com/kubernetes-sigs/cloud-provider-azure>

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| cloudControllerManager.allocateNodeCidrs | string | `"true"` |  |
| cloudControllerManager.caCertDir | string | `"/etc/ssl"` |  |
| cloudControllerManager.cloudConfig | string | `"/etc/kubernetes/azure.json"` |  |
| cloudControllerManager.clusterCIDR | string | `"10.244.0.0/16"` |  |
| cloudControllerManager.configureCloudRoutes | string | `"true"` |  |
| cloudControllerManager.containerResourceManagement.limitsCPU | string | `"4"` |  |
| cloudControllerManager.containerResourceManagement.limitsMem | string | `"2Gi"` |  |
| cloudControllerManager.containerResourceManagement.requestsCPU | string | `"100m"` |  |
| cloudControllerManager.containerResourceManagement.requestsMem | string | `"128Mi"` |  |
| cloudControllerManager.enabled | bool | `true` |  |
| cloudControllerManager.federatedTokenPath | string | `"/var/run/secrets/azure/tokens"` |  |
| cloudControllerManager.imageName | string | `"azure-cloud-controller-manager"` |  |
| cloudControllerManager.imagePullPolicy | string | `"IfNotPresent"` |  |
| cloudControllerManager.imageRepository | string | `"mcr.microsoft.com/oss/v2/kubernetes"` |  |
| cloudControllerManager.leaderElect | string | `"true"` |  |
| cloudControllerManager.logVerbosity | string | `"2"` |  |
| cloudControllerManager.nodeSelector."node-role.kubernetes.io/control-plane" | string | `""` |  |
| cloudControllerManager.replicas | int | `1` |  |
| cloudControllerManager.routeReconciliationPeriod | string | `"10s"` |  |
| cloudControllerManager.securePort | int | `10268` |  |
| cloudControllerManager.tolerations[0].effect | string | `"NoSchedule"` |  |
| cloudControllerManager.tolerations[0].key | string | `"node-role.kubernetes.io/master"` |  |
| cloudControllerManager.tolerations[1].effect | string | `"NoSchedule"` |  |
| cloudControllerManager.tolerations[1].key | string | `"node-role.kubernetes.io/control-plane"` |  |
| cloudControllerManager.tolerations[2].effect | string | `"NoExecute"` |  |
| cloudControllerManager.tolerations[2].key | string | `"node-role.kubernetes.io/etcd"` |  |
| cloudControllerManager.updateStrategy | object | `{}` |  |
| cloudNodeManager.containerResourceManagement.limitsCPU | string | `"2"` |  |
| cloudNodeManager.containerResourceManagement.limitsCPUWin | string | `"2"` |  |
| cloudNodeManager.containerResourceManagement.limitsMem | string | `"512Mi"` |  |
| cloudNodeManager.containerResourceManagement.limitsMemWin | string | `"512Mi"` |  |
| cloudNodeManager.containerResourceManagement.requestsCPU | string | `"50m"` |  |
| cloudNodeManager.containerResourceManagement.requestsCPUWin | string | `"50m"` |  |
| cloudNodeManager.containerResourceManagement.requestsMem | string | `"50Mi"` |  |
| cloudNodeManager.containerResourceManagement.requestsMemWin | string | `"50Mi"` |  |
| cloudNodeManager.enableHealthProbeProxy | bool | `false` |  |
| cloudNodeManager.enabled | bool | `true` |  |
| cloudNodeManager.healthCheckPort | int | `10356` |  |
| cloudNodeManager.healthProbeProxyImage | string | `"mcr.microsoft.com/oss/v2/kubernetes/azure-health-probe-proxy:v1.34.3"` |  |
| cloudNodeManager.healthProbeProxyImageWindows | string | `"mcr.microsoft.com/oss/v2/kubernetes/azure-health-probe-proxy:v1.34.3"` |  |
| cloudNodeManager.imageName | string | `"azure-cloud-node-manager"` |  |
| cloudNodeManager.imagePullPolicy | string | `"IfNotPresent"` |  |
| cloudNodeManager.imageRepository | string | `"mcr.microsoft.com/oss/kubernetes"` |  |
| cloudNodeManager.logVerbosity | string | `"2"` |  |
| cloudNodeManager.targetPort | int | `10256` |  |
| infra.clusterName | string | `"kubernetes"` |  |

