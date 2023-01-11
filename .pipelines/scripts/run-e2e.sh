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

REPO_ROOT=$(realpath $(dirname "${BASH_SOURCE[0]}")/../..)
export GOPATH="/home/vsts/go"
export PATH="${PATH:-}:${GOPATH}/bin"
export AKS_CLUSTER_ID="/subscriptions/${AZURE_SUBSCRIPTION_ID:-}/resourcegroups/${RESOURCE_GROUP:-}/providers/Microsoft.ContainerService/managedClusters/${CLUSTER_NAME:-}"

if [[ -z "${RELEASE_PIPELINE:-}" ]]; then
  az extension add -n aks-preview
  az login --service-principal -u "${AZURE_CLIENT_ID:-}" -p "${AZURE_CLIENT_SECRET:-}" --tenant "${AZURE_TENANT_ID:-}"
fi

get_random_location() {
  local LOCATIONS=("eastus")
  echo "${LOCATIONS[${RANDOM} % ${#LOCATIONS[@]}]}"
}

get_k8s_version() {
  if [[ -n "${AKS_KUBERNETES_VERSION:-}" ]]; then
    return
  fi
  K8S_RELEASE="${K8S_RELEASE:-$(echo ${BUILD_SOURCE_BRANCH_NAME:-} | cut -f2 -d'-')}"
  AKS_KUBERNETES_VERSION=$(az aks get-versions -l "${AZURE_LOCATION:-}" --subscription ${AZURE_SUBSCRIPTION_ID:-} --output json \
    | jq -r --arg K8S_RELEASE "${K8S_RELEASE:-}" 'last(.orchestrators |.[] | select(.orchestratorVersion | startswith($K8S_RELEASE))) | .orchestratorVersion')
  # Normally, K8S_RELEASE has at least one match in AKS, but in case the k8s release is the first minor version,
  # not picked by AKS, we use the latest AKS k8s version as a try-your-best workaround.
  if [[ "${AKS_KUBERNETES_VERSION:-}" == "null" ]]; then
  AKS_KUBERNETES_VERSION=$(az aks get-versions -l "${AZURE_LOCATION:-}" --subscription ${AZURE_SUBSCRIPTION_ID:-} --output json \
    | jq -r '.orchestrators |.[] |select(.upgrades | .==null) |.orchestratorVersion')
  fi
}

cleanup() {
  if [[ -n ${KUBECONFIG:-} ]]; then
      kubectl get node -owide || echo "Unable to get nodes"
      kubectl get pod --all-namespaces=true -owide || echo "Unable to get pods"
      ${REPO_ROOT}/.pipelines/scripts/collect-log.sh || echo "Unable to collect logs"
  fi
  echo "gc the aks cluster"
  # It is possible that kubetest2-aks is not built before cleanup(), so the following command
  # is fine to fail.
  kubetest2 aks --down --rgName "${RESOURCE_GROUP:-}" --clusterName "${CLUSTER_NAME:-}" || true
}
trap cleanup EXIT

if [[ -z "${AZURE_LOCATION:-}" ]]; then
  export AZURE_LOCATION="$(get_random_location)"
fi

if [[ -z "${IMAGE_TAG:-}" ]]; then
  IMAGE_TAG="$(git describe --tags)"
fi

if [[ -z "${CLUSTER_CONFIG_PATH:-}" ]]; then
  CLUSTER_CONFIG_PATH="${REPO_ROOT}/.pipelines/templates/basic-lb.json"
  if [[ "${CLUSTER_TYPE:-}" == "autoscaling" ]]; then
    CLUSTER_CONFIG_PATH="${REPO_ROOT}/kubetest2-aks/cluster-templates/autoscaling.json"
    export AZURE_LOADBALANCER_SKU=standard
  elif [[ "${CLUSTER_TYPE:-}" == "autoscaling-multipool" ]]; then
    CLUSTER_CONFIG_PATH="${REPO_ROOT}/kubetest2-aks/cluster-templates/autoscaling-multipool.json"
    export AZURE_LOADBALANCER_SKU=standard
  fi
fi

if [[ "${CLUSTER_TYPE:-}" =~ "autoscaling" ]]; then
  CUSTOM_CONFIG_PATH="${CUSTOM_CONFIG_PATH:-${REPO_ROOT}/.pipelines/templates/customconfiguration-autoscaling.json}"
else
  CUSTOM_CONFIG_PATH="${CUSTOM_CONFIG_PATH:-${REPO_ROOT}/.pipelines/templates/customconfiguration.json}"
fi

rm -rf kubetest2-aks
git clone --single-branch --branch "cas_config_setup" https://github.com/aagusuab/cloud-provider-azure.git
cp -r cloud-provider-azure/kubetest2-aks .
rm -rf cloud-provider-azure
git config --global --add safe.directory "$(pwd)" || true
pushd kubetest2-aks
go get -d sigs.k8s.io/kubetest2@latest
go install sigs.k8s.io/kubetest2@latest
go mod tidy
make deployer
if [[ -n "${RELEASE_PIPELINE:-}" ]]; then
  make install
else
  sudo GOPATH="/home/vsts/go" make install
fi
popd
if [[ -n "${RELEASE_PIPELINE:-}" ]]; then
  rm -rf kubetest2-aks
  go mod tidy
  go mod vendor
fi

get_k8s_version
echo "AKS Kubernetes version is: ${AKS_KUBERNETES_VERSION:-}"

kubetest2 aks --up --rgName "${RESOURCE_GROUP:-}" \
--location "${AZURE_LOCATION:-}" \
--config "${CLUSTER_CONFIG_PATH:-}" \
--customConfig "${CUSTOM_CONFIG_PATH}" \
--clusterName "${CLUSTER_NAME:-}" \
--ccmImageTag "${IMAGE_TAG:-}" \
--casImageTag "${CUSTOM_CAS_IMAGE:-}" \
--kubernetesImageTag "${IMAGE_TAG:-}" \
--kubeletURL "${KUBELET_URL:-}" \
--k8sVersion "${AKS_KUBERNETES_VERSION:-}"

export KUBECONFIG="${REPO_ROOT}/_kubeconfig/${RESOURCE_GROUP:-}_${CLUSTER_NAME:-}.kubeconfig"
if [[ ! -f "${KUBECONFIG:-}" ]]; then
  echo "kubeconfig not exists"
  exit 1
fi

# Ensure the provisioned cluster can be accessed with the kubeconfig
for i in `seq 1 6`; do
  kubectl get pod --all-namespaces=true && break
  sleep 10
done

kubectl wait --for=condition=Ready node --all --timeout=5m
kubectl get node -owide

echo "Running e2e"

# TODO: We should do it in autoscaling-multipool.json
if [[ "${CLUSTER_TYPE:-}" == "autoscaling-multipool" ]]; then
  az aks update --subscription ${AZURE_SUBSCRIPTION_ID:-} --resource-group "${RESOURCE_GROUP:-}" --name "${CLUSTER_NAME:-}" --cluster-autoscaler-profile balance-similar-node-groups=true
fi

export E2E_ON_AKS_CLUSTER=true
if [[ "${CLUSTER_TYPE:-}" =~ "autoscaling" ]]; then
  export LABEL_FILTER="Feature:Autoscaling"
fi
make test-ccm-e2e
