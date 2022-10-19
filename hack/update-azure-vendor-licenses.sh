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

REPO_ROOT=$(realpath $(dirname ${BASH_SOURCE})/..)
K8S_RELEASE="release-1.25"

# Download scripts from kubernetes/kubernetes
curl -o "${REPO_ROOT}/hack/update-vendor-licenses.sh" "https://raw.githubusercontent.com/kubernetes/kubernetes/${K8S_RELEASE}/hack/update-vendor-licenses.sh"
chmod +x "${REPO_ROOT}/hack/update-vendor-licenses.sh"

mkdir -p "${REPO_ROOT}/hack/lib"
LIBS=("init.sh" "util.sh" "logging.sh" "version.sh" "golang.sh" "etcd.sh")
for lib in "${LIBS[@]}"; do
  curl -o "${REPO_ROOT}/hack/lib/${lib}" "https://raw.githubusercontent.com/kubernetes/kubernetes/${K8S_RELEASE}/hack/lib/${lib}"
done

mkdir -p "${REPO_ROOT}/third_party"

echo "Start updating vendor licenses"
./hack/update-vendor-licenses.sh

rm -rf "${REPO_ROOT}/third_party"
rm -rf "${REPO_ROOT}/hack/lib"
rm "${REPO_ROOT}/hack/update-vendor-licenses.sh"
