---
name: debug-e2e-pipeline
description: >-
  Fetch and analyze Prow e2e pipeline failures for cloud-provider-azure.
  Downloads build logs, JUnit reports, and node-level artifacts from GCS,
  extracts failed tests, parses Ginkgo results, and surfaces cluster node
  info. Use when debugging a failing Prow e2e job, investigating a
  cloud-provider-azure-ccm-windows-capz or cloud-provider-azure-master-capz
  build, triaging CI failures, or when the user pastes a prow.k8s.io URL.
---

# Debug E2E Pipeline

## When To Use

Use this skill when the user wants to debug a failing Prow e2e pipeline for
cloud-provider-azure. Trigger on:

- Prow build URLs (`prow.k8s.io/view/...`, `gcsweb.k8s.io/...`,
  `storage.googleapis.com/kubernetes-ci-logs/...`)
- References to job names like `cloud-provider-azure-*-capz`
- Requests to "debug the e2e failure", "check why CI failed", "look at the
  build log", or "analyze the pipeline"

## Inputs

| Input | Required | Description |
|-------|----------|-------------|
| Pipeline URL | Yes | Any Prow URL form (prow.k8s.io, gcsweb, or storage.googleapis.com) |
| `--output-dir` | No | Directory for downloaded artifacts (default: `_artifacts/e2e-debug/{JOB}/{BUILD}/` in the repo root) |
| `--fetch PATTERN [PATTERN ...]` | No | Download artifacts matching glob pattern(s) from available_artifacts (reuses existing manifest if present) |

## Evidence Discipline

Follow these rules when analyzing failures and presenting findings.

- **Distinguish facts from inferences.** A *fact* is something directly
  visible in a log line, timestamp, or artifact. An *inference* is a
  conclusion drawn from facts. Always label inferences explicitly —
  say "this suggests" or "I infer," never state an inference as though
  it were directly observed.

- **Beware single-snapshot bias.** A single `kubectl get nodes` output
  or one status dump captures one moment in time. Do not treat a
  point-in-time observation as permanent state. Look for corroborating
  evidence across the full timeline: were pods scheduled to the node
  later? Do CAPI Machine conditions show it healthy at a later time?
  Do controller logs reference it as Ready?

- **Require corroboration for strong claims.** Before asserting a root
  cause, require at least two independent pieces of evidence that
  support the claim and verify that no available evidence contradicts
  it. If you only have one data point, say so.

- **State what's missing.** If key evidence is unavailable, say so
  explicitly rather than filling gaps with assumptions. Missing evidence
  is not confirming evidence.

- **Classify confidence in findings.** When presenting root cause
  analysis, label each finding:
  - **Confirmed**: supported by multiple independent pieces of evidence
    with no counter-evidence.
  - **Likely**: supported by evidence but with gaps — state what
    additional evidence would confirm it.
  - **Uncertain**: plausible hypothesis but insufficient evidence —
    state what you would need to investigate to confirm or rule it out.

## Workflow

The workflow has two phases: **investigation** (delegated to a sub-agent)
and **review** (done by you). The sub-agent gets a fresh context window
dedicated to log analysis; you review its structured findings and present
to the user.

### Phase 1 — Investigation (sub-agent)

Delegate the investigation to a sub-agent. For multiple jobs, spawn one
sub-agent per job in parallel — each gets a dedicated context for its
job. Pass each sub-agent the context listed below, then tell it to
follow the investigation steps that follow.

**Context to pass the sub-agent:**

- The pipeline URL
- The absolute skill directory path (for running the fetch script)
- The output directory path (default:
  `_artifacts/e2e-debug/{JOB}/{BUILD}/` in the repo root)
- Any user-provided context about what to investigate
- The Evidence Discipline rules from this document
- Any specific investigation questions you have based on the user's
  request

Replace `<SKILL_DIR>` with the absolute path to this skill directory.

#### Step 1 — Fetch and Parse Artifacts

```bash
python3 <SKILL_DIR>/scripts/fetch_artifacts.py "<PIPELINE_URL>"
```

This downloads key artifacts from GCS, parses JUnit and build-log, and
outputs a **concise summary JSON to stdout**. The full manifest is written
to `manifest.json` inside the output directory (default:
`_artifacts/e2e-debug/{JOB}/{BUILD}/`).

The stdout summary contains the most important fields for quick triage:
- `status`, `duration`, `prow_url`
- `test_counts`: {total, passed, failed, skipped}
- `failed_tests`: [{name, duration, message}]
- `build_log_summary.ginkgo_summary`: Ginkgo result line
- `build_log_summary.ginkgo_result`: "Test Suite Failed" / "Test Suite Passed" / null
- `build_log_summary.node_info`: raw `kubectl get nodes -o wide` table

**If `ginkgo_result` is null**, tests never produced results. This does
**not** necessarily mean cluster provisioning failed — check
`build_log_summary.node_info` and the build log to determine where the
job stalled. Common causes: cluster failed to provision, tests timed out
during initialization (e.g. log watcher setup), or pods were stuck
Pending for hours. Only conclude "provisioning failure" if node_info is
absent or shows NotReady nodes.

If the download fails (404, network error), report the error and ask the user
to verify the URL.

#### Step 2 — Understand the Job

Prefer reading output files directly using your file-reading tools over
shell commands for post-processing.

Read the full `manifest.json` from disk. Start with `job_summary`:

- `job_summary.command` / `job_summary.args`: the entrypoint script the job runs
- `job_summary.extra_refs`: which repos are cloned (e.g. CAPZ, windows-testing)
- `job_summary.env`: key environment variables — in particular
  `CLUSTER_TEMPLATE` (determines cluster topology and credential provider
  support), `KUBERNETES_VERSION`, and `AZURE_LOADBALANCER_SKU`
- `job_summary.presets`: preset labels that configure the job
- `job_summary.repos`: repo versions under test
- `job_summary.duration`: how long the job ran

This tells you *what the job does* — which script it runs, which cluster
template it uses, and how it's configured. The cluster template determines
whether the cluster has OOT credential provider, dual-stack networking,
machine pools vs machine deployments, etc. To find which template was used:
1. Check `job_summary.env` for `CLUSTER_TEMPLATE`
2. If not set, search build-log.txt for `"Using cluster template:"`
3. The rendered resources in `available_artifacts` (e.g.
   `*/resources/default/KubeadmControlPlane/*.yaml`) show exactly
   what was applied to the cluster

Check `build_log_summary.node_info` to see the cluster topology — OS
versions, node roles, and container runtime versions. This is critical for
platform mismatch issues (e.g. Windows Server 2019 images on 2022 nodes).

Also note `available_artifacts` — a flat list of all artifact paths available
in GCS for this build. Use this as a table of contents: fetch any of these
on demand via `fetch_webpage` on `{gcs_base_url}/{path}`.

#### Step 3 — Review Test Results

Check `test_counts` and `tests` in the manifest:

- `tests.failed`: each entry has `name`, `duration`, and `message`
- `tests.passed`: list of test names that succeeded
- `tests.skipped`: list of test names that were skipped

If `test_counts.failed` is 0 but the job failed, the issue is in cluster
setup or the test harness, not in any individual test.

#### Step 4 — Trace the Root Cause

For each failed test, trace through the build log to understand *why* it
failed — don't stop at the first symptom. Distinguish between what failed
(the symptom) and why it failed (the root cause). Every claim must be
backed by a specific log line, timestamp, or artifact.

1. **Find the test's log section**: search build-log.txt for the test name.
   Ginkgo tests are delimited by `------------------------------` markers.
2. **Read the full test output**: look at pod events, error messages, and
   the Ginkgo failure block.
3. **Build a timeline**: correlate timestamps across build-log.txt and
   other log sources (controller logs, node logs) to establish the
   sequence of events. Key questions: what was the cluster state before
   the test? Did anything change during the test? When exactly did the
   failure occur relative to other events?
4. **Trace the causal chain**: for each event in the timeline, identify
   what triggered it — was it a test action (`kubectl` command), a
   controller reconciliation, or something else? Cite the specific log
   line as evidence. Follow the chain until you reach the originating
   cause: test code → cluster operation → controller decision → etc.
5. **Verify assumptions before concluding**: if you believe "X caused Y",
   check whether the evidence supports it. Look for counter-evidence:
   did X happen before or after Y? Did X happen in other passing runs
   too? Could something else explain Y? Compare against a passing run
   if one is available.
6. **Cross-reference external repos**: if `job_summary.extra_refs` shows repos
   like `windows-testing` or `cluster-api-provider-azure`, the cluster template
   or setup script may live there. Use `fetch_webpage` to read the relevant
   files from those repos.
7. **Check bootstrap controller logs**: `clusters/bootstrap/controllers/`
   contains logs from cluster management controllers (e.g. CAPZ, CAPI)
   that can reveal background operations like node replacements, rolling
   updates, or health check remediations affecting the test environment.
   Download with `--fetch "*/controllers/*/manager.log"`.

#### Step 5 — Fetch Additional Artifacts (optional deep-dive)

Use `--fetch` to download any artifact listed in `available_artifacts`
by glob pattern:

```bash
python3 <SKILL_DIR>/scripts/fetch_artifacts.py "<PIPELINE_URL>" --fetch "*/controllers/*/manager.log"
python3 <SKILL_DIR>/scripts/fetch_artifacts.py "<PIPELINE_URL>" --fetch "*/MachinePool/*.yaml" "*/MachineHealthCheck/*.yaml"
```

If the build was already fetched, `--fetch` reuses the existing manifest
and only downloads the matched artifacts.

For node-level issues (kubelet errors, credential provider problems),
use `--fetch` with a pattern like `"*/machines/*/kubelet.log"` or
fetch individual files via `fetch_webpage` on `{gcs_base_url}/{path}`
using paths from `available_artifacts`.

#### Step 6 — Return Findings

Write findings to `findings.md` in the output directory, then return
them to the main agent. Use this structure:

1. **Output directory**: the absolute path where artifacts were downloaded
2. **Job overview**: name, build ID, status, duration, cluster topology
3. **Failed tests**: which tests failed and their error messages
4. **Root cause analysis**: the full causal chain explaining *why* the
   test failed, not just *what* failed. Each link in the chain must cite
   a specific file path and line number as evidence. Clearly separate
   symptoms (the error the test hit) from the root cause (what created
   the conditions for that error).
5. **Hypotheses considered**: each with supporting evidence,
   contradicting evidence, missing evidence, and confidence level
   (Confirmed / Likely / Uncertain)
6. **Relevant log snippets**: key error lines with file path, line
   number, and surrounding context
7. **Suggested next steps**: what to investigate or fix. Do not propose
   a fix until the root cause is established with evidence.

### Phase 2 — Review and Present (main agent)

Review the sub-agent's findings before presenting to the user:

1. **Spot-check key citations**: for the primary causal chain and any
   Confirmed findings, verify the cited log lines and timestamps by
   reading them directly. Do not blindly trust sub-agent citations.
2. **Check confidence labels**: are they appropriate given the evidence?
   Downgrade if the sub-agent overclaimed.
3. **Look for gaps**: did the sub-agent miss an obvious log source or
   artifact that could confirm or refute the hypothesis?
4. **Cross-compare jobs**: when multiple sub-agents ran (one per job),
   compare their `findings.md` files. Classify failures into shared
   root causes — identify which jobs hit the same underlying issue so
   you only explain each root cause once. Look for what varies across
   passing and failing jobs (e.g. K8s version, cluster template, worker
   topology, region, timing) to narrow down what correlates with
   failure.

Then present the reviewed findings to the user, including confidence
labels on each root cause claim.

## Comparing Jobs

When debugging, it's often useful to compare a failing job against a
passing run of the same or a similar job. If the root cause is not
clear from the failing job alone, ask the user for a passing run URL
to compare against. The workflow:

1. Run the script twice — once per job URL
2. Compare `job_summary.command` and `job_summary.args` — different
   entrypoint scripts or post-commands?
3. Compare `job_summary.env` — different environment variables?
4. Compare `test_counts` — did one job run more/fewer tests?
5. Compare `tests.failed` — same failures or different?
6. Check if one job has no `junit_files` at all — tests never started
7. Compare `build_log_summary.node_info` — different OS versions or
   node counts?

## Bundled Resources

- `scripts/fetch_artifacts.py` — download, parse, and discover GCS artifacts
