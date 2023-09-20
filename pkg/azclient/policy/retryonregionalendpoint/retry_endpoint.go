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

package retryonregionalendpoint

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

type Policy struct {
	regionalEndpoint string
}

func NewPolicy(regionalEndpoint string) *Policy {
	return &Policy{regionalEndpoint: regionalEndpoint}
}

// Hack: retry the regional ARM endpoint in case of ARM traffic split and arm resource group replication is too slow
// Issue: https://github.com/kubernetes-sigs/cloud-provider-azure/issues/1296
// Send the request to the regional endpoint if the response contains error code "ResourceGroupNotFound".
// Such situation also needs retrying that ContentLength is -1, StatusCode is 200 and an empty body is returned.
func (c *Policy) Do(request *policy.Request) (*http.Response, error) {
	response, err := request.Next()
	if err != nil || response == nil {
		return response, err
	}
	bodyBytes, _ := io.ReadAll(response.Body)
	bodyString := string(bodyBytes)
	trimmed := strings.TrimSpace(bodyString)

	if response.ContentLength == 0 || trimmed == "" || trimmed == "{}" { // empty body
		if response.StatusCode < 200 || response.StatusCode >= 300 { // retry on 2xx status code
			return response, err
		}
	} else { // non-empty body
		var body map[string]map[string]interface{}
		if unmarshalErr := json.Unmarshal([]byte(trimmed), &body); unmarshalErr != nil {
			return response, err
		}
		if errSection, ok := body["error"]; !ok {
			return response, err
		} else if code, ok := errSection["code"]; !ok {
			return response, err
		} else if !strings.EqualFold(code.(string), "ResourceGroupNotFound") { // retry on ResourceGroupNotFound error code
			return response, err
		}
	}

	// retry on regional ARM host
	currentHost := request.Raw().URL.Host
	if request.Raw().Host != "" {
		currentHost = request.Raw().Host
	}
	if strings.HasPrefix(strings.ToLower(currentHost), c.regionalEndpoint) {
		return response, err
	}

	regionalRequest := request.Clone(request.Raw().Context())
	regionalRequest.Raw().URL.Host = c.regionalEndpoint
	regionalRequest.Raw().Host = c.regionalEndpoint
	rewindErr := request.RewindBody()
	if rewindErr != nil {
		return response, err
	}
	regionalResponse, regionalError := regionalRequest.Next()

	// only use the result if the regional request actually goes through and returns 2xx status code, for two reasons:
	// 1. the retry on regional ARM host approach is a hack.
	// 2. the concatenated regional uri could be wrong as the rule is not officially declared by ARM.
	if regionalResponse == nil || regionalResponse.StatusCode < 200 || regionalResponse.StatusCode > 299 {
		return response, err
	}
	return regionalResponse, regionalError
}
