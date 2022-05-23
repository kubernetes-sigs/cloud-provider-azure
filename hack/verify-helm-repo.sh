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

set -o nounset
set -o errexit
set -o pipefail
set -x

REPO_ROOT=$(dirname "${BASH_SOURCE}")/..

for chart in "cloud-provider-azure"; do
  LATEST_CHART=$(ls ${REPO_ROOT}/helm/repo/${chart}*.tgz | sort -rV | head -n 1)
  LATEST_VERSION=$(echo $LATEST_CHART | grep -Eoq [0-9]+\.[0-9]+\.[0-9]+)
  MATCH_STRING="version: $LATEST_VERSION"
  # verify that the current version in Chart.yaml is the most recent, packaged chart in the repo
  if ! grep -q "${MATCH_STRING}" ${REPO_ROOT}/helm/${chart}/Chart.yaml; then
    echo The version of the $chart helm chart checked into the git repository does not match the latest packaged version $LATEST_VERSION in the repo
    exit 1
  fi
  rm -Rf ${REPO_ROOT}/chart_verify
  mkdir ${REPO_ROOT}/chart_verify
  tar -xf $LATEST_CHART -C chart_verify
  diff -r ${REPO_ROOT}/chart_verify/cloud-provider-azure/ ${REPO_ROOT}/helm/cloud-provider-azure/ --exclude README.md || exit 1
  rm -Rf ${REPO_ROOT}/chart_verify
done

curl -fsSL -o get_helm.sh https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3
chmod 700 get_helm.sh
./get_helm.sh
helm template ./helm/cloud-provider-azure > /dev/null

exit 0
