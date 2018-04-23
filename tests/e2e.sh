#!/bin/bash

set -e

[ -z "$K8S_AZURE_TEST_GOPATH" ] && { echo "K8S_AZURE_TEST_GOPATH not set"; exit 1; }
export GOPATH=$K8S_AZURE_TEST_GOPATH


# E2E base
[ -z "$K8S_AZURE_E2E_K8S_VERSION" ] && { echo "K8S_AZURE_E2E_K8S_VERSION not set"; exit 1; }
export K8S_ROOT=$GOPATH/src/k8s.io/kubernetes
mkdir -p $K8S_ROOT
pushd $K8S_ROOT
if [ -d .git ]
then
    git fetch --all -p
else
    git clone https://github.com/kubernetes/kubernetes $K8S_ROOT
fi
git checkout $K8S_AZURE_E2E_K8S_VERSION
echo "On branch $K8S_AZURE_E2E_K8S_VERSION"
popd

# acs-engine
[ -z "$K8S_AZURE_ACSENGINE_VERSION" ] && { echo "K8S_AZURE_ACSENGINE_VERSION not set"; exit 1; }
export ACSENGINE_ROOT=$GOPATH/src/github.com/Azure/acs-engine
mkdir -p $ACSENGINE_ROOT
pushd $ACSENGINE_ROOT
if [ -d .git ]
then
    git fetch --all -p
else
    git clone https://github.com/Azure/acs-engine $ACSENGINE_ROOT
fi
git checkout $K8S_AZURE_ACSENGINE_VERSION
make
export PATH=$ACSENGINE_ROOT/bin:$PATH
popd


export IMAGE_TAG=$(scripts/image-tag.sh)
[ -z "$K8S_AZURE_IMAGE_REPOSITORY" ] && { echo "K8S_AZURE_IMAGE_REPOSITORY not set"; exit 1; }
make clean image
docker push $IMAGE_TAG


# run
export ARTIFACTS_DIR='_artifacts.kai'
rm -rf $ARTIFACTS_DIR *.xml
export K8S_AZURE_workspace=$ARTIFACTS_DIR
export K8S_AZURE_global_skip_files="$(pwd)/.jenkins/skip.txt"
tests/k8s-azure/k8s-azure e2e -v -caccm_image=$IMAGE_TAG 
# -ctype=default \    -cskipdeploy=1 -cbuild_e2e_test=1 -cname=kai-at11-e58e50a-default

echo Done
