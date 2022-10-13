#!/bin/bash
# Copyright 2019 The Kubernetes Authors.
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

set -o errexit
set -o nounset
set -o pipefail

# Check for necessary commands and env vars
if ! command -v kubectl > /dev/null; then
  echo "kubectl not found"
  exit 1
elif ! command -v jq > /dev/null; then
  echo "jq not found"
  exit 1
elif [[ -z "${ARTIFACTS:-}" ]]; then
  echo "ARTIFACTS not set, defaulting to /workspace/_artifacts"
  export ARTIFACTS="/workspace/_artifacts"
fi

# Prefixes of log files we are collecting
readonly master_logfiles=(
  "kube-apiserver"
  "kube-scheduler"
  "kube-controller-manager"
  "kube-addon-manager"
  "cloud-controller-manager"
  "kube-proxy"
  "cluster-autoscaler"
  "cloud-node-manager"
  "csi-azuredisk"
  "csi-azurefile"
)
readonly node_logfiles=(
  "csi-azuredisk"
  "csi-azurefile"
  "kube-proxy"
  "cloud-node-manager"
)
readonly systemd_services=(
  "kubelet"
  "docker"
  "etcd"
)
readonly log_dir="/var/log/containers"
readonly script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" > /dev/null 2>&1 && pwd)"

# Return the name of the node that pod $1 is running on
function get-node-name() {
  local -r pod_name="${1}"
  echo "$(kubectl get pod "${pod_name}" -ojsonpath={.spec.nodeName})"
}

# Check if we should export log file $1
# $2 is true if we are currently exporting logs for master
function should-export-log() {
  local -r log_file="${1}"
  local -r is_master="${2}"
  if [[ "${is_master}" == "true" ]]; then
    local -r log_files_to_check=( ${master_logfiles[@]} )
  else
    local -r log_files_to_check=( ${node_logfiles[@]} )
  fi

  for log_file_to_check in "${log_files_to_check[@]}"; do
    if [[ "${log_file}" =~ "${log_file_to_check}" ]]; then
      echo "true"
      return
    fi
  done

  echo "false"
}

# Dump log files from node $1 to artifacts folder via a log dump pod $3
# $3 if true if node $1 is master
function dump-log() {
  local -r node_name="${1}"
  local -r pod_name="${2}"
  local -r is_master="${3}"
  local -r log_files=( $(kubectl exec "${pod_name}" -- ls "${log_dir}") )
  local -r dir="${ARTIFACTS}/${node_name}"

  echo "Creating ${dir} for storing ${node_name}'s logs"
  mkdir -p "${dir}"

  for log_file in "${log_files[@]}"; do
    if [[ "$(should-export-log "${log_file}" "${is_master}")" == "true" ]]; then
      echo "Dumping ${log_file}"
      # Job pod logs may be deleted between listing and the attempt to dump, so we ignore errors from cat.
      kubectl exec "${pod_name}" -- sh -c "(cat \"${log_dir}/${log_file}\" > \"${dir}/${log_file}\") || true"
    fi
  done

  dump-systemd-log "${pod_name}" "${dir}"

  echo "Finished dumping logs for ${node_name}"
}

# Dump systemd services logs via a log dump pod $1 to a local folder $2
function dump-systemd-log() {
  local -r pod_name="${1}"
  local -r dir="${2}"
  for systemd_service in "${systemd_services[@]}"; do
    echo "Dumping ${systemd_service}.log"
    kubectl exec "${pod_name}" -- journalctl --output=short-precise -u "${systemd_service}" > "${dir}/${systemd_service}.log"
  done
}

# Convert json-based logs to slightly more human-readable logs
function post-dump-log() {
  local -r log_files=( $(find "${ARTIFACTS}" -name "*.log") )
  for log_file in "${log_files[@]}"; do
    # Check if the log file can be parsed by jq
    if cat "${log_file}" | jq -ers ".[].log" > /dev/null 2>&1; then
      # Select "log" field from json and remove lines with no character
      cat "${log_file}" | jq -rs ".[].log" | sed "/^$/d" > "${log_file}.tmp" && mv "${log_file}.tmp" "${log_file}"
    fi
  done
}

function cleanup() {
  echo "Uninstalling log-dump-daemonset.yaml via kubectl"
  kubectl delete -f "${script_dir}/log-dump-daemonset.yaml"
}

trap cleanup EXIT

function main() {
  echo "Installing log-dump-daemonset.yaml via kubectl"
  kubectl apply -f "${script_dir}/log-dump-daemonset.yaml"
  if kubectl wait --for condition=ready pod -l app=log-dump-node --timeout=5m; then
    echo "All pods from log-dump-daemonset are ready"
  fi
  kubectl get pod -owide

  # Get pods in Running phase only in case some pods failed to run
  local -r log_dump_pods=( $(kubectl get pod -l app=log-dump-node --field-selector=status.phase=Running -ojsonpath={.items[*].metadata.name}) )
  for pod_name in "${log_dump_pods[@]}"; do
    local node_name="$(get-node-name "${pod_name}")"
    if [[ "${node_name}" =~ "master" ]]; then
      local is_master="true"
    else
      local is_master="false"
    fi

    dump-log "${node_name}" "${pod_name}" "${is_master}"
  done

  post-dump-log

  echo "Logs successfully dumped to ${ARTIFACTS}"
}

main
