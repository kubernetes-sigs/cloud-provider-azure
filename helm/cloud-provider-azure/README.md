# cloud-provider-azure Helm Chart

This Helm chart enables installation and maintenance of Azure cloud provider components. The provided components are compatible with all releases of the Azure cloud provider since v1 (Kubernetes 1.21).

# Defaults

## Installation

By default the chart will choose the correct, latest released version of `cloud-controller-manager` and `cloud-node-manager` that corresponds with the cloud-provider-azure-supported version of Kubernetes that you are running.

```bash
$ helm install --repo https://raw.githubusercontent.com/kubernetes-sigs/cloud-provider-azure/master/helm/repo cloud-provider-azure --generate-name --set cloudControllerManager.imageRepository=mcr.microsoft.com/oss/kubernetes --set cloudControllerManager.imageName=azure-cloud-controller-manager --set cloudNodeManager.imageRepository=mcr.microsoft.com/oss/kubernetes --set cloudNodeManager.imageName=azure-cloud-node-manager
```

If you are running a version of Kubernetes prior to 1.21, then you must provide a known-working image URI that references a build of the cloud-provider-azure runtimes that works with your version of Kubernetes. For example:

```bash
$ helm install --repo https://raw.githubusercontent.com/kubernetes-sigs/cloud-provider-azure/master/helm/repo cloud-provider-azure --generate-name --set cloudControllerManager.imageRepository=mcr.microsoft.com/oss/kubernetes --set cloudControllerManager.imageName=azure-cloud-controller-manager --set cloudControllerManager.imageTag=v0.6.0 --set cloudNodeManager.imageRepository=mcr.microsoft.com/oss/kubernetes --set cloudNodeManager.imageName=azure-cloud-node-manager --set cloudNodeManager.imageTag=v0.6.0
```

Similarly, if you are running a development release of Kubernetes, or wish to test development builds of the cloud-provider-azure runtime components, then you must include the `cloudControllerManager.imageRepository`, `cloudControllerManager.imageName`, `cloudControllerManager.imageTag`, `cloudNodeManager.imageRepository`, `cloudNodeManager.imageName`, and `cloudNodeManager.imageTag` configuration variables:

```bash
$ helm install --repo https://raw.githubusercontent.com/kubernetes-sigs/cloud-provider-azure/master/helm/repo cloud-provider-azure --generate-name --set cloudControllerManager.imageRepository=docker.io/me/ccm --set cloudControllerManager.imageName=azure-cloud-controller-manager --set cloudControllerManager.imageTag=canary --set cloudNodeManager.imageRepository=docker.io/me/ccm --set cloudNodeManager.imageName=azure-cloud-node-manager --set cloudNodeManager.imageTag=canary
```

The following error will be returned if you attempt to install the Helm chart (relying upon default image values) onto a cluster running a version of Kubernetes that doesn't have a supported cloud-provider-azure release:

```
Error: INSTALLATION FAILED: DaemonSet.apps "cloud-node-manager" is invalid: spec.template.spec.containers[0].image: Required value
```

The matrix defining Azure cloud provider releases and their corresponding supported Kubernetes versions can always be found [here](../../README.md)

## Uninstallation

Use the following commands to get the `cloud-provider-azure` Helm chart release name and uninstall it.

```bash
$ helm list
$ helm delete <cloud-provider-azure-chart-release-name>
```

# Helm Repo

A Helm repo will be maintained at the following URI:

- https://raw.githubusercontent.com/kubernetes-sigs/cloud-provider-azure/master/helm/repo

To install cloud-provider-azure you may use the following `helm` command to install with defaults onto a cluster with a name of "my-azure-cluster":

```bash
$ helm install --repo https://raw.githubusercontent.com/kubernetes-sigs/cloud-provider-azure/master/helm/repo cloud-provider-azure --generate-name --set infra.clusterName=my-azure-cluster
```

# Configurable values

Below is the complete set of configuration that you may include when invoking `helm install` against your Kubernetes cluster. Each configuration value is used via the `--set` command line argument (See the example usage of `--set infra.clusterName=my-azure-cluster` above).

## cloud-controller-manager configuration

| configuration value                                               | default value                        | description                                                                                                                                                                      |
|-------------------------------------------------------------------|--------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `infra.clusterName`                                               | `"kubernetes"`                       | Set the cluster name appropriate for your infra provider (e.g., capz, AKS).                                                                                                      |
| `cloudControllerManager.enabled`                                  | `true`                               | Enable or disable the azure-cloud-controller-manager deployment.                                                                                                                 |
| `cloudControllerManager.caCertDir`                                | `"/etc/ssl"`                         | Specify the CA cert directory to mount on the azure-cloud-controller-manager pod.                                                                                                |
| `cloudControllerManager.cloudConfig`                              | `"/etc/kubernetes/azure.json"`       | The path to the cloud provider configuration file. Empty string for no configuration file.                                                                                       |
| `cloudControllerManager.clusterCIDR`                              | `"10.244.0.0/16"`                    | set to the network CIDR for pod IP addresses                                                                                                                                     |
| `cloudControllerManager.configureCloudRoutes`                     | `"true"`                             | if you're using Azure CNI set to `"false"`                                                                                                                                       |
| `cloudControllerManager.imageRepository`                          | `"mcr.microsoft.com/oss/kubernetes"` | container image repository (including any image project directories) location where the Azure `cloud-controller-manager` container image is hosted                               |
| `cloudControllerManager.imageName`                                | `"azure-cloud-controller-manager"`   | container image name for the Azure `cloud-controller-manager` runtime                                                                                                            |
| `cloudControllerManager.imagePullPolicy`                          | `"IfNotPresent"`                     | you may change to`"Always"` or `"Never"` if appropriate for your environment, see [here](https://kubernetes.io/docs/concepts/containers/images/#image-pull-policy) for more info |
| `cloudControllerManager.logVerbosity`                             | `"2"`                                | set to a higher number when debugging the azure-cloud-controller-manager runtime                                                                                                 |
| `cloudControllerManager.securePort`                               | `"10268"`                            | The port on which to serve HTTPS with authentication and authorization.                                                                                                          |
| `cloudControllerManager.replicas`                                 | `"1"`                                | Number of replicas for the azure-cloud-controller-manager deployment. It should be no more than the number of control plane Nodes                                                |
| `cloudControllerManager.routeReconciliationPeriod`                | `"10s"`                              | how often to reconcile node routes                                                                                                                                               |
| `cloudControllerManager.containerResourceManagement.requestsCPU`  | `"100m"`                             | CPU requests configuration for the azure-cloud-controller-manager pod                                                                                                            |
| `cloudControllerManager.containerResourceManagement.requestsMem`  | `"128Mi"`                            | Memory requests configuration for the azure-cloud-controller-manager pod                                                                                                         |
| `cloudControllerManager.containerResourceManagement.limitsCPU`    | `"4"`                                | CPU limits configuration for the azure-cloud-controller-manager pod                                                                                                              |
| `cloudControllerManager.containerResourceManagement.limitsMem`    | `"2Gi"`                              | Memory limits configuration for the azure-cloud-controller-manager pod                                                                                                           |
| `cloudControllerManager.federatedTokenPath`                       | `"/var/run/secrets/azure/tokens"`    | Directory path where service account token will be projected. This is used in workload identity                                                                                  |

## cloud-node-manager configuration

| configuration value | default value | description |
| --- | --- | --- |
| `cloudNodeManager.enabled` | `true` | Enable or disable the azure-cloud-node-manager deployment. |
| `cloudNodeManager.imageRepository` | `"mcr.microsoft.com/oss/kubernetes"` | container image repository (including any image project directories) location where the Azure `cloud-node-manager` container image is hosted |
| `cloudNodeManager.imageName` | `"azure-cloud-node-manager"` | container image name for the Azure `cloud-node-manager` runtime |
| `cloudControllerManager.imagePullPolicy` | `"IfNotPresent"` | you may change to`"Always"` or `"Never"` if appropriate for your environment, see [here](https://kubernetes.io/docs/concepts/containers/images/#image-pull-policy) for more info |
| `cloudNodeManager.logVerbosity`                            | `"2"`                                | set to a higher number when debugging the azure-cloud-node-manager runtime                                                                                                 |
| `cloudNodeManager.containerResourceManagement.requestsCPU` | `"50m"` | CPU requests configuration for the azure-cloud-node-manager pod running on Linux nodes |
| `cloudNodeManager.containerResourceManagement.requestsMem` | `"50Mi"` | Memory requests configuration for the azure-cloud-node-manager pod running on Linux nodes |
| `cloudNodeManager.containerResourceManagement.limitsCPU` | `"2"` | CPU limits configuration for the azure-cloud-node-manager pod running on Linux nodes |
| `cloudNodeManager.containerResourceManagement.limitsMem` | `"512Mi"` | Memory limits configuration for the azure-cloud-node-manager pod running on Linux nodes |
| `cloudNodeManager.containerResourceManagement.requestsCPUWin` | `"50m"` | CPU requests configuration for the azure-cloud-node-manager pod running on Windows nodes |
| `cloudNodeManager.containerResourceManagement.requestsMemWin` | `"50Mi"` | Memory requests configuration for the azure-cloud-node-manager pod running on Windows nodes |
| `cloudNodeManager.containerResourceManagement.limitsCPUWin` | `"2"` | CPU limits configuration for the azure-cloud-node-manager pod running on Windows nodes |
| `cloudNodeManager.containerResourceManagement.limitsMemWin` | `"512Mi"` | Memory limits configuration for the azure-cloud-node-manager pod running on Windows nodes |

The following configuration is made available for advanced users. There are no default values applied, and normally you wouldn't need to include these when deploying your Helm release. See [the values.yaml file](values.yaml) for example values for each configuration.

## optional cloud-controller-manager configuration

| configuration value | description |
| --- | --- |
| `cloudControllerManager.imageTag` | `"v1.23.11"` | container image tag for the Azure `cloud-controller-manager` runtime |
| `cloudControllerManager.allocateNodeCidrs` | Should CIDRs for Pods be allocated and set on the cloud provider. If you're using Azure CNI set to "false" |
| `cloudControllerManager.bindAddress` | The IP address on which to listen for the --secure-port port. The associated interface(s) must be reachable by the rest of the cluster, and by CLI/web clients. If blank or an unspecified address (0.0.0.0 or ::), all interfaces will be used.|
| `cloudControllerManager.certDir` | The directory where the TLS certs are located. If --tls-cert-file and --tls-private-key-file are provided, this flag will be ignored. |
| `cloudControllerManager.cloudConfigSecretName` | The name of the cloud config secret. |
| `cloudControllerManager.contentionProfiling` | Enable lock contention profiling, if profiling is enabled. |
| `cloudControllerManager.controllerStartInterval` | Interval between starting controller managers. |
| `cloudControllerManager.enableDynamicReloading` | Enable re-configuring cloud controller manager from secret without restarting. |
| `cloudControllerManager.http2MaxStreamsPerConnection` | The limit that the server gives to clients for the maximum number of streams in an HTTP/2 connection. Zero means to use golang's default. |
| `cloudControllerManager.kubeAPIBurst` | Burst to use while talking with kubernetes apiserver. |
| `cloudControllerManager.kubeAPIContentType` | Content type of requests sent to apiserver. |
| `cloudControllerManager.kubeAPIQPS` | QPS to use while talking with kubernetes apiserver. |
| `cloudControllerManager.kubeconfig` | Path to kubeconfig file with authorization and master location information. |
| `cloudControllerManager.leaderElectLeaseDuration` | The duration that non-leader candidates will wait after observing a leadership renewal until attempting to acquire leadership of a led but unrenewed leader slot. This is effectively the maximum duration that a leader can be stopped before it is replaced by another candidate. This is only applicable if leader election is enabled.|
| `cloudControllerManager.leaderElectRenewDeadline` | The interval between attempts by the acting master to renew a leadership slot before it stops leading. This must be less than or equal to the lease duration. This is only applicable if leader election is enabled. |
| `cloudControllerManager.leaderElectRetryPeriod` | The duration the clients should wait between attempting acquisition and renewal of a leadership. This is only applicable if leader election is enabled. |
| `cloudControllerManager.leaderElectResourceLock` | The type of resource object that is used for locking during leader election. Supported options are 'endpoints', 'configmaps', 'leases', 'endpointsleases' and 'configmapsleases'.|
| `cloudControllerManager.master` | The address of the Kubernetes API server (overrides any value in kubeconfig). |
| `cloudControllerManager.minResyncPeriod` | The resync period in reflectors will be random between MinResyncPeriod and 2*MinResyncPeriod. |
| `cloudControllerManager.nodeStatusUpdateFrequency` | Specifies how often the controller updates nodes' status. |
| `cloudControllerManager.profiling` | Enable profiling via web interface host:port/debug/pprof/ |
| `cloudControllerManager.useServiceAccountCredentials` | If true, use individual service account credentials for each controller. |

## optional cloud-node-manager configuration

| configuration value                             | description                                                                                                                                      |
|-------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------|
| `cloudNodeManager.imageTag`                     | `"v1.23.11"`                                                                                                                                     | container image tag for the Azure `cloud-node-manager` runtime |
| `cloudNodeManager.cloudConfig`                  | The path to the cloud config file to be used when using ARM (i.e., when `cloudNodeManager.useInstanceMetadata=false`) to fetch node information. |
| `cloudNodeManager.kubeAPIBurst`                 | Burst to use while talking with kubernetes apiserver.                                                                                            |
| `cloudNodeManager.kubeAPIContentType`           | Content type of requests sent to apiserver.                                                                                                      |
| `cloudNodeManager.kubeAPIQPS`                   | QPS to use while talking with kubernetes apiserver.                                                                                              |
| `cloudNodeManager.kubeconfig`                   | Path to kubeconfig file with authorization and master location information.                                                                      |
| `cloudNodeManager.master`                       | The address of the Kubernetes API server (overrides any value in kubeconfig).                                                                    |
| `cloudNodeManager.minResyncPeriod`              | The resync period in reflectors will be random between MinResyncPeriod and 2*MinResyncPeriod.                                                    |
| `cloudNodeManager.nodeStatusUpdateFrequency`    | Specifies how often the controller updates nodes' status.                                                                                        |
| `cloudNodeManager.waitRoutes`                   | Whether the nodes should wait for routes created on Azure route table. It should be set to true when using kubenet plugin.                       |
| `cloudNodeManager.useInstanceMetadata`          | Should use Instance Metadata Service for fetching node information; if false will use ARM instead.                                               |
| `cloudNodeManager.enableHealthProbeProxy`       | Enable health probe proxy sidecar. [Documentation](../health-probe-proxy/README.md)                                                              |
| `cloudNodeManager.healthProbePort`              | Port for health probe proxy sidecar. [Documentation](../health-probe-proxy/README.md)                                                            |
| `cloudNodeManager.targetPort`                   | Target port for health probe proxy sidecar. [Documentation](../health-probe-proxy/README.md)                                                     |
| `cloudNodeManager.healthProbeProxyImage`        | Container image name for the health probe proxy sidecar. [Documentation](../health-probe-proxy/README.md)                                        |
| `cloudNodeManager.healthProbeProxyImageWindows` | Windows container image name for the health probe proxy sidecar. [Documentation](../health-probe-proxy/README.md)                                |

# Maintaining the Repo

Whenever changes have been made to the `cloud-provider-azure` Helm chart, a new version of the chart should be released. First, pick an appropriate next, higher version and update the `version` property in `helm/cloud-provider-azure/Chart.yaml`. Then, package the entire set of changes to the chart into a new repo version:

From the git root:

```bash
$ make update-helm
Successfully packaged chart and saved it to: helm/repo/cloud-provider-azure-1.23.11.tgz
```

Changes to the chart should *always* include a new version, and then an update to the Helm repo as described above.

If changes to the chart don't include an update to the Helm repo as described above, then the PR CI job will fail, preventing a merge to `master`.

To check if your Helm chart changes will pass CI, you can manually run the verification script:

```bash
$ hack/verify-helm-repo.sh
++ dirname hack/verify-helm-repo.sh
+ REPO_ROOT=hack/..
+ for chart in '"cloud-provider-azure"'
++ ls hack/../helm/repo/cloud-provider-azure-1.23.10.tgz hack/../helm/repo/cloud-provider-azure-1.23.11.tgz hack/../helm/repo/cloud-provider-azure-1.23.8.tgz
++ sort -rV
++ head -n 1
+ LATEST_CHART=hack/../helm/repo/cloud-provider-azure-1.23.11.tgz
++ echo hack/../helm/repo/cloud-provider-azure-1.23.11.tgz
++ grep -Eoq '[0-9]+.[0-9]+.[0-9]+'
+ LATEST_VERSION=
+ MATCH_STRING='version: '
+ grep -q 'version: ' hack/../helm/cloud-provider-azure/Chart.yaml
+ rm -Rf hack/../chart_verify
+ mkdir hack/../chart_verify
+ tar -xf hack/../helm/repo/cloud-provider-azure-1.23.11.tgz -C chart_verify
+ diff -r hack/../chart_verify/cloud-provider-azure/ hack/../helm/cloud-provider-azure/ --exclude README.md
+ rm -Rf hack/../chart_verify
+ exit 0
$ echo $?
0
```

## How to pick an appropriate new version when updating the Helm chart

By convention, we pin the Helm chart version to the most recent release of cloud-provider-azure supporting the most recent release of Kubernetes. For example, at the time of this writing, the most recent version of Kubernetes supported by cloud-provider-azure is 1.23, and the most recent release of cloud-provider-azure is `v1.23.11`. When the next version of cloud-provider-azure is released (either `v1.23.12`, or `v1.24.0` in the event that the next release includes support for a new minor release of Kubernetes) we would update the Helm chart to refer to the newly released, versioned image references of `cloud-controller-manager` and `cloud-node-manager`, and then update the Helm chart version to the same version of of the new release (e.g., either `v1.23.12` or `1.24.0`).

If, however, changes to the chart are made independently of a new release of cloud-provider-azure, we can follow the semantic versioning convention of adding `suffix` data to the existing release. For example, if the current release of the Helm chart is `v1.23.11` and we want to add additional configuration capabilities to `values.yaml`, we may do so, and then update the version of the chart to `v1.23.11+20220505` following the formula `<existing release>+<current date in format YYYYMMDD>`.

As described [above](#maintaining-the-repo), always run `make update-helm` after making changes to the Helm chart, and updating the version.
