/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package providererrors

import "errors"

// ErrLoadBalancerMetadataUnavailable marks errors reporting that instance metadata
// was retrieved but load balancer metadata, which supplies public IPs, could not be
// retrieved due to a transient failure. Addresses returned alongside such an error
// are valid but may lack ExternalIPs.
var ErrLoadBalancerMetadataUnavailable = errors.New("load balancer metadata unavailable")
