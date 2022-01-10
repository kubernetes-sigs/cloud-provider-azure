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
set -u

WORKING_DIR=$(dirname "${BASH_SOURCE[0]}")

while [ -n "${1-}" ]
do  
  case "$1" in   
    --kubemark-rg)  
        if [ -n "$2" ]; then
            KUBEMARK_CLUSTER_RESOURCE_GROUP="$2"
            shift
        fi
        ;;
    --external-rg)  
        if [ -n "$2" ]; then
            EXTERNAL_CLUSTER_RESOURCE_GROUP="$2"
            shift
        fi
        ;;
    --location)  
        if [ -n "$2" ]; then
            LOCATION="$2"
            shift
        fi
        ;;
    --kubemark-size)
        if [ -n "$2" ]; then
            KUBEMARK_SIZE="$2"
            shift
        fi
        ;;
    --kubemark-cluster-template-url)
        if [ -n "$2" ]; then
            KUBEMARK_CLUSTER_TEMPLATE_URL="$2"
            shift
        fi
        ;;
    --external-cluster-template-url)
        if [ -n "$2" ]; then
            EXTERNAL_CLUSTER_TEMPLATE_URL="$2"
            shift
        fi
        ;;
    --hollow-nodes-deployment-url)
        if [ -n "$2" ]; then
            HOLLOW_NODES_DEPLOYMENT_URL="$2"
            shift
        fi
        ;;
    --clusterloader2-bin-url)
        if [ -n "$2" ]; then
            CLUSTERLOADER2_BIN_URL="$2"
            shift
        fi
        ;;
    *)  
        echo "$1 is not a supported option"
        exit 99
        ;;  
  esac  
  shift  
done

psd="/proc/sys/kernel/random/uuid"
uuid=$(cat $psd)
prefix=${uuid:0:4}

KUBEMARK_CLUSTER_RESOURCE_GROUP="${KUBEMARK_CLUSTER_RESOURCE_GROUP:-kubemark-cluster-$prefix}"
EXTERNAL_CLUSTER_RESOURCE_GROUP="${EXTERNAL_CLUSTER_RESOURCE_GROUP:-kubemark-external-cluster-$prefix}"

echo "kubemark rg is: ${KUBEMARK_CLUSTER_RESOURCE_GROUP}, external rg is: ${KUBEMARK_CLUSTER_RESOURCE_GROUP}"

LOCATION="${LOCATION:-southcentralus}"

KUBEMARK_SIZE="${KUBEMARK_SIZE:-100}"

echo "generating ssh key pair"
ssh-keygen -t rsa -n '' -f "${WORKING_DIR}"/id_rsa -P "" > /dev/null
PRIVATE_KEY="${PRIVATE_KEY:-${WORKING_DIR}/id_rsa}"
PUBLIC_KEY="${PUBLIC_KEY:-${WORKING_DIR}/id_rsa.pub}"

# install azure cli
if ! command -v az > /dev/null; then
    echo "installing azure cli"

    echo "getting packages needed for the install process"
    apt update >> /dev/null
    apt install -y ca-certificates curl apt-transport-https lsb-release gnupg >> /dev/null

    echo "downloading and installing the Microsoft signing key"
    curl -sL https://packages.microsoft.com/keys/microsoft.asc | gpg --dearmor | tee /etc/apt/trusted.gpg.d/microsoft.asc.gpg > /dev/null
    AZ_REPO=$(lsb_release -cs)
    echo "deb [arch=amd64] https://packages.microsoft.com/repos/azure-cli/ $AZ_REPO main" | tee /etc/apt/sources.list.d/azure-cli.list

    echo "updating repository information and install the azure-cli package"
    apt update >> /dev/null
    apt install azure-cli
fi

# read azure credentials
echo "reading azure credentials from environment variables"
ClientID="${AZURE_CLIENT_ID}"
ClientSecret="${AZURE_CLIENT_SECRET}"
TenantID="${AZURE_TENANT_ID}"

echo "logging in to azure"
az login --service-principal --username "${ClientID}" --password "${ClientSecret}" --tenant "${TenantID}" > /dev/null

function create_resource_group {
    az group create -n "$1" -l "${LOCATION}" --tags "autostop=no"
}

function cleanup {
    echo "cleaning up resource groups..."

    az group delete -n "${KUBEMARK_CLUSTER_RESOURCE_GROUP}" -y --no-wait
    az group delete -n "${EXTERNAL_CLUSTER_RESOURCE_GROUP}" -y --no-wait
}

trap cleanup ERR EXIT

function get_master_ip {
    KUBEMARK_MASTER_IP=$(az network public-ip list -g "$1" | jq -r '.[0].ipAddress')
    echo "got kubemark master IP: ${KUBEMARK_MASTER_IP}"
}

function build_kubemark_cluster {
    echo "generating kubemark cluster manifests to ${WORKING_DIR}"
    "${AKS_ENGINE}" generate "$1"

    echo "deploying kubemark cluster"
    "${AKS_ENGINE}" deploy --api-model "$1" \
      --location "${LOCATION}" \
      --client-id "${ClientID}" \
      --client-secret "${ClientSecret}" \
      --resource-group "${KUBEMARK_CLUSTER_RESOURCE_GROUP}" \
      --force-overwrite

    get_master_ip "${KUBEMARK_CLUSTER_RESOURCE_GROUP}"

    echo "copying etcd key"
    scp -o 'StrictHostKeyChecking=no' -o 'ConnectionAttempts=10' -i "${PRIVATE_KEY}" "${WORKING_DIR}/_output/${KUBEMARK_CLUSTER_DNS_PREFIX}/etcdclient.crt" \
      "${WORKING_DIR}/_output/${KUBEMARK_CLUSTER_DNS_PREFIX}/etcdclient.key" kubernetes@"${KUBEMARK_MASTER_IP}":~/
}

function build_external_cluster {
    echo "generating external cluster manifests to ${WORKING_DIR}"
    "${AKS_ENGINE}" generate "$1"

    echo "deploying external cluster"
    "${AKS_ENGINE}" deploy --api-model "$1" \
      --location "${LOCATION}" \
      --client-id "${ClientID}" \
      --client-secret "${ClientSecret}" \
      --resource-group "${EXTERNAL_CLUSTER_RESOURCE_GROUP}" \
      --force-overwrite
    
    echo "building external cluster"
    export KUBECONFIG="${WORKING_DIR}/_output/${EXTERNAL_CLUSTER_DNS_PREFIX}/kubeconfig/kubeconfig.${LOCATION}.json"
    kubectl create namespace "kubemark"
    kubectl create configmap node-configmap -n "kubemark" --from-literal=content.type="test-cluster"
    kubectl create secret generic kubeconfig \
      --type=Opaque \
      --namespace="kubemark" \
      --from-file="kubelet.kubeconfig=${WORKING_DIR}/_output/${KUBEMARK_CLUSTER_DNS_PREFIX}/kubeconfig/kubeconfig.${LOCATION}.json" \
      --from-file="kubeproxy.kubeconfig=${WORKING_DIR}/_output/${KUBEMARK_CLUSTER_DNS_PREFIX}/kubeconfig/kubeconfig.${LOCATION}.json"
}

echo "creating resource groups for kubemark and external clusters"
create_resource_group "${KUBEMARK_CLUSTER_RESOURCE_GROUP}" &
create_resource_group "${EXTERNAL_CLUSTER_RESOURCE_GROUP}" &
wait

echo "replacing deploying templates"
curl -o "kubemark-cluster.json" "${KUBEMARK_CLUSTER_TEMPLATE_URL}"
curl -o "external-cluster.json" "${EXTERNAL_CLUSTER_TEMPLATE_URL}"
curl -o "hollow-node.yaml" "${HOLLOW_NODES_DEPLOYMENT_URL}"

KUBEMARK_CLUSTER_DNS_PREFIX="${KUBEMARK_CLUSTER_DNS_PREFIX:-kubemark-$prefix}"
EXTERNAL_CLUSTER_DNS_PREFIX="${EXTERNAL_CLUSTER_DNS_PREFIX:-kubemark-external-$prefix}"

sed -i "s/{{DNS_PREFIX}}/$KUBEMARK_CLUSTER_DNS_PREFIX/" "${WORKING_DIR}/kubemark-cluster.json"
sed -i "s:{{SSH_PUBLIC_KEY}}:$(cat $PUBLIC_KEY):" "${WORKING_DIR}/kubemark-cluster.json"
sed -i "s/{{AZURE_CLIENT_ID}}/$ClientID/" "${WORKING_DIR}/kubemark-cluster.json"
sed -i "s/{{AZURE_CLIENT_SECRET}}/$ClientSecret/" "${WORKING_DIR}/kubemark-cluster.json"

sed -i "s/{{DNS_PREFIX}}/$EXTERNAL_CLUSTER_DNS_PREFIX/" "${WORKING_DIR}/external-cluster.json"
sed -i "s:{{SSH_PUBLIC_KEY}}:$(cat $PUBLIC_KEY):" "${WORKING_DIR}/external-cluster.json"
sed -i "s/{{AZURE_CLIENT_ID}}/$ClientID/" "${WORKING_DIR}/external-cluster.json"
sed -i "s/{{AZURE_CLIENT_SECRET}}/$ClientSecret/" "${WORKING_DIR}/external-cluster.json"

sed -i "s/{{numreplicas}}/$KUBEMARK_SIZE/" "${WORKING_DIR}/hollow-node.yaml"
sed -i "s/{{kubemark_image_registry}}/ss104301/g" "${WORKING_DIR}/hollow-node.yaml"
sed -i "s/{{kubemark_image_tag}}/latest/g" "${WORKING_DIR}/hollow-node.yaml"

if ! command -v aks-engine > /dev/null; then
    echo "getting aks-engine"
    curl -o get-akse.sh https://raw.githubusercontent.com/Azure/aks-engine/master/scripts/get-akse.sh
    chmod 700 get-akse.sh
    DESIRED_VERSION=v0.66.1 ./get-akse.sh
fi
AKS_ENGINE="aks-engine"
"${AKS_ENGINE}" version

build_kubemark_cluster "${WORKING_DIR}/kubemark-cluster.json"
build_external_cluster "${WORKING_DIR}/external-cluster.json"

echo "deploying hollow nodes"
export KUBECONFIG="${WORKING_DIR}/_output/${EXTERNAL_CLUSTER_DNS_PREFIX}/kubeconfig/kubeconfig.${LOCATION}.json"

kubectl apply -f "${WORKING_DIR}/hollow-node.yaml"
sleep 30

export KUBECONFIG="${WORKING_DIR}/_output/${KUBEMARK_CLUSTER_DNS_PREFIX}/kubeconfig/kubeconfig.${LOCATION}.json"

echo "waiting ${KUBEMARK_SIZE} hollow nodes to be ready"
total_retry=0
while : 
do
    total_retry=$(( $total_retry + 1 ))
    none_count=$(kubectl get no | awk '{print $3}' | grep -c "<none>" || true)
    node_count=$(kubectl get no | grep "hollow" | awk '{print $2}' | grep -c "^Ready$" || true)
    if [ "${node_count}" -eq "${KUBEMARK_SIZE}" ] && [ "${none_count}" -eq 0 ]; then
        break
    else 
        echo "there're ${node_count} ready hollow nodes, ${none_count} <none> nodes, will retry after 10 seconds"
        sleep 10
    fi

    if [ "${total_retry}" -eq 100 ]; then
        echo "maximum retry times reached"
        exit 100
    fi
done

echo "all hollow nodes are ready, starting test with clusterloader2"

export KUBE_CONFIG="${WORKING_DIR}/_output/${KUBEMARK_CLUSTER_DNS_PREFIX}/kubeconfig/kubeconfig.${LOCATION}.json"

# Test by clusterloader2
PROVIDER="kubemark"

# SSH config for metrics' collection
export KUBE_SSH_KEY_PATH="${PRIVATE_KEY}"
export KUBE_SSH_USER="kubernetes"
MASTER_SSH_IP="${KUBEMARK_MASTER_IP}"

MASTER_NAME="$(kubectl get no | grep "k8s-master" | awk '{print $1}')"

# etcd https params
export ETCD_CERTIFICATE=/home/kubernetes/etcdclient.crt
export ETCD_KEY=/home/kubernetes/etcdclient.key

# apiserver
export GET_APISERVER_PPROF_BY_K8S_CLIENT=true

echo "fetching all test configs"
git clone https://github.com/kubernetes-sigs/cloud-provider-azure.git
cp -r cloud-provider-azure/tests/kubemark/configs "${WORKING_DIR}"

echo "configuring clusterloader2"
# Clusterloader2 testing strategy config paths
# It supports setting up multiple test strategy. Each testing strategy is individual and serial.
TEST_CONFIG="${TEST_CONFIG:-${WORKING_DIR}/configs/density/config.yaml}"
# TEST_CONFIG="${TEST_CONFIG:-${WORKING_DIR}/configs/load/config.yaml"

# Clusterloader2 testing override config paths
# It supports setting up multiple override config files. All of override config files will be applied to each testing strategy.
# OVERRIDE_CONFIG='${WORKING_DIR}/configs/density/override/200-nodes.yaml'

# Log config
REPORT_DIR="/logs/artifacts"
LOG_FILE="/logs/artifacts/cl2-test.log"
if [ ! -d "${REPORT_DIR}" ]; then
    mkdir -p "${REPORT_DIR}"
    touch "${LOG_FILE}"
    echo "report directory created"
fi

echo "downloading clusterloader2"
curl -L "${CLUSTERLOADER2_BIN_URL}" -o clusterloader2
CLUSTERLOADER2="${WORKING_DIR}/clusterloader2"
chmod +x "${CLUSTERLOADER2}"

echo "testing ${TEST_CONFIG} by clusterloader2"
${CLUSTERLOADER2} \
    --kubeconfig="${KUBE_CONFIG}" \
    --kubemark-root-kubeconfig="${KUBE_CONFIG}" \
    --provider="${PROVIDER}" \
    --masterip="${MASTER_SSH_IP}" \
    --master-internal-ip="10.240.255.5" \
    --mastername="${MASTER_NAME}" \
    --testconfig="${TEST_CONFIG}" \
    --report-dir="${REPORT_DIR}" \
    --alsologtostderr 2>&1 | tee "${LOG_FILE}"
