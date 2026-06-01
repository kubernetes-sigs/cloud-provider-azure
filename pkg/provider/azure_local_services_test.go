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
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"
	"github.com/stretchr/testify/assert"

	"go.uber.org/mock/gomock"

	v1 "k8s.io/api/core/v1"
	discovery_v1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/backendaddresspoolclient/mock_backendaddresspoolclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func TestLoadBalancerBackendPoolUpdater(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	addOperationPool1 := getAddIPsToBackendPoolOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"})
	removeOperationPool1 := getRemoveIPsFromBackendPoolOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"})
	addOperationPool2 := getAddIPsToBackendPoolOperation("ns1/svc1", "lb1", "pool2", []string{"10.0.0.1", "10.0.0.2"})

	testCases := []struct {
		name                               string
		operations                         []batchOperation
		existingBackendPools               []*armnetwork.BackendAddressPool
		expectedGetBackendPool             *armnetwork.BackendAddressPool
		extraWait                          bool
		notLocal                           bool
		changeLB                           bool
		removeOperationServiceName         string
		expectedCreateOrUpdateBackendPools []*armnetwork.BackendAddressPool
		expectedBackendPools               []*armnetwork.BackendAddressPool
	}{
		{
			name:       "Add node IPs to backend pool",
			operations: []batchOperation{addOperationPool1},
			existingBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedCreateOrUpdateBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
			expectedBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
		},
		{
			name:       "Remove node IPs from backend pool",
			operations: []batchOperation{addOperationPool1, removeOperationPool1},
			existingBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedCreateOrUpdateBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
		},
		{
			name:       "Multiple operations targeting different backend pools",
			operations: []batchOperation{addOperationPool1, addOperationPool2, removeOperationPool1},
			existingBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
				getTestBackendAddressPoolWithIPs("lb1", "pool2", []string{}),
			},
			expectedCreateOrUpdateBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
				getTestBackendAddressPoolWithIPs("lb1", "pool2", []string{"10.0.0.1", "10.0.0.2"}),
			},
			expectedBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
				getTestBackendAddressPoolWithIPs("lb1", "pool2", []string{"10.0.0.1", "10.0.0.2"}),
			},
		},
		{
			name:       "Multiple operations in two batches",
			operations: []batchOperation{addOperationPool1, removeOperationPool1},
			extraWait:  true,
			existingBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedCreateOrUpdateBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedGetBackendPool: getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			expectedBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
		},
		{
			name:                       "remove operations by service name",
			operations:                 []batchOperation{addOperationPool1, removeOperationPool1},
			removeOperationServiceName: "ns1/svc1",
		},
		{
			name:       "not local service",
			operations: []batchOperation{addOperationPool1},
			notLocal:   true,
		},
		{
			name:       "not on this load balancer",
			operations: []batchOperation{addOperationPool1},
			changeLB:   true,
		},
		{
			name:       "empty queue returns without ARM calls",
			operations: nil,
		},
		{
			name:       "removing non-existent IPs does not trigger CreateOrUpdate",
			operations: []batchOperation{removeOperationPool1},
			existingBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
		},
		{
			name:       "adding already-present IPs does not trigger CreateOrUpdate",
			operations: []batchOperation{addOperationPool1},
			existingBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
			expectedBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
		},
		{
			name: "add operation deduplicates IPs in empty pool",
			operations: []batchOperation{
				getAddIPsToBackendPoolOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1", "10.0.0.1", "10.0.0.2"}),
			},
			existingBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedCreateOrUpdateBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
			expectedBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
		},
		{
			name: "remove-then-add deduplicates IPs",
			operations: []batchOperation{
				getRemoveIPsFromBackendPoolOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
				getAddIPsToBackendPoolOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1", "10.0.0.1", "10.0.0.2", "10.0.0.2"}),
			},
			existingBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
			expectedCreateOrUpdateBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
			expectedBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
		},
		{
			name: "add-then-remove deduplicates IPs and removes target",
			operations: []batchOperation{
				getAddIPsToBackendPoolOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1", "10.0.0.1", "10.0.0.2", "10.0.0.2", "10.0.0.3", "10.0.0.3"}),
				getRemoveIPsFromBackendPoolOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.2"}),
			},
			existingBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
			expectedCreateOrUpdateBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.3"}),
			},
			expectedBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.3"}),
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(_ *testing.T) {
			cloud := GetTestCloud(ctrl)
			cloud.localServiceNameToServiceInfoMap = sync.Map{}
			if !tc.notLocal {
				cloud.localServiceNameToServiceInfoMap.Store("ns1/svc1", &serviceInfo{lbName: "lb1"})
			}
			if tc.changeLB {
				cloud.localServiceNameToServiceInfoMap.Store("ns1/svc1", &serviceInfo{lbName: "lb2"})
			}
			svc := getTestService("svc1", v1.ProtocolTCP, nil, false)
			client := fake.NewSimpleClientset(&svc)
			informerFactory := informers.NewSharedInformerFactory(client, 0)
			cloud.serviceLister = informerFactory.Core().V1().Services().Lister()
			mockbpClient := cloud.NetworkClientFactory.GetBackendAddressPoolClient().(*mock_backendaddresspoolclient.MockInterface)
			if len(tc.existingBackendPools) > 0 {
				mockbpClient.EXPECT().Get(
					gomock.Any(),
					gomock.Any(),
					"lb1",
					*tc.existingBackendPools[0].Name,
				).Return(tc.existingBackendPools[0], nil)
			}
			if len(tc.existingBackendPools) == 2 {
				mockbpClient.EXPECT().Get(
					gomock.Any(),
					gomock.Any(),
					"lb1",
					*tc.existingBackendPools[1].Name,
				).Return(tc.existingBackendPools[1], nil)
			}
			if tc.extraWait {
				mockbpClient.EXPECT().Get(
					gomock.Any(),
					gomock.Any(),
					"lb1",
					*tc.expectedGetBackendPool.Name,
				).Return(tc.expectedGetBackendPool, nil)
			}
			if len(tc.expectedCreateOrUpdateBackendPools) > 0 {
				mockbpClient.EXPECT().CreateOrUpdate(
					gomock.Any(),
					gomock.Any(),
					"lb1",
					*tc.expectedCreateOrUpdateBackendPools[0].Name,
					*tc.expectedCreateOrUpdateBackendPools[0],
				).Return(nil, nil)
			}
			if len(tc.existingBackendPools) == 2 || tc.extraWait {
				mockbpClient.EXPECT().CreateOrUpdate(
					gomock.Any(),
					gomock.Any(),
					"lb1",
					*tc.expectedCreateOrUpdateBackendPools[1].Name,
					*tc.expectedCreateOrUpdateBackendPools[1],
				).Return(nil, nil)
			}

			u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			// Use WaitGroup to properly synchronize goroutine completion
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				u.run(ctx)
			}()

			results := sync.Map{}
			operationsDone := make(chan struct{})
			var operationsWg sync.WaitGroup

			for _, op := range tc.operations {
				op := op
				operationsWg.Add(1)
				go func() {
					defer operationsWg.Done()
					u.addOperation(op)
					result := op.wait()
					results.Store(result, true)
				}()
				// Small delay to ensure operations are properly queued
				time.Sleep(50 * time.Millisecond)
				if tc.extraWait {
					time.Sleep(time.Second)
				}
			}

			// Handle operation removal if specified
			if tc.removeOperationServiceName != "" {
				u.removeOperation(tc.removeOperationServiceName)
			}

			// Wait for all operations to complete with timeout
			go func() {
				operationsWg.Wait()
				close(operationsDone)
			}()

			select {
			case <-operationsDone:
				// Operations completed successfully
				// Allow extra time for backend processing
				time.Sleep(2 * time.Second)
			case <-time.After(8 * time.Second):
				// Timeout - cancel context and wait for cleanup
				t.Logf("Test timeout waiting for operations to complete")
			}

			// Ensure proper cleanup - cancel context and wait for goroutine
			cancel()
			wg.Wait()
		})
	}
}

func TestLoadBalancerBackendPoolUpdaterFailed(t *testing.T) {
	addOperationPool1 := getAddIPsToBackendPoolOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"})

	testCases := []struct {
		name                               string
		operations                         []batchOperation
		existingBackendPools               []*armnetwork.BackendAddressPool
		expectedGetBackendPool             *armnetwork.BackendAddressPool
		getBackendPoolErr                  error
		putBackendPoolErr                  error
		expectedCreateOrUpdateBackendPools []*armnetwork.BackendAddressPool
		expectedBackendPools               []*armnetwork.BackendAddressPool
	}{
		{
			name:       "Non-retriable error when getting backend pool",
			operations: []batchOperation{addOperationPool1},
			existingBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			getBackendPoolErr: &azcore.ResponseError{ErrorCode: "error"},
			expectedBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
		},
		{
			name:       "Non-retriable error when updating backend pool",
			operations: []batchOperation{addOperationPool1},
			existingBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedGetBackendPool: getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			putBackendPoolErr:      &azcore.ResponseError{ErrorCode: "error"},
			expectedCreateOrUpdateBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
		},
		{
			name:       "Backend pool not found",
			operations: []batchOperation{addOperationPool1},
			existingBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			getBackendPoolErr: &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: "error"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(_ *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			cloud := GetTestCloud(ctrl)
			cloud.localServiceNameToServiceInfoMap = sync.Map{}
			cloud.localServiceNameToServiceInfoMap.Store("ns1/svc1", &serviceInfo{lbName: "lb1"})
			svc := getTestService("svc1", v1.ProtocolTCP, nil, false)
			client := fake.NewSimpleClientset(&svc)
			informerFactory := informers.NewSharedInformerFactory(client, 0)
			cloud.serviceLister = informerFactory.Core().V1().Services().Lister()
			mockLBClient := cloud.NetworkClientFactory.GetBackendAddressPoolClient().(*mock_backendaddresspoolclient.MockInterface)
			mockBPClient := cloud.NetworkClientFactory.GetBackendAddressPoolClient().(*mock_backendaddresspoolclient.MockInterface)
			mockLBClient.EXPECT().Get(
				gomock.Any(),
				gomock.Any(),
				"lb1",
				*tc.existingBackendPools[0].Name,
			).Return(tc.existingBackendPools[0], tc.getBackendPoolErr)
			if len(tc.existingBackendPools) == 2 {
				mockLBClient.EXPECT().Get(
					gomock.Any(),
					gomock.Any(),
					"lb1",
					*tc.existingBackendPools[1].Name,
				).Return(tc.existingBackendPools[1], nil)
			}
			if len(tc.expectedCreateOrUpdateBackendPools) > 0 {
				mockBPClient.EXPECT().CreateOrUpdate(
					gomock.Any(),
					gomock.Any(),
					"lb1",
					*tc.expectedCreateOrUpdateBackendPools[0].Name,
					*tc.expectedCreateOrUpdateBackendPools[0],
				).Return(nil, tc.putBackendPoolErr)
			}
			if len(tc.expectedCreateOrUpdateBackendPools) == 2 {
				mockLBClient.EXPECT().CreateOrUpdate(
					gomock.Any(),
					gomock.Any(),
					"lb1",
					*tc.expectedCreateOrUpdateBackendPools[1].Name,
					*tc.expectedCreateOrUpdateBackendPools[1],
				).Return(nil, nil)
			}

			u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			// Use WaitGroup to properly synchronize goroutine completion
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				u.run(ctx)
			}()

			operationsDone := make(chan struct{})
			go func() {
				defer close(operationsDone)
				for _, op := range tc.operations {
					op := op
					u.addOperation(op)
					time.Sleep(50 * time.Millisecond)
				}
				// Allow time for processing
				time.Sleep(2 * time.Second)
			}()

			// Wait for operations to complete with timeout
			select {
			case <-operationsDone:
				// Operations completed successfully
			case <-time.After(8 * time.Second):
				// Timeout - cancel context
				t.Logf("Test timeout waiting for operations to complete")
			}

			// Ensure proper cleanup - cancel context and wait for goroutine
			cancel()
			wg.Wait()
		})
	}
}

func getTestBackendAddressPoolWithIPs(lbName, bpName string, ips []string) *armnetwork.BackendAddressPool {
	bp := &armnetwork.BackendAddressPool{
		ID:   ptr.To(fmt.Sprintf("/subscriptions/subscriptionID/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/%s/backendAddressPools/%s", lbName, bpName)),
		Name: ptr.To(bpName),
		Properties: &armnetwork.BackendAddressPoolPropertiesFormat{
			VirtualNetwork: &armnetwork.SubResource{
				ID: ptr.To("/subscriptions/subscriptionID/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet"),
			},
			Location:                     ptr.To("eastus"),
			LoadBalancerBackendAddresses: []*armnetwork.LoadBalancerBackendAddress{},
		},
	}
	for _, ip := range ips {
		if len(ip) > 0 {
			bp.Properties.LoadBalancerBackendAddresses = append(bp.Properties.LoadBalancerBackendAddresses, &armnetwork.LoadBalancerBackendAddress{
				Name: ptr.To(""),
				Properties: &armnetwork.LoadBalancerBackendAddressPropertiesFormat{
					IPAddress: ptr.To(ip),
				},
			})
		}
	}
	return bp
}

func getTestEndpointSlice(name, namespace, svcName string, nodeNames ...string) *discovery_v1.EndpointSlice {
	endpoints := make([]discovery_v1.Endpoint, 0)
	for _, nodeName := range nodeNames {
		nodeName := nodeName
		endpoints = append(endpoints, discovery_v1.Endpoint{
			NodeName: &nodeName,
		})
	}
	return &discovery_v1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				consts.ServiceNameLabel: svcName,
			},
		},
		Endpoints: endpoints,
	}
}

func TestEndpointSlicesInformer(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for _, tc := range []struct {
		name                        string
		existingEPS                 *discovery_v1.EndpointSlice
		updatedEPS                  *discovery_v1.EndpointSlice
		notLocal                    bool
		expectedGetBackendPoolCount int
		expectedPutBackendPoolCount int
	}{
		{
			name:                        "remove unwanted ips and add wanted ones",
			existingEPS:                 getTestEndpointSlice("eps1", "test", "svc1", "node1"),
			updatedEPS:                  getTestEndpointSlice("eps1", "test", "svc1", "node2"),
			expectedGetBackendPoolCount: 1,
			expectedPutBackendPoolCount: 1,
		},
		{
			name:        "skip non-local services",
			existingEPS: getTestEndpointSlice("eps1", "test", "svc2", "node1"),
			updatedEPS:  getTestEndpointSlice("eps1", "test", "svc2", "node2"),
		},
		{
			name:        "skip an endpoint slice that don't belong to a service",
			existingEPS: getTestEndpointSlice("eps1", "test", "", "node1"),
			updatedEPS:  getTestEndpointSlice("eps1", "test", "", "node2"),
		},
		{
			name:        "not a local service",
			existingEPS: getTestEndpointSlice("eps1", "test", "", "node1"),
			updatedEPS:  getTestEndpointSlice("eps1", "test", "", "node2"),
			notLocal:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cloud := GetTestCloud(ctrl)
			cloud.localServiceNameToServiceInfoMap = sync.Map{}
			if !tc.notLocal {
				cloud.localServiceNameToServiceInfoMap.Store("test/svc1", &serviceInfo{lbName: "lb1"})
			}
			svc := getTestService("svc1", v1.ProtocolTCP, nil, false)
			client := fake.NewSimpleClientset(&svc, tc.existingEPS)
			informerFactory := informers.NewSharedInformerFactory(client, 0)
			cloud.serviceLister = informerFactory.Core().V1().Services().Lister()
			cloud.LoadBalancerBackendPoolUpdateIntervalInSeconds = 1
			cloud.LoadBalancerSKU = consts.LoadBalancerSKUStandard
			cloud.MultipleStandardLoadBalancerConfigurations = []config.MultipleStandardLoadBalancerConfiguration{
				{
					Name: "lb1",
				},
			}
			cloud.localServiceNameToServiceInfoMap.Store("test/svc1", newServiceInfo(consts.IPVersionIPv4String, "lb1"))
			cloud.nodePrivateIPs = map[string]*utilsets.IgnoreCaseSet{
				"node1": utilsets.NewString("10.0.0.1"),
				"node2": utilsets.NewString("10.0.0.2"),
			}

			existingBackendPool := getTestBackendAddressPoolWithIPs("lb1", "test-svc1", []string{"10.0.0.1"})
			expectedBackendPool := getTestBackendAddressPoolWithIPs("lb1", "test-svc1", []string{"10.0.0.2"})
			mockLBClient := cloud.NetworkClientFactory.GetBackendAddressPoolClient().(*mock_backendaddresspoolclient.MockInterface)
			mockLBClient.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "test-svc1").Return(existingBackendPool, nil).Times(tc.expectedGetBackendPoolCount)
			mockLBClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "test-svc1", *expectedBackendPool).Return(nil, nil).Times(tc.expectedPutBackendPoolCount)

			u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cloud.backendPoolUpdater = u

			// Use WaitGroup to properly synchronize goroutine completion
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				cloud.backendPoolUpdater.run(ctx)
			}()

			cloud.setUpEndpointSlicesInformer(informerFactory)
			stopChan := make(chan struct{})
			informerFactory.Start(stopChan)

			// Allow informer to initialize
			time.Sleep(100 * time.Millisecond)

			// Perform the update operation
			_, err := client.DiscoveryV1().EndpointSlices("test").Update(context.Background(), tc.updatedEPS, metav1.UpdateOptions{})
			assert.NoError(t, err)

			// Wait for operations to complete with timeout
			operationsDone := make(chan struct{})
			go func() {
				defer close(operationsDone)
				time.Sleep(2 * time.Second)
			}()

			select {
			case <-operationsDone:
				// Operations completed successfully
			case <-time.After(8 * time.Second):
				// Timeout
				t.Logf("Test timeout waiting for operations to complete")
			}

			// Cleanup - stop informer first, then cancel context and wait for goroutine
			close(stopChan)
			cancel()
			wg.Wait()
		})
	}
}

func TestEndpointSlicesInformerDeduplicatesNodeIPs(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for _, tc := range []struct {
		name        string
		existingEPS *discovery_v1.EndpointSlice
		updatedEPS  *discovery_v1.EndpointSlice
		expectedOps []*loadBalancerBackendPoolUpdateOperation
	}{
		{
			name:        "multiple endpoints on same node produces unique IPs",
			existingEPS: getTestEndpointSlice("eps1", "test", "svc1", "node1"),
			updatedEPS:  getTestEndpointSlice("eps1", "test", "svc1", "node2", "node2", "node2"),
			expectedOps: []*loadBalancerBackendPoolUpdateOperation{
				getRemoveIPsFromBackendPoolOperation("test/svc1", "lb1", "test-svc1", []string{"10.0.0.1"}),
				getAddIPsToBackendPoolOperation("test/svc1", "lb1", "test-svc1", []string{"10.0.0.2"}),
			},
		},
		{
			name:        "unchanged node set with different endpoint counts is a no-op",
			existingEPS: getTestEndpointSlice("eps1", "test", "svc1", "node1", "node1"),
			updatedEPS:  getTestEndpointSlice("eps1", "test", "svc1", "node1", "node1", "node1"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cloud := GetTestCloud(ctrl)
			cloud.localServiceNameToServiceInfoMap = sync.Map{}
			cloud.localServiceNameToServiceInfoMap.Store("test/svc1", newServiceInfo(consts.IPVersionIPv4String, "lb1"))
			cloud.LoadBalancerBackendPoolUpdateIntervalInSeconds = 1
			cloud.LoadBalancerSKU = consts.LoadBalancerSKUStandard
			cloud.MultipleStandardLoadBalancerConfigurations = []config.MultipleStandardLoadBalancerConfiguration{
				{Name: "lb1"},
			}
			cloud.nodePrivateIPs = map[string]*utilsets.IgnoreCaseSet{
				"node1": utilsets.NewString("10.0.0.1"),
				"node2": utilsets.NewString("10.0.0.2"),
			}

			svc := getTestService("svc1", v1.ProtocolTCP, nil, false)
			client := fake.NewSimpleClientset(&svc, tc.existingEPS)
			informerFactory := informers.NewSharedInformerFactory(client, 0)
			cloud.serviceLister = informerFactory.Core().V1().Services().Lister()

			// Create updater without starting run() so we can inspect queued operations.
			u := newLoadBalancerBackendPoolUpdater(cloud, time.Hour)
			cloud.backendPoolUpdater = u

			cloud.setUpEndpointSlicesInformer(informerFactory)
			stopChan := make(chan struct{})
			informerFactory.Start(stopChan)
			defer close(stopChan)

			// Allow informer to initialize.
			time.Sleep(100 * time.Millisecond)

			_, err := client.DiscoveryV1().EndpointSlices("test").Update(context.Background(), tc.updatedEPS, metav1.UpdateOptions{})
			assert.NoError(t, err)

			// Allow the async UpdateFunc to fire and enqueue operations.
			time.Sleep(500 * time.Millisecond)

			ops := u.drainOperations()
			var actual []*loadBalancerBackendPoolUpdateOperation
			for _, op := range ops {
				actual = append(actual, op.(*loadBalancerBackendPoolUpdateOperation))
			}
			assert.Equal(t, tc.expectedOps, actual)
		})
	}
}

func TestGetBackendPoolNamesAndIDsForService(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	cloud := GetTestCloud(ctrl)
	cloud.MultipleStandardLoadBalancerConfigurations = []config.MultipleStandardLoadBalancerConfiguration{
		{},
	}
	svc := getTestService("test", v1.ProtocolTCP, nil, false)
	svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyLocal
	_ = cloud.getBackendPoolNamesForService(&svc, "test")
	_ = cloud.getBackendPoolIDsForService(&svc, "test", "lb")
}

func TestCheckAndApplyLocalServiceBackendPoolUpdates(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for _, tc := range []struct {
		description string
		existingEPS *discovery_v1.EndpointSlice
	}{
		{
			description: "should update backend pool as expected",
			existingEPS: getTestEndpointSlice("eps1", "default", "svc1", "node2"),
		},
		{
			description: "should not report an error if failed to get the endpointslice",
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			cloud := GetTestCloud(ctrl)
			cloud.localServiceNameToServiceInfoMap.Store("default/svc1", &serviceInfo{lbName: "lb1"})
			svc := getTestService("svc1", v1.ProtocolTCP, nil, false)
			client := fake.NewSimpleClientset(&svc)
			cloud.KubeClient = client
			informerFactory := informers.NewSharedInformerFactory(client, 0)
			cloud.serviceLister = informerFactory.Core().V1().Services().Lister()
			cloud.LoadBalancerBackendPoolUpdateIntervalInSeconds = 1
			cloud.LoadBalancerSKU = consts.LoadBalancerSKUStandard
			cloud.MultipleStandardLoadBalancerConfigurations = []config.MultipleStandardLoadBalancerConfiguration{
				{
					Name: "lb1",
				},
			}
			cloud.localServiceNameToServiceInfoMap.Store("default/svc1", newServiceInfo(consts.IPVersionIPv4String, "lb1"))
			cloud.nodePrivateIPs = map[string]*utilsets.IgnoreCaseSet{
				"node1": utilsets.NewString("10.0.0.1", "fd00::1"),
				"node2": utilsets.NewString("10.0.0.2", "fd00::2"),
			}
			if tc.existingEPS != nil {
				cloud.endpointSlicesCache.Store(fmt.Sprintf("%s/%s", tc.existingEPS.Name, tc.existingEPS.Namespace), tc.existingEPS)
			}

			existingBackendPool := getTestBackendAddressPoolWithIPs("lb1", "default-svc1", []string{"10.0.0.1"})
			existingBackendPoolIPv6 := getTestBackendAddressPoolWithIPs("lb1", "default-svc1-ipv6", []string{"fd00::1"})
			existingLB := armnetwork.LoadBalancer{
				Name: ptr.To("lb1"),
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					BackendAddressPools: []*armnetwork.BackendAddressPool{
						existingBackendPool,
						existingBackendPoolIPv6,
					},
				},
			}
			expectedBackendPool := getTestBackendAddressPoolWithIPs("lb1", "default-svc1", []string{"10.0.0.2"})
			expectedBackendPoolIPv6 := getTestBackendAddressPoolWithIPs("lb1", "default-svc1-ipv6", []string{"fd00::2"})
			mockLBClient := cloud.NetworkClientFactory.GetBackendAddressPoolClient().(*mock_backendaddresspoolclient.MockInterface)
			if tc.existingEPS != nil {
				mockLBClient.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "default-svc1").Return(existingBackendPool, nil)
				mockLBClient.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "default-svc1-ipv6").Return(existingBackendPoolIPv6, nil)
				mockLBClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "default-svc1", *expectedBackendPool).Return(nil, nil)
				mockLBClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "default-svc1-ipv6", *expectedBackendPoolIPv6).Return(nil, nil)
			}

			u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cloud.backendPoolUpdater = u

			// Use WaitGroup to properly synchronize goroutine completion
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				cloud.backendPoolUpdater.run(ctx)
			}()

			if tc.existingEPS != nil {
				_, _ = client.DiscoveryV1().EndpointSlices("default").Create(context.Background(), tc.existingEPS, metav1.CreateOptions{})
			}

			err := cloud.checkAndApplyLocalServiceBackendPoolUpdates(existingLB, &svc)
			assert.NoError(t, err)

			// Wait for operations to complete with timeout
			operationsDone := make(chan struct{})
			go func() {
				defer close(operationsDone)
				time.Sleep(2 * time.Second)
			}()

			select {
			case <-operationsDone:
				// Operations completed successfully
			case <-time.After(8 * time.Second):
				// Timeout
				t.Logf("Test timeout waiting for operations to complete")
			}

			// Ensure proper cleanup - cancel context and wait for goroutine
			cancel()
			wg.Wait()
		})
	}
}

func TestCountOperations(t *testing.T) {
	op1 := getAddIPsToBackendPoolOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1"})
	op2 := getRemoveIPsFromBackendPoolOperation("ns1/svc2", "lb1", "pool1", []string{"10.0.0.2"})

	tests := []struct {
		name     string
		ops      []batchOperation
		expected int
	}{
		{name: "empty queue", ops: nil, expected: 0},
		{name: "one operation", ops: []batchOperation{op1}, expected: 1},
		{name: "two operations", ops: []batchOperation{op1, op2}, expected: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cloud := &Cloud{}
			u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
			for _, op := range tc.ops {
				u.addOperation(op)
			}
			assert.Equal(t, tc.expected, u.countOperations())
		})
	}

	t.Run("acquires updater lock", func(t *testing.T) {
		cloud := &Cloud{}
		u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
		u.addOperation(op1)

		u.lock.Lock()

		done := make(chan struct{})
		go func() {
			defer close(done)
			u.countOperations()
		}()

		select {
		case <-done:
			t.Fatal("countOperations should block while lock is held")
		case <-time.After(100 * time.Millisecond):
		}

		u.lock.Unlock()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("countOperations did not complete after lock released")
		}
	})
}

func TestDrainOperations(t *testing.T) {
	op1 := getAddIPsToBackendPoolOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1"})
	op2 := getAddIPsToBackendPoolOperation("ns1/svc2", "lb1", "pool2", []string{"10.0.0.2"})

	testCases := []struct {
		name       string
		operations []batchOperation
		expected   []batchOperation
	}{
		{
			name:     "returns nil when queue is empty",
			expected: nil,
		},
		{
			name:       "returns single operation",
			operations: []batchOperation{op1},
			expected:   []batchOperation{op1},
		},
		{
			name:       "returns all operations in order",
			operations: []batchOperation{op1, op2},
			expected:   []batchOperation{op1, op2},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cloud := &Cloud{}
			u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
			for _, op := range tc.operations {
				u.addOperation(op)
			}

			ops := u.drainOperations()

			assert.Equal(t, tc.expected, ops)

			u.lock.Lock()
			assert.Equal(t, 0, len(u.operations), "queue should be cleared after drain")
			u.lock.Unlock()
		})
	}

	t.Run("acquires updater lock", func(t *testing.T) {
		cloud := &Cloud{}
		u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
		u.addOperation(op1)

		u.lock.Lock()

		done := make(chan struct{})
		go func() {
			defer close(done)
			u.drainOperations()
		}()

		select {
		case <-done:
			t.Fatal("drainOperations returned while lock was held")
		case <-time.After(100 * time.Millisecond):
		}

		u.lock.Unlock()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("drainOperations did not complete after lock released")
		}
	})
}

func TestGroupOperations(t *testing.T) {
	addPool1 := getAddIPsToBackendPoolOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1"})
	removePool1 := getRemoveIPsFromBackendPoolOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1"})
	addPool1WithExtra := getAddIPsToBackendPoolOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"})
	addPool2 := getAddIPsToBackendPoolOperation("ns1/svc2", "lb1", "pool2", []string{"10.0.0.2"})

	testCases := []struct {
		name           string
		operations     []batchOperation
		localServices  map[string]*serviceInfo
		expectedGroups map[string][]batchOperation
	}{
		{
			name:           "returns empty map for nil input",
			operations:     nil,
			localServices:  map[string]*serviceInfo{"ns1/svc1": {lbName: "lb1"}},
			expectedGroups: map[string][]batchOperation{},
		},
		{
			name:           "groups single operation by pool key",
			operations:     []batchOperation{addPool1},
			localServices:  map[string]*serviceInfo{"ns1/svc1": {lbName: "lb1"}},
			expectedGroups: map[string][]batchOperation{"lb1:pool1": {addPool1}},
		},
		{
			name:           "groups operations targeting same pool together",
			operations:     []batchOperation{removePool1, addPool1WithExtra},
			localServices:  map[string]*serviceInfo{"ns1/svc1": {lbName: "lb1"}},
			expectedGroups: map[string][]batchOperation{"lb1:pool1": {removePool1, addPool1WithExtra}},
		},
		{
			name:       "groups operations targeting different pools separately",
			operations: []batchOperation{addPool1, addPool2},
			localServices: map[string]*serviceInfo{
				"ns1/svc1": {lbName: "lb1"},
				"ns1/svc2": {lbName: "lb1"},
			},
			expectedGroups: map[string][]batchOperation{"lb1:pool1": {addPool1}, "lb1:pool2": {addPool2}},
		},
		{
			name:           "skips operations for non-local services",
			operations:     []batchOperation{addPool1},
			localServices:  map[string]*serviceInfo{},
			expectedGroups: map[string][]batchOperation{},
		},
		{
			name:           "skips operations targeting stale load balancer",
			operations:     []batchOperation{addPool1},
			localServices:  map[string]*serviceInfo{"ns1/svc1": {lbName: "lb2"}},
			expectedGroups: map[string][]batchOperation{},
		},
		{
			name:          "keeps valid operation when another is filtered",
			operations:    []batchOperation{addPool1, addPool2},
			localServices: map[string]*serviceInfo{"ns1/svc1": {lbName: "lb1"}},
			expectedGroups: map[string][]batchOperation{
				"lb1:pool1": {addPool1},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cloud := &Cloud{}
			for k, v := range tc.localServices {
				cloud.localServiceNameToServiceInfoMap.Store(k, v)
			}
			u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)

			groups := u.groupOperations(context.Background(), tc.operations)

			assert.Equal(t, len(tc.expectedGroups), len(groups))
			for key, expectedOps := range tc.expectedGroups {
				assert.Equal(t, len(expectedOps), len(groups[key]), "group %s count", key)
				for i, expectedOp := range expectedOps {
					assert.Equal(t, expectedOp, groups[key][i], "group %s op %d", key, i)
				}
			}
		})
	}
}

func TestLoadBalancerBackendPoolUpdaterSerializesWithServiceReconcileLock(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	cloud := GetTestCloud(ctrl)
	cloud.localServiceNameToServiceInfoMap = sync.Map{}
	cloud.localServiceNameToServiceInfoMap.Store("ns1/svc1", &serviceInfo{lbName: "lb1"})

	svc := getTestService("svc1", v1.ProtocolTCP, nil, false)
	client := fake.NewSimpleClientset(&svc)
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	cloud.serviceLister = informerFactory.Core().V1().Services().Lister()

	existingBP := getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{})
	mockBPClient := cloud.NetworkClientFactory.GetBackendAddressPoolClient().(*mock_backendaddresspoolclient.MockInterface)

	// ARM calls should only happen after the lock is released.
	armCalled := make(chan struct{})
	mockBPClient.EXPECT().Get(
		gomock.Any(), gomock.Any(), "lb1", "pool1",
	).DoAndReturn(func(_ context.Context, _, _, _ string) (*armnetwork.BackendAddressPool, error) {
		close(armCalled)
		return existingBP, nil
	})
	mockBPClient.EXPECT().CreateOrUpdate(
		gomock.Any(), gomock.Any(), "lb1", "pool1",
		*getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1"}),
	).Return(nil, nil)

	u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
	u.addOperation(getAddIPsToBackendPoolOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1"}))

	// Hold serviceReconcileLock to simulate the main reconcile path.
	cloud.serviceReconcileLock.Lock()

	processDone := make(chan struct{})
	go func() {
		defer close(processDone)
		u.process(context.Background())
	}()

	// process() should be blocked, ARM call should not have happened yet.
	select {
	case <-armCalled:
		t.Fatal("ARM call happened while serviceReconcileLock was held")
	case <-time.After(500 * time.Millisecond):
		// Expected: process() is blocked waiting for the lock.
	}

	// Release the lock, process() should now complete.
	cloud.serviceReconcileLock.Unlock()

	select {
	case <-processDone:
		// process() completed after lock was released.
	case <-time.After(5 * time.Second):
		t.Fatal("process() did not complete after serviceReconcileLock was released")
	}
}

func TestLoadBalancerBackendPoolUpdaterAddOperationNotBlockedDuringProcess(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	cloud := GetTestCloud(ctrl)
	cloud.localServiceNameToServiceInfoMap = sync.Map{}
	cloud.localServiceNameToServiceInfoMap.Store("ns1/svc1", &serviceInfo{lbName: "lb1"})

	svc := getTestService("svc1", v1.ProtocolTCP, nil, false)
	client := fake.NewSimpleClientset(&svc)
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	cloud.serviceLister = informerFactory.Core().V1().Services().Lister()

	// Block the ARM Get call so process() is in the middle of ARM work.
	armBlocked := make(chan struct{})
	armUnblock := make(chan struct{})
	existingBP := getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{})
	mockBPClient := cloud.NetworkClientFactory.GetBackendAddressPoolClient().(*mock_backendaddresspoolclient.MockInterface)
	mockBPClient.EXPECT().Get(
		gomock.Any(), gomock.Any(), "lb1", "pool1",
	).DoAndReturn(func(_ context.Context, _, _, _ string) (*armnetwork.BackendAddressPool, error) {
		close(armBlocked)
		<-armUnblock
		return existingBP, nil
	})
	mockBPClient.EXPECT().CreateOrUpdate(
		gomock.Any(), gomock.Any(), "lb1", "pool1",
		*getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1"}),
	).Return(nil, nil)

	u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
	u.addOperation(getAddIPsToBackendPoolOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1"}))

	processDone := make(chan struct{})
	go func() {
		defer close(processDone)
		u.process(context.Background())
	}()

	// Wait until the ARM call is in flight (updater.lock has been released).
	select {
	case <-armBlocked:
	case <-time.After(5 * time.Second):
		t.Fatal("ARM call did not start")
	}

	// addOperation should not block because updater.lock is released during ARM calls.
	addDone := make(chan struct{})
	go func() {
		defer close(addDone)
		u.addOperation(getAddIPsToBackendPoolOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.2"}))
	}()

	select {
	case <-addDone:
		// addOperation returned without blocking.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("addOperation blocked while process() was performing ARM calls")
	}

	// Unblock the ARM call and wait for process() to finish
	// so that mock expectations (CreateOrUpdate) are satisfied.
	close(armUnblock)
	select {
	case <-processDone:
	case <-time.After(5 * time.Second):
		t.Fatal("process() did not complete after unblocking ARM call")
	}
}

func TestLoadBalancerBackendPoolUpdaterPreservesOperationsOnLeaseLockFailure(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	cloud := GetTestCloud(ctrl)
	cloud.localServiceNameToServiceInfoMap = sync.Map{}
	cloud.localServiceNameToServiceInfoMap.Store("ns1/svc1", &serviceInfo{lbName: "lb1"})

	// Set up azureResourceLocker with a KubeClient that fails lease operations.
	leaseClient := fake.NewSimpleClientset()
	leaseClient.PrependReactor("get", "leases", func(_ clientgotesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("simulated lease API failure")
	})

	cloud.KubeClient = leaseClient
	cloud.azureResourceLocker = NewAzureResourceLocker(
		cloud,
		"test-holder",
		"test-lease",
		"test-namespace",
		15,
	)

	u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
	u.addOperation(getAddIPsToBackendPoolOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1"}))

	// No ARM mock expectations, ARM call should not happen.
	u.process(context.Background())

	u.lock.Lock()
	assert.Equal(t, 1, len(u.operations), "Operations should be preserved after lease lock failure")
	u.lock.Unlock()
}

func TestLoadBalancerBackendPoolUpdaterCompletesOnUnlockFailure(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	cloud := GetTestCloud(ctrl)
	cloud.localServiceNameToServiceInfoMap = sync.Map{}
	cloud.localServiceNameToServiceInfoMap.Store("ns1/svc1", &serviceInfo{lbName: "lb1"})

	svc := getTestService("svc1", v1.ProtocolTCP, nil, false)
	client := fake.NewSimpleClientset(&svc)
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	cloud.serviceLister = informerFactory.Core().V1().Services().Lister()

	// Fail the second lease update (releaseLease), let the first (acquireLease) pass.
	var updateCount int32
	leaseClient := fake.NewSimpleClientset()
	leaseClient.PrependReactor("update", "leases", func(_ clientgotesting.Action) (bool, runtime.Object, error) {
		if atomic.AddInt32(&updateCount, 1) >= 2 {
			return true, nil, fmt.Errorf("simulated unlock failure")
		}
		return false, nil, nil
	})

	cloud.KubeClient = leaseClient
	cloud.azureResourceLocker = NewAzureResourceLocker(
		cloud,
		"test-holder",
		"test-lease",
		"test-namespace",
		15,
	)

	existingBP := getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{})
	mockBPClient := cloud.NetworkClientFactory.GetBackendAddressPoolClient().(*mock_backendaddresspoolclient.MockInterface)
	mockBPClient.EXPECT().Get(
		gomock.Any(), gomock.Any(), "lb1", "pool1",
	).Return(existingBP, nil)
	mockBPClient.EXPECT().CreateOrUpdate(
		gomock.Any(), gomock.Any(), "lb1", "pool1",
		*getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1"}),
	).Return(nil, nil)

	u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
	u.addOperation(getAddIPsToBackendPoolOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1"}))

	// ARM calls should complete despite unlock failure.
	u.process(context.Background())

	u.lock.Lock()
	assert.Equal(t, 0, len(u.operations), "Operations should have been processed despite unlock failure")
	u.lock.Unlock()
}

func TestLoadBalancerBackendPoolUpdaterFiltersOperationsWhenLBChangedDuringProcess(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	cloud := GetTestCloud(ctrl)
	cloud.localServiceNameToServiceInfoMap = sync.Map{}
	cloud.localServiceNameToServiceInfoMap.Store("ns1/svc1", &serviceInfo{lbName: "lb1"})

	svc := getTestService("svc1", v1.ProtocolTCP, nil, false)
	client := fake.NewSimpleClientset(&svc)
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	cloud.serviceLister = informerFactory.Core().V1().Services().Lister()

	// No ARM mock expectations. The operation targets lb1 but the service moves
	// to lb2 while process() is blocked on serviceReconcileLock, so groupOperations
	// filters it.

	u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
	u.addOperation(getAddIPsToBackendPoolOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1"}))

	// Hold serviceReconcileLock to simulate the main reconcile loop running.
	cloud.serviceReconcileLock.Lock()

	processDone := make(chan struct{})
	go func() {
		defer close(processDone)
		u.process(context.Background())
	}()

	// process() is blocked on serviceReconcileLock. The queue has not been drained yet.
	time.Sleep(200 * time.Millisecond)
	u.lock.Lock()
	assert.Equal(t, 1, len(u.operations), "queue should not be drained while process() is blocked")
	u.lock.Unlock()

	// Simulate the main reconcile loop moving svc1 from lb1 to lb2.
	cloud.localServiceNameToServiceInfoMap.Store("ns1/svc1", &serviceInfo{lbName: "lb2"})

	// Release the lock. process() will drain and filter the operation via groupOperations.
	cloud.serviceReconcileLock.Unlock()

	select {
	case <-processDone:
	case <-time.After(5 * time.Second):
		t.Fatal("process() did not complete")
	}

	u.lock.Lock()
	assert.Equal(t, 0, len(u.operations))
	u.lock.Unlock()
}

func TestLoadBalancerBackendPoolUpdaterRemoveOperationCancelsOperationsBeforeDrain(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	cloud := GetTestCloud(ctrl)
	cloud.localServiceNameToServiceInfoMap = sync.Map{}
	cloud.localServiceNameToServiceInfoMap.Store("ns1/svc1", &serviceInfo{lbName: "lb1"})

	svc := getTestService("svc1", v1.ProtocolTCP, nil, false)
	client := fake.NewSimpleClientset(&svc)
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	cloud.serviceLister = informerFactory.Core().V1().Services().Lister()

	// No ARM mock expectations. removeOperation cancels the operation
	// before process() drains the queue.

	u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
	u.addOperation(getAddIPsToBackendPoolOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1"}))

	// Hold serviceReconcileLock to simulate the main reconcile loop running.
	cloud.serviceReconcileLock.Lock()

	processDone := make(chan struct{})
	go func() {
		defer close(processDone)
		u.process(context.Background())
	}()

	// process() is blocked on serviceReconcileLock. The queue has not been drained yet.
	time.Sleep(200 * time.Millisecond)
	u.lock.Lock()
	assert.Equal(t, 1, len(u.operations), "queue should not be drained while process() is blocked")
	u.lock.Unlock()

	// Simulate the main reconcile loop calling removeOperation to cancel pending operations.
	u.removeOperation("ns1/svc1")

	// Release the lock. process() should not make ARM calls for the cancelled service.
	cloud.serviceReconcileLock.Unlock()

	select {
	case <-processDone:
	case <-time.After(5 * time.Second):
		t.Fatal("process() did not complete")
	}

	u.lock.Lock()
	assert.Equal(t, 0, len(u.operations))
	u.lock.Unlock()
}
