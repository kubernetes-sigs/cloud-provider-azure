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
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

type RetryOnRegionalEndpointPolicy struct {
	regionalEndpoint string
}

func (c *RetryOnRegionalEndpointPolicy) Do(request *policy.Request) (*http.Response, error) {
	response, err := request.Next()
	switch true {
	case response == nil,
		err == nil,
		request.Raw().Method != http.MethodGet,
		response.StatusCode == http.StatusNotFound,
		c.regionalEndpoint == "":
		return response, err
	}

	currentHost := request.Raw().URL.Host
	if request.Raw().Host != "" {
		currentHost = request.Raw().Host
	}
	if strings.HasPrefix(strings.ToLower(currentHost), c.regionalEndpoint) {
		return response, err
	}

	bodyBytes, _ := io.ReadAll(response.Body)
	bodyString := string(bodyBytes)
	trimmed := strings.TrimSpace(bodyString)

	var retryNeeded bool
	if (response.ContentLength == 0 || trimmed == "" || trimmed == "{}") && response.StatusCode >= 200 && response.StatusCode < 300 {
		retryNeeded = true
	}

	// Hack: retry the regional ARM endpoint in case of ARM traffic split and arm resource group replication is too slow
	// Empty content and 2xx http status code are returned in this case.
	// Issue: https://github.com/kubernetes-sigs/cloud-provider-azure/issues/1296
	// Such situation also needs retrying that ContentLength is -1, StatusCode is 200 and an empty body is returned.
	var body map[string]map[string]interface{}
	if unmarshalErr := json.Unmarshal(bodyBytes, &body); unmarshalErr == nil {
		if errSection, ok := body["error"]; ok {
			if code, ok := errSection["code"]; ok {
				if strings.EqualFold(code.(string), "ResourceGroupNotFound") {
					retryNeeded = true
				}
			}
		}
	}
	if retryNeeded {
		regionalRequest := request.Clone(request.Raw().Context())
		regionalRequest.Raw().URL.Host = c.regionalEndpoint
		request.RewindBody()

		regionalResponse, regionalError := regionalRequest.Next()

		// only use the result if the regional request actually goes through and returns 2xx status code, for two reasons:
		// 1. the retry on regional ARM host approach is a hack.
		// 2. the concatenated regional uri could be wrong as the rule is not officially declared by ARM.
		if regionalResponse == nil || regionalResponse.StatusCode > 299 {
			return response, err
		}
		return regionalResponse, regionalError
	}
	response.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
	return response, err
}
