#!/bin/bash

# Copyright 2022 The Kubernetes Authors.
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
set -o nounset
set -o pipefail

CLUSTER_NAME="aks-cluster"
CURRENT_DATE="$(date +%s)"

az login --identity
az group list --tag usage=aks-cluster-e2e | jq -r '.[].name' | awk '{print $1}' | while read -r RESOURCE_GROUP; do
  RG_DATE="$(az group show --resource-group ${RESOURCE_GROUP} | jq -r '.tags.creation_date')"
  DATE_DIFF="$(expr ${CURRENT_DATE} - ${RG_DATE})"
  # GC clusters older than 1 day
  if (( "${DATE_DIFF}" > 86400 )); then
    echo "Deleting resource group: ${RESOURCE_GROUP}"
    az group delete --resource-group "${RESOURCE_GROUP}" -y --no-wait
  fi
done
