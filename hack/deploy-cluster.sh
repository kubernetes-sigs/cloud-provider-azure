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
RESOURCE_GROUP_NAME=${RESOURCE_GROUP_NAME:-""}
LOCATION=${LOCATION:-""}
SUBSCRIPTION_ID=${SUBSCRIPTION_ID:-""}
CLIENT_ID=${CLIENT_ID:-""}
CLIENT_SECRET=${CLIENT_SECRET:-""}
TENANT_ID=${TENANT_ID:-""}

# check for variables is initialized or not
if [ -z "$LOCATION" ] || [ -z "${SUBSCRIPTION_ID}" ] || [ -z "${CLIENT_ID}" ] || [ -z "${CLIENT_SECRET}" ] || [ -z "${TENANT_ID}" ]; then
  echo "SUBSCRIPTION_ID, CLIENT_ID, TENANT_ID, CLIENT_SECRET and LOCATION must be specified"
  exit 1
fi
if [ -z "$IMAGE" ] || [ -z "${HYPERKUBE_IMAGE}" ]; then
  echo "Please deploy the cluster by running 'IMAGE_REGISTRY=<your-registry> make deploy'."
  exit 1
fi

# check for commands which would be used in following steps.
if ! [ -x "$(command -v jq)" ]; then
  echo 'Error: jq is not installed. Please follow https://stedolan.github.io/jq/ to install it.'
  exit 1
fi
if ! [ -x "$(command -v aks-engine)" ]; then
  echo 'Error: aks-engine is not installed. Please follow https://github.com/Azure/aks-engine to install it.'
  exit 1
fi

# initialize variables
manifest_file=$(mktemp)
if [ -z "$RESOURCE_GROUP_NAME" ]; then
  UUID=$(cat /dev/urandom | tr -dc 'a-zA-Z0-9' | fold -w 8 | head -n 1)
  RESOURCE_GROUP_NAME="k8s-$UUID"
fi

# Add handler for cleanup
function cleanup() {
  rm -f ${manifest_file}
}
trap cleanup EXIT

# Configure the manifests for aks-engine
cat ${REPO_ROOT}/examples/aks-engine.json | \
  jq ".properties.orchestratorProfile.kubernetesConfig.customCcmImage=\"${IMAGE}\"" | \
  jq ".properties.orchestratorProfile.kubernetesConfig.customHyperkubeImage=\"${HYPERKUBE_IMAGE}\"" | \
  jq ".properties.servicePrincipalProfile.clientID=\"${CLIENT_ID}\"" | \
  jq ".properties.servicePrincipalProfile.secret=\"${CLIENT_SECRET}\"" \
  > ${manifest_file}

# Deploy the cluster
echo "Deploying kubernetes cluster to resource group ${RESOURCE_GROUP_NAME}..."
aks-engine deploy --subscription-id ${SUBSCRIPTION_ID} \
  --auth-method client_secret \
  --auto-suffix \
  --resource-group ${RESOURCE_GROUP_NAME} \
  --location ${LOCATION} \
  --api-model ${manifest_file} \
  --client-id ${CLIENT_ID} \
  --client-secret ${CLIENT_SECRET}
echo "Kubernetes cluster deployed. Please find the kubeconfig for it in _output/"

