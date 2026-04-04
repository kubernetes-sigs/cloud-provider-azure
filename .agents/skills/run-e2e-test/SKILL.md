---
name: run-e2e-test
description: Parse a Go e2e test from tests/e2e/, translate each step to kubectl and az CLI commands, and interactively replay the test against a live cluster.
---

# Run E2E Test

## When To Use

Use this skill when the user wants to manually replay a cloud-provider-azure e2e
test case against a live Kubernetes cluster. The skill parses the Go test source,
translates each step to CLI commands (`kubectl` / `az`), and guides interactive
execution.

## Prerequisites

- `az login` completed (active Azure session)
- Valid `KUBECONFIG` pointing at the target cluster
- `kubectl` available on `$PATH`
- `python3` available on `$PATH`

## Inputs

| Input | Required | Description |
|-------|----------|-------------|
| Test file path | Yes | Path to a Go test file under `tests/e2e/`, e.g. `tests/e2e/network/ensureloadbalancer.go` |
| Test name | No | Name of a specific `It(...)` block to replay. If omitted, list all test cases and ask which one(s) to run |
| `--skip-context-check` | No | Skip the interactive cluster confirmation prompt |

## Workflow

Replace `<SKILL_DIR>` with the path to this skill directory.

### Step 1 — Confirm Cluster Context

Unless `--skip-context-check` is passed:

```bash
python3 <SKILL_DIR>/scripts/check_context.py
```

This displays the current kube context, cluster endpoint, and Azure subscription.
**Ask the user to confirm** this is the correct cluster before proceeding. If the
user says no, stop and ask them to switch context.

### Step 2 — Analyze the Test

```bash
python3 <SKILL_DIR>/scripts/analyze_test.py <test-file-path> [--test-name "<test name>"] [--json]
```

This parses the Go test file and outputs a structured plan with:
- `variables`: Go `const`/`var` declarations (deployment names, service names,
  labels, ports) extracted from the file — use these to resolve `${VAR}`
  placeholders in emitted commands
- `setup`: BeforeEach steps (namespace, deployment, Azure client init)
- `test_cases`: list of `It(...)` blocks, each with ordered steps
- `teardown`: AfterEach cleanup steps

Each step includes:
- `type`: `k8s_create`, `k8s_update`, `k8s_delete`, `k8s_wait`, `az_create`,
  `az_read`, `az_delete`, `az_wait`, `connectivity_check`, `assertion`, `phase`
- `description`: human-readable explanation
- `command`: suggested CLI command(s)
- `wait`: polling parameters if applicable
- `verify`: what to check in the output

Use `--json` to get machine-readable JSON output instead of the human-readable
format:

```bash
python3 <SKILL_DIR>/scripts/analyze_test.py tests/e2e/network/ensureloadbalancer.go --json
```

If `--test-name` is provided, only that test case is included. Otherwise all
test cases are listed.

#### Variable Resolution

The `variables` field in the JSON output lists Go constants and variable
declarations extracted from the test file header. Use them to populate
`${VAR}` placeholders in the emitted commands. For example, if the output
contains `"testBaseName": "service-lb"`, substitute `${DEPLOYMENT_NAME}` with
the value derived from that constant. Some variables may reference other Go
symbols — in that case, read the test source to resolve the chain.

### Step 3 — Review the Plan

Present the extracted plan to the user. For each test case show:
1. Setup steps (namespace, deployment, pre-created Azure resources)
2. Test body steps in order
3. Verification checks
4. Cleanup steps

Ask the user to confirm before executing. They may choose to skip certain
steps or modify parameters.

### Step 4 — Execute Setup

Run the setup steps:

1. **Create namespace**: `kubectl create namespace <generated-name>`
2. **Create deployment**: generate YAML from the test's deployment manifest
   and `kubectl apply -f -`
3. **Wait for pods**: `kubectl wait --for=condition=ready pod -l <labels> -n <ns> --timeout=5m`
4. **Pre-create Azure resources** (if needed): PIPs, subnets, IP prefixes via
   `az network ...` commands

Track all created resources for cleanup.

If any setup step fails:
1. Run the diagnostic command from the Error Handling section
2. Report the error with diagnostic output
3. **Ask the user** whether to clean up now (jump to Step 6) or keep
   the environment for manual investigation
4. **Do not proceed to Step 5**

### Step 5 — Execute Test Steps

For each step in the test body:

1. **Create/update K8s resources**: generate YAML, `kubectl apply -f -`
2. **Wait for conditions**: poll with the timeouts from the test
   - Service IP: `kubectl get svc -n <ns> <name> -o jsonpath='{.status.loadBalancer.ingress[*].ip}'`
   - Poll every 10s, timeout per test constants (typically 5m for Standard LB)
3. **Verify Azure state**: run `az` commands and check output matches assertions
4. **Connectivity checks**: `kubectl exec <exec-pod> -- nc -vz -w 4 <ip> <port>`
5. **Report results**: for each assertion, show PASS/FAIL with actual vs expected

Before executing, read `references/INDEX.md` (the patterns index). For each
step in the plan, match the step's description keywords and `step_type` against
the index to identify which sections of `references/patterns.md` are needed.
Read only those sections using the line ranges from the index. Do not read the
entire `patterns.md` file.

**On step failure**, follow the Failure Flow waterfall in the Error
Handling section. Key behaviors:
- Transient errors (429, 409, 5xx, timeout): retry per the retry policy
- Poll timeout (condition never met): record as assertion FAIL
- Assertion/verification mismatch: record FAIL with actual vs expected,
  run diagnostics, then **ask the user** whether to (a) continue,
  (b) pause for investigation, (c) abort to cleanup, or (d) re-check
- Non-recoverable errors: stop and jump to Step 6

### Step 6 — Cleanup

Run cleanup in reverse order:

1. Delete K8s services
2. Delete K8s deployments
3. Delete pre-created Azure resources (PIPs, subnets, prefixes)
4. Delete namespace

Always offer cleanup even if a step failed. Track which resources were
actually created to avoid deleting things that don't exist.

If a cleanup step fails:
1. Retry the deletion once after 30s
2. If it still fails, log the resource as orphaned and continue to the
   next cleanup step
3. Include all orphaned resources in the Step 7 report

### Step 7 — Report

Summarize the execution:

**Always include:**
- Total steps executed: N
- Passed assertions: N
- Failed assertions: N

**Include if any failures occurred:**
- Skipped steps: N (with reason for each)
- For each failure:
  - Step description
  - Expected vs actual
  - Diagnostic output (if collected)
- Infrastructure errors encountered
- Re-checked assertions: N (list step name, attempts before pass or final fail)

**Always include:**
- Resources created: list
- Resources cleaned up: list
- Orphaned resources: list (if any cleanup failed)

## Bundled Resources

- `scripts/check_context.py` — display and validate current kube/Azure context
- `scripts/analyze_test.py` — parse Go e2e test file into structured replay plan
- `references/INDEX.md` — generated keyword and step-type index into
  patterns.md; read this first to find which sections to load
- `references/patterns.md` — comprehensive mapping of Go e2e test patterns to
  kubectl and az CLI commands, including manifest templates and polling patterns
- `scripts/gen_patterns_index.py` — regenerate `references/INDEX.md` from
  keyword annotations in patterns.md

## Error Handling

### Error Categories

| Category | Examples | Behavior |
|---|---|---|
| **Infrastructure** | `kubectl`/`az` not found, cluster unreachable, Azure token expired, RBAC denied | **Stop immediately.** Report error. Jump to Step 6 (Cleanup) for already-created resources. |
| **Setup failure** | Pods stuck Pending/CrashLoopBackOff, PIP quota exceeded, subnet conflict | **Stop.** Run diagnostics. Report findings. Jump to Step 6. Do not proceed to Step 5. |
| **Skip condition** | Cluster is Basic LB but test requires Standard, wrong node pool type, VMSS-only test on non-VMSS | **Skip the test.** Report SKIP with reason. No cleanup needed (nothing was created for this test). |
| **Assertion failure** | LB rules don't match, NSG rule missing, wrong probe config, connectivity timeout, poll timeout | **Record FAIL** with actual vs expected. Run the matching diagnostic from the Diagnostics table. **Ask the user**: (a) Continue — skip dependent steps, proceed to next; (b) Pause — keep environment for manual investigation; (c) Abort — jump to Step 6; (d) Re-check — re-run this step's verification. |
| **Cleanup failure** | Resource in use, namespace stuck Terminating, deletion timeout | **Warn.** Retry once after 30s. List orphaned resources in final report. Continue to next cleanup step. |

### Retry Policy

Retry transient errors before classifying as a failure:

| Error Pattern | Retry | Wait |
|---|---|---|
| K8s API 409 (Conflict) — service/resource update | Yes, 5× | ~100ms, 200ms, 400ms, 800ms, 1s (match `retry.DefaultRetry`) |
| Azure 429 (TooManyRequests) | Yes, 3× | Honor `Retry-After` header; fall back to 60s, 120s, 240s |
| Azure 409 (Conflict) — resource being modified | Yes, 3× | 10s, 20s, 40s |
| K8s/Azure 5xx (InternalError, ServerTimeout) | Yes, 3× | 15s, 30s, 60s |
| `kubectl` connection timeout | Yes, 3× | 15s, 30s, 60s |
| Auth/RBAC errors | No | — |
| Quota exceeded | No | — |
| Resource not found (404) | No | — |

### Diagnostics

When a step fails, run the relevant diagnostic before reporting:

| Failure | Diagnostic |
|---|---|
| Pod stuck Pending | `kubectl describe pod -l app=${LABEL} -n $NS` — check Events section |
| Pod CrashLoopBackOff | `kubectl logs -l app=${LABEL} -n $NS --previous --tail=20` |
| Service no external IP | `kubectl describe svc $SVC_NAME -n $NS` — check Events section |
| Connectivity timeout | `kubectl get endpoints $SVC_NAME -n $NS` — check if endpoints exist |
| Azure resource not found | `az resource show -g $RG -n <exact-name> --resource-type <type> -o json` |
| LB provisioning stuck | `az network lb show -g $RG -n $LB_NAME --query provisioningState -o tsv` |
| PIP allocation failure | `az network public-ip show -g $RG -n $PIP_NAME --query provisioningState -o tsv` |
| Node issues | `kubectl describe node <node-name>` — check Conditions and Events |

When reporting diagnostic output, redact values of environment variables whose
names contain SECRET, PASSWORD, TOKEN, KEY, or CONNECTION_STRING. Truncate log
output to 20 lines.

### Resource Tracking

Track all resources created during execution in your working memory:

```
Created resources:
  k8s: namespace/e2e-test-abc123
  k8s: deployment/deployment-lb-test (ns: e2e-test-abc123)
  k8s: service/svc-test (ns: e2e-test-abc123)
  k8s: pod/exec-agnhost (ns: e2e-test-abc123)
  az:  public-ip/pip-test (rg: mc_mygroup)
```

Include both directly created resources (from your commands) and
indirectly created ones (discovered via `az` queries after Service
creation — e.g., LB rules, NSG rules, PIP allocated by the cloud
controller manager).

On early exit, use this list for targeted cleanup in Step 6.

If the agent session is interrupted during a pause, created resources
become orphans. Re-run cleanup manually using the namespace name and
resource names from the test's variable declarations.

### Failure Flow

When a command fails, evaluate in this order:

1. **Is it retryable?** Check the retry table. If yes, retry.
2. **Is it an infrastructure error?** (auth, cluster unreachable, command
   not found) → **Stop.** Report error. Jump to Step 6.
3. **Is it a setup step failure?** (Step 4) → **Stop.** Run diagnostics.
   Report. Jump to Step 6.
4. **Is it a poll timeout?** (wait for condition that never became true) →
   Treat as **assertion failure**.
5. **Is it an assertion/verification failure?** → Record FAIL with actual
   vs expected. Run the matching diagnostic from the Diagnostics table.
   **Ask the user**:
   - **(a) Continue**: skip dependent steps, proceed to next independent step
   - **(b) Pause**: keep environment alive for manual investigation; suggest
     the diagnostic command, the failed step's verification command, and
     1–2 exploratory commands for the resource under test; wait for user
     to say "continue", "re-check", or "abort"
   - **(c) Abort**: jump to Step 6 (Cleanup)
   - **(d) Re-check**: re-run this step's verification (including waits/polls);
     if it passes, clear the FAIL and proceed normally (do not skip dependent
     steps); if it fails again, re-present the same 4 options; after 3
     re-check failures on the same step, warn that repeated re-checks are
     failing and suggest aborting

   **Batch mode**: if more than 2 assertion failures have occurred in the
   current test case and the user selects Continue, offer: "Multiple
   assertions are failing. (a) Keep pause-on-failure, (b) Switch to
   auto-continue (record all remaining FAILs without pausing), (c) Abort
   to cleanup."

   **Pause warnings**:
   - ⚠️ The cluster and Azure resources remain live during the pause.
     Cloud controllers and Azure reconciliation may modify resources
     (LB rules, NSG rules, endpoints).
   - Azure resources created by this test remain provisioned and billable
     during the pause.
   - If the pause exceeds 10 minutes, remind the user that the
     environment may have changed and suggest re-running diagnostics.

To identify dependent steps, check if a subsequent step uses a variable
produced by the failed step. For example, if "Wait for service external IP"
fails (no IP assigned), skip "Verify connectivity to service IP" since it
depends on having a valid IP.

6. **Is it a cleanup failure?** → Warn, retry once, log orphan, continue.

## Limitations

- The analysis script uses **pattern matching on Go source**, not a full Go
  parser. It handles the standard Ginkgo patterns used in this repo (`Describe`,
  `When`, `Context`, `It`, `BeforeEach`, `AfterEach`, `By`). Unusual or deeply
  nested test structures may need manual interpretation by the agent.
- `By(...)` annotations are emitted as `phase` type steps rather than merged
  into other steps. The agent should use them as context markers.
- Annotation resolution relies on a static map. If a test uses an annotation
  constant not in the map, the raw `consts.XxxAnnotation` name is emitted.
  Check `pkg/consts/consts.go` for the string value.
- The `variables` field captures simple `const`/`var` string assignments. It
  does not evaluate Go expressions or resolve cross-references.
- Commands use `sed` for POSIX-portable field extraction (no GNU `grep -oP`).

## Notes

- The agent should read `references/patterns.md` when it encounters a pattern
  not covered by the analysis script output.
- Azure resource group is discovered from node providerID:
  `kubectl get nodes -o jsonpath='{.items[0].spec.providerID}'` → parse RG name.
- Some tests require specific environment conditions (Standard LB, AKS cluster,
  VMSS node pools). The analysis script flags these as skip conditions that the
  agent should check before executing.
