/*
Copyright 2023 The Kubernetes Authors.

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

package utils

import (
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/tracing"

	custompolicy "sigs.k8s.io/cloud-provider-azure/pkg/azclient/policy"
)

func GetDefaultOption() *arm.ClientOptions {
	return &arm.ClientOptions{
		ClientOptions: policy.ClientOptions{
			Retry: policy.RetryOptions{
				RetryDelay:    5 * time.Second,
				MaxRetryDelay: 3 * time.Minute,
				MaxRetries:    3,
				TryTimeout:    1 * time.Minute,
				StatusCodes:   custompolicy.GetRetriableStatusCode(),
			},
			PerRetryPolicies: []policy.Policy{
				custompolicy.NewThrottlingPolicy(),
			},
			Transport: defaultHTTPClient,
			TracingProvider: tracing.NewProvider(func(name, version string) tracing.Tracer {
				return tracing.NewTracer(NewOtlpSpan, nil)
			}, nil),
		},
	}
}
