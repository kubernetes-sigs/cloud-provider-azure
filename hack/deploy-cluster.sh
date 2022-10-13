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

set -e

REPO_ROOT=$(dirname "${BASH_SOURCE}")/..

# Verify the required Environment Variables are present.
: "${AZURE_SUBSCRIPTION_ID:?Environment variable empty or not defined.}"
: "${AZURE_TENANT_ID:?Environment variable empty or not defined.}"
: "${AZURE_CLIENT_ID:?Environment variable empty or not defined.}"
: "${AZURE_CLIENT_SECRET:?Environment variable empty or not defined.}"
: "${USE_CSI_DEFAULT_STORAGECLASS:?Environment variable empty or not defined.}"
: "${K8S_RELEASE_VERSION:?Environment variable empty or not defined.}"
: "${AZURE_LOCATION:?Environment variable empty or not defined.}"
: "${CCM_IMAGE:?Environment variable empty or not defined.}"
: "${CNM_IMAGE:?Environment variable empty or not defined.}"
: "${RESOURCE_GROUP_NAME:?Environment variable empty or not defined.}"

# Check for commands which would be used in following steps.
if ! [ -x "$(command -v jq)" ]; then
  echo 'Error: jq is not installed. Please follow https://stedolan.github.io/jq/ to install it.'
  exit 1
fi
if ! [ -x "$(command -v aks-engine)" ]; then
  echo 'Error: aks-engine is not installed. Please follow https://github.com/Azure/aks-engine to install it.'
  exit 1
fi

# Initialize variables
manifest_file=$(mktemp)
if [ -z "${RESOURCE_GROUP_NAME}" ]; then
  UUID=$(cat /dev/urandom | tr -dc 'a-zA-Z0-9' | fold -w 8 | head -n 1)
  RESOURCE_GROUP_NAME="k8s-$UUID"
fi

# Add handler for cleanup
function cleanup() {
  rm -f ${manifest_file}
}
trap cleanup EXIT

base_manifest=${REPO_ROOT}/examples/aks-engine.json
if [ ! -z "${ENABLE_AVAILABILITY_ZONE:-}" ]; then
    base_manifest=${REPO_ROOT}/examples/az.json
fi

# Configure the manifests for aks-engine
cat ${base_manifest} | \
  jq ".properties.orchestratorProfile.orchestratorRelease=\"${K8S_RELEASE_VERSION}\"" | \
  jq ".properties.orchestratorProfile.kubernetesConfig.customCcmImage=\"${CCM_IMAGE}\"" | \
  jq ".properties.orchestratorProfile.kubernetesConfig.addons[0].containers[0].image=\"${CNM_IMAGE}\"" | \
  jq ".properties.servicePrincipalProfile.clientID=\"${AZURE_CLIENT_ID}\"" | \
  jq ".properties.servicePrincipalProfile.secret=\"${AZURE_CLIENT_SECRET}\"" \
  > ${manifest_file}

# Deploy the cluster
echo "Deploying kubernetes cluster to resource group ${RESOURCE_GROUP_NAME}..."
aks-engine deploy --subscription-id ${AZURE_SUBSCRIPTION_ID} \
  --auth-method client_secret \
  --auto-suffix \
  --resource-group ${RESOURCE_GROUP_NAME} \
  --location ${AZURE_LOCATION} \
  --api-model ${manifest_file} \
  --client-id ${AZURE_CLIENT_ID} \
  --client-secret ${AZURE_CLIENT_SECRET}
echo "Kubernetes cluster deployed. Please find the kubeconfig for it in _output/"

export KUBECONFIG=_output/$(ls -t _output | head -n 1)/kubeconfig/kubeconfig.${AZURE_LOCATION}.json
echo "Kubernetes cluster deployed. Please find the kubeconfig at ${KUBECONFIG}"

# Deploy AzureDisk CSI Plugin
curl -skSL https://raw.githubusercontent.com/kubernetes-sigs/azuredisk-csi-driver/master/deploy/install-driver.sh | bash -s master --
# create storage class.
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/azuredisk-csi-driver/master/deploy/example/storageclass-azuredisk-csi.yaml

# Deploy AzureFile CSI Plugin
curl -skSL https://raw.githubusercontent.com/kubernetes-sigs/azurefile-csi-driver/master/deploy/install-driver.sh | bash -s master --
# create storage class.
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/azurefile-csi-driver/master/deploy/example/storageclass-azurefile-csi.yaml

if [ ${USE_CSI_DEFAULT_STORAGECLASS} = "true" ]
then
kubectl delete storageclass default
cat <<EOF | kubectl apply -f-
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  annotations:
    storageclass.beta.kubernetes.io/is-default-class: "true"
  name: default
provisioner: disk.csi.azure.com
parameters:
  skuname: Standard_LRS # available values: Standard_LRS, Premium_LRS, StandardSSD_LRS and UltraSSD_LRS
  kind: managed         # value "dedicated", "shared" are deprecated since it's using unmanaged disk
  cachingMode: ReadOnly
reclaimPolicy: Delete
volumeBindingMode: Immediate
EOF
fi
