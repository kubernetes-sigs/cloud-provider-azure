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
USER="cloudtest"
if [[ -n "${RELEASE_PIPELINE:-}" ]]; then
  # release pipeline uses sp
  USER="vsts"
else
  # aks pipeline uses managed identity
  az login --identity --username "${AZURE_MANAGED_IDENTITY_CLIENT_ID:-}"
  export E2E_MANAGED_IDENTITY_TYPE="userassigned"
fi

export GOPATH="/home/${USER}/go"
export PATH="${PATH}:${GOPATH}/bin"
export AKS_CLUSTER_ID="/subscriptions/${AZURE_SUBSCRIPTION_ID:-}/resourcegroups/${RESOURCE_GROUP:-}/providers/Microsoft.ContainerService/managedClusters/${CLUSTER_NAME:-}"

get_random_location() {
  local LOCATIONS=("eastus")
  echo "${LOCATIONS[${RANDOM} % ${#LOCATIONS[@]}]}"
}

get_k8s_version() {
  if [[ -n "${AKS_KUBERNETES_VERSION:-}" ]]; then
    return
  fi
  K8S_RELEASE="${K8S_RELEASE:-$(echo ${BUILD_SOURCE_BRANCH_NAME:-} | cut -f2 -d'-')}"
  az aks get-versions -l "${AZURE_LOCATION:-}" --subscription ${AZURE_SUBSCRIPTION_ID:-} --output json > az_aks_versions
  echo "Current AKS versions are: $(cat az_aks_versions)"
  if [[ $(cat az_aks_versions | jq 'has("orchestrators")') == true ]]; then
    # Old format
    AKS_KUBERNETES_VERSION=$(cat az_aks_versions \
      | jq -r --arg K8S_RELEASE "${K8S_RELEASE:-}" 'last(.orchestrators |.[] | select(.orchestratorVersion | startswith($K8S_RELEASE))) | .orchestratorVersion')
    if [[ "${AKS_KUBERNETES_VERSION:-}" == "null" ]]; then
      cat az_aks_versions | jq -r '.orchestrators[] | .orchestratorVersion' | sort -V > sorted_k8s_versions 
      AKS_KUBERNETES_VERSION=$(cat sorted_k8s_versions | tail -1)
    fi
  else
    # Preview format
    AKS_KUBERNETES_VERSION=$(cat az_aks_versions \
      | jq -r --arg K8S_RELEASE "${K8S_RELEASE:-}" 'last(.values[] | select(.version | startswith($K8S_RELEASE)) | .patchVersions | keys | .[])')
    if [[ "${AKS_KUBERNETES_VERSION:-}" == "null" ]]; then
      cat az_aks_versions | jq -r '.values[] | .patchVersions | keys | .[]' | sort -V > sorted_k8s_versions
      AKS_KUBERNETES_VERSION=$(cat sorted_k8s_versions | tail -1)
    fi
  fi
}

cleanup() {
  if [[ "${SKIP_COLLECT_LOG_AND_CLEAN_UP:-}" == "true" ]]; then
    return
  fi

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

# TODO: remove this and use user-input CUSTOM_CCM_IMAGE_TAG to defined the image tag for
# cloud provider azure. This will remove dependency on cloud-provider-azure repo, and use
# explicit variables makes the interface more clear.
if [[ -z "${IMAGE_TAG:-}" ]]; then
  IMAGE_TAG="$(git describe --tags --match "v[0-9].*")"
fi

if [[ -z "${CLUSTER_CONFIG_PATH:-}" ]]; then
  CLUSTER_CONFIG_PATH="${REPO_ROOT}/.pipelines/templates/basic-lb.json"
  if [[ "${CLUSTER_TYPE:-}" == "autoscaling" ]]; then
    CLUSTER_CONFIG_PATH="${REPO_ROOT}/.pipelines/templates/autoscaling.json"
    export AZURE_LOADBALANCER_SKU=standard
  elif [[ "${CLUSTER_TYPE:-}" == "autoscaling-multipool" ]]; then
    CLUSTER_CONFIG_PATH="${REPO_ROOT}/.pipelines/templates/autoscaling-multipool.json"
    export AZURE_LOADBALANCER_SKU=standard
  elif [[ "${CLUSTER_TYPE:-}" == "vmas" ]]; then
    CLUSTER_CONFIG_PATH="${REPO_ROOT}/.pipelines/templates/slb-vmas.json"
    export AZURE_LOADBALANCER_SKU=standard
  fi
fi
echo "Choose cluster config file: ${CLUSTER_CONFIG_PATH:-}"

if [[ -z "${CUSTOM_CONFIG_PATH:-}" ]]; then
  # basic-lb and vmas
  CUSTOM_CONFIG_PATH="${REPO_ROOT}/.pipelines/templates/customconfiguration.json"
  if [[ "${CLUSTER_TYPE:-}" == "autoscaling" ]]; then
    CUSTOM_CONFIG_PATH="${REPO_ROOT}/.pipelines/templates/customconfiguration-autoscaling.json"
  elif [[ "${CLUSTER_TYPE:-}" == "autoscaling-multipool" ]]; then
    CUSTOM_CONFIG_PATH="${REPO_ROOT}/.pipelines/templates/customconfiguration-autoscaling-multipool.json"
  fi
fi
echo "Choose custom config file: ${CUSTOM_CONFIG_PATH:-}"

if [[ "${SKIP_BUILD_KUBETEST2_AKS:-}" != "true" ]]; then
  # Considering building kubetest2-aks has no dependency on cloud-provider-azure repo, and to avoid
  # the potential conflict between the cloud-provider-azure repo and the kubetest2-aks, we use a
  # tmp folder to clone the cloud-provider-azure repo and build kubetest2-aks. 
  git clone https://github.com/kubernetes-sigs/cloud-provider-azure.git /tmp/cloud-provider-azure
  pushd /tmp/cloud-provider-azure/kubetest2-aks
  go get -d sigs.k8s.io/kubetest2@latest
  go install sigs.k8s.io/kubetest2@latest
  go mod tidy
  make deployer
  if [[ -n "${RELEASE_PIPELINE:-}" ]]; then
    make install
  else
    sudo GOPATH="/home/${USER}/go" make install
  fi
  rm /tmp/cloud-provider-azure -rf
  popd
fi

get_k8s_version
echo "AKS Kubernetes version is: ${AKS_KUBERNETES_VERSION:-}"

if [[ "${SKIP_BUILD_KUBETEST2_AKS:-}" != "true" ]]; then
  if [[ -n "${RELEASE_PIPELINE:-}" ]]; then
    rm -rf kubetest2-aks
    if [[ "${AKS_KUBERNETES_VERSION:-}" < "1.24" ]]; then
      go mod tidy -compat=1.17
    else
      go mod tidy
    fi
    go mod vendor
  fi
fi

kubetest2 aks --up --rgName "${RESOURCE_GROUP:-}" \
--location "${AZURE_LOCATION:-}" \
--config "${CLUSTER_CONFIG_PATH:-}" \
--customConfig "${CUSTOM_CONFIG_PATH}" \
--clusterName "${CLUSTER_NAME:-}" \
--ccmImageTag "${CUSTOM_CCM_IMAGE_TAG:-$IMAGE_TAG}" \
--casImageTag "${CUSTOM_CAS_IMAGE:-}" \
--kubernetesImageTag "${CUSTOM_K8S_IMAGE_TAG:-}" \
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

if [[ "${CLUSTER_TYPE:-}" =~ "autoscaling" ]]; then
  az aks update --subscription ${AZURE_SUBSCRIPTION_ID:-} --resource-group "${RESOURCE_GROUP:-}" --name "${CLUSTER_NAME:-}" \
    --cluster-autoscaler-profile skip-nodes-with-system-pods=false scale-down-delay-after-add=1m scale-down-unneeded-time=1m scale-down-unready-time=2m
  if [[ "${CLUSTER_TYPE:-}" == "autoscaling-multipool" ]]; then
    az aks update --subscription ${AZURE_SUBSCRIPTION_ID:-} --resource-group "${RESOURCE_GROUP:-}" --name "${CLUSTER_NAME:-}" \
      --cluster-autoscaler-profile balance-similar-node-groups=true
  fi
fi

# Do role assignment for the managed identity
if [[ -z "${RELEASE_PIPELINE:-}" ]]; then
  MC_RESOURCE_ID="/subscriptions/${AZURE_SUBSCRIPTION_ID:-}/resourceGroups/MC_${RESOURCE_GROUP:-}_${CLUSTER_NAME:-}_eastus"
  AGENTPOOL_MI_PRINCIPAL_ID="$(az identity show -n ${CLUSTER_NAME:-}-agentpool -g MC_${RESOURCE_GROUP:-}_${CLUSTER_NAME:-}_eastus --subscription ${AZURE_SUBSCRIPTION_ID:-} | jq -r '.principalId')"
  az role assignment create --assignee "${AGENTPOOL_MI_PRINCIPAL_ID}" --role "AcrPull" --scope "${MC_RESOURCE_ID}" --subscription "${AZURE_SUBSCRIPTION_ID:-}"
fi

if [[ "${SKIP_E2E:-}" != "true" ]]; then
  echo "Running e2e"
  export E2E_ON_AKS_CLUSTER=true
  if [[ "${CLUSTER_TYPE:-}" =~ "autoscaling" ]]; then
    export LABEL_FILTER=${LABEL_FILTER:-Feature:Autoscaling || !Serial && !Slow}
    export SKIP_ARGS=${SKIP_ARGS:-""}
  fi
  make test-ccm-e2e
fi
