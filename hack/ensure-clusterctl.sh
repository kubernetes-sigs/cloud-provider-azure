#!/usr/bin/env bash

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
set -o pipefail

REPO_ROOT=$(dirname "${BASH_SOURCE[0]}")/..
TOOLS_BIN="${REPO_ROOT}/bin"
CLUSTERCTL="${TOOLS_BIN}/clusterctl"
MINIMUM_CLUSTERCTL_VERSION=v1.6.0
goarch="$(go env GOARCH)"
goos="$(go env GOOS)"

export CLUSTERCTL

# Ensure the clusterctl exists and is a viable version, or installs it
function verify_clusterctl_version() {
   if ! [ -x "$(command -v "${CLUSTERCTL}")" ]; then
     if [ "$goos" == "linux" ] || [ "$goos" == "darwin" ]; then
       echo "clusterctl not found, installing"
       if ! [ -d "${TOOLS_BIN}" ]; then
         mkdir -p "${TOOLS_BIN}"
       fi
       curl -sLo "${CLUSTERCTL}" "https://github.com/kubernetes-sigs/cluster-api/releases/download/${MINIMUM_CLUSTERCTL_VERSION}/clusterctl-${goos}-${goarch}"
       chmod +x "${CLUSTERCTL}"
     else
       echo "Missing required binary in path: clusterctl"
       return 2
     fi
   fi

   local clusterctl_version
   read -ra clusterctl_version <<< "$(${CLUSTERCTL} version -o short)"
   if [[ "${MINIMUM_CLUSTERCTL_VERSION}" != $(echo -e "${MINIMUM_CLUSTERCTL_VERSION}\n${clusterctl_version[0]}" | sort -s -t. -k 1,1 -k 2,2n -k 3,3n | head -n1) ]]; then
    cat <<EOF
Detected kind version: ${clusterctl_version[0]}.
Requires ${MINIMUM_CLUSTERCTL_VERSION} or greater.
Please install ${MINIMUM_CLUSTERCTL_VERSION} or later.
You can delete the bin/clusterctl and re-run the script.
EOF
    return 2
  fi
}

verify_clusterctl_version
