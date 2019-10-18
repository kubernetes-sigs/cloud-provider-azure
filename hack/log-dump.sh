# !/bin/bash
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
elif [[ -z "${AZURE_SSH_PRIVATE_KEY_FILE:-}" ]]; then
  echo "AZURE_SSH_PRIVATE_KEY_FILE not set, but required to SSH into nodes"
  exit 1
elif [[ -z "${AZURE_SSH_USER:-}" ]]; then
  echo "AZURE_SSH_USER not set, but required to SSH into nodes"
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
)
readonly node_logfiles=(
  "csi-azuredisk"
  "csi-azurefile"
  "kube-proxy"
)
readonly systemd_services=(
  "kubelet"
  "docker"
  "etcd"
)

# SSH into host $1 and execute command $2 inside the host
function log-dump-ssh() {
  local -r host="${1}"
  local -r cmd="${2}"
  if [[ -z "${host}" ]] || [[ -z "${cmd}" ]]; then
    return
  fi
  ssh -oLogLevel=quiet -oConnectTimeout=30 -oStrictHostKeyChecking=no -i "${AZURE_SSH_PRIVATE_KEY_FILE}" "${AZURE_SSH_USER}@${host}" "${cmd}" || true
}

# Copy each file in $1 from host $2 to destination folder $3 via scp
function copy-log-from-node() {
  local -r scp_files="${1}"
  local -r host="${2}"
  local -r destination="${3}"
  scp -T -oLogLevel=quiet -oConnectTimeout=30 -oStrictHostKeyChecking=no -i "${AZURE_SSH_PRIVATE_KEY_FILE}" "${AZURE_SSH_USER}@${host}:${scp_files}" "${destination}" || true
}

# Return the URL / IP address of node $1.
# $2 is true if $1 is master.
function get-host() {
  local -r node_name="${1}"
  local -r is_master="${2:-"false"}"
  if [[ "${is_master}" == "true" ]]; then
    # Remove "https://" from the URL
    echo "$(kubectl config view --minify -ojsonpath="{.clusters[0].cluster.server}" | sed -e "s/https\:\/\///")"
  else
    echo "$(kubectl get nodes -ojsonpath={.items[?\(@.metadata.name==\"${node_name}\"\)].status.addresses[?\(@.type==\"ExternalIP\"\)].address})"
  fi
}

# Dump log files from each master to a local folder
function dump-master-log() {
  local -r master_names=( $(kubectl get nodes -l kubernetes.io/role=master -ojsonpath={.items[*].metadata.name}) )
  for master_name in "${master_names[@]}"; do
    local master_host="$(get-host "${master_name}" "true")"
    if [[ -z "${master_host}" ]]; then
      echo "${master_name} does not have an external IP for the script to SSH into. Skipping..."
      return
    fi

    dump-log "true" "${master_host}" "${master_name}"
  done
}

# Dump log files from each node to a local folder
function dump-node-log() {
  local -r node_names=( $(kubectl get nodes -l kubernetes.io/role!=master -ojsonpath={.items[*].metadata.name}) )
  for node_name in "${node_names[@]}"; do
    local node_host="$(get-host "${node_name}" "false")"
    if [[ -z "${node_host}" ]]; then
      echo "${node_name} does not have an external IP for the script to SSH into. Skipping..."
      return
    fi

    dump-log "false" "${node_host}" "${node_name}"
  done
}

# Dump log files in $1 from host $2 to a local folder named after $3
function dump-log() {
  local -r is_master="${1}"
  local -r host="${2}"
  local -r name="${3}"
  if [[ "${is_master}" == "true" ]]; then
    local -r logfiles_batch="{$(printf "/var/log/containers/%s*," "${master_logfiles[@]}")}"
    local -r scp_files="{$(printf "/var/log/%s*," "${master_logfiles[@]}")}"
  else
    local -r logfiles_batch="{$(printf "/var/log/containers/%s*," "${node_logfiles[@]}")}"
    local -r scp_files="{$(printf "/var/log/%s*," "${node_logfiles[@]}")}"
  fi

  # We can't directly scp files from /var/log/containers to local folder due to premission issue
  # So we have to copy log files from /var/log/containers to /var/log first
  log-dump-ssh "${host}" "sudo cp ${logfiles_batch} /var/log || true"

  # Changing log files to be world-readable for download
  log-dump-ssh "${host}" "sudo chmod -R a+r /var/log/* || true"

  local -r dir="${ARTIFACTS}/${name}"
  echo "Creating ${dir} for storing logs..."
  mkdir -p "${dir}"

  # Comma-delimit file names, prepend /var/log, and append "*" to file names
  copy-log-from-node "${scp_files}" "${host}" "${dir}"

  dump-systemd-service-log "${host}" "${dir}"

  echo "Finished dumping logs for ${name}"
}

# Dump systemd services logs from host $1 to local folder $2
function dump-systemd-service-log() {
  local -r host="${1}"
  local -r dir="${2}"
  for systemd_service in "${systemd_services[@]}"; do
    echo "Dumping ${systemd_service}.log..."
    log-dump-ssh "${host}" "sudo journalctl --output=short-precise -u ${systemd_service}" > "${dir}/${systemd_service}.log"
  done
}

function main() {
  dump-master-log
  dump-node-log
}

main
