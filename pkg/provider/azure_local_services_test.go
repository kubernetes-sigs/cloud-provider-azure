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

package provider

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/pointer"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/loadbalancerclient/mockloadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

func TestLoadBalancerBackendPoolUpdater(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	addOperationPool1 := getAddIPsToBackendPoolOperation("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"})
	removeOperationPool1 := getRemoveIPsFromBackendPoolOperation("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"})
	addOperationPool2 := getAddIPsToBackendPoolOperation("lb1", "pool2", []string{"10.0.0.1", "10.0.0.2"})

	testCases := []struct {
		name                               string
		operations                         []batchOperation
		existingBackendPools               []network.BackendAddressPool
		expectedGetBackendPool             network.BackendAddressPool
		extraWait                          bool
		expectedCreateOrUpdateBackendPools []network.BackendAddressPool
		expectedBackendPools               []network.BackendAddressPool
		expectedResult                     map[batchOperationResult]bool
	}{
		{
			name:       "Add node IPs to backend pool",
			operations: []batchOperation{addOperationPool1},
			existingBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedCreateOrUpdateBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
			expectedBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
			expectedResult: map[batchOperationResult]bool{
				newBatchOperationResult("lb1/pool1", true, nil): true,
			},
		},
		{
			name:       "Remove node IPs from backend pool",
			operations: []batchOperation{addOperationPool1, removeOperationPool1},
			existingBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedCreateOrUpdateBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedResult: map[batchOperationResult]bool{
				newBatchOperationResult("lb1/pool1", true, nil): true,
			},
		},
		{
			name:       "Multiple operations targeting different backend pools",
			operations: []batchOperation{addOperationPool1, addOperationPool2, removeOperationPool1},
			existingBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
				getTestBackendAddressPoolWithIPs("lb1", "pool2", []string{}),
			},
			expectedCreateOrUpdateBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
				getTestBackendAddressPoolWithIPs("lb1", "pool2", []string{"10.0.0.1", "10.0.0.2"}),
			},
			expectedBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
				getTestBackendAddressPoolWithIPs("lb1", "pool2", []string{"10.0.0.1", "10.0.0.2"}),
			},
			expectedResult: map[batchOperationResult]bool{
				newBatchOperationResult("lb1/pool1", true, nil): true,
				newBatchOperationResult("lb1/pool2", true, nil): true,
			},
		},
		{
			name:       "Multiple operations in two batches",
			operations: []batchOperation{addOperationPool1, removeOperationPool1},
			extraWait:  true,
			existingBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedCreateOrUpdateBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedGetBackendPool: getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			expectedBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedResult: map[batchOperationResult]bool{
				newBatchOperationResult("lb1/pool1", true, nil): true,
				newBatchOperationResult("lb1/pool1", true, nil): true,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cloud := GetTestCloud(ctrl)
			mockLBClient := mockloadbalancerclient.NewMockInterface(ctrl)
			mockLBClient.EXPECT().GetLBBackendPool(
				gomock.Any(),
				gomock.Any(),
				"lb1",
				*tc.existingBackendPools[0].Name,
				gomock.Any(),
			).Return(tc.existingBackendPools[0], nil)
			if len(tc.existingBackendPools) == 2 {
				mockLBClient.EXPECT().GetLBBackendPool(
					gomock.Any(),
					gomock.Any(),
					"lb1",
					*tc.existingBackendPools[1].Name,
					gomock.Any(),
				).Return(tc.existingBackendPools[1], nil)
			}
			if tc.extraWait {
				mockLBClient.EXPECT().GetLBBackendPool(
					gomock.Any(),
					gomock.Any(),
					"lb1",
					*tc.expectedGetBackendPool.Name,
					gomock.Any(),
				).Return(tc.expectedGetBackendPool, nil)
			}
			mockLBClient.EXPECT().CreateOrUpdateBackendPools(
				gomock.Any(),
				gomock.Any(),
				"lb1",
				*tc.expectedCreateOrUpdateBackendPools[0].Name,
				tc.expectedCreateOrUpdateBackendPools[0],
				gomock.Any(),
			).Return(nil)
			if len(tc.existingBackendPools) == 2 || tc.extraWait {
				mockLBClient.EXPECT().CreateOrUpdateBackendPools(
					gomock.Any(),
					gomock.Any(),
					"lb1",
					*tc.expectedCreateOrUpdateBackendPools[1].Name,
					tc.expectedCreateOrUpdateBackendPools[1],
					gomock.Any(),
				).Return(nil)
			}
			cloud.LoadBalancerClient = mockLBClient

			u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go u.run(ctx)

			results := sync.Map{}
			for _, op := range tc.operations {
				op := op
				go func() {
					u.addOperation(op)
					result := op.wait()
					results.Store(result, true)
				}()
				time.Sleep(100 * time.Millisecond)
				if tc.extraWait {
					time.Sleep(time.Second)
				}
			}
			err := wait.PollUntilContextTimeout(context.Background(), time.Second, 5*time.Second, true, func(ctx context.Context) (bool, error) {
				resultLen := 0
				results.Range(func(key, value interface{}) bool {
					resultLen++
					return true
				})
				return resultLen == len(tc.expectedResult), nil
			})
			assert.NoError(t, err)

			actualResults := make(map[batchOperationResult]bool)
			results.Range(func(key, value interface{}) bool {
				actualResults[key.(batchOperationResult)] = true
				return true
			})

			err = wait.PollUntilContextTimeout(ctx, time.Second, 5*time.Second, true, func(ctx context.Context) (bool, error) {
				return assert.Equal(t, tc.expectedResult, actualResults, tc.name), nil
			})
			assert.NoError(t, err)
		})
	}
}

func TestLoadBalancerBackendPoolUpdaterFailed(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	addOperationPool1 := getAddIPsToBackendPoolOperation("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"})

	testCases := []struct {
		name                               string
		operations                         []batchOperation
		existingBackendPools               []network.BackendAddressPool
		expectedGetBackendPool             network.BackendAddressPool
		getBackendPoolErr                  *retry.Error
		putBackendPoolErr                  *retry.Error
		expectedCreateOrUpdateBackendPools []network.BackendAddressPool
		expectedBackendPools               []network.BackendAddressPool
		expectedResult                     map[batchOperationResult]bool
		expectedPoolNameToErrMsg           map[string]string
		expectedResultCount                int
	}{
		{
			name:       "Retriable error when getting backend pool",
			operations: []batchOperation{addOperationPool1},
			existingBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			getBackendPoolErr: retry.NewError(true, errors.New("error")),
			expectedCreateOrUpdateBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
			expectedBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
			expectedResult: map[batchOperationResult]bool{
				newBatchOperationResult("lb1/pool1", true, nil): true,
			},
			expectedResultCount: 1,
		},
		{
			name:       "Retriable error when updating backend pool",
			operations: []batchOperation{addOperationPool1},
			existingBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedGetBackendPool: getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			putBackendPoolErr:      retry.NewError(true, errors.New("error")),
			expectedCreateOrUpdateBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
			expectedBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
			expectedResult: map[batchOperationResult]bool{
				newBatchOperationResult("lb1/pool1", true, nil): true,
			},
			expectedResultCount: 1,
		},
		{
			name:       "Non-retriable error when getting backend pool",
			operations: []batchOperation{addOperationPool1},
			existingBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			getBackendPoolErr: retry.NewError(false, fmt.Errorf("error")),
			expectedBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedPoolNameToErrMsg: map[string]string{"lb1/pool1": "Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: error"},
			expectedResultCount:      1,
		},
		{
			name:       "Non-retriable error when updating backend pool",
			operations: []batchOperation{addOperationPool1},
			existingBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedGetBackendPool: getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			putBackendPoolErr:      retry.NewError(false, fmt.Errorf("error")),
			expectedCreateOrUpdateBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
			expectedPoolNameToErrMsg: map[string]string{"lb1/pool1": "Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: error"},
			expectedResultCount:      1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cloud := GetTestCloud(ctrl)
			mockLBClient := mockloadbalancerclient.NewMockInterface(ctrl)
			mockLBClient.EXPECT().GetLBBackendPool(
				gomock.Any(),
				gomock.Any(),
				"lb1",
				*tc.existingBackendPools[0].Name,
				gomock.Any(),
			).Return(tc.existingBackendPools[0], tc.getBackendPoolErr)
			if tc.getBackendPoolErr != nil && tc.getBackendPoolErr.Retriable {
				mockLBClient.EXPECT().GetLBBackendPool(
					gomock.Any(),
					gomock.Any(),
					"lb1",
					*tc.existingBackendPools[0].Name,
					gomock.Any(),
				).Return(tc.existingBackendPools[0], nil)
			}
			if len(tc.existingBackendPools) == 2 {
				mockLBClient.EXPECT().GetLBBackendPool(
					gomock.Any(),
					gomock.Any(),
					"lb1",
					*tc.existingBackendPools[1].Name,
					gomock.Any(),
				).Return(tc.existingBackendPools[1], nil)
			}
			if tc.putBackendPoolErr != nil && tc.putBackendPoolErr.Retriable {
				mockLBClient.EXPECT().GetLBBackendPool(
					gomock.Any(),
					gomock.Any(),
					"lb1",
					*tc.expectedGetBackendPool.Name,
					gomock.Any(),
				).Return(tc.expectedGetBackendPool, nil)
			}
			if len(tc.expectedCreateOrUpdateBackendPools) > 0 {
				mockLBClient.EXPECT().CreateOrUpdateBackendPools(
					gomock.Any(),
					gomock.Any(),
					"lb1",
					*tc.expectedCreateOrUpdateBackendPools[0].Name,
					tc.expectedCreateOrUpdateBackendPools[0],
					gomock.Any(),
				).Return(tc.putBackendPoolErr)
			}
			if len(tc.expectedCreateOrUpdateBackendPools) == 2 {
				mockLBClient.EXPECT().CreateOrUpdateBackendPools(
					gomock.Any(),
					gomock.Any(),
					"lb1",
					*tc.expectedCreateOrUpdateBackendPools[1].Name,
					tc.expectedCreateOrUpdateBackendPools[1],
					gomock.Any(),
				).Return(nil)
			}
			cloud.LoadBalancerClient = mockLBClient

			u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go u.run(ctx)

			results := sync.Map{}
			for _, op := range tc.operations {
				op := op
				go func() {
					u.addOperation(op)
					result := op.wait()
					results.Store(result, true)
				}()
				time.Sleep(100 * time.Millisecond)
			}
			err := wait.PollUntilContextTimeout(context.Background(), time.Second, 5*time.Second, true, func(ctx context.Context) (bool, error) {
				resultLen := 0
				results.Range(func(key, value interface{}) bool {
					resultLen++
					return true
				})
				return resultLen == tc.expectedResultCount, nil
			})
			assert.NoError(t, err)

			actualResults := make(map[batchOperationResult]bool)
			poolNameToErrMsg := make(map[string]string)
			results.Range(func(key, value interface{}) bool {
				actualResults[key.(batchOperationResult)] = true
				if tc.getBackendPoolErr != nil && !tc.getBackendPoolErr.Retriable ||
					tc.putBackendPoolErr != nil && !tc.putBackendPoolErr.Retriable {
					poolNameToErrMsg[key.(batchOperationResult).name] = key.(batchOperationResult).err.Error()
				}
				return true
			})

			err = wait.PollUntilContextTimeout(ctx, time.Second, 5*time.Second, true, func(ctx context.Context) (bool, error) {
				if tc.expectedResult != nil {
					return assert.Equal(t, tc.expectedResult, actualResults, tc.name), nil
				}
				if tc.expectedPoolNameToErrMsg != nil {
					return assert.Equal(t, tc.expectedPoolNameToErrMsg, poolNameToErrMsg, tc.name), nil
				}
				return false, errors.New("unexpected result")
			})
			assert.NoError(t, err)
		})
	}
}

func getTestBackendAddressPoolWithIPs(lbName, bpName string, ips []string) network.BackendAddressPool {
	bp := network.BackendAddressPool{
		ID:   pointer.String(fmt.Sprintf("/subscriptions/subscriptionID/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/%s/backendAddressPools/%s", lbName, bpName)),
		Name: pointer.String(bpName),
		BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
			Location:                     pointer.String("eastus"),
			LoadBalancerBackendAddresses: &[]network.LoadBalancerBackendAddress{},
		},
	}
	for _, ip := range ips {
		if len(ip) > 0 {
			*bp.LoadBalancerBackendAddresses = append(*bp.LoadBalancerBackendAddresses, network.LoadBalancerBackendAddress{
				Name: pointer.String(""),
				LoadBalancerBackendAddressPropertiesFormat: &network.LoadBalancerBackendAddressPropertiesFormat{
					IPAddress: pointer.String(ip),
				},
			})
		}
	}
	return bp
}
