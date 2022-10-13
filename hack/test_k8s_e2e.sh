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

KUBECONFIG=${KUBECONFIG:-""}

if [ -z "$KUBECONFIG" ]
echo "KUBECONFIG not set"
exit 1
fi

cd $GOPATH/src/k8s.io/kubernetes
 [ ! -d "$GOPATH/src/k8s.io/kubernetes" ] && cd $GOPATH/src/k8s.io && git clone https://github.com/kubernetes/kubernetes.git	

make WHAT='test/e2e/e2e.test'
make WHAT=cmd/kubectl
export KUBERNETES_PROVIDER=azure
export KUBERNETES_CONFORMANCE_TEST=y
export KUBERNETES_CONFORMANCE_PROVIDER=azure
export CLOUD_CONFIG=$GOPATH/src/sigs.k8s.io/cloud-provider-azure/tests/k8s-azure/manifest/azure.json

# Replace the test_args with your own.
go run hack/e2e.go -- --test --provider=local --check-version-skew=false --test_args=$1
