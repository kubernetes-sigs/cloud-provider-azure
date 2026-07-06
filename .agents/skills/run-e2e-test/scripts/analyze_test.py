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

"""Analyze a Go e2e test file and produce a structured replay plan.

Parses Ginkgo test structure (Describe/When/Context/It/BeforeEach/AfterEach/By
blocks) and translates Go SDK calls to kubectl/az CLI equivalents.

Usage:
    python3 analyze_test.py <test-file> [--test-name "<name>"] [--json]

Output: JSON replay plan with setup, test_cases, and teardown sections.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from dataclasses import asdict, dataclass, field
from pathlib import Path


# ---------------------------------------------------------------------------
# Data model
# ---------------------------------------------------------------------------


@dataclass
class Step:
    """A single replay step."""

    step_type: str  # k8s_create, k8s_update, k8s_delete, k8s_wait,
    # az_create, az_read, az_delete, az_wait,
    # connectivity_check, assertion, skip_check, phase
    description: str
    commands: list[str] = field(default_factory=list)
    wait_interval: str = ""
    wait_timeout: str = ""
    verify: str = ""
    manifest: str = ""


@dataclass
class TestCase:
    """A single It(...) block."""

    name: str
    steps: list[Step] = field(default_factory=list)
    skip_conditions: list[str] = field(default_factory=list)


@dataclass
class ReplayPlan:
    """Full replay plan for a test file."""

    file: str
    suite_name: str
    labels: list[str] = field(default_factory=list)
    variables: dict[str, str] = field(default_factory=dict)
    setup: list[Step] = field(default_factory=list)
    test_cases: list[TestCase] = field(default_factory=list)
    teardown: list[Step] = field(default_factory=list)
    skip_conditions: list[str] = field(default_factory=list)


# ---------------------------------------------------------------------------
# Annotation mapping  (all ServiceAnnotation* from pkg/consts/consts.go)
# ---------------------------------------------------------------------------

ANNOTATION_MAP: dict[str, str] = {
    # Load balancer core
    "consts.ServiceAnnotationLoadBalancerInternal": "service.beta.kubernetes.io/azure-load-balancer-internal",
    "consts.ServiceAnnotationLoadBalancerInternalSubnet": "service.beta.kubernetes.io/azure-load-balancer-internal-subnet",
    "consts.ServiceAnnotationLoadBalancerMode": "service.beta.kubernetes.io/azure-load-balancer-mode",
    "consts.ServiceAnnotationLoadBalancerResourceGroup": "service.beta.kubernetes.io/azure-load-balancer-resource-group",
    "consts.ServiceAnnotationLoadBalancerConfigurations": "service.beta.kubernetes.io/azure-load-balancer-configurations",
    # Load balancer IP / dual stack
    "consts.ServiceAnnotationLoadBalancerIPv4": "service.beta.kubernetes.io/azure-load-balancer-ipv4",
    "consts.ServiceAnnotationLoadBalancerIPv6": "service.beta.kubernetes.io/azure-load-balancer-ipv6",
    # Idle timeout / TCP reset
    "consts.ServiceAnnotationLoadBalancerIdleTimeout": "service.beta.kubernetes.io/azure-load-balancer-tcp-idle-timeout",
    "consts.ServiceAnnotationDisableTCPReset": "service.beta.kubernetes.io/azure-load-balancer-disable-tcp-reset",
    "consts.ServiceAnnotationLoadBalancerDisableTCPReset": "service.beta.kubernetes.io/azure-load-balancer-disable-tcp-reset",
    # HA ports / floating IP
    "consts.ServiceAnnotationLoadBalancerEnableHighAvailabilityPorts": "service.beta.kubernetes.io/azure-load-balancer-enable-high-availability-ports",
    "consts.ServiceAnnotationDisableLoadBalancerFloatingIP": "service.beta.kubernetes.io/azure-disable-load-balancer-floating-ip",
    # Health probes
    "consts.ServiceAnnotationLoadBalancerHealthProbeProtocol": "service.beta.kubernetes.io/azure-load-balancer-health-probe-protocol",
    "consts.ServiceAnnotationLoadBalancerHealthProbeRequestPath": "service.beta.kubernetes.io/azure-load-balancer-health-probe-request-path",
    "consts.ServiceAnnotationLoadBalancerHealthProbeInterval": "service.beta.kubernetes.io/azure-load-balancer-health-probe-interval",
    "consts.ServiceAnnotationLoadBalancerHealthProbeNumOfProbe": "service.beta.kubernetes.io/azure-load-balancer-health-probe-num-of-probe",
    # DNS
    "consts.ServiceAnnotationDNSLabelName": "service.beta.kubernetes.io/azure-dns-label-name",
    # PIP
    "consts.ServiceAnnotationPIPName": "service.beta.kubernetes.io/azure-pip-name",
    "consts.ServiceAnnotationPIPPrefixID": "service.beta.kubernetes.io/azure-pip-prefix-id",
    "consts.ServiceAnnotationAzurePIPTags": "service.beta.kubernetes.io/azure-pip-tags",
    "consts.ServiceAnnotationIPTagsForPublicIP": "service.beta.kubernetes.io/azure-pip-ip-tags",
    "consts.ServiceAnnotationAdditionalPublicIPs": "service.beta.kubernetes.io/azure-additional-public-ips",
    # Mixed protocols
    "consts.ServiceAnnotationLoadBalancerMixedProtocols": "service.beta.kubernetes.io/azure-load-balancer-mixed-protocols",
    # Security
    "consts.ServiceAnnotationSharedSecurityRule": "service.beta.kubernetes.io/azure-shared-securityrule",
    "consts.ServiceAnnotationAllowedServiceTags": "service.beta.kubernetes.io/azure-allowed-service-tags",
    "consts.ServiceAnnotationAllowedIPRanges": "service.beta.kubernetes.io/azure-allowed-ip-ranges",
    "consts.ServiceAnnotationDenyAllExceptLoadBalancerSourceRanges": "service.beta.kubernetes.io/azure-deny-all-except-load-balancer-source-ranges",
    # Private Link Service
    "consts.ServiceAnnotationPLSCreation": "service.beta.kubernetes.io/azure-pls-create",
    "consts.ServiceAnnotationPLSName": "service.beta.kubernetes.io/azure-pls-name",
    "consts.ServiceAnnotationPLSResourceGroup": "service.beta.kubernetes.io/azure-pls-resource-group",
    "consts.ServiceAnnotationPLSIpConfigurationSubnet": "service.beta.kubernetes.io/azure-pls-ip-configuration-subnet",
    "consts.ServiceAnnotationPLSIpConfigurationIPAddressCount": "service.beta.kubernetes.io/azure-pls-ip-configuration-ip-address-count",
    "consts.ServiceAnnotationPLSIpConfigurationIPAddress": "service.beta.kubernetes.io/azure-pls-ip-configuration-ip-address",
    "consts.ServiceAnnotationPLSFqdns": "service.beta.kubernetes.io/azure-pls-fqdns",
    "consts.ServiceAnnotationPLSProxyProtocol": "service.beta.kubernetes.io/azure-pls-proxy-protocol",
    "consts.ServiceAnnotationPLSVisibility": "service.beta.kubernetes.io/azure-pls-visibility",
    "consts.ServiceAnnotationPLSAutoApproval": "service.beta.kubernetes.io/azure-pls-auto-approval",
}


# ---------------------------------------------------------------------------
# Parsers
# ---------------------------------------------------------------------------


def extract_string_arg(line: str, func_name: str) -> str:
    """Extract the first quoted string argument from a function call."""
    pattern = rf'{func_name}\s*\(\s*"([^"]*)"'
    match = re.search(pattern, line)
    return match.group(1) if match else ""


def extract_labels(source: str) -> list[str]:
    """Extract Ginkgo Label(...) values from Describe declarations."""
    labels: list[str] = []
    # Search Describe headers (up to the opening func() {), not bodies
    for match in re.finditer(r'Describe\s*\([^{]+', source):
        desc_header = match.group(0)
        for label_match in re.finditer(r"Label\(([^)]+)\)", desc_header):
            content = label_match.group(1)
            # Handle utils.TestSuiteLabelXxx constants
            for const_match in re.finditer(r"utils\.(\w+)", content):
                labels.append(const_match.group(1))
            # Handle string literals
            for str_match in re.finditer(r'"([^"]+)"', content):
                labels.append(str_match.group(1))
    # Deduplicate while preserving order
    seen = set()
    unique = []
    for label in labels:
        if label not in seen:
            seen.add(label)
            unique.append(label)
    return unique


def find_block_end(source: str, start: int) -> int:
    """Find the end of a brace-delimited block starting at the opening brace."""
    depth = 0
    i = start
    while i < len(source):
        if source[i] == "{":
            depth += 1
        elif source[i] == "}":
            depth -= 1
            if depth == 0:
                return i
        i += 1
    return len(source)


def extract_blocks(source: str, block_type: str) -> list[tuple[str, str]]:
    """Extract named blocks like It("name", func() { ... }).

    Returns list of (name, body) tuples.
    """
    blocks = []
    pattern = rf'{block_type}\s*\(\s*"([^"]*)"'
    for match in re.finditer(pattern, source):
        name = match.group(1)
        # Find the opening brace of the func() { ... }
        rest = source[match.end() :]
        brace_pos = rest.find("{")
        if brace_pos == -1:
            continue
        abs_brace = match.end() + brace_pos
        end = find_block_end(source, abs_brace)
        body = source[abs_brace + 1 : end]
        blocks.append((name, body))
    return blocks


def extract_unnamed_blocks(source: str, block_type: str) -> list[str]:
    """Extract unnamed blocks like BeforeEach(func() { ... })."""
    blocks = []
    pattern = rf"{block_type}\s*\(\s*func\s*\(\s*\)\s*\{{"
    for match in re.finditer(pattern, source):
        brace_start = match.end() - 1
        end = find_block_end(source, brace_start)
        blocks.append(source[brace_start + 1 : end])
    return blocks


def extract_top_level_describe_blocks(source: str) -> list[tuple[str, str]]:
    """Extract top-level var _ = Describe(...) blocks.

    Returns list of (name, body) tuples for each Describe block.
    """
    blocks = []
    pattern = r'var\s+_\s*=\s*Describe\s*\(\s*"([^"]*)"'
    for match in re.finditer(pattern, source):
        name = match.group(1)
        rest = source[match.end() :]
        brace_pos = rest.find("{")
        if brace_pos == -1:
            continue
        abs_brace = match.end() + brace_pos
        end = find_block_end(source, abs_brace)
        body = source[abs_brace + 1 : end]
        blocks.append((name, body))
    return blocks


def parse_annotations(body: str) -> dict[str, str]:
    """Extract annotation map entries from test body."""
    annotations: dict[str, str] = {}
    # Match patterns like: annotation[consts.XxxAnnotation] = "value"
    # or: consts.XxxAnnotation: "value"
    for match in re.finditer(
        r"(consts\.\w+)\s*(?:\]|\})\s*=\s*\"([^\"]*)\"|"
        r"(consts\.\w+)\s*:\s*\"([^\"]*)\"|"
        r'\"(service\.beta\.kubernetes\.io/[^\"]+)\"\s*:\s*\"([^\"]*)\"',
        body,
    ):
        if match.group(1):
            key = ANNOTATION_MAP.get(match.group(1), match.group(1))
            annotations[key] = match.group(2)
        elif match.group(3):
            key = ANNOTATION_MAP.get(match.group(3), match.group(3))
            annotations[key] = match.group(4)
        elif match.group(5):
            annotations[match.group(5)] = match.group(6)
    return annotations


def parse_skip_conditions(body: str) -> list[str]:
    """Extract Skip(...) conditions."""
    conditions = []
    for match in re.finditer(r'Skip\s*\(\s*"([^"]*)"', body):
        conditions.append(match.group(1))
    # Also check for env-based skips
    for match in re.finditer(
        r'os\.Getenv\(\s*"(\w+)"\s*\)\s*(?:!=|==)\s*"([^"]*)"', body
    ):
        conditions.append(f"env {match.group(1)} check against '{match.group(2)}'")
    return conditions


def parse_setup_steps(body: str) -> list[Step]:
    """Parse BeforeEach body into setup steps."""
    steps: list[Step] = []

    # Kube client creation
    if "CreateKubeClientSet" in body:
        steps.append(
            Step(
                step_type="k8s_create",
                description="Initialize Kubernetes client (uses current KUBECONFIG)",
                commands=["kubectl cluster-info"],
            )
        )

    # Namespace creation
    if "CreateTestingNamespace" in body:
        # Try to extract the basename
        basename = ""
        match = re.search(r'CreateTestingNamespace\s*\(\s*(\w+)', body)
        if match:
            basename = match.group(1)
        steps.append(
            Step(
                step_type="k8s_create",
                description=f"Create test namespace (basename: {basename})",
                commands=[
                    'NS="e2e-test-$(head -c 5 /dev/urandom | xxd -p)"',
                    'kubectl create namespace "$NS"',
                ],
            )
        )

    # Azure test client creation
    if "CreateAzureTestClient" in body:
        steps.append(
            Step(
                step_type="az_read",
                description="Initialize Azure client (discover RG from node providerID)",
                commands=[
                    "RG=$(kubectl get nodes -o jsonpath='{.items[0].spec.providerID}' | sed -n 's|.*[Rr]esource[Gg]roups/\\([^/]*\\).*|\\1|p')",
                    "SUB=$(kubectl get nodes -o jsonpath='{.items[0].spec.providerID}' | sed -n 's|.*[Ss]ubscriptions/\\([^/]*\\).*|\\1|p')",
                    'LOCATION=$(az group show -n "$RG" --query location -o tsv)',
                ],
            )
        )

    # Deployment creation
    if "createServerDeploymentManifest" in body or "Deployments(" in body:
        dep_name = ""
        match = re.search(
            r"createServerDeploymentManifest\s*\(\s*(\w+)", body
        )
        if match:
            dep_name = match.group(1)
        steps.append(
            Step(
                step_type="k8s_create",
                description=f"Create agnhost server deployment ({dep_name})",
                commands=[
                    "kubectl apply -f - <<'EOF'\n"
                    "apiVersion: apps/v1\n"
                    "kind: Deployment\n"
                    "metadata:\n"
                    '  name: ${DEPLOYMENT_NAME}\n'
                    "  namespace: ${NS}\n"
                    "spec:\n"
                    "  replicas: 5\n"
                    "  selector:\n"
                    "    matchLabels:\n"
                    "      app: ${LABEL}\n"
                    "  template:\n"
                    "    metadata:\n"
                    "      labels:\n"
                    "        app: ${LABEL}\n"
                    "    spec:\n"
                    "      nodeSelector:\n"
                    "        kubernetes.io/os: linux\n"
                    "      containers:\n"
                    "      - name: agnhost\n"
                    "        image: registry.k8s.io/e2e-test-images/agnhost:2.36\n"
                    "        args: [netexec, --http-port=80]\n"
                    "        ports:\n"
                    "        - containerPort: 80\n"
                    "          name: http\n"
                    "EOF"
                ],
            )
        )

    # Wait for pods
    if "WaitPodsToBeReady" in body:
        steps.append(
            Step(
                step_type="k8s_wait",
                description="Wait for all pods to be ready",
                commands=[
                    "kubectl wait --for=condition=ready pod -l app=${LABEL} -n $NS --timeout=5m"
                ],
                wait_interval="10s",
                wait_timeout="5m",
            )
        )

    return steps


def _parse_by_steps(body: str) -> list[tuple[int, Step]]:
    """Parse By(...) annotations into positioned phase steps.

    Returns list of (char_position, Step) tuples for ordering.
    """
    results = []
    for match in re.finditer(r'By\s*\(\s*"([^"]*)"', body):
        results.append((
            match.start(),
            Step(step_type="phase", description=f"[By] {match.group(1)}"),
        ))
    return results


def parse_test_body_steps(body: str) -> list[Step]:
    """Parse an It(...) body into ordered replay steps."""
    steps: list[Step] = []

    # Collect By() annotations as positioned phase markers
    by_steps = _parse_by_steps(body)

    # Service creation
    if "CreateLoadBalancerServiceManifest" in body or "Services(" in body:
        annotations = parse_annotations(body)
        ann_yaml = "\n".join(
            f"    {k}: \"{v}\"" for k, v in annotations.items()
        )
        steps.append(
            Step(
                step_type="k8s_create",
                description="Create LoadBalancer Service",
                commands=["kubectl apply -f - <<'EOF'\n... (see manifest)\nEOF"],
                manifest=(
                    "apiVersion: v1\nkind: Service\nmetadata:\n"
                    f"  name: ${{SVC_NAME}}\n  namespace: ${{NS}}\n"
                    f"  annotations:\n{ann_yaml}\n"
                    "spec:\n  type: LoadBalancer\n"
                    "  ipFamilyPolicy: PreferDualStack\n"
                    "  selector:\n    app: ${LABEL}\n"
                    "  ports:\n  - name: http\n    port: 80\n"
                    "    targetPort: 80\n    protocol: TCP"
                ),
            )
        )

    # Wait for service IP
    if (
        "WaitServiceExposureAndValidateConnectivity" in body
        or "createAndExposeDefaultServiceWithAnnotation" in body
    ):
        steps.append(
            Step(
                step_type="k8s_wait",
                description="Wait for service to get external IP",
                commands=[
                    "kubectl get svc $SVC_NAME -n $NS -o jsonpath='{.status.loadBalancer.ingress[*].ip}'"
                ],
                wait_interval="10s",
                wait_timeout="5m",
                verify="IP address(es) are assigned",
            )
        )

    # PIP creation
    for match in re.finditer(r"WaitCreatePIP\s*\(", body):
        steps.append(
            Step(
                step_type="az_create",
                description="Create public IP address",
                commands=[
                    "az network public-ip create --name $PIP_NAME -g $RG "
                    "--location $LOCATION --sku Standard "
                    "--allocation-method Static --version IPv4"
                ],
                wait_interval="10s",
                wait_timeout="2m",
            )
        )

    # PIP prefix creation
    if "WaitCreatePIPPrefix" in body:
        steps.append(
            Step(
                step_type="az_create",
                description="Create public IP prefix",
                commands=[
                    "az network public-ip prefix create --name $PREFIX_NAME "
                    "-g $RG --location $LOCATION --sku Standard "
                    "--length 28 --version IPv4"
                ],
            )
        )

    # Subnet creation
    if "CreateSubnet" in body:
        steps.append(
            Step(
                step_type="az_create",
                description="Create subnet",
                commands=[
                    "az network vnet subnet create --name $SUBNET_NAME "
                    "--vnet-name $VNET_NAME -g $RG --address-prefix $CIDR"
                ],
            )
        )

    # LB verification
    if "getAzureLoadBalancerFromPIP" in body or "GetLoadBalancer" in body:
        steps.append(
            Step(
                step_type="az_read",
                description="Verify load balancer configuration",
                commands=[
                    "az network lb list -g $RG -o json",
                    "az network lb show -g $RG -n $LB_NAME -o json",
                ],
                verify="Check LB rules, probes, and backend pools match expectations",
            )
        )

    # LB rule assertions
    if "LoadBalancingRules" in body:
        steps.append(
            Step(
                step_type="assertion",
                description="Verify LB rules (ports, protocol, idle timeout, floating IP, HA ports)",
                commands=["az network lb rule list -g $RG --lb-name $LB_NAME -o json"],
                verify="Check rule properties match test expectations",
            )
        )

    # Health probe assertions
    if "Probes" in body or "HealthProbe" in body:
        steps.append(
            Step(
                step_type="assertion",
                description="Verify health probes (protocol, interval, threshold, path)",
                commands=["az network lb probe list -g $RG --lb-name $LB_NAME -o json"],
                verify="Check probe properties match test expectations",
            )
        )

    # NSG verification
    if "GetClusterSecurityGroups" in body or "SecurityGroupValidator" in body:
        steps.append(
            Step(
                step_type="az_read",
                description="Verify NSG rules",
                commands=[
                    "az network nsg list -g $RG -o json",
                    "az network nsg rule list -g $RG --nsg-name $NSG_NAME -o json",
                ],
                verify="Check security rules match expected allow/deny rules",
            )
        )

    # PLS verification
    if "GetPrivateLinkService" in body or "PrivateLinkService" in body:
        steps.append(
            Step(
                step_type="az_read",
                description="Verify Private Link Service",
                commands=[
                    "az network private-link-service show -g $RG -n $PLS_NAME -o json"
                ],
            )
        )

    # Connectivity check
    if "ValidateServiceConnectivity" in body or "nc -vz" in body:
        # Emit exec-agnhost pod creation step
        steps.append(
            Step(
                step_type="k8s_create",
                description="Create hostNetwork exec-agnhost pod for connectivity checks",
                commands=[
                    "kubectl apply -f - <<'EOF'\n"
                    "apiVersion: v1\n"
                    "kind: Pod\n"
                    "metadata:\n"
                    "  name: exec-agnhost\n"
                    "  namespace: ${NS}\n"
                    "spec:\n"
                    "  hostNetwork: true\n"
                    "  terminationGracePeriodSeconds: 0\n"
                    "  nodeSelector:\n"
                    "    kubernetes.io/os: linux\n"
                    "  tolerations:\n"
                    "  - key: node-role.kubernetes.io/control-plane\n"
                    "    operator: Exists\n"
                    "    effect: NoSchedule\n"
                    "  - key: node-role.kubernetes.io/master\n"
                    "    operator: Exists\n"
                    "    effect: NoSchedule\n"
                    "  containers:\n"
                    "  - name: agnhost\n"
                    "    image: registry.k8s.io/e2e-test-images/agnhost:2.36\n"
                    "    args: [\"pause\"]\n"
                    "    imagePullPolicy: IfNotPresent\n"
                    "EOF",
                    "kubectl wait --for=condition=ready pod/exec-agnhost -n $NS --timeout=2m",
                ],
            )
        )
        steps.append(
            Step(
                step_type="connectivity_check",
                description="Verify TCP/UDP connectivity to service IP",
                commands=[
                    "kubectl exec exec-agnhost -n $NS -- nc -vz -w 4 $SVC_IP $SVC_PORT"
                ],
                wait_interval="10s",
                wait_timeout="10m",
                verify='Output contains "succeeded"',
            )
        )

    # Service update
    if "Services(" in body and ".Update(" in body:
        steps.append(
            Step(
                step_type="k8s_update",
                description="Update service (check test for specific changes)",
                commands=[
                    "kubectl get svc $SVC_NAME -n $NS -o yaml > /tmp/svc-update.yaml",
                    "# Edit /tmp/svc-update.yaml with required changes",
                    "kubectl apply -f /tmp/svc-update.yaml",
                ],
            )
        )

    # Service deletion
    if "Services(" in body and ".Delete(" in body:
        steps.append(
            Step(
                step_type="k8s_delete",
                description="Delete service",
                commands=["kubectl delete svc $SVC_NAME -n $NS --wait=true --timeout=5m"],
            )
        )

    # Backend pool check
    if "waitForNodesInLBBackendPool" in body or "BackendAddressPools" in body:
        steps.append(
            Step(
                step_type="az_wait",
                description="Verify backend pool node count",
                commands=[
                    "az network lb address-pool list -g $RG --lb-name $LB_NAME -o json"
                ],
                wait_interval="10s",
                wait_timeout="10m",
                verify="Backend pool has expected number of addresses",
            )
        )

    # ETag stability check
    if "getResourceEtags" in body or "Etag" in body.lower():
        steps.append(
            Step(
                step_type="assertion",
                description="Verify resource ETags unchanged (no spurious Azure reconciliation)",
                commands=[
                    "az network lb show -g $RG -n $LB_NAME --query etag -o tsv",
                    "az network nsg show -g $RG -n $NSG_NAME --query etag -o tsv",
                    "az network public-ip show -g $RG -n $PIP_NAME --query etag -o tsv",
                ],
                verify="ETags should match pre-update values",
            )
        )

    # PIP cleanup verification
    if "ListPublicIPs" in body and ("Fail" in body or "should have been deleted" in body):
        steps.append(
            Step(
                step_type="assertion",
                description="Verify managed PIPs were cleaned up",
                commands=["az network public-ip list -g $RG -o json"],
                verify="No PIPs with service tag for the deleted service",
            )
        )

    # IP availability check
    if "CheckIPAddressAvailability" in body or "SelectAvailablePrivateIPs" in body:
        steps.append(
            Step(
                step_type="az_read",
                description="Check IP address availability in VNet",
                commands=[
                    "az network vnet check-ip-address -g $RG -n $VNET_NAME --ip-address $IP"
                ],
            )
        )

    # Kubectl describe for error events
    if "RunKubectl" in body and "describe" in body:
        steps.append(
            Step(
                step_type="k8s_wait",
                description="Check service events for expected messages",
                commands=["kubectl describe svc $SVC_NAME -n $NS"],
                verify="Events section contains expected message",
            )
        )

    # Interleave By() phase markers based on their position relative to steps
    # Since we can't perfectly correlate char positions of parsed steps,
    # append By() markers at the end in order
    for _pos, by_step in sorted(by_steps, key=lambda x: x[0]):
        steps.append(by_step)

    return steps


def parse_teardown_steps(body: str) -> list[Step]:
    """Parse AfterEach body into teardown steps."""
    steps: list[Step] = []

    if "Deployments(" in body and "Delete" in body:
        steps.append(
            Step(
                step_type="k8s_delete",
                description="Delete test deployment",
                commands=[
                    "kubectl delete deployment $DEPLOYMENT_NAME -n $NS --wait=true"
                ],
            )
        )

    if "DeleteNamespace" in body:
        steps.append(
            Step(
                step_type="k8s_delete",
                description="Delete test namespace",
                commands=["kubectl delete namespace $NS --wait=true --timeout=5m"],
            )
        )

    # PIP cleanup
    if "DeletePIPWithRetry" in body:
        steps.append(
            Step(
                step_type="az_delete",
                description="Delete test public IPs",
                commands=["az network public-ip delete -n $PIP_NAME -g $RG"],
            )
        )

    # PIP prefix cleanup
    if "DeletePIPPrefixWithRetry" in body:
        steps.append(
            Step(
                step_type="az_delete",
                description="Delete test public IP prefix",
                commands=["az network public-ip prefix delete -n $PREFIX_NAME -g $RG"],
            )
        )

    # Subnet cleanup
    if "DeleteSubnet" in body:
        steps.append(
            Step(
                step_type="az_delete",
                description="Delete test subnet",
                commands=[
                    "az network vnet subnet delete --name $SUBNET_NAME "
                    "--vnet-name $VNET_NAME -g $RG"
                ],
            )
        )

    return steps


def extract_variables(source: str) -> dict[str, str]:
    """Extract Go const/var string declarations from the test file.

    Looks for patterns like:
        const testBaseName = "service-lb"
        serviceName = "annotation-test"
    """
    variables: dict[str, str] = {}
    # Match: const name = "value" or name = "value" in const/var blocks
    for match in re.finditer(
        r'(?:const\s+)?(\w+)\s*(?:string\s*)?=\s*"([^"]*)"', source[:3000]
    ):
        name = match.group(1)
        value = match.group(2)
        # Skip import paths and annotation values
        if "/" in value and "." in value:
            continue
        if value.startswith("service.beta.kubernetes.io"):
            continue
        variables[name] = value

    # Also look inside const blocks
    for const_match in re.finditer(r'const\s*\((.*?)\)', source[:3000], re.DOTALL):
        block = const_match.group(1)
        for match in re.finditer(r'(\w+)\s*=\s*"([^"]*)"', block):
            name = match.group(1)
            value = match.group(2)
            if "/" in value and "." in value:
                continue
            if value.startswith("service.beta.kubernetes.io"):
                continue
            variables[name] = value

    return variables


def parse_test_file(filepath: str, test_name: str | None = None) -> ReplayPlan:
    """Parse a Go e2e test file into a ReplayPlan."""
    source = Path(filepath).read_text()

    # Extract top-level Describe blocks to scope BeforeEach/AfterEach correctly
    describe_blocks = extract_top_level_describe_blocks(source)

    # Use the first Describe block's name as suite name
    suite_name = ""
    if describe_blocks:
        suite_name = describe_blocks[0][0]

    # Extract labels from Describe headers (searches all Describe headers, not just body)
    labels = extract_labels(source)

    # Extract variables
    variables = extract_variables(source)

    # Extract setup from the FIRST Describe block only to avoid duplication
    # When a file has multiple Describe blocks with identical BeforeEach,
    # we only need setup from one.
    setup_steps: list[Step] = []
    teardown_steps: list[Step] = []

    if describe_blocks:
        first_describe_body = describe_blocks[0][1]
        for body in extract_unnamed_blocks(first_describe_body, "BeforeEach"):
            setup_steps.extend(parse_setup_steps(body))
        for body in extract_unnamed_blocks(first_describe_body, "AfterEach"):
            teardown_steps.extend(parse_teardown_steps(body))

    # Extract skip conditions from all BeforeEach bodies across all Describe blocks
    skip_conditions: list[str] = []
    for _desc_name, desc_body in describe_blocks:
        for be_body in extract_unnamed_blocks(desc_body, "BeforeEach"):
            skip_conditions.extend(parse_skip_conditions(be_body))

    # Collect test cases from all Describe blocks
    test_cases: list[TestCase] = []

    for _desc_name, desc_body in describe_blocks:
        # Collect It blocks that are NOT inside When/Context blocks
        when_blocks = extract_blocks(desc_body, "When")
        context_blocks = extract_blocks(desc_body, "Context")
        container_blocks = when_blocks + context_blocks

        # Track bodies of It blocks found inside When/Context to avoid duplication
        when_it_bodies: set[str] = set()

        for container_name, container_body in container_blocks:
            # Extract When/Context-level BeforeEach as additional setup
            container_setup: list[Step] = []
            for be_body in extract_unnamed_blocks(container_body, "BeforeEach"):
                container_setup.extend(parse_setup_steps(be_body))
                container_setup.extend(parse_test_body_steps(be_body))

            nested_its = extract_blocks(container_body, "It")
            for it_name, it_body in nested_its:
                # Track this It body to exclude from top-level collection
                when_it_bodies.add(it_body[:100])

                full_name = f"{container_name} → {it_name}"
                if test_name and test_name.lower() not in full_name.lower():
                    continue

                tc = TestCase(
                    name=full_name,
                    steps=container_setup + parse_test_body_steps(it_body),
                    skip_conditions=parse_skip_conditions(it_body),
                )
                test_cases.append(tc)

        # Collect top-level It blocks (not inside When/Context)
        all_its = extract_blocks(desc_body, "It")
        for it_name, it_body in all_its:
            if it_body[:100] in when_it_bodies:
                continue
            if test_name and test_name.lower() not in it_name.lower():
                continue
            tc = TestCase(
                name=it_name,
                steps=parse_test_body_steps(it_body),
                skip_conditions=parse_skip_conditions(it_body),
            )
            test_cases.append(tc)

    if not test_cases:
        print(
            f"Warning: no test cases found in {filepath}",
            file=sys.stderr,
        )

    return ReplayPlan(
        file=filepath,
        suite_name=suite_name,
        labels=labels,
        variables=variables,
        setup=setup_steps,
        test_cases=test_cases,
        teardown=teardown_steps,
        skip_conditions=skip_conditions,
    )


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Analyze a Go e2e test and produce a replay plan"
    )
    parser.add_argument("test_file", help="Path to the Go test file")
    parser.add_argument(
        "--test-name",
        help="Name of a specific It() block to include (substring match)",
    )
    parser.add_argument(
        "--json",
        action="store_true",
        default=False,
        help="Output raw JSON instead of human-readable format",
    )
    args = parser.parse_args()

    if not Path(args.test_file).exists():
        print(f"Error: {args.test_file} not found", file=sys.stderr)
        return 1

    plan = parse_test_file(args.test_file, args.test_name)

    if args.json:
        print(json.dumps(asdict(plan), indent=2))
        return 0

    # Human-readable output
    print(f"{'=' * 70}")
    print(f"E2E Test Replay Plan")
    print(f"{'=' * 70}")
    print(f"File:   {plan.file}")
    print(f"Suite:  {plan.suite_name}")
    if plan.labels:
        print(f"Labels: {', '.join(plan.labels)}")
    if plan.variables:
        print(f"\n📌 Variables:")
        for k, v in plan.variables.items():
            print(f"   {k} = \"{v}\"")
    if plan.skip_conditions:
        print(f"\n⚠️  Suite-level skip conditions:")
        for cond in plan.skip_conditions:
            print(f"   - {cond}")

    if plan.setup:
        print(f"\n{'─' * 70}")
        print("SETUP (BeforeEach)")
        print(f"{'─' * 70}")
        for i, step in enumerate(plan.setup, 1):
            print(f"\n  {i}. [{step.step_type}] {step.description}")
            for cmd in step.commands:
                for line in cmd.splitlines():
                    print(f"     $ {line}")

    print(f"\n{'─' * 70}")
    print(f"TEST CASES ({len(plan.test_cases)} found)")
    print(f"{'─' * 70}")
    for tc in plan.test_cases:
        print(f"\n  📋 {tc.name}")
        if tc.skip_conditions:
            print(f"     ⚠️  Skip if: {'; '.join(tc.skip_conditions)}")
        for i, step in enumerate(tc.steps, 1):
            print(f"\n     {i}. [{step.step_type}] {step.description}")
            for cmd in step.commands:
                for line in cmd.splitlines():
                    print(f"        $ {line}")
            if step.wait_interval:
                print(
                    f"        ⏱  Poll every {step.wait_interval}, "
                    f"timeout {step.wait_timeout}"
                )
            if step.verify:
                print(f"        ✅ Verify: {step.verify}")
            if step.manifest:
                print(f"        📄 Manifest:")
                for line in step.manifest.splitlines():
                    print(f"           {line}")

    if plan.teardown:
        print(f"\n{'─' * 70}")
        print("TEARDOWN (AfterEach)")
        print(f"{'─' * 70}")
        for i, step in enumerate(plan.teardown, 1):
            print(f"\n  {i}. [{step.step_type}] {step.description}")
            for cmd in step.commands:
                for line in cmd.splitlines():
                    print(f"     $ {line}")

    print(f"\n{'=' * 70}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
