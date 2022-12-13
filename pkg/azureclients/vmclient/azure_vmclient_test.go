/*
Copyright 2020 The Kubernetes Authors.

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

package vmclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-03-01/compute"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	"k8s.io/client-go/util/flowcontrol"

	azclients "sigs.k8s.io/cloud-provider-azure/pkg/azureclients"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/armclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/armclient/mockarmclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

const (
	testResourceID                      = "/subscriptions/subscriptionID/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1"
	testResourcePrefix                  = "/subscriptions/subscriptionID/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines"
	testSubscriptionLevelResourcePrefix = "/subscriptions/subscriptionID/providers/Microsoft.Compute/virtualMachines"
)

func TestNew(t *testing.T) {
	config := &azclients.ClientConfig{
		SubscriptionID:          "sub",
		ResourceManagerEndpoint: "endpoint",
		Location:                "eastus",
		RateLimitConfig: &azclients.RateLimitConfig{
			CloudProviderRateLimit:            true,
			CloudProviderRateLimitQPS:         0.5,
			CloudProviderRateLimitBucket:      1,
			CloudProviderRateLimitQPSWrite:    0.5,
			CloudProviderRateLimitBucketWrite: 1,
		},
		Backoff: &retry.Backoff{Steps: 1},
	}

	vmClient := New(config)
	assert.Equal(t, "sub", vmClient.subscriptionID)
	assert.NotEmpty(t, vmClient.rateLimiterReader)
	assert.NotEmpty(t, vmClient.rateLimiterWriter)
}

func TestNewAzureStack(t *testing.T) {
	config := &azclients.ClientConfig{
		CloudName:               "AZURESTACKCLOUD",
		SubscriptionID:          "sub",
		ResourceManagerEndpoint: "endpoint",
		Location:                "eastus",
		RateLimitConfig: &azclients.RateLimitConfig{
			CloudProviderRateLimit:            true,
			CloudProviderRateLimitQPS:         0.5,
			CloudProviderRateLimitBucket:      1,
			CloudProviderRateLimitQPSWrite:    0.5,
			CloudProviderRateLimitBucketWrite: 1,
		},
		Backoff: &retry.Backoff{Steps: 1},
	}

	vmClient := New(config)
	assert.Equal(t, "AZURESTACKCLOUD", vmClient.cloudName)
	assert.Equal(t, "sub", vmClient.subscriptionID)
}

func TestGet(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusOK,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte("{}"))),
	}

	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().GetResourceWithExpandQuery(gomock.Any(), testResourceID, "InstanceView").Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	expected := compute.VirtualMachine{Response: autorest.Response{Response: response}}
	vmClient := getTestVMClient(armClient)
	result, rerr := vmClient.Get(context.TODO(), "rg", "vm1", "InstanceView")
	assert.Equal(t, expected, result)
	assert.Nil(t, rerr)
}

func TestGetNeverRateLimiter(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	vmGetErr := &retry.Error{
		RawError:  fmt.Errorf("azure cloud provider rate limited(%s) for operation %q", "read", "VMGet"),
		Retriable: true,
	}

	armClient := mockarmclient.NewMockInterface(ctrl)
	vmClient := getTestVMClientWithNeverRateLimiter(armClient)
	expected := compute.VirtualMachine{}
	result, rerr := vmClient.Get(context.TODO(), "rg", "vm1", "InstanceView")
	assert.Equal(t, expected, result)
	assert.Equal(t, vmGetErr, rerr)
}

func TestGetRetryAfterReader(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	vmGetErr := &retry.Error{
		RawError:   fmt.Errorf("azure cloud provider throttled for operation %s with reason %q", "VMGet", "client throttled"),
		Retriable:  true,
		RetryAfter: getFutureTime(),
	}

	armClient := mockarmclient.NewMockInterface(ctrl)
	vmClient := getTestVMClientWithRetryAfterReader(armClient)
	expected := compute.VirtualMachine{}
	result, rerr := vmClient.Get(context.TODO(), "rg", "vm1", "InstanceView")
	assert.Equal(t, expected, result)
	assert.Equal(t, vmGetErr, rerr)
}

func TestGetNotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().GetResourceWithExpandQuery(gomock.Any(), testResourceID, "InstanceView").Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	vmClient := getTestVMClient(armClient)
	expectedVM := compute.VirtualMachine{Response: autorest.Response{}}
	result, rerr := vmClient.Get(context.TODO(), "rg", "vm1", "InstanceView")
	assert.Equal(t, expectedVM, result)
	assert.NotNil(t, rerr)
	assert.Equal(t, http.StatusNotFound, rerr.HTTPStatusCode)
}

func TestGetInternalError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().GetResourceWithExpandQuery(gomock.Any(), testResourceID, "InstanceView").Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	vmClient := getTestVMClient(armClient)
	expectedVM := compute.VirtualMachine{Response: autorest.Response{}}
	result, rerr := vmClient.Get(context.TODO(), "rg", "vm1", "InstanceView")
	assert.Equal(t, expectedVM, result)
	assert.NotNil(t, rerr)
	assert.Equal(t, http.StatusInternalServerError, rerr.HTTPStatusCode)
}

func TestGetThrottle(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	throttleErr := &retry.Error{
		HTTPStatusCode: http.StatusTooManyRequests,
		RawError:       fmt.Errorf("error"),
		Retriable:      true,
		RetryAfter:     time.Unix(100, 0),
	}
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().GetResourceWithExpandQuery(gomock.Any(), testResourceID, "InstanceView").Return(response, throttleErr).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	vmClient := getTestVMClient(armClient)
	result, rerr := vmClient.Get(context.TODO(), "rg", "vm1", "InstanceView")
	assert.Empty(t, result)
	assert.Equal(t, throttleErr, rerr)
}

func TestList(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	armClient := mockarmclient.NewMockInterface(ctrl)
	vmList := []compute.VirtualMachine{getTestVM("vm1"), getTestVM("vm1"), getTestVM("vm1")}
	responseBody, err := json.Marshal(compute.VirtualMachineListResult{Value: &vmList})
	assert.NoError(t, err)
	armClient.EXPECT().GetResource(gomock.Any(), testResourcePrefix).Return(
		&http.Response{
			StatusCode: http.StatusOK,
			Body:       ioutil.NopCloser(bytes.NewReader(responseBody)),
		}, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	vmClient := getTestVMClient(armClient)
	result, rerr := vmClient.List(context.TODO(), "rg")
	assert.Nil(t, rerr)
	assert.Equal(t, 3, len(result))
}

func TestListNotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().GetResource(gomock.Any(), testResourcePrefix).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	vmClient := getTestVMClient(armClient)
	expected := []compute.VirtualMachine{}
	result, rerr := vmClient.List(context.TODO(), "rg")
	assert.Equal(t, expected, result)
	assert.NotNil(t, rerr)
	assert.Equal(t, http.StatusNotFound, rerr.HTTPStatusCode)
}

func TestListInternalError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().GetResource(gomock.Any(), testResourcePrefix).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	vmClient := getTestVMClient(armClient)
	expected := []compute.VirtualMachine{}
	result, rerr := vmClient.List(context.TODO(), "rg")
	assert.Equal(t, expected, result)
	assert.NotNil(t, rerr)
	assert.Equal(t, http.StatusInternalServerError, rerr.HTTPStatusCode)
}

func TestListThrottle(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	armClient := mockarmclient.NewMockInterface(ctrl)
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	throttleErr := &retry.Error{
		HTTPStatusCode: http.StatusTooManyRequests,
		RawError:       fmt.Errorf("error"),
		Retriable:      true,
		RetryAfter:     time.Unix(100, 0),
	}
	armClient.EXPECT().GetResource(gomock.Any(), testResourcePrefix).Return(response, throttleErr).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)
	vmClient := getTestVMClient(armClient)
	result, rerr := vmClient.List(context.TODO(), "rg")
	assert.Empty(t, result)
	assert.NotNil(t, rerr)
	assert.Equal(t, throttleErr, rerr)
}

func TestListWithListResponderError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	armClient := mockarmclient.NewMockInterface(ctrl)
	vmList := []compute.VirtualMachine{getTestVM("vm1"), getTestVM("vm2"), getTestVM("vm3")}
	responseBody, err := json.Marshal(compute.VirtualMachineListResult{Value: &vmList})
	assert.NoError(t, err)
	armClient.EXPECT().GetResource(gomock.Any(), testResourcePrefix).Return(
		&http.Response{
			StatusCode: http.StatusNotFound,
			Body:       ioutil.NopCloser(bytes.NewReader(responseBody)),
		}, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)
	vmClient := getTestVMClient(armClient)
	result, rerr := vmClient.List(context.TODO(), "rg")
	assert.NotNil(t, rerr)
	assert.Equal(t, 0, len(result))
}

func TestListWithNextPage(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	armClient := mockarmclient.NewMockInterface(ctrl)
	vmList := []compute.VirtualMachine{getTestVM("vm1"), getTestVM("vm2"), getTestVM("vm3")}
	partialResponse, err := json.Marshal(compute.VirtualMachineListResult{Value: &vmList, NextLink: to.StringPtr("nextLink")})
	assert.NoError(t, err)
	pagedResponse, err := json.Marshal(compute.VirtualMachineListResult{Value: &vmList})
	assert.NoError(t, err)
	armClient.EXPECT().PrepareGetRequest(gomock.Any(), gomock.Any()).Return(&http.Request{}, nil)
	armClient.EXPECT().Send(gomock.Any(), gomock.Any()).Return(
		&http.Response{
			StatusCode: http.StatusOK,
			Body:       ioutil.NopCloser(bytes.NewReader(pagedResponse)),
		}, nil)
	armClient.EXPECT().GetResource(gomock.Any(), testResourcePrefix).Return(
		&http.Response{
			StatusCode: http.StatusOK,
			Body:       ioutil.NopCloser(bytes.NewReader(partialResponse)),
		}, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(2)
	vmClient := getTestVMClient(armClient)
	result, rerr := vmClient.List(context.TODO(), "rg")
	assert.Nil(t, rerr)
	assert.Equal(t, 6, len(result))
}

func TestListNeverRateLimiter(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	vmListErr := &retry.Error{
		RawError:  fmt.Errorf("azure cloud provider rate limited(%s) for operation %q", "read", "VMList"),
		Retriable: true,
	}

	armClient := mockarmclient.NewMockInterface(ctrl)
	vmClient := getTestVMClientWithNeverRateLimiter(armClient)
	result, rerr := vmClient.List(context.TODO(), "rg")
	assert.Equal(t, 0, len(result))
	assert.NotNil(t, rerr)
	assert.Equal(t, vmListErr, rerr)
}

func TestListRetryAfterReader(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	vmListErr := &retry.Error{
		RawError:   fmt.Errorf("azure cloud provider throttled for operation %s with reason %q", "VMList", "client throttled"),
		Retriable:  true,
		RetryAfter: getFutureTime(),
	}

	armClient := mockarmclient.NewMockInterface(ctrl)
	vmClient := getTestVMClientWithRetryAfterReader(armClient)
	result, rerr := vmClient.List(context.TODO(), "rg")
	assert.Equal(t, 0, len(result))
	assert.NotNil(t, rerr)
	assert.Equal(t, vmListErr, rerr)
}

func TestListNextResultsMultiPages(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tests := []struct {
		name           string
		prepareErr     error
		sendErr        *retry.Error
		expectedErrMsg string
	}{
		{
			name:       "testlistNextResultsSuccessful",
			prepareErr: nil,
			sendErr:    nil,
		},
		{
			name:           "testPrepareGetRequestError",
			prepareErr:     fmt.Errorf("error"),
			expectedErrMsg: "Failure preparing next results request",
		},
		{
			name:           "testSendError",
			sendErr:        &retry.Error{RawError: fmt.Errorf("error")},
			expectedErrMsg: "Failure sending next results request",
		},
	}

	lastResult := compute.VirtualMachineListResult{
		NextLink: to.StringPtr("next"),
	}

	for _, test := range tests {
		armClient := mockarmclient.NewMockInterface(ctrl)
		req := &http.Request{
			Method: "GET",
		}
		armClient.EXPECT().PrepareGetRequest(gomock.Any(), gomock.Any()).Return(req, test.prepareErr)
		if test.prepareErr == nil {
			armClient.EXPECT().Send(gomock.Any(), req).Return(&http.Response{
				StatusCode: http.StatusOK,
				Body:       ioutil.NopCloser(bytes.NewReader([]byte(`{"foo":"bar"}`))),
			}, test.sendErr)
			armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any())
		}

		vmssClient := getTestVMClient(armClient)
		result, err := vmssClient.listNextResults(context.TODO(), lastResult)
		if err != nil {
			detailedErr := &autorest.DetailedError{}
			assert.True(t, errors.As(err, detailedErr))
			assert.Equal(t, detailedErr.Message, test.expectedErrMsg)
		} else {
			assert.NoError(t, err)
		}

		if test.prepareErr != nil {
			assert.Empty(t, result)
		} else {
			assert.NotEmpty(t, result)
		}
	}
}

func TestListNextResultsMultiPagesWithListResponderError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tests := []struct {
		name       string
		prepareErr error
		sendErr    *retry.Error
	}{
		{
			name:       "testListResponderError",
			prepareErr: nil,
			sendErr:    nil,
		},
		{
			name:    "testSendError",
			sendErr: &retry.Error{RawError: fmt.Errorf("error")},
		},
	}

	lastResult := compute.VirtualMachineListResult{
		NextLink: to.StringPtr("next"),
	}

	for _, test := range tests {
		armClient := mockarmclient.NewMockInterface(ctrl)
		req := &http.Request{
			Method: "GET",
		}
		armClient.EXPECT().PrepareGetRequest(gomock.Any(), gomock.Any()).Return(req, test.prepareErr)
		if test.prepareErr == nil {
			armClient.EXPECT().Send(gomock.Any(), req).Return(&http.Response{
				StatusCode: http.StatusNotFound,
				Body:       ioutil.NopCloser(bytes.NewReader([]byte(`{"foo":"bar"}`))),
			}, test.sendErr)
			armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any())
		}

		response := &http.Response{
			StatusCode: http.StatusNotFound,
			Body:       ioutil.NopCloser(bytes.NewBuffer([]byte(`{"foo":"bar"}`))),
		}
		expected := compute.VirtualMachineListResult{}
		expected.Response = autorest.Response{Response: response}
		vmssClient := getTestVMClient(armClient)
		result, err := vmssClient.listNextResults(context.TODO(), lastResult)
		assert.Error(t, err)
		if test.sendErr != nil {
			assert.NotEqual(t, expected, result)
		} else {
			assert.Equal(t, expected, result)
		}
	}
}

func TestListVmssFlexVMsWithoutInstanceView(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	armClient := mockarmclient.NewMockInterface(ctrl)
	vmList := []compute.VirtualMachine{getTestVM("vm1"), getTestVM("vm1"), getTestVM("vm1")}
	responseBody, err := json.Marshal(compute.VirtualMachineListResult{Value: &vmList})
	assert.NoError(t, err)
	armClient.EXPECT().GetResourceWithQueries(gomock.Any(), testSubscriptionLevelResourcePrefix, gomock.Any()).Return(
		&http.Response{
			StatusCode: http.StatusOK,
			Body:       ioutil.NopCloser(bytes.NewReader(responseBody)),
		}, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	vmClient := getTestVMClient(armClient)
	result, rerr := vmClient.ListVmssFlexVMsWithoutInstanceView(context.TODO(), "vmssFlexID")
	assert.Nil(t, rerr)
	assert.Equal(t, 3, len(result))
}

func TestListVmssFlexVMsWithoutInstanceViewNotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().GetResourceWithQueries(gomock.Any(), testSubscriptionLevelResourcePrefix, gomock.Any()).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	vmClient := getTestVMClient(armClient)
	expected := []compute.VirtualMachine{}
	result, rerr := vmClient.ListVmssFlexVMsWithoutInstanceView(context.TODO(), "vmssFlexID")
	assert.Equal(t, expected, result)
	assert.NotNil(t, rerr)
	assert.Equal(t, http.StatusNotFound, rerr.HTTPStatusCode)
}

func TestListVmssFlexVMsWithoutInstanceViewNInternalError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().GetResourceWithQueries(gomock.Any(), testSubscriptionLevelResourcePrefix, gomock.Any()).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	vmClient := getTestVMClient(armClient)
	expected := []compute.VirtualMachine{}
	result, rerr := vmClient.ListVmssFlexVMsWithoutInstanceView(context.TODO(), "vmssFlexID")
	assert.Equal(t, expected, result)
	assert.NotNil(t, rerr)
	assert.Equal(t, http.StatusInternalServerError, rerr.HTTPStatusCode)
}

func TestListVmssFlexVMsWithoutInstanceViewThrottle(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	armClient := mockarmclient.NewMockInterface(ctrl)
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	throttleErr := &retry.Error{
		HTTPStatusCode: http.StatusTooManyRequests,
		RawError:       fmt.Errorf("error"),
		Retriable:      true,
		RetryAfter:     time.Unix(100, 0),
	}
	armClient.EXPECT().GetResourceWithQueries(gomock.Any(), testSubscriptionLevelResourcePrefix, gomock.Any()).Return(response, throttleErr).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)
	vmClient := getTestVMClient(armClient)
	result, rerr := vmClient.ListVmssFlexVMsWithoutInstanceView(context.TODO(), "vmssFlexID")
	assert.Empty(t, result)
	assert.NotNil(t, rerr)
	assert.Equal(t, throttleErr, rerr)
}

func TestListVmssFlexVMsWithoutInstanceViewWithListResponderError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	armClient := mockarmclient.NewMockInterface(ctrl)
	vmList := []compute.VirtualMachine{getTestVM("vm1"), getTestVM("vm2"), getTestVM("vm3")}
	responseBody, err := json.Marshal(compute.VirtualMachineListResult{Value: &vmList})
	assert.NoError(t, err)
	armClient.EXPECT().GetResourceWithQueries(gomock.Any(), testSubscriptionLevelResourcePrefix, gomock.Any()).Return(
		&http.Response{
			StatusCode: http.StatusNotFound,
			Body:       ioutil.NopCloser(bytes.NewReader(responseBody)),
		}, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)
	vmClient := getTestVMClient(armClient)
	result, rerr := vmClient.ListVmssFlexVMsWithoutInstanceView(context.TODO(), "vmssFlexID")
	assert.NotNil(t, rerr)
	assert.Equal(t, 0, len(result))
}

func TestListVmssFlexVMsWithoutInstanceViewWithNextPage(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	armClient := mockarmclient.NewMockInterface(ctrl)
	vmList := []compute.VirtualMachine{getTestVM("vm1"), getTestVM("vm2"), getTestVM("vm3")}
	partialResponse, err := json.Marshal(compute.VirtualMachineListResult{Value: &vmList, NextLink: to.StringPtr("nextLink")})
	assert.NoError(t, err)
	pagedResponse, err := json.Marshal(compute.VirtualMachineListResult{Value: &vmList})
	assert.NoError(t, err)
	armClient.EXPECT().PrepareGetRequest(gomock.Any(), gomock.Any()).Return(&http.Request{}, nil)
	armClient.EXPECT().Send(gomock.Any(), gomock.Any()).Return(
		&http.Response{
			StatusCode: http.StatusOK,
			Body:       ioutil.NopCloser(bytes.NewReader(pagedResponse)),
		}, nil)
	armClient.EXPECT().GetResourceWithQueries(gomock.Any(), testSubscriptionLevelResourcePrefix, gomock.Any()).Return(
		&http.Response{
			StatusCode: http.StatusOK,
			Body:       ioutil.NopCloser(bytes.NewReader(partialResponse)),
		}, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(2)
	vmClient := getTestVMClient(armClient)
	result, rerr := vmClient.ListVmssFlexVMsWithoutInstanceView(context.TODO(), "vmssFlexID")
	assert.Nil(t, rerr)
	assert.Equal(t, 6, len(result))
}

func TestListVmssFlexVMsWithoutInstanceViewNeverRateLimiter(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	vmListErr := &retry.Error{
		RawError:  fmt.Errorf("azure cloud provider rate limited(%s) for operation %q", "read", "VMList"),
		Retriable: true,
	}

	armClient := mockarmclient.NewMockInterface(ctrl)
	vmClient := getTestVMClientWithNeverRateLimiter(armClient)
	result, rerr := vmClient.ListVmssFlexVMsWithoutInstanceView(context.TODO(), "vmssFlexID")
	assert.Equal(t, 0, len(result))
	assert.NotNil(t, rerr)
	assert.Equal(t, vmListErr, rerr)
}

func TestListVmssFlexVMsWithoutInstanceViewRetryAfterReader(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	vmListErr := &retry.Error{
		RawError:   fmt.Errorf("azure cloud provider throttled for operation %s with reason %q", "VMList", "client throttled"),
		Retriable:  true,
		RetryAfter: getFutureTime(),
	}

	armClient := mockarmclient.NewMockInterface(ctrl)
	vmClient := getTestVMClientWithRetryAfterReader(armClient)
	result, rerr := vmClient.ListVmssFlexVMsWithoutInstanceView(context.TODO(), "vmssFlexID")
	assert.Equal(t, 0, len(result))
	assert.NotNil(t, rerr)
	assert.Equal(t, vmListErr, rerr)
}

func TestListVmssFlexVMsWithOnlyInstanceView(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	armClient := mockarmclient.NewMockInterface(ctrl)
	vmList := []compute.VirtualMachine{getTestVM("vm1"), getTestVM("vm1"), getTestVM("vm1")}
	responseBody, err := json.Marshal(compute.VirtualMachineListResult{Value: &vmList})
	assert.NoError(t, err)
	armClient.EXPECT().GetResourceWithQueries(gomock.Any(), testSubscriptionLevelResourcePrefix, gomock.Any()).Return(
		&http.Response{
			StatusCode: http.StatusOK,
			Body:       ioutil.NopCloser(bytes.NewReader(responseBody)),
		}, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	vmClient := getTestVMClient(armClient)
	result, rerr := vmClient.ListVmssFlexVMsWithOnlyInstanceView(context.TODO(), "vmssFlexID")
	assert.Nil(t, rerr)
	assert.Equal(t, 3, len(result))
}

func TestListVmssFlexVMsWithOnlyInstanceViewNotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().GetResourceWithQueries(gomock.Any(), testSubscriptionLevelResourcePrefix, gomock.Any()).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	vmClient := getTestVMClient(armClient)
	expected := []compute.VirtualMachine{}
	result, rerr := vmClient.ListVmssFlexVMsWithOnlyInstanceView(context.TODO(), "vmssFlexID")
	assert.Equal(t, expected, result)
	assert.NotNil(t, rerr)
	assert.Equal(t, http.StatusNotFound, rerr.HTTPStatusCode)
}

func TestListVmssFlexVMsWithOnlyInstanceViewNInternalError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().GetResourceWithQueries(gomock.Any(), testSubscriptionLevelResourcePrefix, gomock.Any()).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	vmClient := getTestVMClient(armClient)
	expected := []compute.VirtualMachine{}
	result, rerr := vmClient.ListVmssFlexVMsWithOnlyInstanceView(context.TODO(), "vmssFlexID")
	assert.Equal(t, expected, result)
	assert.NotNil(t, rerr)
	assert.Equal(t, http.StatusInternalServerError, rerr.HTTPStatusCode)
}

func TestListVmssFlexVMsWithOnlyInstanceViewThrottle(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	armClient := mockarmclient.NewMockInterface(ctrl)
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	throttleErr := &retry.Error{
		HTTPStatusCode: http.StatusTooManyRequests,
		RawError:       fmt.Errorf("error"),
		Retriable:      true,
		RetryAfter:     time.Unix(100, 0),
	}
	armClient.EXPECT().GetResourceWithQueries(gomock.Any(), testSubscriptionLevelResourcePrefix, gomock.Any()).Return(response, throttleErr).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)
	vmClient := getTestVMClient(armClient)
	result, rerr := vmClient.ListVmssFlexVMsWithOnlyInstanceView(context.TODO(), "vmssFlexID")
	assert.Empty(t, result)
	assert.NotNil(t, rerr)
	assert.Equal(t, throttleErr, rerr)
}

func TestListVmssFlexVMsWithOnlyInstanceViewWithListResponderError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	armClient := mockarmclient.NewMockInterface(ctrl)
	vmList := []compute.VirtualMachine{getTestVM("vm1"), getTestVM("vm2"), getTestVM("vm3")}
	responseBody, err := json.Marshal(compute.VirtualMachineListResult{Value: &vmList})
	assert.NoError(t, err)
	armClient.EXPECT().GetResourceWithQueries(gomock.Any(), testSubscriptionLevelResourcePrefix, gomock.Any()).Return(
		&http.Response{
			StatusCode: http.StatusNotFound,
			Body:       ioutil.NopCloser(bytes.NewReader(responseBody)),
		}, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)
	vmClient := getTestVMClient(armClient)
	result, rerr := vmClient.ListVmssFlexVMsWithOnlyInstanceView(context.TODO(), "vmssFlexID")
	assert.NotNil(t, rerr)
	assert.Equal(t, 0, len(result))
}

func TestListVmssFlexVMsWithOnlyInstanceViewWithNextPage(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	armClient := mockarmclient.NewMockInterface(ctrl)
	vmList := []compute.VirtualMachine{getTestVM("vm1"), getTestVM("vm2"), getTestVM("vm3")}
	partialResponse, err := json.Marshal(compute.VirtualMachineListResult{Value: &vmList, NextLink: to.StringPtr("nextLink")})
	assert.NoError(t, err)
	pagedResponse, err := json.Marshal(compute.VirtualMachineListResult{Value: &vmList})
	assert.NoError(t, err)
	armClient.EXPECT().PrepareGetRequest(gomock.Any(), gomock.Any()).Return(&http.Request{}, nil)
	armClient.EXPECT().Send(gomock.Any(), gomock.Any()).Return(
		&http.Response{
			StatusCode: http.StatusOK,
			Body:       ioutil.NopCloser(bytes.NewReader(pagedResponse)),
		}, nil)
	armClient.EXPECT().GetResourceWithQueries(gomock.Any(), testSubscriptionLevelResourcePrefix, gomock.Any()).Return(
		&http.Response{
			StatusCode: http.StatusOK,
			Body:       ioutil.NopCloser(bytes.NewReader(partialResponse)),
		}, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(2)
	vmClient := getTestVMClient(armClient)
	result, rerr := vmClient.ListVmssFlexVMsWithOnlyInstanceView(context.TODO(), "vmssFlexID")
	assert.Nil(t, rerr)
	assert.Equal(t, 6, len(result))
}

func TestListVmssFlexVMsWithOnlyInstanceViewNeverRateLimiter(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	vmListErr := &retry.Error{
		RawError:  fmt.Errorf("azure cloud provider rate limited(%s) for operation %q", "read", "VMList"),
		Retriable: true,
	}

	armClient := mockarmclient.NewMockInterface(ctrl)
	vmClient := getTestVMClientWithNeverRateLimiter(armClient)
	result, rerr := vmClient.ListVmssFlexVMsWithOnlyInstanceView(context.TODO(), "vmssFlexID")
	assert.Equal(t, 0, len(result))
	assert.NotNil(t, rerr)
	assert.Equal(t, vmListErr, rerr)
}

func TestListVmssFlexVMsWithOnlyInstanceViewRetryAfterReader(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	vmListErr := &retry.Error{
		RawError:   fmt.Errorf("azure cloud provider throttled for operation %s with reason %q", "VMList", "client throttled"),
		Retriable:  true,
		RetryAfter: getFutureTime(),
	}

	armClient := mockarmclient.NewMockInterface(ctrl)
	vmClient := getTestVMClientWithRetryAfterReader(armClient)
	result, rerr := vmClient.ListVmssFlexVMsWithOnlyInstanceView(context.TODO(), "vmssFlexID")
	assert.Equal(t, 0, len(result))
	assert.NotNil(t, rerr)
	assert.Equal(t, vmListErr, rerr)
}

func TestUpdate(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testVM := compute.VirtualMachineUpdate{}
	armClient := mockarmclient.NewMockInterface(ctrl)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte(""))),
	}
	armClient.EXPECT().PatchResource(gomock.Any(), testResourceID, testVM).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	vmClient := getTestVMClient(armClient)
	_, rerr := vmClient.Update(context.TODO(), "rg", "vm1", testVM, "test")
	assert.Nil(t, rerr)
}

func TestUpdateAsync(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testVM := compute.VirtualMachineUpdate{}
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().PatchResourceAsync(gomock.Any(), testResourceID, testVM).Times(1)

	vmClient := getTestVMClient(armClient)
	future, rerr := vmClient.UpdateAsync(context.TODO(), "rg", "vm1", testVM, "test")
	assert.Nil(t, future)
	assert.Nil(t, rerr)
}

func TestWaitForUpdateResult(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	preemptErr := fmt.Errorf("operation execution has been preempted by a more recent operation")
	response := &http.Response{
		StatusCode: http.StatusOK,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte(""))),
	}

	tests := []struct {
		name           string
		response       *http.Response
		responseErr    error
		expectedResult *retry.Error
	}{
		{
			name:           "Success",
			response:       response,
			responseErr:    nil,
			expectedResult: nil,
		},
		{
			name:           "Success with nil response",
			response:       nil,
			responseErr:    nil,
			expectedResult: nil,
		},
		{
			name:           "Failed",
			response:       response,
			responseErr:    preemptErr,
			expectedResult: retry.GetError(response, preemptErr),
		},
		{
			name:           "Failed with nil response",
			response:       nil,
			responseErr:    preemptErr,
			expectedResult: retry.GetError(nil, preemptErr),
		},
	}

	for _, test := range tests {
		armClient := mockarmclient.NewMockInterface(ctrl)
		armClient.EXPECT().WaitForAsyncOperationResult(gomock.Any(), gomock.Any(), "VMWaitForUpdateResult").Return(test.response, test.responseErr).Times(1)
		armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

		vmClient := getTestVMClient(armClient)
		_, err := vmClient.WaitForUpdateResult(context.TODO(), &azure.Future{}, "rg", "test")
		assert.Equal(t, err, test.expectedResult)
	}
}

func TestUpdateWithUpdateResponderError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testVM := compute.VirtualMachineUpdate{}
	armClient := mockarmclient.NewMockInterface(ctrl)
	response := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte(""))),
	}
	armClient.EXPECT().PatchResource(gomock.Any(), testResourceID, testVM).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	vmClient := getTestVMClient(armClient)
	_, rerr := vmClient.Update(context.TODO(), "rg", "vm1", testVM, "test")
	assert.NotNil(t, rerr)
}

func TestUpdateNeverRateLimiter(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	vmUpdateErr := &retry.Error{
		RawError:  fmt.Errorf("azure cloud provider rate limited(%s) for operation %q", "write", "VMUpdate"),
		Retriable: true,
	}

	armClient := mockarmclient.NewMockInterface(ctrl)
	vmClient := getTestVMClientWithNeverRateLimiter(armClient)
	testVM := compute.VirtualMachineUpdate{}
	_, rerr := vmClient.Update(context.TODO(), "rg", "vm1", testVM, "test")
	assert.NotNil(t, rerr)
	assert.Equal(t, vmUpdateErr, rerr)
}

func TestUpdateRetryAfterReader(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	vmUpdateErr := &retry.Error{
		RawError:   fmt.Errorf("azure cloud provider throttled for operation %s with reason %q", "VMUpdate", "client throttled"),
		Retriable:  true,
		RetryAfter: getFutureTime(),
	}

	testVM := compute.VirtualMachineUpdate{}
	armClient := mockarmclient.NewMockInterface(ctrl)
	vmClient := getTestVMClientWithRetryAfterReader(armClient)
	_, rerr := vmClient.Update(context.TODO(), "rg", "vm1", testVM, "test")
	assert.NotNil(t, rerr)
	assert.Equal(t, vmUpdateErr, rerr)
}

func TestUpdateThrottle(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	throttleErr := &retry.Error{
		HTTPStatusCode: http.StatusTooManyRequests,
		RawError:       fmt.Errorf("error"),
		Retriable:      true,
		RetryAfter:     time.Unix(100, 0),
	}

	testVM := compute.VirtualMachineUpdate{}
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().PatchResource(gomock.Any(), testResourceID, testVM).Return(response, throttleErr).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	vmClient := getTestVMClient(armClient)
	_, rerr := vmClient.Update(context.TODO(), "rg", "vm1", testVM, "test")
	assert.NotNil(t, rerr)
	assert.Equal(t, throttleErr, rerr)
}

func TestCreateOrUpdate(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testVM := getTestVM("vm1")
	armClient := mockarmclient.NewMockInterface(ctrl)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte(""))),
	}
	armClient.EXPECT().PutResource(gomock.Any(), to.String(testVM.ID), testVM).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	vmClient := getTestVMClient(armClient)
	rerr := vmClient.CreateOrUpdate(context.TODO(), "rg", "vm1", testVM, "test")
	assert.Nil(t, rerr)
}

func TestCreateOrUpdateWithCreateOrUpdateResponderError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	testVM := getTestVM("vm1")
	armClient := mockarmclient.NewMockInterface(ctrl)
	response := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte(""))),
	}
	armClient.EXPECT().PutResource(gomock.Any(), to.String(testVM.ID), testVM).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	vmClient := getTestVMClient(armClient)
	rerr := vmClient.CreateOrUpdate(context.TODO(), "rg", "vm1", testVM, "test")
	assert.NotNil(t, rerr)
}

func TestCreateOrUpdateNeverRateLimiter(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	vmCreateOrUpdateErr := retry.GetRateLimitError(true, "VMCreateOrUpdate")

	armClient := mockarmclient.NewMockInterface(ctrl)
	vmClient := getTestVMClientWithNeverRateLimiter(armClient)
	testVM := getTestVM("vm1")
	rerr := vmClient.CreateOrUpdate(context.TODO(), "rg", "vm1", testVM, "test")
	assert.NotNil(t, rerr)
	assert.Equal(t, vmCreateOrUpdateErr, rerr)
}

func TestCreateOrUpdateRetryAfterReader(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	vmCreateOrUpdateErr := retry.GetThrottlingError("VMCreateOrUpdate", "client throttled", getFutureTime())

	testVM := getTestVM("vm1")
	armClient := mockarmclient.NewMockInterface(ctrl)
	vmClient := getTestVMClientWithRetryAfterReader(armClient)
	rerr := vmClient.CreateOrUpdate(context.TODO(), "rg", "vm1", testVM, "test")
	assert.NotNil(t, rerr)
	assert.Equal(t, vmCreateOrUpdateErr, rerr)
}

func TestCreateOrUpdateThrottle(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	throttleErr := &retry.Error{
		HTTPStatusCode: http.StatusTooManyRequests,
		RawError:       fmt.Errorf("error"),
		Retriable:      true,
		RetryAfter:     time.Unix(100, 0),
	}

	testVM := getTestVM("vm1")
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().PutResource(gomock.Any(), to.String(testVM.ID), testVM).Return(response, throttleErr).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	vmClient := getTestVMClient(armClient)
	rerr := vmClient.CreateOrUpdate(context.TODO(), "rg", "vm1", testVM, "test")
	assert.NotNil(t, rerr)
	assert.Equal(t, throttleErr, rerr)
}

func TestDelete(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	r := getTestVM("vm1")
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().DeleteResource(gomock.Any(), to.String(r.ID)).Return(nil).Times(1)

	client := getTestVMClient(armClient)
	rerr := client.Delete(context.TODO(), "rg", "vm1")
	assert.Nil(t, rerr)
}

func TestDeleteNeverRateLimiter(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	vmDeleteErr := &retry.Error{
		RawError:  fmt.Errorf("azure cloud provider rate limited(%s) for operation %q", "write", "VMDelete"),
		Retriable: true,
	}

	armClient := mockarmclient.NewMockInterface(ctrl)
	vmClient := getTestVMClientWithNeverRateLimiter(armClient)
	rerr := vmClient.Delete(context.TODO(), "rg", "vm1")
	assert.NotNil(t, rerr)
	assert.Equal(t, vmDeleteErr, rerr)
}

func TestDeleteRetryAfterReader(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	vmDeleteErr := &retry.Error{
		RawError:   fmt.Errorf("azure cloud provider throttled for operation %s with reason %q", "VMDelete", "client throttled"),
		Retriable:  true,
		RetryAfter: getFutureTime(),
	}

	armClient := mockarmclient.NewMockInterface(ctrl)
	vmClient := getTestVMClientWithRetryAfterReader(armClient)
	rerr := vmClient.Delete(context.TODO(), "rg", "vm1")
	assert.NotNil(t, rerr)
	assert.Equal(t, vmDeleteErr, rerr)
}

func TestDeleteThrottle(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	throttleErr := &retry.Error{
		HTTPStatusCode: http.StatusTooManyRequests,
		RawError:       fmt.Errorf("error"),
		Retriable:      true,
		RetryAfter:     time.Unix(100, 0),
	}

	testVM := getTestVM("vm1")
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().DeleteResource(gomock.Any(), to.String(testVM.ID)).Return(throttleErr).Times(1)

	vmClient := getTestVMClient(armClient)
	rerr := vmClient.Delete(context.TODO(), "rg", "vm1")
	assert.NotNil(t, rerr)
	assert.Equal(t, throttleErr, rerr)
}

func getTestVM(vmName string) compute.VirtualMachine {
	resourceID := fmt.Sprintf("/subscriptions/subscriptionID/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/%s", vmName)
	return compute.VirtualMachine{
		ID:       to.StringPtr(resourceID),
		Name:     to.StringPtr(vmName),
		Location: to.StringPtr("eastus"),
	}
}

func getTestVMClient(armClient armclient.Interface) *Client {
	rateLimiterReader, rateLimiterWriter := azclients.NewRateLimiter(&azclients.RateLimitConfig{})
	return &Client{
		armClient:         armClient,
		subscriptionID:    "subscriptionID",
		rateLimiterReader: rateLimiterReader,
		rateLimiterWriter: rateLimiterWriter,
	}
}

func getTestVMClientWithNeverRateLimiter(armClient armclient.Interface) *Client {
	rateLimiterReader := flowcontrol.NewFakeNeverRateLimiter()
	rateLimiterWriter := flowcontrol.NewFakeNeverRateLimiter()
	return &Client{
		armClient:         armClient,
		subscriptionID:    "subscriptionID",
		rateLimiterReader: rateLimiterReader,
		rateLimiterWriter: rateLimiterWriter,
	}
}

func getTestVMClientWithRetryAfterReader(armClient armclient.Interface) *Client {
	rateLimiterReader := flowcontrol.NewFakeAlwaysRateLimiter()
	rateLimiterWriter := flowcontrol.NewFakeAlwaysRateLimiter()
	return &Client{
		armClient:         armClient,
		subscriptionID:    "subscriptionID",
		rateLimiterReader: rateLimiterReader,
		rateLimiterWriter: rateLimiterWriter,
		RetryAfterReader:  getFutureTime(),
		RetryAfterWriter:  getFutureTime(),
	}
}

func getFutureTime() time.Time {
	return time.Unix(3000000000, 0)
}
