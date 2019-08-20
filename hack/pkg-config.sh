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
cd $(dirname "$BASH_SOURCE")/..

VERSION_PKG=k8s.io/cloud-provider-azure/pkg/version
LDFLAGS="-s -w"
LDFLAGS="$LDFLAGS -X $VERSION_PKG.gitVersion=$(git describe --tags --always --abbrev=9 || echo)"
LDFLAGS="$LDFLAGS -X $VERSION_PKG.gitCommit=$(git rev-parse HEAD)"
LDFLAGS="$LDFLAGS -X $VERSION_PKG.buildDate=$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
echo -ldflags \'$LDFLAGS\'
