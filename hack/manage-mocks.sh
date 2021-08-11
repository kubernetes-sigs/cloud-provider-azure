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

COPYRIGHT_FILE=./hack/boilerplate/boilerplate.generatego.txt
TARGET_DIR=./pkg/azureclients
IFACE_FILE=interface.go

# createAll_mock creates mocks for all modules
function createAll_mocks(){
    for dir in $TARGET_DIR/* 
    do
        if [ -d "${dir}" ]; then \
            echo "Mocks updated for ${dir%*/}"
            mockgen -copyright_file=$COPYRIGHT_FILE -source=$TARGET_DIR/${dir##*/}/$IFACE_FILE -package=mock${dir##*/} Interface > $TARGET_DIR/${dir##*/}/mock${dir##*/}/$IFACE_FILE
        fi
    done
}

# create_mock creates mock for specific module
function create_mock(){
    mock_module=$1
    echo "Mocks updated for $mock_module"
    mockgen -copyright_file=$COPYRIGHT_FILE -source=$TARGET_DIR/$mock_module/$IFACE_FILE -package=mock$mock_module Interface > $TARGET_DIR/$mock_module/mock$mock_module/$IFACE_FILE
}

read -p "Enter name of module for which we want to create mock. (Press enter if mocks are to be created for all modules): " mock_module

if [ -z $mock_module ]
then
    createAll_mocks
else
    create_mock $mock_module
fi
