#!/bin/bash

# Copyright 2018 The Kubernetes Authors.
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
cd $(dirname "$BASH_SOURCE")/..
export GLIDE_HOME=$(pwd)/.glide

function update-cloudprovider {
    AZURE_PROVIDER_DIR=pkg/azureprovider
    rm -rf $AZURE_PROVIDER_DIR
    mkdir -p $AZURE_PROVIDER_DIR
    cp -aT vendor/k8s.io/kubernetes/pkg/cloudprovider/providers/azure $AZURE_PROVIDER_DIR
    find $AZURE_PROVIDER_DIR \( -name BUILD -o -name OWNERS \) -exec rm {} +
    find $AZURE_PROVIDER_DIR -name '*.go' -exec perl -i -pe \
        's/^package azure\K$/provider/;' {} \;
    
    pushd $AZURE_PROVIDER_DIR
    for file in azure.go azure_test.go
    do
        perl -i -pe \
            's#k8s.io/kubernetes/pkg/cloudprovider/providers/azure#k8s.io/cloud-provider-azure/cloud-controller-manager/azureprovider#;' $file
        gofmt -s $file > bak
        mv bak $file
    done
    popd
}

function glide-update-staging-mirror {
    K8S_STAGING=$GLIDE_HOME/k8s_staging/k8s.io
    # remove local glide cache for file repo, since we're to setup new git repo
    rm -rf $GLIDE_HOME/cache/{src,info}/file*
    rm -rf $K8S_STAGING
    mkdir -p $K8S_STAGING
    cp -aT vendor/k8s.io/kubernetes/staging/src/k8s.io $K8S_STAGING

    find $K8S_STAGING -name Godeps.json -exec perl -i -pe 's/xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx//' {} \;

    for dir in $(ls -d $K8S_STAGING/*); do
        glide mirror set https://k8s.io/$(basename $dir) file://$dir --vcs git
        pushd $dir
        git init -q && git add -A . && git commit -q -m 'staging commit'
        popd
    done
}

# Install dependencies, first round
glide update --no-recursive
# Uncomment when moving cloud provider code
# update-cloudprovider
glide-update-staging-mirror

# A first round, update install dependencies
glide update
# A second round, this minimizes package in 'glide.lock'
glide update --strip-vendor
# Clean up, turn on use-lock-file, otherwise test dependencies are removed
glide-vc --use-lock-file --only-code --no-tests > /dev/null
 