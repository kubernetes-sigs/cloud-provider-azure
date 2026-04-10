# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Display current kube context and Azure subscription for user confirmation.

Exits 0 on success (context info displayed), 1 on error.
"""

import json
import subprocess
import sys


def run(cmd: list[str], capture: bool = True) -> subprocess.CompletedProcess:
    try:
        return subprocess.run(cmd, capture_output=capture, text=True, timeout=30)
    except FileNotFoundError:
        # Command not installed
        result = subprocess.CompletedProcess(cmd, returncode=127, stdout="", stderr=f"{cmd[0]}: command not found")
        return result
    except subprocess.TimeoutExpired:
        result = subprocess.CompletedProcess(cmd, returncode=124, stdout="", stderr=f"{cmd[0]}: timed out after 30s")
        return result


def get_kube_context() -> dict:
    """Return current kube context info."""
    result = run(["kubectl", "config", "current-context"])
    if result.returncode != 0:
        return {"error": "kubectl not configured or KUBECONFIG not set"}

    context_name = result.stdout.strip()

    # Get cluster info from the context
    result = run(
        [
            "kubectl",
            "config",
            "view",
            "-o",
            f"jsonpath={{.contexts[?(@.name==\"{context_name}\")].context.cluster}}",
        ]
    )
    cluster_name = result.stdout.strip() if result.returncode == 0 else "unknown"

    # Get the server URL
    result = run(
        [
            "kubectl",
            "config",
            "view",
            "-o",
            f"jsonpath={{.clusters[?(@.name==\"{cluster_name}\")].cluster.server}}",
        ]
    )
    server = result.stdout.strip() if result.returncode == 0 else "unknown"

    # Get node count
    result = run(["kubectl", "get", "nodes", "--no-headers"])
    node_count = len(result.stdout.strip().splitlines()) if result.returncode == 0 else "unknown"

    # Get resource group from first node's providerID
    rg = "unknown"
    result = run(
        [
            "kubectl",
            "get",
            "nodes",
            "-o",
            "jsonpath={.items[0].spec.providerID}",
        ]
    )
    if result.returncode == 0 and result.stdout.strip():
        provider_id = result.stdout.strip()
        # Parse: azure:///subscriptions/<sub>/resourceGroups/<rg>/...
        parts = provider_id.split("/")
        for i, part in enumerate(parts):
            if part.lower() == "resourcegroups" and i + 1 < len(parts):
                rg = parts[i + 1]
                break

    return {
        "context": context_name,
        "cluster": cluster_name,
        "server": server,
        "node_count": node_count,
        "resource_group": rg,
    }


def get_azure_context() -> dict:
    """Return current Azure subscription info."""
    result = run(["az", "account", "show", "-o", "json"])
    if result.returncode != 0:
        return {"error": "az login required or az CLI not installed"}

    try:
        account = json.loads(result.stdout)
        return {
            "subscription": account.get("name", "unknown"),
            "subscription_id": account.get("id", "unknown"),
            "tenant": account.get("tenantId", "unknown"),
            "user": account.get("user", {}).get("name", "unknown"),
        }
    except json.JSONDecodeError:
        return {"error": "Failed to parse az account output"}


def main() -> int:
    print("=" * 60)
    print("CLUSTER CONTEXT CHECK")
    print("=" * 60)

    kube = get_kube_context()
    if "error" in kube:
        print(f"\n❌ Kubernetes: {kube['error']}")
        return 1

    print(f"\n📦 Kubernetes Context:")
    print(f"   Context:        {kube['context']}")
    print(f"   Cluster:        {kube['cluster']}")
    print(f"   Server:         {kube['server']}")
    print(f"   Nodes:          {kube['node_count']}")
    print(f"   Resource Group: {kube['resource_group']}")

    azure = get_azure_context()
    if "error" in azure:
        print(f"\n❌ Azure: {azure['error']}")
        return 1

    print(f"\n☁️  Azure Subscription:")
    print(f"   Subscription:   {azure['subscription']}")
    print(f"   Subscription ID: {azure['subscription_id']}")
    print(f"   Tenant:         {azure['tenant']}")
    print(f"   User:           {azure['user']}")

    print(f"\n{'=' * 60}")
    print("⚠️  Please confirm this is the correct cluster before proceeding.")
    print(f"{'=' * 60}")

    return 0


if __name__ == "__main__":
    sys.exit(main())
