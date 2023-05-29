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

package regionalendpointretry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

const (
	DefaultMaxRetries    = 3
	DefaultMaxRetryDelay = math.MaxInt64
	DefaultRetryDelay    = 5 * time.Second
	DefaultTryTimeout    = 1 * time.Second
)

type RetryConfig struct {
	PollingDelay  *time.Duration
	RetryAttempts *int

	RetryDuration *time.Duration
	// Backoff holds parameters applied to a Backoff function.
	// The initial duration.
	Duration time.Duration
	// Duration is multiplied by factor each iteration, if factor is not zero
	// and the limits imposed by Steps and Cap have not been reached.
	// Should not be negative.
	// The jitter does not contribute to the updates to the duration parameter.
	Factor float64
	// The sleep at each iteration is the duration plus an additional
	// amount chosen uniformly at random from the interval between
	// zero and `jitter*duration`.
	Jitter float64
	// The remaining number of iterations in which the duration
	// parameter may change (but progress can be stopped earlier by
	// hitting the cap). If not positive, the duration is not
	// changed. Used for exponential backoff in combination with
	// Factor and Cap.
	Steps int
	// A limit on revised values of the duration parameter. If a
	// multiplication by the factor parameter would make the duration
	// exceed the cap then the duration is set to the cap and the
	// steps parameter is set to zero.
	Cap time.Duration
	// The errors indicate that the request shouldn't do more retrying.
	NonRetriableErrors []string
	// The RetriableHTTPStatusCodes indicates that the HTTPStatusCode should do more retrying.
	RetriableHTTPStatusCodes []int
}

func NewGetRequestRegionalEndpointRetryPolicy(regionalEndpoint string) policy.Policy {
	if regionalEndpoint == "" {
		return nil
	}
	return &GetRequestRegionalEndpointRetryPolicy{
		regionalEndpoint: regionalEndpoint,
	}
}

// GetRequestRegionalEndpointRetryPolicy is a policy that retries requests on ResourceGroupNotFound error.
type GetRequestRegionalEndpointRetryPolicy struct {
	regionalEndpoint string
}

func (p *GetRequestRegionalEndpointRetryPolicy) Do(req *policy.Request) (*http.Response, error) {
	response, err := req.Next()
	if response == nil {
		return response, err
	}
	//retry only when request is read operation and response is 404
	if req.Raw().Method != http.MethodGet || req.Raw().Method != http.MethodHead {
		return response, err
	}
	bodyBytes, _ := io.ReadAll(response.Body)
	defer func() {
		response.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
	}()

	bodyString := string(bodyBytes)
	trimmed := strings.TrimSpace(bodyString)
	// Hack: retry the regional ARM endpoint in case of ARM traffic split and arm resource group replication is too slow
	// Empty content and 2xx http status code are returned in this case.
	// Issue: https://github.com/kubernetes-sigs/cloud-provider-azure/issues/1296
	// Such situation also needs retrying that ContentLength is -1, StatusCode is 200 and an empty body is returned.
	emptyResp := (response.ContentLength == 0 || trimmed == "" || trimmed == "{}") && response.StatusCode >= 200 && response.StatusCode < 300
	if !emptyResp {
		if err == nil || response.StatusCode == http.StatusNotFound || p.regionalEndpoint == "" {
			return response, err
		}

		var body map[string]interface{}
		if e := json.Unmarshal(bodyBytes, &body); e != nil {
			return response, err
		}

		errBody, ok := body["error"].(map[string]interface{})
		if !ok || errBody["code"] == nil || !strings.EqualFold(errBody["code"].(string), "ResourceGroupNotFound") {
			return response, err
		}
	}

	// Do regional request
	currentHost := req.Raw().URL.Host
	if req.Raw().Host != "" {
		currentHost = req.Raw().Host
	}

	if strings.HasPrefix(strings.ToLower(currentHost), strings.ToLower(p.regionalEndpoint)) {
		return response, err
	}

	req.Raw().Host = p.regionalEndpoint
	req.Raw().URL.Host = p.regionalEndpoint

	regionalResponse, regionalError := req.Next()

	// only use the result if the regional request actually goes through and returns 2xx status code, for two reasons:
	// 1. the retry on regional ARM host approach is a hack.
	// 2. the concatenated regional uri could be wrong as the rule is not officially declared by ARM.
	if regionalResponse == nil || regionalResponse.StatusCode > 299 {
		return response, err
	}

	// Do the same check on regional response just like the global one
	bodyBytes, _ = io.ReadAll(regionalResponse.Body)
	defer func() {
		regionalResponse.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
	}()
	bodyString = string(bodyBytes)
	trimmed = strings.TrimSpace(bodyString)
	emptyResp = (regionalResponse.ContentLength == 0 || trimmed == "" || trimmed == "{}") && regionalResponse.StatusCode >= 200 && regionalResponse.StatusCode < 300
	if emptyResp {
		contentLengthErrStr := fmt.Sprintf("empty response with trimmed body %q, ContentLength %d and StatusCode %d", trimmed, regionalResponse.ContentLength, regionalResponse.StatusCode)
		return response, fmt.Errorf(contentLengthErrStr)
	}

	return regionalResponse, regionalError
}
