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

set -e

REPO_ROOT=$(realpath $(dirname ${BASH_SOURCE})/..)

# Check KUBECONFIG env var is set or not
# If not, then check if kubeconfig is under cloud-provider-azure repo's root
# If still not, then exist 1
if [ -z "$KUBECONFIG" ]; then
  if [ -f "${REPO_ROOT}/kubernetes" ]; then
    export KUBECONFIG="${REPO_ROOT}/kubeconfig"
  else
    echo "KUBECONFIG not set"
    exit 1
  fi
fi

K8S_ORG_PATH="$GOPATH/src/k8s.io"
K8S_REPO_PATH="${K8S_ORG_PATH}/kubernetes"
if [ ! -d "${K8S_REPO_PATH}" ]; then
  echo "Kubernetes repo not exists, clone one"
  mkdir -p "${K8S_ORG_PATH}" && cd "${K8S_ORG_PATH}"
  git clone https://github.com/kubernetes/kubernetes.git
fi

# Workaround for adding feature gate "ServiceTrafficDistribution=true" in kube-proxy config
if [[ "${K8S_FEATURE_GATES}" =~ "ServiceTrafficDistribution" ]]; then
  kubectl create configmap -n kube-system kube-proxy --from-file "${REPO_ROOT}/tests/k8s-azure/manifest/kube-proxy/config.conf" -o yaml --dry-run=client | kubectl apply -f -
  kubectl rollout restart -n kube-system daemonset kube-proxy
fi

cd "${K8S_REPO_PATH}"
make WHAT='test/e2e/e2e.test'
make WHAT=cmd/kubectl
make ginkgo

export KUBERNETES_PROVIDER=azure
export KUBERNETES_CONFORMANCE_TEST=y
export KUBERNETES_CONFORMANCE_PROVIDER=azure
export CLOUD_CONFIG=$GOPATH/src/sigs.k8s.io/cloud-provider-azure/tests/k8s-azure/manifest/azure.json

# Set GINKGO_ARGS env var with your preferred ginkgo options like ginkgo.focus and ginkgo.skip.
./hack/ginkgo-e2e.sh ${GINKGO_ARGS}
