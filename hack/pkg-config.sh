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
cd $(dirname "${BASH_SOURCE}")/..

if [ "${ENABLE_GIT_COMMAND}" = true ]; then
  GIT_VERSION=$(git describe --tags --always --abbrev=9 || echo)
  GIT_COMMIT=$(git rev-parse HEAD)
  version_regex="^v(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)-([a-zA-Z0-9-]+)?$"
  [[ "${GIT_VERSION}" =~ ${version_regex} ]] && {
    # shellcheck disable=SC2034
    VERSION_MAJOR="${BASH_REMATCH[1]}"
    # shellcheck disable=SC2034
    VERSION_MINOR="${BASH_REMATCH[2]}+"
  }
else
  GIT_VERSION="latest"
  GIT_COMMIT="latest"
  VERSION_MAJOR="latest"
  VERSION_MINOR="latest"
fi

VERSION_PKG=sigs.k8s.io/cloud-provider-azure/pkg/version
LDFLAGS="-s -w"
LDFLAGS="$LDFLAGS -X $VERSION_PKG.gitVersion=${GIT_VERSION}"
LDFLAGS="$LDFLAGS -X $VERSION_PKG.gitCommit=${GIT_COMMIT}"
LDFLAGS="$LDFLAGS -X $VERSION_PKG.gitMajor=${VERSION_MAJOR}"
LDFLAGS="$LDFLAGS -X $VERSION_PKG.gitMinor=${VERSION_MINOR}"
LDFLAGS="$LDFLAGS -X $VERSION_PKG.buildDate=$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
echo -ldflags \'$LDFLAGS\'
