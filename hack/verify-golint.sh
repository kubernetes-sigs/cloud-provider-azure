#!/bin/bash

# Copyright 2020 The Kubernetes Authors.
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

set -euo pipefail

if [[ -z "$(command -v golangci-lint)" ]]; then
  echo "Cannot find golangci-lint. Installing golangci-lint..."
  go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.45.0
  export PATH=$PATH:$(go env GOPATH)/bin
fi

echo "Verifying golint"
readonly PKG_ROOT="$(git rev-parse --show-toplevel)"

LINTERS=("deadcode" "errcheck" "goconst" "gofmt" "goimports" "revive" "gosec" "gosimple" "govet" "ineffassign" "misspell" "nakedret" "staticcheck" "structcheck" "typecheck" "unconvert" "unused" "varcheck" )
GOLANGCI_YML="${PKG_ROOT}/.golangci.yml"
GOLANGCI_YML_TEST="${PKG_ROOT}/.golangci-test.yml"
for LINTER in "${LINTERS[@]}"; do
  cp -rf "${GOLANGCI_YML}" "${GOLANGCI_YML_TEST}"
  sed -i "s|LINTER_TO_RUN|${LINTER}|g" "${GOLANGCI_YML_TEST}"
  golangci-lint run -v --config "${GOLANGCI_YML_TEST}"
  echo ""
done

echo "Congratulations! Lint check completed for all Go source files."
