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
  local LOCATIONS=("eastus" "eastus2" "southcentralus" "westus3")
  echo "${LOCATIONS[${RANDOM} % ${#LOCATIONS[@]}]}"
}

cleanup() {
  kubectl get node -owide
  echo "gc the aks cluster"
  az group delete --resource-group "${RESOURCE_GROUP}" -y --no-wait
}
trap cleanup EXIT

export AZURE_LOCATION="$(get_random_location)"
if [[ "${CLUSTER_TYPE}" =~ "autoscaling" ]]; then
  export AZURE_LOCATION="australiaeast"
fi

echo "Setting up customconfiguration.json"
IMAGE_TAG="$(git rev-parse --short=7 HEAD)"
CUSTOM_CONFIG_PATH=".pipelines/templates/customconfiguration.json"
CCM_NAME="${IMAGE_REGISTRY}/azure-cloud-controller-manager:${IMAGE_TAG}"
CNM_NAME="${IMAGE_REGISTRY}/azure-cloud-node-manager:${IMAGE_TAG}-linux-amd64"
sed -i "s|CUSTOM_CCM_IMAGE|${CCM_NAME}|g" "${CUSTOM_CONFIG_PATH}"
sed -i "s|CUSTOM_CNM_IMAGE|${CNM_NAME}|g" "${CUSTOM_CONFIG_PATH}"
export CUSTOM_CONFIG="$(base64 -w 0 ${CUSTOM_CONFIG_PATH})"
export TOKEN=$(az account get-access-token -o json | jq -r '.accessToken')

CLUSTER_CONFIG_PATH="${REPO_ROOT}/.pipelines/templates/basic-lb.json"
if [[ "${CLUSTER_TYPE}" == "autoscaling" ]]; then
  CLUSTER_CONFIG_PATH="${REPO_ROOT}/.pipelines/templates/autoscaling.json"
  export AZURE_LOADBALANCER_SKU=standard
elif [[ "${CLUSTER_TYPE}" == "autoscaling-multipool" ]]; then
  CLUSTER_CONFIG_PATH="${REPO_ROOT}/.pipelines/templates/autoscaling-multipool.json"
  export AZURE_LOADBALANCER_SKU=standard
fi

CLUSTER_CONFIG_KEYS=("AKS_CLUSTER_ID" "CLUSTER_NAME" "AZURE_LOCATION" "KUBERNETES_VERSION" "AZURE_CLIENT_ID" "AZURE_CLIENT_SECRET" "CUSTOM_CONFIG")
CLUSTER_CONFIG_VALS=("${AKS_CLUSTER_ID}" "${CLUSTER_NAME}" "${AZURE_LOCATION}" "${KUBERNETES_VERSION}" "${AZURE_CLIENT_ID}" "${AZURE_CLIENT_SECRET}" "${CUSTOM_CONFIG}")
for i in "${!CLUSTER_CONFIG_KEYS[@]}";do
  sed -i "s|${CLUSTER_CONFIG_KEYS[$i]}|${CLUSTER_CONFIG_VALS[$i]}|g" "${CLUSTER_CONFIG_PATH}"
done

echo "Creating an AKS cluster in resource group ${RESOURCE_GROUP}"
CREATION_DATE="$(date +%s)"
az group create --name "${RESOURCE_GROUP}" -l "${AZURE_LOCATION}" --tags "creation_date=${CREATION_DATE}" "usage=aks-cluster-e2e"
curl -i -X PUT -k -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    -H "AKSHTTPCustomFeatures: Microsoft.ContainerService/EnableCloudControllerManager" \
    "https://management.azure.com${AKS_CLUSTER_ID}?api-version=2022-04-02-preview" \
    -d "$(cat ${CLUSTER_CONFIG_PATH})"

echo ""
echo "Waiting until cluster creation finishes"
az aks wait -g "${RESOURCE_GROUP}" -n "${CLUSTER_NAME}" --created --interval 60 --timeout 1800
az aks get-credentials --resource-group "${RESOURCE_GROUP}" --name "${CLUSTER_NAME}" -f "${REPO_ROOT}/aks-cluster.kubeconfig" --overwrite-existing
export KUBECONFIG="${REPO_ROOT}/aks-cluster.kubeconfig"
kubectl wait --for=condition=Ready node --all --timeout=5m
kubectl get node -owide

echo "Running e2e"

# TODO: We should do it in autoscaling-multipool.json
if [[ "${CLUSTER_TYPE}" == "autoscaling-multipool" ]]; then
  az aks update --resource-group "${RESOURCE_GROUP}" --name "${CLUSTER_NAME}" --cluster-autoscaler-profile balance-similar-node-groups=true
fi

export E2E_ON_AKS_CLUSTER=true
if [[ "${CLUSTER_TYPE}" =~ "autoscaling" ]]; then
  export LABEL_FILTER="Feature:Autoscaling"
fi
make test-ccm-e2e
