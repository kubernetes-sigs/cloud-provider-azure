# Standalone VM check-in

`Standalone-VM` is a read-only check for the CAPZ check-in fixture. It runs in
the default suite without `Serial` or `Slow` labels. Run from the repository root:

```sh
LABEL_FILTER='Standalone-VM' ./hack/test-ccm-e2e.sh
```

Use the normal e2e kubeconfig and Azure credentials, plus the existing CAPZ
environment: `TEST_CCM=true`, `CLUSTER_NAME`, and `AZURE_RESOURCE_GROUP`.
The [Prow jobs](https://github.com/kubernetes/test-infra/blob/master/config/jobs/kubernetes-sigs/cloud-provider-azure/cloud-provider-azure-config.yaml)
use CAPZ's `scripts/ci-entrypoint.sh`, which exports these variables and waits
for provisioning and Node readiness before invoking the suite.

The supported templates put control-plane VMs, workers, and their NICs in the
dedicated `AzureCluster.spec.resourceGroup`. The test verifies that the Azure
test client's agent-derived resource group matches `AZURE_RESOURCE_GROUP`.
Separate VM/NIC resource groups and kubelet hostname overrides are not supported
by this test. The templates register Nodes using Azure's computer name via
`ds.meta_data["local_hostname"]`.

Discovery lists Azure VMs with the cluster's CAPZ `owned` tag, independently of
registered Nodes. An empty owned-VM list fails: this fixture must contain
control-plane VMs even with VMSS workers. Availability-set and VMSS Flex members
are excluded; if all owned VMs are members of those groups, the test skips.
Non-CAPZ environments, including ordinary AKS VMSS-only clusters, skip before
client setup. CAPZ MachineDeployment templates can cover standalone workers as
well as control-plane VMs, but CAPZ may use availability sets in regions without
zones; a MachineDeployment alone does not guarantee standalone coverage.

For every discovered standalone VM, the test checks the corresponding Node,
provider ID, instance type, region, initialization taint removal, readiness,
hostname, and at least one valid InternalIP. Every InternalIP must belong to the
primary NIC; no address family is inferred from Service configuration. Zonal
VMs require the exact region-zone label; regional VMs require a nonempty zone
label under the provider's current fault-domain fallback behavior.

Azure metadata is read once after fixture provisioning. Only Node convergence
is polled, with a five-minute deadline for the entire spec, including setup.
Missing Nodes fail with the VM ID and expected Node name. The test neither
provisions VMs nor compares desired CAPI replica counts with Azure inventory.
