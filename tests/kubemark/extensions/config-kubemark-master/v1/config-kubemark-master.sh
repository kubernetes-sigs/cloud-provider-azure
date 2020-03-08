#! /bin/bash

set -x

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
