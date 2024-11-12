# Copyright 2023 The Kubernetes Authors.
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

# syntax=docker/dockerfile:1

FROM gcr.io/distroless/base:latest@sha256:71667b504b57a3aa13b1a61d9ba4147aeed27bb024b9ae1833970fae9f5ed34e
COPY bin/health-probe-proxy /usr/local/bin/health-probe-proxy
ENTRYPOINT [ "/usr/local/bin/health-probe-proxy" ]
