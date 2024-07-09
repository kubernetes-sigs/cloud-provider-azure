#!/usr/bin/env bash

# Copyright 2024 The Kubernetes Authors.
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
set -o pipefail

: "${CLUSTER_NAME:?empty or not defined.}"

export KIND="${KIND:-true}"
export MANAGEMENT_CLUSTER_NAME="${MANAGEMENT_CLUSTER_NAME:-capz}"


export MGMT_CLUSTER_CONTEXT="${MGMT_CLUSTER_CONTEXT:-kind-${MANAGEMENT_CLUSTER_NAME}}"
if [ "${KIND}" = "false" ]; then
  MGMT_CLUSTER_CONTEXT="${MANAGEMENT_CLUSTER_NAME}"
fi

echo "Deleting cluster ${CLUSTER_NAME}."
kubectl --context="${MGMT_CLUSTER_CONTEXT}" delete cluster "${CLUSTER_NAME}" -n "${CLUSTER_NAME}"

echo "Deleting namespace ${CLUSTER_NAME}."
kubectl --context="${MGMT_CLUSTER_CONTEXT}" delete ns "${CLUSTER_NAME}"

echo "Finished."
