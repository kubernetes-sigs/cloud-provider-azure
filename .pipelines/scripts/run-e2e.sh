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
export PATH="${PATH}:${GOPATH}/bin"
export AKS_CLUSTER_ID="/subscriptions/${AZURE_SUBSCRIPTION_ID}/resourcegroups/${RESOURCE_GROUP}/providers/Microsoft.ContainerService/managedClusters/${CLUSTER_NAME}"

az extension add -n aks-preview
az login --service-principal -u "${AZURE_CLIENT_ID}" -p "${AZURE_CLIENT_SECRET}" --tenant "${AZURE_TENANT_ID}"

get_random_location() {
  local LOCATIONS=("eastus")
  echo "${LOCATIONS[${RANDOM} % ${#LOCATIONS[@]}]}"
}

cleanup() {
  if [[ -n ${KUBECONFIG:-} ]]; then
      kubectl get node -owide || echo "Unable to get nodes"
      kubectl get pod -A -owide || echo "Unable to get pods"
      ${REPO_ROOT}/.pipelines/scripts/collect-log.sh || echo "Unable to collect logs"
  fi
  echo "gc the aks cluster"
  kubetest2 aks --down --rgName "${RESOURCE_GROUP}" --clusterName "${CLUSTER_NAME}"
}
trap cleanup EXIT

export AZURE_LOCATION="$(get_random_location)"

IMAGE_TAG="$(git rev-parse --short=7 HEAD)"
CLUSTER_CONFIG_PATH="${REPO_ROOT}/.pipelines/templates/basic-lb.json"
if [[ "${CLUSTER_TYPE}" == "autoscaling" ]]; then
  CLUSTER_CONFIG_PATH="${REPO_ROOT}/.pipelines/templates/autoscaling.json"
  export AZURE_LOADBALANCER_SKU=standard
elif [[ "${CLUSTER_TYPE}" == "autoscaling-multipool" ]]; then
  CLUSTER_CONFIG_PATH="${REPO_ROOT}/.pipelines/templates/autoscaling-multipool.json"
  export AZURE_LOADBALANCER_SKU=standard
fi

pushd kubetest2-aks
go get -d sigs.k8s.io/kubetest2@latest
go install sigs.k8s.io/kubetest2@latest
make deployer
sudo GOPATH="/home/vsts/go" make install
popd
kubetest2 aks --up --rgName "${RESOURCE_GROUP}" \
--location "${AZURE_LOCATION}" \
--config "${CLUSTER_CONFIG_PATH}" \
--customConfig "${REPO_ROOT}/.pipelines/templates/customconfiguration.json" \
--clusterName "${CLUSTER_NAME}" \
--ccmImageTag "${IMAGE_TAG}" \
--k8sVersion "${KUBERNETES_VERSION}"

export KUBECONFIG="${REPO_ROOT}/_kubeconfig/${RESOURCE_GROUP}_${CLUSTER_NAME}.kubeconfig"
if [[ ! -f "${KUBECONFIG}" ]]; then
  echo "kubeconfig not exists"
  exit 1
fi

# Ensure the provisioned cluster can be accessed with the kubeconfig
for i in `seq 1 6`; do
  kubectl get pod -A && break
  sleep 10
done

kubectl wait --for=condition=Ready node --all --timeout=5m
kubectl get node -owide

echo "Running e2e"

# TODO: We should do it in autoscaling-multipool.json
if [[ "${CLUSTER_TYPE}" == "autoscaling-multipool" ]]; then
  az aks update --subscription ${AZURE_SUBSCRIPTION_ID} --resource-group "${RESOURCE_GROUP}" --name "${CLUSTER_NAME}" --cluster-autoscaler-profile balance-similar-node-groups=true
fi

export E2E_ON_AKS_CLUSTER=true
if [[ "${CLUSTER_TYPE}" =~ "autoscaling" ]]; then
  export LABEL_FILTER="Feature:Autoscaling || !Serial && !Slow"
fi
make test-ccm-e2e
