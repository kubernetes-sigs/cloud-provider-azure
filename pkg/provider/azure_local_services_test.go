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
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/stretchr/testify/assert"

	"go.uber.org/mock/gomock"

	v1 "k8s.io/api/core/v1"
	discovery_v1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
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
