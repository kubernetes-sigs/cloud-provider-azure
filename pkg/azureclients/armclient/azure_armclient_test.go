/*
Copyright 2019 The Kubernetes Authors.

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

package armclient

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients"

	"github.com/Azure/go-autorest/autorest"
	"github.com/stretchr/testify/assert"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

const (
	testResourceID = "/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP"
	operationURI   = "/subscriptions/subscription/providers/Microsoft.Network/locations/eastus/operations/op?api-version=2019-01-01"
	expectedURI    = "/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP?api-version=2019-01-01"
)

func TestNew(t *testing.T) {
	azConfig := azureclients.ClientConfig{Backoff: &retry.Backoff{Steps: 3}, UserAgent: "test", Location: "eastus"}
	armClient := New(nil, azConfig, "", "2019-01-01")
	assert.NotNil(t, armClient.backoff)
	assert.Equal(t, 3, armClient.backoff.Steps, "Backoff steps should be same as the value passed in")

	azConfig = azureclients.ClientConfig{Backoff: &retry.Backoff{Steps: 0}, UserAgent: "test", Location: "eastus"}
	armClient = New(nil, azConfig, "", "2019-01-01")
	assert.NotNil(t, armClient.backoff)
	assert.Equal(t, 1, armClient.backoff.Steps, "Backoff steps should be default to 1 if it is 0")

	azConfig = azureclients.ClientConfig{UserAgent: "test", Location: "eastus"}
	armClient = New(nil, azConfig, "", "2019-01-01")
	assert.NotNil(t, armClient.backoff)
	assert.Equal(t, 1, armClient.backoff.Steps, "Backoff steps should be default to 1 if it is not set")
}

func TestSend(t *testing.T) {
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if count <= 1 {
			http.Error(w, "failed", http.StatusInternalServerError)
			count++
		}
	}))

	azConfig := azureclients.ClientConfig{Backoff: &retry.Backoff{Steps: 3}, UserAgent: "test", Location: "eastus"}
	armClient := New(nil, azConfig, server.URL, "2019-01-01")
	pathParameters := map[string]interface{}{
		"resourceGroupName": autorest.Encode("path", "testgroup"),
		"subscriptionId":    autorest.Encode("path", "testid"),
		"resourceName":      autorest.Encode("path", "testname"),
	}

	decorators := []autorest.PrepareDecorator{
		autorest.WithPathParameters(
			"/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Network/vNets/{resourceName}", pathParameters),
	}

	ctx := context.Background()
	request, err := armClient.PrepareGetRequest(ctx, decorators...)
	assert.NoError(t, err)

	response, rerr := armClient.Send(ctx, request)
	assert.Nil(t, rerr)
	assert.Equal(t, 2, count)
	assert.Equal(t, http.StatusOK, response.StatusCode)
}

func TestSendFailure(t *testing.T) {
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "failed", http.StatusInternalServerError)
		count++
	}))

	azConfig := azureclients.ClientConfig{Backoff: &retry.Backoff{Steps: 3}, UserAgent: "test", Location: "eastus"}
	armClient := New(nil, azConfig, server.URL, "2019-01-01")
	pathParameters := map[string]interface{}{
		"resourceGroupName": autorest.Encode("path", "testgroup"),
		"subscriptionId":    autorest.Encode("path", "testid"),
		"resourceName":      autorest.Encode("path", "testname"),
	}

	decorators := []autorest.PrepareDecorator{
		autorest.WithPathParameters(
			"/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Network/vNets/{resourceName}", pathParameters),
	}

	ctx := context.Background()
	request, err := armClient.PreparePatchRequest(ctx, decorators...)
	assert.NoError(t, err)

	response, rerr := armClient.Send(ctx, request)
	assert.NotNil(t, rerr)
	assert.Equal(t, 3, count)
	assert.Equal(t, http.StatusInternalServerError, response.StatusCode)
}

func TestSendThrottled(t *testing.T) {
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(consts.RetryAfterHeaderKey, "30")
		http.Error(w, "failed", http.StatusTooManyRequests)
		count++
	}))

	azConfig := azureclients.ClientConfig{Backoff: &retry.Backoff{Steps: 3}, UserAgent: "test", Location: "eastus"}
	armClient := New(nil, azConfig, server.URL, "2019-01-01")
	pathParameters := map[string]interface{}{
		"resourceGroupName": autorest.Encode("path", "testgroup"),
		"subscriptionId":    autorest.Encode("path", "testid"),
		"resourceName":      autorest.Encode("path", "testname"),
	}
	decorators := []autorest.PrepareDecorator{
		autorest.WithPathParameters(
			"/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Network/vNets/{resourceName}", pathParameters),
	}

	ctx := context.Background()
	request, err := armClient.PrepareGetRequest(ctx, decorators...)
	assert.NoError(t, err)

	response, rerr := armClient.Send(ctx, request)
	assert.NotNil(t, rerr)
	assert.Equal(t, 1, count)
	assert.Equal(t, http.StatusTooManyRequests, response.StatusCode)
}

func TestSendAsync(t *testing.T) {
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		http.Error(w, "failed", http.StatusForbidden)
	}))

	azConfig := azureclients.ClientConfig{Backoff: &retry.Backoff{Steps: 1}, UserAgent: "test", Location: "eastus"}
	armClient := New(nil, azConfig, server.URL, "2019-01-01")
	armClient.client.RetryDuration = time.Millisecond * 1

	pathParameters := map[string]interface{}{
		"resourceGroupName": autorest.Encode("path", "testgroup"),
		"subscriptionId":    autorest.Encode("path", "testid"),
		"resourceName":      autorest.Encode("path", "testname"),
	}
	decorators := []autorest.PrepareDecorator{
		autorest.WithPathParameters(
			"/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Network/vNets/{resourceName}", pathParameters),
	}

	ctx := context.Background()
	request, err := armClient.PreparePutRequest(ctx, decorators...)
	assert.NoError(t, err)

	future, response, rerr := armClient.SendAsync(ctx, request)
	assert.Nil(t, future)
	assert.Nil(t, response)
	assert.Equal(t, 1, count)
	assert.NotNil(t, rerr)
	assert.Equal(t, false, rerr.Retriable)
}

func TestSendAsyncSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	azConfig := azureclients.ClientConfig{Backoff: &retry.Backoff{Steps: 1}, UserAgent: "test", Location: "eastus"}
	armClient := New(nil, azConfig, server.URL, "2019-01-01")
	armClient.client.RetryDuration = time.Millisecond * 1

	pathParameters := map[string]interface{}{
		"resourceGroupName": autorest.Encode("path", "testgroup"),
		"subscriptionId":    autorest.Encode("path", "testid"),
		"resourceName":      autorest.Encode("path", "testname"),
	}
	decorators := []autorest.PrepareDecorator{
		autorest.WithPathParameters(
			"/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Network/vNets/{resourceName}", pathParameters),
	}

	ctx := context.Background()
	request, err := armClient.PreparePostRequest(ctx, decorators...)
	assert.NoError(t, err)

	future, response, rerr := armClient.SendAsync(ctx, request)
	assert.Nil(t, rerr)
	assert.NotNil(t, response)
	assert.NotNil(t, future)
}

func TestNormalizeAzureRegion(t *testing.T) {
	tests := []struct {
		region   string
		expected string
	}{
		{
			region:   "eastus",
			expected: "eastus",
		},
		{
			region:   " eastus ",
			expected: "eastus",
		},
		{
			region:   " eastus\t",
			expected: "eastus",
		},
		{
			region:   " eastus\v",
			expected: "eastus",
		},
		{
			region:   " eastus\v\r\f\n",
			expected: "eastus",
		},
	}

	for i, test := range tests {
		real := NormalizeAzureRegion(test.region)
		assert.Equal(t, test.expected, real, "test[%d]: NormalizeAzureRegion(%q) != %q", i, test.region, test.expected)
	}
}

func TestGetResource(t *testing.T) {
	expectedURIResource := "/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP?%24expand=data&api-version=2019-01-01"
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "GET", r.Method)
		assert.Equal(t, expectedURIResource, r.URL.String())
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{data: testPIP}"))
		count++
	}))

	azConfig := azureclients.ClientConfig{Backoff: &retry.Backoff{Steps: 1}, UserAgent: "test", Location: "eastus"}
	armClient := New(nil, azConfig, server.URL, "2019-01-01")
	armClient.client.RetryDuration = time.Millisecond * 1

	ctx := context.Background()
	response, rerr := armClient.GetResource(ctx, testResourceID, "data")
	byteResponseBody, _ := ioutil.ReadAll(response.Body)
	stringResponseBody := string(byteResponseBody)
	assert.Nil(t, rerr)
	assert.Equal(t, "{data: testPIP}", stringResponseBody)
	assert.Equal(t, 1, count)
}

func TestGetResourceWithDecorators(t *testing.T) {
	expectedURIResource := "/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP?api-version=2019-01-01&param1=value1&param2=value2"

	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "GET", r.Method)
		assert.Equal(t, expectedURIResource, r.URL.String())
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{data: testPIP}"))
		count++
	}))

	azConfig := azureclients.ClientConfig{Backoff: &retry.Backoff{Steps: 1}, UserAgent: "test", Location: "eastus"}
	armClient := New(nil, azConfig, server.URL, "2019-01-01")
	armClient.client.RetryDuration = time.Millisecond * 1

	params := map[string]interface{}{
		"param1": "value1",
		"param2": "value2",
	}
	decorators := []autorest.PrepareDecorator{
		autorest.WithQueryParameters(params),
	}

	ctx := context.Background()
	response, rerr := armClient.GetResourceWithDecorators(ctx, testResourceID, decorators)
	byteResponseBody, _ := ioutil.ReadAll(response.Body)
	stringResponseBody := string(byteResponseBody)
	assert.Nil(t, rerr)
	assert.Equal(t, "{data: testPIP}", stringResponseBody)
	assert.Equal(t, 1, count)
}

func TestPutResource(t *testing.T) {
	handlers := []func(http.ResponseWriter, *http.Request){
		func(rw http.ResponseWriter, req *http.Request) {
			assert.Equal(t, "PUT", req.Method)
			assert.Equal(t, expectedURI, req.URL.String())
			rw.Header().Set("Azure-AsyncOperation",
				fmt.Sprintf("http://%s%s", req.Host, operationURI))
			rw.WriteHeader(http.StatusCreated)
		},

		func(rw http.ResponseWriter, req *http.Request) {
			assert.Equal(t, "GET", req.Method)
			assert.Equal(t, operationURI, req.URL.String())

			rw.WriteHeader(http.StatusOK)
			_, _ = rw.Write([]byte(`{"error":{"code":"InternalServerError"},"status":"Failed"}`))
		},
	}

	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers[count](w, r)
		count++
		if count > 1 {
			count = 1
		}
	}))

	azConfig := azureclients.ClientConfig{Backoff: &retry.Backoff{Steps: 1}, UserAgent: "test", Location: "eastus"}
	armClient := New(nil, azConfig, server.URL, "2019-01-01")
	armClient.client.RetryDuration = time.Millisecond * 1

	ctx := context.Background()
	response, rerr := armClient.PutResource(ctx, testResourceID, nil)
	assert.Equal(t, 1, count)
	assert.Nil(t, response)
	assert.NotNil(t, rerr)
	assert.Equal(t, true, rerr.Retriable)
}

func TestPutResources(t *testing.T) {
	total := 0
	server := getTestServer(t, &total)

	azConfig := azureclients.ClientConfig{Backoff: &retry.Backoff{Steps: 1}, UserAgent: "test", Location: "eastus"}
	armClient := New(nil, azConfig, server.URL, "2019-01-01")
	armClient.client.RetryDuration = time.Millisecond * 1

	ctx := context.Background()
	resources := map[string]interface{}{
		"/id/1": nil,
		"/id/2": nil,
	}
	responses := armClient.PutResources(ctx, nil)
	assert.Nil(t, responses)
	responses = armClient.PutResources(ctx, resources)
	assert.NotNil(t, responses)
	assert.Equal(t, 3, total)
}

func getTestServer(t *testing.T, counter *int) *httptest.Server {
	serverFuncs := []func(rw http.ResponseWriter, req *http.Request){
		func(rw http.ResponseWriter, req *http.Request) {
			assert.Equal(t, "PUT", req.Method)

			rw.Header().Set("Azure-AsyncOperation",
				fmt.Sprintf("http://%s%s", req.Host, "/id/1?api-version=2019-01-01"))
			rw.WriteHeader(http.StatusCreated)
		},
		func(rw http.ResponseWriter, req *http.Request) {
			assert.Equal(t, "PUT", req.Method)

			rw.Header().Set("Azure-AsyncOperation",
				fmt.Sprintf("http://%s%s", req.Host, "/id/2?api-version=2019-01-01"))
			rw.WriteHeader(http.StatusInternalServerError)
		},
		func(rw http.ResponseWriter, req *http.Request) {
			assert.Equal(t, "GET", req.Method)

			rw.WriteHeader(http.StatusOK)
			_, _ = rw.Write([]byte(`{"error":{"code":"InternalServerError"},"status":"Failed"}`))
		},
		func(rw http.ResponseWriter, req *http.Request) {
			assert.Equal(t, "GET", req.Method)

			rw.WriteHeader(http.StatusOK)
			_, _ = rw.Write([]byte(`{"error":{"code":"InternalServerError"},"status":"Failed"}`))
		},
	}

	i := 0
	var l sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.Lock()
		serverFuncs[i](w, r)
		i++
		if i > 3 {
			i = 3
		}
		*counter++
		l.Unlock()
	}))
}

func TestPutResourcesInBatches(t *testing.T) {
	for _, testCase := range []struct {
		description                  string
		resources                    map[string]interface{}
		batchSize, expectedCallTimes int
	}{
		{
			description: "",
			resources: map[string]interface{}{
				"/id/1": nil,
				"/id/2": nil,
			},
			batchSize:         2,
			expectedCallTimes: 3,
		},
		{
			description: "",
			resources: map[string]interface{}{
				"/id/1": nil,
				"/id/2": nil,
			},
			batchSize:         1,
			expectedCallTimes: 3,
		},
		{
			description: "",
			resources:   nil,
		},
		{
			description: "PutResourcesInBatches should set the batch size to the length of the resources if the batch size is larger than it",
			resources: map[string]interface{}{
				"/id/1": nil,
				"/id/2": nil,
			},
			batchSize:         10,
			expectedCallTimes: 3,
		},
		{
			description: "PutResourcesInBatches should call PutResources if the batch size is smaller than or equal to zero",
			resources: map[string]interface{}{
				"/id/1": nil,
				"/id/2": nil,
			},
			expectedCallTimes: 3,
		},
	} {
		t.Run(testCase.description, func(t *testing.T) {
			total := 0
			server := getTestServer(t, &total)

			azConfig := azureclients.ClientConfig{Backoff: &retry.Backoff{Steps: 1}, UserAgent: "test", Location: "eastus"}
			armClient := New(nil, azConfig, server.URL, "2019-01-01")
			armClient.client.RetryDuration = time.Millisecond * 1

			ctx := context.Background()
			responses := armClient.PutResourcesInBatches(ctx, testCase.resources, testCase.batchSize)
			assert.Equal(t, testCase.resources == nil, responses == nil)
			assert.Equal(t, testCase.expectedCallTimes, total)
		})
	}

}

func TestPutResourceAsync(t *testing.T) {
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		http.Error(w, "failed", http.StatusInternalServerError)
	}))

	azConfig := azureclients.ClientConfig{Backoff: &retry.Backoff{Steps: 3}, UserAgent: "test", Location: "eastus"}
	armClient := New(nil, azConfig, server.URL, "2019-01-01")
	armClient.client.RetryDuration = time.Millisecond * 1

	ctx := context.Background()
	resourceID := testResourceID
	future, rerr := armClient.PutResourceAsync(ctx, resourceID, "")
	assert.Equal(t, 3, count)
	assert.Nil(t, future)
	assert.NotNil(t, rerr)
	assert.Equal(t, true, rerr.Retriable)
}

func TestDeleteResourceAsync(t *testing.T) {
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		http.Error(w, "failed", http.StatusInternalServerError)
	}))

	azConfig := azureclients.ClientConfig{Backoff: &retry.Backoff{Steps: 3}, UserAgent: "test", Location: "eastus"}
	armClient := New(nil, azConfig, server.URL, "2019-01-01")
	armClient.client.RetryDuration = time.Millisecond * 1

	ctx := context.Background()
	resourceID := testResourceID
	future, rerr := armClient.DeleteResourceAsync(ctx, resourceID, "")
	assert.Equal(t, 3, count)
	assert.Nil(t, future)
	assert.NotNil(t, rerr)
	assert.Equal(t, true, rerr.Retriable)
}

func TestPatchResource(t *testing.T) {
	handlers := []func(http.ResponseWriter, *http.Request){
		func(rw http.ResponseWriter, req *http.Request) {
			assert.Equal(t, "PATCH", req.Method)
			assert.Equal(t, expectedURI, req.URL.String())
			rw.Header().Set("Azure-AsyncOperation",
				fmt.Sprintf("http://%s%s", req.Host, operationURI))
			rw.WriteHeader(http.StatusCreated)
		},

		func(rw http.ResponseWriter, req *http.Request) {
			assert.Equal(t, "GET", req.Method)
			assert.Equal(t, operationURI, req.URL.String())

			rw.WriteHeader(http.StatusOK)
			_, _ = rw.Write([]byte(`{"error":{"code":"InternalServerError"},"status":"Failed"}`))
		},
	}

	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers[count](w, r)
		count++
		if count > 1 {
			count = 1
		}
	}))

	azConfig := azureclients.ClientConfig{Backoff: &retry.Backoff{Steps: 1}, UserAgent: "test", Location: "eastus"}
	armClient := New(nil, azConfig, server.URL, "2019-01-01")
	armClient.client.RetryDuration = time.Millisecond * 1

	ctx := context.Background()
	response, rerr := armClient.PatchResource(ctx, testResourceID, nil)
	assert.Equal(t, 1, count)
	assert.Nil(t, response)
	assert.NotNil(t, rerr)
	assert.Equal(t, true, rerr.Retriable)
}

func TestPatchResourceAsync(t *testing.T) {
	expectedURI := "/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP?api-version=2019-01-01"
	operationURI := "/subscriptions/subscription/providers/Microsoft.Network/locations/eastus/operations/op?api-version=2019-01-01"
	handlers := []func(http.ResponseWriter, *http.Request){
		func(rw http.ResponseWriter, req *http.Request) {
			assert.Equal(t, "PATCH", req.Method)
			assert.Equal(t, expectedURI, req.URL.String())
			rw.Header().Set("Azure-AsyncOperation",
				fmt.Sprintf("http://%s%s", req.Host, operationURI))
			rw.WriteHeader(http.StatusCreated)
		},

		func(rw http.ResponseWriter, req *http.Request) {
			assert.Equal(t, "GET", req.Method)
			assert.Equal(t, operationURI, req.URL.String())

			rw.WriteHeader(http.StatusOK)
			_, _ = rw.Write([]byte(`{"error":{"code":"InternalServerError"},"status":"Failed"}`))
		},
	}

	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers[count](w, r)
		count++
		if count > 1 {
			count = 1
		}
	}))

	azConfig := azureclients.ClientConfig{Backoff: &retry.Backoff{Steps: 3}, UserAgent: "test", Location: "eastus"}
	armClient := New(nil, azConfig, server.URL, "2019-01-01")
	armClient.client.RetryDuration = time.Millisecond * 1

	ctx := context.Background()
	future, rerr := armClient.PatchResourceAsync(ctx, testResourceID, nil)
	assert.Equal(t, 1, count)
	assert.NotNil(t, future)
	assert.Nil(t, rerr)
}

func TestPostResource(t *testing.T) {
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		http.Error(w, "failed", http.StatusInternalServerError)
	}))
	azConfig := azureclients.ClientConfig{Backoff: &retry.Backoff{Steps: 3}, UserAgent: "test", Location: "eastus"}
	armClient := New(nil, azConfig, server.URL, "2019-01-01")
	armClient.client.RetryDuration = time.Millisecond * 1

	ctx := context.Background()
	resourceID := testResourceID
	future, rerr := armClient.PostResource(ctx, resourceID, "post", "", map[string]interface{}{})
	assert.Equal(t, 3, count)
	assert.NotNil(t, future)
	assert.NotNil(t, rerr)
	assert.Equal(t, true, rerr.Retriable)
}

func TestDeleteResource(t *testing.T) {
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		http.Error(w, "failed", http.StatusInternalServerError)
	}))

	azConfig := azureclients.ClientConfig{Backoff: &retry.Backoff{Steps: 3}, Location: "eastus"}
	armClient := New(nil, azConfig, server.URL, "2019-01-01")
	armClient.client.RetryDuration = time.Millisecond * 1

	ctx := context.Background()
	resourceID := testResourceID
	rerr := armClient.DeleteResource(ctx, resourceID, "")
	assert.Equal(t, 3, count)
	assert.NotNil(t, rerr)
	assert.Equal(t, true, rerr.Retriable)
}

func TestHeadResource(t *testing.T) {
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		http.Error(w, "failed", http.StatusInternalServerError)
	}))

	azConfig := azureclients.ClientConfig{Backoff: &retry.Backoff{Steps: 3}, Location: "eastus"}
	armClient := New(nil, azConfig, server.URL, "2019-01-01")
	armClient.client.RetryDuration = time.Millisecond * 1

	ctx := context.Background()
	resourceID := testResourceID
	response, rerr := armClient.HeadResource(ctx, resourceID)
	assert.Equal(t, 3, count)
	assert.NotNil(t, response)
	assert.NotNil(t, rerr)
	assert.Equal(t, true, rerr.Retriable)
}

func TestGetResourceID(t *testing.T) {
	expectedResourceID := "/subscriptions/sub/resourceGroups/rg/providers/type/name"

	resourceID := GetResourceID("sub", "rg", "type", "name")
	assert.Equal(t, expectedResourceID, resourceID)
}

func TestGetUserAgent(t *testing.T) {
	armClient := New(nil, azureclients.ClientConfig{}, "", "2019-01-01")
	assert.Contains(t, armClient.client.UserAgent, "kubernetes-cloudprovider")

	userAgent := GetUserAgent(armClient.client)
	assert.Contains(t, userAgent, armClient.client.UserAgent)
}

func TestGetSender(t *testing.T) {
	sender := getSender()
	j, _ := cookiejar.New(nil)
	assert.Equal(t, j, sender.(*http.Client).Jar)
	assert.Equal(t, commTransport, sender.(*http.Client).Transport)
}

func TestGetChildResourceID(t *testing.T) {
	expectedResourceID := "/subscriptions/sub/resourceGroups/rg/providers/type/name-1/name-2/name-3"

	resourceID := GetChildResourceID("sub", "rg", "type", "name-1", "name-2", "name-3")
	assert.Equal(t, expectedResourceID, resourceID)
}

func TestGetChildResourcesListID(t *testing.T) {
	expectedResourceID := "/subscriptions/sub/resourceGroups/rg/providers/type/name-1/name-2"

	resourceID := GetChildResourcesListID("sub", "rg", "type", "name-1", "name-2")
	assert.Equal(t, expectedResourceID, resourceID)
}

func TestGetProviderResourceID(t *testing.T) {
	expectedResourceID := "/subscriptions/sub/providers/namespace"

	resourceID := GetProviderResourceID("sub", "namespace")
	assert.Equal(t, expectedResourceID, resourceID)
}

func TestGetProviderResourcesListID(t *testing.T) {
	expectedResourceID := "/subscriptions/sub/providers"

	resourceID := GetProviderResourcesListID("sub")
	assert.Equal(t, expectedResourceID, resourceID)
}
