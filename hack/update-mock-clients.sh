#!/bin/bash
# Copyright 2021 The Kubernetes Authors.
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
COPYRIGHT_FILE="${REPO_ROOT}/hack/boilerplate/boilerplate.generatego.txt"
TARGET_DIR="${REPO_ROOT}/pkg/azureclients"

# update_all_mocks update mocks for all modules
function update_all_mocks(){
    for dir in $TARGET_DIR/*
    do
        if [ -d "${dir}" ]; then \
            echo "Updating mocks for ${dir%*/}"
            mockgen -copyright_file=$COPYRIGHT_FILE -source=$TARGET_DIR/${dir##*/}/interface.go -package=mock${dir##*/} Interface > $TARGET_DIR/${dir##*/}/mock${dir##*/}/interface.go
        fi
    done
}

# update_mock creates mock for specific module
function update_mock(){
    mock_module=$1
    echo "Updating mock for $mock_module"
    mockgen -copyright_file=$COPYRIGHT_FILE -source=$TARGET_DIR/$mock_module/interface.go -package=mock$mock_module Interface > $TARGET_DIR/$mock_module/mock$mock_module/interface.go
}

if [ "$#" -eq "0" ]
then
    update_all_mocks
else
    update_mock "$1"
fi
