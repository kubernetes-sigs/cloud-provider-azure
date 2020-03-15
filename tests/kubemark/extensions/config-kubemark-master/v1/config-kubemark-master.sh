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

sudo bash -c 'cat >> /etc/kubernetes/addons/kube-proxy-daemonset.yaml << EOF
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
            - matchExpressions:
              - key: failure-domain.beta.kubernetes.io/zone
                operator: Exists
EOF'

sudo sed -i 's/configure-cloud-routes=true/configure-cloud-routes=false/' "/etc/kubernetes/manifests/kube-controller-manager.yaml"

kubectl apply -f "/etc/kubernetes/addons/kube-proxy-daemonset.yaml"
kubectl apply -f "/etc/kubernetes/manifests/kube-controller-manager.yaml"
kubectl -n kube-system get po | grep "kube-controller-manager" | awk '{print $1}' | xargs kubectl -n kube-system delete po

KUBEMARK_MASTER_NAME=$(kubectl get no | grep "master" | awk '{print $1}')
kubectl taint node "${KUBEMARK_MASTER_NAME}" node-role.kubernetes.io/master=:NoSchedule
