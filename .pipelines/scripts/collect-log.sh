#!/bin/bash

# Copyright 2022 The Kubernetes Authors.
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

echo "collecting logs of all pods in kube-system"
mkdir "${ARTIFACT_DIR}/kube-system"
kubectl get pod -A | grep "kube-system" | awk '{printf("%s\n",$2)}' | while read -r POD; do
    kubectl logs ${POD} -n kube-system > "${ARTIFACT_DIR}/kube-system/${POD}.log" || echo "Cannot collect log from pod ${POD}"
done

echo "collecting logs of all nodes"
mkdir "${ARTIFACT_DIR}/node-log"
LOG_IMAGE="mcr.microsoft.com/dotnet/runtime-deps:6.0"
NODE_CMDS=("journalctl --no-pager --output=short-precise" \
           "journalctl --no-pager --output=short-precise -k" \
           "kubelet --version" "journalctl --no-pager --output=short-precise -u kubelet.service" \
           "journalctl --no-pager --output=short-precise -u containerd.service" \
           "cat /var/log/cloud-init.log" \
           "cat /var/log/cloud-init-output.log" \
           "ls /run/cluster-api/")
NODE_LOG_FILE_NAMES=("journal.log" "kern.log" "kubelet-version.txt" "kubelet.log" "containerd.log" "cloud-init.log" "cloud-init-output.log" "sentinel-file-dir.txt")
kubectl get node | grep "aks-" | awk '{printf("%s\n",$1)}' | while read -r NODE; do
    NODE_LOG_DIR="${ARTIFACT_DIR}/node-log/${NODE}"
    mkdir "${NODE_LOG_DIR}"
    for i in "${!NODE_CMDS[@]}"; do
        kubectl debug node/${NODE} -it --image=${LOG_IMAGE} -- /bin/sh -c "chroot /host ${NODE_CMDS[$i]}" > "${NODE_LOG_DIR}/${NODE_LOG_FILE_NAMES[$i]}" || echo "Cannot collect ${NODE_LOG_FILE_NAMES[$i]} log from node ${NODE}"
    done
done
