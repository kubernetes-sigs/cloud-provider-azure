#!/usr/bin/env bash
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

set -euo pipefail

usage() {
  cat <<'EOF'
Draft-first release orchestration for cloud-provider-azure.

Usage:
  release.sh prepare --branch <release-X.Y> [--tag <vX.Y.Z>] [options]
  release.sh publish --tag <vX.Y.Z> [options]

Prepare options:
  --branch <branch>      Release branch to tag, for example release-1.35 (required)
  --tag <tag>            Stable tag to create, for example v1.35.7 (default: compute next patch tag)
  --remote <name>        Git remote for tag push and repo inference (default: upstream)
  --github-repo <repo>   GitHub repo as owner/repo (default: derived from --remote)
  --workflow <file>      Workflow file or name for manual dispatch (default: release.yaml)
  --force-branch         Pass through to create-release-tags when local branch diverges
  --no-fetch             Pass through to create-release-tags
  --message <message>    Annotated tag message override
  --sign                 Create a signed tag
  --lightweight          Create a lightweight tag instead of an annotated tag
  --dry-run              Print the plan without mutating state

Publish options:
  --tag <tag>            Stable tag to publish (required)
  --remote <name>        Git remote for repo inference (default: upstream)
  --github-repo <repo>   GitHub repo as owner/repo (default: derived from --remote)
  --latest <mode>        Latest-release policy: auto, always, or never (default: auto)
  --base-remote <name>   Forwarded to create-release-note-doc-pr
  --push-remote <name>   Forwarded to create-release-note-doc-pr
  --base-branch <name>   Forwarded to create-release-note-doc-pr (default: documentation)
  --target-repo <repo>   Forwarded to create-release-note-doc-pr
  --head-owner <owner>   Forwarded to create-release-note-doc-pr
  --dry-run              Print the plan without mutating state

Examples:
  release.sh prepare --branch release-1.35
  release.sh prepare --branch release-1.35 --tag v1.35.7 --dry-run
  release.sh publish --tag v1.35.7
  release.sh publish --tag v1.35.7 --latest never --dry-run
EOF
}

err() {
  echo "[ERROR] $*" >&2
}

info() {
  echo "[INFO] $*" >&2
}

print_cmd() {
  printf '+'
  for arg in "$@"; do
    printf ' %q' "${arg}"
  done
  printf '\n'
}

run() {
  if [[ "${DRY_RUN}" == "true" ]]; then
    print_cmd "$@"
    return 0
  fi
  "$@"
}

require_command() {
  local cmd="$1"
  if ! command -v "${cmd}" >/dev/null 2>&1; then
    err "Required command not found in PATH: ${cmd}"
    exit 1
  fi
}

validate_tag() {
  local tag="$1"
  if [[ ! "${tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    err "Expected stable tag in the form vX.Y.Z, got '${tag}'."
    exit 1
  fi
}

parse_remote_owner_repo() {
  local remote="$1"
  local remote_url normalized
  remote_url="$(git -C "${REPO_ROOT}" remote get-url "${remote}" 2>/dev/null || true)"
  normalized="${remote_url%.git}"

  if [[ "${normalized}" =~ ^git@[^:]+:([^/]+)/([^/]+)$ ]]; then
    printf '%s/%s\n' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}"
    return 0
  fi
  if [[ "${normalized}" =~ ^https?://[^/]+/([^/]+)/([^/]+)$ ]]; then
    printf '%s/%s\n' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}"
    return 0
  fi
  if [[ "${normalized}" =~ ^ssh://git@[^/]+/([^/]+)/([^/]+)$ ]]; then
    printf '%s/%s\n' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}"
    return 0
  fi

  return 1
}

resolve_github_repo() {
  local remote="$1"
  local explicit_repo="$2"

  if [[ -n "${explicit_repo}" ]]; then
    printf '%s\n' "${explicit_repo}"
    return 0
  fi

  if ! git -C "${REPO_ROOT}" remote get-url "${remote}" >/dev/null 2>&1; then
    err "Remote '${remote}' does not exist."
    exit 1
  fi

  local derived_repo
  derived_repo="$(parse_remote_owner_repo "${remote}" || true)"
  if [[ -z "${derived_repo}" ]]; then
    err "Could not derive owner/repo from remote '${remote}'. Pass --github-repo explicitly."
    exit 1
  fi

  printf '%s\n' "${derived_repo}"
}

build_create_release_tags_cmd() {
  local branch="$1"
  local remote="$2"
  local tag="$3"
  local force_branch="$4"
  local no_fetch="$5"
  local message="$6"
  local sign="$7"
  local lightweight="$8"
  local mode="$9"

  CREATE_RELEASE_TAGS_CMD=(python3 "${CREATE_RELEASE_TAGS_SCRIPT}" --repo "${REPO_ROOT}" --remote "${remote}" --branch "${branch}")
  if [[ -n "${tag}" ]]; then
    CREATE_RELEASE_TAGS_CMD+=(--tag "${tag}")
  fi
  if [[ "${force_branch}" == "true" ]]; then
    CREATE_RELEASE_TAGS_CMD+=(--force-branch)
  fi
  if [[ "${no_fetch}" == "true" ]]; then
    CREATE_RELEASE_TAGS_CMD+=(--no-fetch)
  fi
  if [[ -n "${message}" ]]; then
    CREATE_RELEASE_TAGS_CMD+=(--message "${message}")
  fi
  if [[ "${sign}" == "true" ]]; then
    CREATE_RELEASE_TAGS_CMD+=(--sign)
  fi
  if [[ "${lightweight}" == "true" ]]; then
    CREATE_RELEASE_TAGS_CMD+=(--lightweight)
  fi
  if [[ "${mode}" == "push" ]]; then
    CREATE_RELEASE_TAGS_CMD+=(--push)
  fi
}

plan_next_tag() {
  local branch="$1"
  local remote="$2"
  local force_branch="$3"

  build_create_release_tags_cmd "${branch}" "${remote}" "" "${force_branch}" "true" "" "false" "false" "plan"

  local output
  if ! output="$("${CREATE_RELEASE_TAGS_CMD[@]}" 2>&1)"; then
    printf '%s\n' "${output}" >&2
    err "Could not compute the next tag from local refs. Pass --tag explicitly or refresh local refs."
    exit 1
  fi

  printf '%s\n' "${output}" >&2

  local next_tag
  next_tag="$(printf '%s\n' "${output}" | awk '$1 == "next_tag:" {print $2; exit}')"
  if [[ -z "${next_tag}" ]]; then
    err "Could not parse next_tag from create-release-tags output."
    exit 1
  fi

  validate_tag "${next_tag}"
  printf '%s\n' "${next_tag}"
}

find_workflow_run() {
  local github_repo="$1"
  local workflow="$2"
  local tag="$3"
  local dispatched_after="$4"
  local attempts=30
  local sleep_seconds=5
  local response run_info

  while (( attempts > 0 )); do
    response="$(gh api "repos/${github_repo}/actions/workflows/${workflow}/runs?event=workflow_dispatch&per_page=30" 2>/dev/null || true)"
    if [[ -n "${response}" ]]; then
      run_info="$(
        RELEASE_WORKFLOW_RUNS_JSON="${response}" python3 -c '
import json
import os
import sys

tag = sys.argv[1]
dispatched_after = sys.argv[2]

data = json.loads(os.environ["RELEASE_WORKFLOW_RUNS_JSON"])
runs = data.get("workflow_runs", [])
candidates = [
    run for run in runs
    if run.get("head_branch") == tag and run.get("created_at", "") >= dispatched_after
]
candidates.sort(key=lambda run: run.get("created_at", ""))
if candidates:
    latest = candidates[-1]
    print(f"{latest.get('id', '')}\t{latest.get('html_url', '')}")
        ' "${tag}" "${dispatched_after}"
      )"
      if [[ -n "${run_info}" ]]; then
        printf '%s\n' "${run_info}"
        return 0
      fi
    fi

    attempts=$((attempts - 1))
    sleep "${sleep_seconds}"
  done

  err "Timed out waiting for workflow run '${workflow}' for tag '${tag}' in ${github_repo}."
  exit 1
}

fetch_release_json() {
  local github_repo="$1"
  local tag="$2"

  gh api "repos/${github_repo}/releases/tags/${tag}" 2>/dev/null
}

check_release_payload() {
  local tag="$1"
  local expect_draft="$2"
  local release_payload="$3"
  shift 3

  RELEASE_PAYLOAD_JSON="${release_payload}" python3 -c '
import json
import os
import sys

tag = sys.argv[1]
expect_draft = sys.argv[2] == "true"
expected_assets = sys.argv[3:]

data = json.loads(os.environ["RELEASE_PAYLOAD_JSON"])

if data.get("tag_name") != tag:
    print(f"release payload tag mismatch: expected {tag}, got {data.get('tag_name')}", file=sys.stderr)
    sys.exit(11)

if data.get("draft") != expect_draft:
    state = "draft" if data.get("draft") else "published"
    expected = "draft" if expect_draft else "published"
    print(f"release {tag} is {state}, expected {expected}", file=sys.stderr)
    sys.exit(10)

if data.get("prerelease"):
    print(f"release {tag} is marked as a prerelease", file=sys.stderr)
    sys.exit(10)

asset_names = {asset.get("name") for asset in data.get("assets", [])}
missing = [name for name in expected_assets if name not in asset_names]
if missing:
    print("missing assets: " + ", ".join(missing), file=sys.stderr)
    sys.exit(10)

release_id = data.get("id")
release_url = data.get("html_url", "")
if not release_id:
    print(f"release {tag} does not have an id", file=sys.stderr)
    sys.exit(11)

print(f"{release_id}\t{release_url}")
  ' "${tag}" "${expect_draft}" "$@"
}

wait_for_draft_release() {
  local github_repo="$1"
  local tag="$2"
  shift 2
  local attempts=24
  local sleep_seconds=5
  local response release_info

  while (( attempts > 0 )); do
    response="$(fetch_release_json "${github_repo}" "${tag}" || true)"
    if [[ -n "${response}" ]]; then
      if release_info="$(check_release_payload "${tag}" "true" "${response}" "$@" 2>/dev/null)"; then
        printf '%s\n' "${release_info}"
        return 0
      fi
    fi

    attempts=$((attempts - 1))
    sleep "${sleep_seconds}"
  done

  err "Timed out waiting for draft release '${tag}' with the expected assets."
  exit 1
}

latest_policy_for_tag() {
  local tag="$1"
  local releases_json="$2"

  RELEASES_JSON="${releases_json}" python3 -c '
import json
import os
import re
import sys

stable = re.compile(r"^v(\d+)\.(\d+)\.(\d+)$")
target = stable.match(sys.argv[1])
if not target:
    print("false")
    sys.exit(0)

target_series = (int(target.group(1)), int(target.group(2)))
data = json.loads(os.environ["RELEASES_JSON"])

releases = []
for page in data:
    if isinstance(page, list):
        releases.extend(page)
    else:
        releases.append(page)

highest_series = None
for release in releases:
    if release.get("draft") or release.get("prerelease"):
        continue
    match = stable.match(release.get("tag_name", ""))
    if not match:
        continue
    series = (int(match.group(1)), int(match.group(2)))
    if highest_series is None or series > highest_series:
        highest_series = series

if highest_series is None or target_series >= highest_series:
    print("true")
else:
    print("false")
  ' "${tag}"
}

prepare_release() {
  local branch=""
  local tag=""
  local remote="upstream"
  local github_repo=""
  local workflow="release.yaml"
  local force_branch="false"
  local no_fetch="false"
  local message=""
  local sign="false"
  local lightweight="false"

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --branch)
        branch="${2:-}"
        shift 2
        ;;
      --tag)
        tag="${2:-}"
        shift 2
        ;;
      --remote)
        remote="${2:-}"
        shift 2
        ;;
      --github-repo)
        github_repo="${2:-}"
        shift 2
        ;;
      --workflow)
        workflow="${2:-}"
        shift 2
        ;;
      --force-branch)
        force_branch="true"
        shift 1
        ;;
      --no-fetch)
        no_fetch="true"
        shift 1
        ;;
      --message)
        message="${2:-}"
        shift 2
        ;;
      --sign)
        sign="true"
        shift 1
        ;;
      --lightweight)
        lightweight="true"
        shift 1
        ;;
      --dry-run)
        DRY_RUN="true"
        shift 1
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        err "Unknown prepare argument: $1"
        usage >&2
        exit 2
        ;;
    esac
  done

  if [[ -z "${branch}" ]]; then
    err "--branch is required for prepare."
    usage >&2
    exit 2
  fi

  if [[ -n "${tag}" ]]; then
    validate_tag "${tag}"
  else
    tag="$(plan_next_tag "${branch}" "${remote}" "${force_branch}")"
  fi

  github_repo="$(resolve_github_repo "${remote}" "${github_repo}")"

  local -a tag_cmd=()
  build_create_release_tags_cmd "${branch}" "${remote}" "${tag}" "${force_branch}" "${no_fetch}" "${message}" "${sign}" "${lightweight}" "push"
  tag_cmd=("${CREATE_RELEASE_TAGS_CMD[@]}")

  if [[ "${DRY_RUN}" == "true" ]]; then
    info "Dry run: prepare ${tag} from ${branch} in ${github_repo}"
    print_cmd "${tag_cmd[@]}"
    print_cmd gh workflow run "${workflow}" -R "${github_repo}" --ref "${tag}"
    echo "[OK] Dry run: would wait for the workflow run and verify the draft release assets:"
    printf '  - %s\n' "${EXPECTED_ASSETS[@]}"
    return 0
  fi

  require_command python3
  require_command gh

  local tag_output
  if ! tag_output="$("${tag_cmd[@]}" 2>&1)"; then
    printf '%s\n' "${tag_output}" >&2
    exit 1
  fi
  printf '%s\n' "${tag_output}"

  local dispatched_after
  dispatched_after="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  run gh workflow run "${workflow}" -R "${github_repo}" --ref "${tag}"
  info "Dispatched ${workflow} for ${tag} in ${github_repo}"

  local run_info run_id run_url
  run_info="$(find_workflow_run "${github_repo}" "${workflow}" "${tag}" "${dispatched_after}")"
  IFS=$'\t' read -r run_id run_url <<<"${run_info}"
  if [[ -n "${run_url}" ]]; then
    info "Watching workflow run ${run_id}: ${run_url}"
  else
    info "Watching workflow run ${run_id}"
  fi

  run gh run watch "${run_id}" -R "${github_repo}" --exit-status --compact

  local release_info release_id release_url
  release_info="$(wait_for_draft_release "${github_repo}" "${tag}" "${EXPECTED_ASSETS[@]}")"
  IFS=$'\t' read -r release_id release_url <<<"${release_info}"
  info "Draft release ${tag} is ready: ${release_url}"
  info "Draft release id: ${release_id}"
}

publish_release() {
  local tag=""
  local remote="upstream"
  local github_repo=""
  local latest_mode="auto"
  local base_remote=""
  local push_remote=""
  local base_branch="documentation"
  local target_repo=""
  local head_owner=""

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --tag)
        tag="${2:-}"
        shift 2
        ;;
      --remote)
        remote="${2:-}"
        shift 2
        ;;
      --github-repo)
        github_repo="${2:-}"
        shift 2
        ;;
      --latest)
        latest_mode="${2:-}"
        shift 2
        ;;
      --base-remote)
        base_remote="${2:-}"
        shift 2
        ;;
      --push-remote)
        push_remote="${2:-}"
        shift 2
        ;;
      --base-branch)
        base_branch="${2:-}"
        shift 2
        ;;
      --target-repo)
        target_repo="${2:-}"
        shift 2
        ;;
      --head-owner)
        head_owner="${2:-}"
        shift 2
        ;;
      --dry-run)
        DRY_RUN="true"
        shift 1
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        err "Unknown publish argument: $1"
        usage >&2
        exit 2
        ;;
    esac
  done

  if [[ -z "${tag}" ]]; then
    err "--tag is required for publish."
    usage >&2
    exit 2
  fi

  validate_tag "${tag}"
  github_repo="$(resolve_github_repo "${remote}" "${github_repo}")"

  local -a doc_pr_cmd=(bash "${CREATE_RELEASE_NOTE_DOC_PR_SCRIPT}" --tag "${tag}")
  if [[ -n "${base_remote}" ]]; then
    doc_pr_cmd+=(--base-remote "${base_remote}")
  fi
  if [[ -n "${push_remote}" ]]; then
    doc_pr_cmd+=(--push-remote "${push_remote}")
  fi
  if [[ -n "${base_branch}" ]]; then
    doc_pr_cmd+=(--base-branch "${base_branch}")
  fi
  if [[ -n "${target_repo}" ]]; then
    doc_pr_cmd+=(--target-repo "${target_repo}")
  fi
  if [[ -n "${head_owner}" ]]; then
    doc_pr_cmd+=(--head-owner "${head_owner}")
  fi

  case "${latest_mode}" in
    auto|always|never)
      ;;
    *)
      err "--latest must be one of auto, always, or never."
      exit 2
      ;;
  esac

  if [[ "${DRY_RUN}" == "true" ]]; then
    info "Dry run: publish ${tag} in ${github_repo}"
    echo "[OK] Dry run: would verify that the draft release exists and includes the expected assets."
    case "${latest_mode}" in
      auto)
        echo "[OK] Dry run: would publish with make_latest decided from the highest published stable major.minor series."
        ;;
      always)
        echo "[OK] Dry run: would publish with make_latest=true."
        ;;
      never)
        echo "[OK] Dry run: would publish with make_latest=false."
        ;;
    esac
    print_cmd gh api --method PATCH "repos/${github_repo}/releases/<release-id>" -F draft=false -F make_latest='<true|false>'
    print_cmd "${doc_pr_cmd[@]}"
    return 0
  fi

  require_command gh

  local release_json release_info release_id release_url
  release_json="$(fetch_release_json "${github_repo}" "${tag}" || true)"
  if [[ -z "${release_json}" ]]; then
    err "Draft release '${tag}' was not found in ${github_repo}."
    exit 1
  fi

  if ! release_info="$(check_release_payload "${tag}" "true" "${release_json}" "${EXPECTED_ASSETS[@]}")"; then
    err "Draft release '${tag}' is not ready to publish."
    exit 1
  fi
  IFS=$'\t' read -r release_id release_url <<<"${release_info}"
  info "Publishing draft release ${tag}: ${release_url}"

  local make_latest="false"
  case "${latest_mode}" in
    always)
      make_latest="true"
      ;;
    never)
      make_latest="false"
      ;;
    auto)
      local releases_json
      releases_json="$(gh api --paginate --slurp "repos/${github_repo}/releases?per_page=100")"
      make_latest="$(latest_policy_for_tag "${tag}" "${releases_json}")"
      ;;
  esac
  info "Publishing ${tag} with make_latest=${make_latest}"

  run gh api --method PATCH "repos/${github_repo}/releases/${release_id}" -F draft=false -F make_latest="${make_latest}"

  run "${doc_pr_cmd[@]}"
}

if ! REPO_ROOT="$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel 2>/dev/null)"; then
  err "release.sh must run from a checkout of this repository."
  exit 1
fi

CREATE_RELEASE_TAGS_SCRIPT="${REPO_ROOT}/.agents/skills/create-release-tags/scripts/create_release_tags.py"
CREATE_RELEASE_NOTE_DOC_PR_SCRIPT="${REPO_ROOT}/.agents/skills/create-release-note-doc-pr/scripts/create_release_note_doc_pr.sh"
DRY_RUN="false"
EXPECTED_ASSETS=(
  azure-cloud-controller-manager-linux-amd64
  azure-cloud-controller-manager-linux-arm
  azure-cloud-controller-manager-linux-arm64
  azure-cloud-node-manager-linux-amd64
  azure-cloud-node-manager-linux-arm
  azure-cloud-node-manager-linux-arm64
  azure-cloud-node-manager-windows-amd64.exe
  azure-acr-credential-provider-linux-amd64
  azure-acr-credential-provider-linux-arm
  azure-acr-credential-provider-linux-arm64
  azure-acr-credential-provider-windows-amd64.exe
)

if [[ ! -f "${CREATE_RELEASE_TAGS_SCRIPT}" ]]; then
  err "Missing dependency script: ${CREATE_RELEASE_TAGS_SCRIPT}"
  exit 1
fi
if [[ ! -f "${CREATE_RELEASE_NOTE_DOC_PR_SCRIPT}" ]]; then
  err "Missing dependency script: ${CREATE_RELEASE_NOTE_DOC_PR_SCRIPT}"
  exit 1
fi

subcommand="${1:-}"
case "${subcommand}" in
  prepare)
    shift
    prepare_release "$@"
    ;;
  publish)
    shift
    publish_release "$@"
    ;;
  -h|--help|help|"")
    usage
    ;;
  *)
    err "Unknown subcommand: ${subcommand}"
    usage >&2
    exit 2
    ;;
esac
