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

function install_ginkgo_v2() {
  echo "Installing latest ginkgo cli"
  go install -mod=mod github.com/onsi/ginkgo/v2/ginkgo@v2.2.0
}

if ! [ -x "$(command -v ginkgo)" ]; then
  install_ginkgo_v2
else
  echo "Checking ginkgo version"
  IFS=" " read -ra ginkgo_version <<< "$(ginkgo version)"
  minimum_ginkgo_version=2.0.0
  if [[ "${minimum_ginkgo_version}" != $(echo -e "${minimum_ginkgo_version}\n${ginkgo_version[2]}" | sort -s -t. -k 1,1 -k 2,2n -k 3,3n |   head -n1) ]]; then
    install_ginkgo_v2
  fi
fi

ginkgo version
