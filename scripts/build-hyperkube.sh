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

TEMPPATH=$(mktemp -d)
REGISTRY=${REGISTRY:-""}
BRANCH=${BRANCH:-"master"}

cleanup() {
    rm -rf $TEMPPATH
}

export GOPATH=$TEMPPATH/gopath
mkdir -p $GOPATH/src/k8s.io
git clone https://github.com/kubernetes/kubernetes $GOPATH/src/k8s.io/kubernetes >&2
cd $GOPATH/src/k8s.io/kubernetes

trap cleanup EXIT

# prefix 'origin/' is required for checking out branches.
if ! [[ "${BRANCH}" =~ "^origin/" ]]; then
    BRANCH="origin/$BRANCH"
fi

if [ "$BRANCH" != "" ]; then
    git checkout -b temp $BRANCH >&2
fi

VERSION=$(git rev-parse --short=7 HEAD)
VERSION=$VERSION REGISTRY=$REGISTRY hack/dev-push-hyperkube.sh >&2
echo -n $VERSION
