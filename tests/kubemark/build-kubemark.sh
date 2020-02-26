#! /bin/bash

set -e
set -u
set -x

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

LOCATION="${LOCATION:-southcentralus}"

KUBEMARK_SIZE="${KUBEMARK_SIZE:-100}"

PRIVATE_KEY="${PRIVATE_KEY:-${HOME}/.ssh/id_rsa}"
PUBLIC_KEY="${PRIVATE_KEY:-${HOME}/.ssh/id_rsa_pub}"

# install azure cli
curl -sL https://aka.ms/InstallAzureCLIDeb | bash

# read azure credentials
ClientID=$(jq '.ClientID' "${AZURE_CREDENTIALS}" | sed 's/"//g')
ClientSecret=$(jq '.ClientSecret' "${AZURE_CREDENTIALS}" | sed 's/"//g')
TenantID=$(jq '.TenantID' "${AZURE_CREDENTIALS}" | sed 's/"//g')

echo "got azure credentials: ClientID ($ClientID), ClientSecret ($ClientSecret), TenantID ($TenantID)"

az login --service-principal --username "${ClientID}" --password "${ClientSecret}" --tenant "${TenantID}"

AKS_ENGINE="${WORKING_DIR}/aks-engine"

function ssh_and_do {
    if [ -f "$2" ]; then
        ssh -o StrictHostKeyChecking=no -i "${PRIVATE_KEY}" kubernetes@"$1" < "$2"
    else
        ssh -o StrictHostKeyChecking=no -i "${PRIVATE_KEY}" kubernetes@"$1" "$2"
    fi
}

function create_resource_group {
    az group create -n "$1" -l "${LOCATION}" --tags "autostop=no"
}

function cleanup {
    echo "cleaning up resource groups..."

    az group delete -n "${KUBEMARK_CLUSTER_RESOURCE_GROUP}" -y --no-wait
    az group delete -n "${EXTERNAL_CLUSTER_RESOURCE_GROUP}" -y --no-wait

    az logout
}

trap cleanup ERR EXIT

function get_master_ip {
    KUBEMARK_MASTER_IP=$(az network public-ip list -g "$1" | jq '.[0].ipAddress' | sed 's/"//g')
}

function build_kubemark_cluster {
    ${AKS_ENGINE} generate "$1"

    KUBEMARK_CLUSTER_DNS_PREFIX=$(jq '.properties.masterProfile.dnsPrefix' "$1" | sed 's/"//g')
    az group deployment create \
      -g "${KUBEMARK_CLUSTER_RESOURCE_GROUP}" \
      --template-file "${WORKING_DIR}/_output/${KUBEMARK_CLUSTER_DNS_PREFIX}/azuredeploy.json" \
      --parameters "${WORKING_DIR}/_output/${KUBEMARK_CLUSTER_DNS_PREFIX}/azuredeploy.parameters.json"

    curl -o "${WORKING_DIR}/build-kubemark-master" "https://raw.githubusercontent.com/kubernetes-sigs/cloud-provider-azure/master/tests/kubemark/build-kubemark-master.sh"

    get_master_ip "${KUBEMARK_CLUSTER_RESOURCE_GROUP}"
    ssh_and_do "${KUBEMARK_MASTER_IP}" "${WORKING_DIR}/build-kubemark-master.sh"

    scp -i "${PRIVATE_KEY}" "${WORKING_DIR}/_output/${KUBEMARK_CLUSTER_DNS_PREFIX}/etcdclient.crt" \
      "${WORKING_DIR}/_output/${KUBEMARK_CLUSTER_DNS_PREFIX}/etcdclient.key" kubernetes@"${KUBEMARK_MASTER_IP}":~/
}

function build_external_cluster {
    ${AKS_ENGINE} generate "$1"

    EXTERNAL_CLUSTER_DNS_PREFIX=$(jq '.properties.masterProfile.dnsPrefix' "$1" | sed 's/"//g')
    az group deployment create \
      -g "${EXTERNAL_CLUSTER_RESOURCE_GROUP}" \
      --template-file "${WORKING_DIR}/_output/${EXTERNAL_CLUSTER_DNS_PREFIX}/azuredeploy.json" \
      --parameters "${WORKING_DIR}/_output/${EXTERNAL_CLUSTER_DNS_PREFIX}/azuredeploy.parameters.json"
    
    export KUBECONFIG="${WORKING_DIR}/_output/${EXTERNAL_CLUSTER_DNS_PREFIX}/kubeconfig/kubeconfig.${LOCATION}.json"
    kubectl create namespace "kubemark"
    kubectl create configmap node-configmap -n "kubemark" --from-literal=content.type="test-cluster"
    kubectl create secret generic kubeconfig \
      --type=Opaque \
      --namespace="kubemark" \
      --from-file="kubelet.kubeconfig=${WORKING_DIR}/_output/${KUBEMARK_CLUSTER_DNS_PREFIX}/kubeconfig/kubeconfig.${LOCATION}.json" \
      --from-file="kubeproxy.kubeconfig=${WORKING_DIR}/_output/${KUBEMARK_CLUSTER_DNS_PREFIX}/kubeconfig/kubeconfig.${LOCATION}.json"
}

create_resource_group "${KUBEMARK_CLUSTER_RESOURCE_GROUP}" &
create_resource_group "${EXTERNAL_CLUSTER_RESOURCE_GROUP}" &
wait

curl -o "kubemark-cluster.json" "${KUBEMARK_CLUSTER_TEMPLATE_URL}"
curl -o "external-cluster.json" "${EXTERNAL_CLUSTER_TEMPLATE_URL}"
curl -o "hollow-node.yaml" "${HOLLOW_NODES_DEPLOYMENT_URL}"

sed -i "s/{{DNS_PREFIX}}/$KUBEMARK_CLUSTER_DNS_PREFIX/" "${WORKING_DIR}/kubemark-cluster.json"
sed -i "s/{{SSH_PUBLIC_KEY}}/$PUBLIC_KEY/" "${WORKING_DIR}/kubemark-cluster.json"
sed -i "s/{{AZURE_CLIENT_ID}}/$ClientID/" "${WORKING_DIR}/kubemark-cluster.json"
sed -i "s/{{AZURE_CLIENT_SECRET}}/$ClientSecret/" "${WORKING_DIR}/kubemark-cluster.json"

sed -i "s/{{DNS_PREFIX}}/$EXTERNAL_CLUSTER_DNS_PREFIX/" "${WORKING_DIR}/external-cluster.json"
sed -i "s/{{SSH_PUBLIC_KEY}}/$PUBLIC_KEY/" "${WORKING_DIR}/external-cluster.json"
sed -i "s/{{AZURE_CLIENT_ID}}/$ClientID/" "${WORKING_DIR}/external-cluster.json"
sed -i "s/{{AZURE_CLIENT_SECRET}}/$ClientSecret/" "${WORKING_DIR}/external-cluster.json"

sed -i "s/{{numreplicas}}/$KUBEMARK_SIZE/" "${WORKING_DIR}/hollow-node.yaml"
sed -i "s/{{kubemark_image_registry}}/ss104301/g" "${WORKING_DIR}/hollow-node.yaml"
sed -i "s/{{kubemark_image_tag}}/latest/g" "${WORKING_DIR}/hollow-node.yaml"

build_kubemark_cluster "${WORKING_DIR}/kubemark-cluster.json"
build_external_cluster "${WORKING_DIR}/external-cluster.json"

export KUBECONFIG="${WORKING_DIR}/_output/${EXTERNAL_CLUSTER_DNS_PREFIX}/kubeconfig/kubeconfig.${LOCATION}.json"

kubectl apply -f "${WORKING_DIR}/hollow-node.yaml"
sleep 30

export KUBECONFIG="${WORKING_DIR}/_output/${KUBEMARK_CLUSTER_DNS_PREFIX}/kubeconfig/kubeconfig.${LOCATION}.json"

while : 
do
    none_count=$(kubectl get no | awk '{print $3}' | grep "<none>" | wc -l)
    node_count=$(kubectl get no | grep "hollow" | wc -l)
    if [ "${node_count}" -eq "${KUBEMARK_SIZE}" ] && [ "${none_count}" -eq 0  ]; then
        break
    else 
        sleep 10
    fi
done

export KUBE_CONFIG="${WORKING_DIR}/_output/${KUBEMARK_CLUSTER_DNS_PREFIX}/kubeconfig/kubeconfig.${LOCATION}.json"

# Test with clusterloader2
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

git clone https://github.com/kubernetes-sigs/cloud-provider-azure.git
cp -r cloud-provider-azure/tests/kubemark/configs "${WORKING_DIR}"

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

curl -o clusterloader2 "${CLUSTERLOADER2_BIN_URL}"
CLUSTERLOADER2="${WORKING_DIR}/clusterloader2"

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
