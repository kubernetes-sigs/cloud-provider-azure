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
	"slices"
	"strings"
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
	"k8s.io/client-go/tools/record"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/backendaddresspoolclient/mock_backendaddresspoolclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/policy/retryrepectthrottled"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// getBackendPoolUpdaterMetrics returns the current latency observation count
// and failure count for the local service backend pool updater metric.
func getBackendPoolUpdaterMetrics(t *testing.T) (latencyCount uint64, failureCount float64) {
	t.Helper()
	const request = "services_local_update_backend_pool"
	families, err := legacyregistry.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "request" && lp.GetValue() == request {
					switch f.GetName() {
					case "cloudprovider_azure_op_duration_seconds":
						latencyCount = m.GetHistogram().GetSampleCount()
					case "cloudprovider_azure_op_failure_count":
						failureCount = m.GetCounter().GetValue()
					}
				}
			}
		}
	}
	return
}

func TestLoadBalancerBackendPoolUpdater(t *testing.T) {
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
		expectedEventReasons               []string
	}{
		{
			name:       "Add node IPs to backend pool",
			operations: []batchOperation{getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"})},
			existingBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedCreateOrUpdateBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
			expectedEventReasons: []string{consts.LoadBalancerBackendPoolUpdated},
		},
		{
			name:       "Remove node IPs from backend pool",
			operations: []batchOperation{getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{})},
			existingBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
			expectedCreateOrUpdateBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedEventReasons: []string{consts.LoadBalancerBackendPoolUpdated},
		},
		{
			name: "Multiple operations targeting different backend pools",
			operations: []batchOperation{
				getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
				getDesiredStateOperation("ns1/svc1", "lb1", "pool2", []string{"10.0.0.1", "10.0.0.2"}),
			},
			existingBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
				getTestBackendAddressPoolWithIPs("lb1", "pool2", []string{}),
			},
			expectedCreateOrUpdateBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
				getTestBackendAddressPoolWithIPs("lb1", "pool2", []string{"10.0.0.1", "10.0.0.2"}),
			},
			expectedEventReasons: []string{consts.LoadBalancerBackendPoolUpdated, consts.LoadBalancerBackendPoolUpdated},
		},
		{
			name: "Multiple operations in two batches",
			operations: []batchOperation{
				getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
				getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{}),
			},
			extraWait: true,
			existingBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedCreateOrUpdateBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedGetBackendPool: getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			expectedEventReasons:   []string{consts.LoadBalancerBackendPoolUpdated, consts.LoadBalancerBackendPoolUpdated},
		},
		{
			name:                       "remove operations by service name",
			operations:                 []batchOperation{getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"})},
			removeOperationServiceName: "ns1/svc1",
		},
		{
			name:       "not local service",
			operations: []batchOperation{getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"})},
			notLocal:   true,
		},
		{
			name:       "not on this load balancer",
			operations: []batchOperation{getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"})},
			changeLB:   true,
		},
		{
			name:       "empty queue returns without ARM calls",
			operations: nil,
		},
		{
			name:       "empty desired state on empty pool does not trigger CreateOrUpdate",
			operations: []batchOperation{getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{})},
			existingBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
		},
		{
			name:       "adding already-present IPs does not trigger CreateOrUpdate",
			operations: []batchOperation{getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"})},
			existingBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
		},
		{
			name: "desired state deduplicates IPs in empty pool",
			operations: []batchOperation{
				getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1", "10.0.0.1", "10.0.0.2"}),
			},
			existingBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedCreateOrUpdateBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
			expectedEventReasons: []string{consts.LoadBalancerBackendPoolUpdated},
		},
		{
			name:       "desired state with mixed add and remove",
			operations: []batchOperation{getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1", "10.0.0.3"})},
			existingBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
			expectedCreateOrUpdateBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.3"}),
			},
			expectedEventReasons: []string{consts.LoadBalancerBackendPoolUpdated},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			cloud := GetTestCloud(ctrl)
			recorder := record.NewFakeRecorder(100)
			cloud.eventRecorder = recorder
			cloud.localServiceNameToServiceInfoMap = sync.Map{}
			if !tc.notLocal {
				cloud.localServiceNameToServiceInfoMap.Store("ns1/svc1", &serviceInfo{lbName: "lb1"})
			}
			if tc.changeLB {
				cloud.localServiceNameToServiceInfoMap.Store("ns1/svc1", &serviceInfo{lbName: "lb2"})
			}
			svc := getTestService("svc1", v1.ProtocolTCP, nil, false)
			svc.Namespace = "ns1"
			client := fake.NewSimpleClientset(&svc)
			informerFactory := informers.NewSharedInformerFactory(client, 0)
			cloud.serviceLister = informerFactory.Core().V1().Services().Lister()
			stopCh := make(chan struct{})
			t.Cleanup(func() { close(stopCh) })
			informerFactory.Start(stopCh)
			informerFactory.WaitForCacheSync(stopCh)
			mockbpClient := cloud.NetworkClientFactory.GetBackendAddressPoolClient().(*mock_backendaddresspoolclient.MockInterface)
			for _, bp := range tc.existingBackendPools {
				mockbpClient.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", *bp.Name).Return(bp, nil)
			}
			if tc.extraWait {
				mockbpClient.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", *tc.expectedGetBackendPool.Name).Return(tc.expectedGetBackendPool, nil)
			}
			for _, expectedBP := range tc.expectedCreateOrUpdateBackendPools {
				mockbpClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", *expectedBP.Name, *expectedBP).Return(nil, nil)
			}

			u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			latencyBefore, _ := getBackendPoolUpdaterMetrics(t)

			// Use WaitGroup to properly synchronize goroutine completion
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				u.run(ctx)
			}()

			operationsDone := make(chan struct{})
			var operationsWg sync.WaitGroup

			for _, op := range tc.operations {
				op := op
				operationsWg.Add(1)
				go func() {
					defer operationsWg.Done()
					u.addOperation(op)
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

			assertEventReasons(t, drainEvents(recorder), tc.expectedEventReasons...)
			latencyAfter, _ := getBackendPoolUpdaterMetrics(t)
			assert.Equal(t, uint64(len(tc.expectedEventReasons)), latencyAfter-latencyBefore)
		})
	}
}

func TestLoadBalancerBackendPoolUpdaterFailed(t *testing.T) {
	desiredOpPool1 := getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"})

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
			operations: []batchOperation{desiredOpPool1},
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
			operations: []batchOperation{desiredOpPool1},
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
			operations: []batchOperation{desiredOpPool1},
			existingBackendPools: []*armnetwork.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			getBackendPoolErr: &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: "error"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			cloud := GetTestCloud(ctrl)
			cloud.localServiceNameToServiceInfoMap = sync.Map{}
			cloud.localServiceNameToServiceInfoMap.Store("ns1/svc1", &serviceInfo{lbName: "lb1"})
			svc := getTestService("svc1", v1.ProtocolTCP, nil, false)
			svc.Namespace = "ns1"
			client := fake.NewSimpleClientset(&svc)
			informerFactory := informers.NewSharedInformerFactory(client, 0)
			cloud.serviceLister = informerFactory.Core().V1().Services().Lister()
			stopCh := make(chan struct{})
			t.Cleanup(func() { close(stopCh) })
			informerFactory.Start(stopCh)
			informerFactory.WaitForCacheSync(stopCh)
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
			svc.Namespace = "test"
			client := fake.NewSimpleClientset(&svc, tc.existingEPS)
			informerFactory := informers.NewSharedInformerFactory(client, 0)
			cloud.serviceLister = informerFactory.Core().V1().Services().Lister()
			stopCh := make(chan struct{})
			t.Cleanup(func() { close(stopCh) })
			informerFactory.Start(stopCh)
			informerFactory.WaitForCacheSync(stopCh)
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

func TestEndpointSlicesInformerDesiredState(t *testing.T) {
	for _, tc := range []struct {
		name                string
		ipFamily            string
		updatedEPS          *discovery_v1.EndpointSlice
		otherEndpointSlices []*discovery_v1.EndpointSlice
		existingQueueOps    []*loadBalancerBackendPoolUpdateOperation
		nodePrivateIPs      map[string]*utilsets.IgnoreCaseSet
		expectedOps         []*loadBalancerBackendPoolUpdateOperation
	}{
		{
			name:       "multiple endpoints on same node produces unique IPs",
			updatedEPS: getTestEndpointSlice("eps1", "test", "svc1", "node2", "node2", "node2"),
			nodePrivateIPs: map[string]*utilsets.IgnoreCaseSet{
				"node2": utilsets.NewString("10.0.0.2"),
			},
			expectedOps: []*loadBalancerBackendPoolUpdateOperation{
				getDesiredStateOperation("test/svc1", "lb1", "test-svc1", []string{"10.0.0.2"}),
			},
		},
		{
			name:       "unchanged node set preserves retry state on existing op",
			updatedEPS: getTestEndpointSlice("eps1", "test", "svc1", "node1", "node1", "node1"),
			existingQueueOps: []*loadBalancerBackendPoolUpdateOperation{
				{serviceName: "test/svc1", loadBalancerName: "lb1", backendPoolName: "test-svc1", nodeIPs: []string{"10.0.0.1"}, retryAttempts: 2},
			},
			nodePrivateIPs: map[string]*utilsets.IgnoreCaseSet{
				"node1": utilsets.NewString("10.0.0.1"),
			},
			expectedOps: []*loadBalancerBackendPoolUpdateOperation{
				{serviceName: "test/svc1", loadBalancerName: "lb1", backendPoolName: "test-svc1", nodeIPs: []string{"10.0.0.1"}, retryAttempts: 2},
			},
		},
		{
			name:       "update includes IPs from all EndpointSlices for the service",
			updatedEPS: getTestEndpointSlice("eps1", "test", "svc1", "node1", "node3"),
			otherEndpointSlices: []*discovery_v1.EndpointSlice{
				getTestEndpointSlice("eps2", "test", "svc1", "node2"),
			},
			nodePrivateIPs: map[string]*utilsets.IgnoreCaseSet{
				"node1": utilsets.NewString("10.0.0.1"),
				"node2": utilsets.NewString("10.0.0.2"),
				"node3": utilsets.NewString("10.0.0.3"),
			},
			expectedOps: []*loadBalancerBackendPoolUpdateOperation{
				getDesiredStateOperation("test/svc1", "lb1", "test-svc1", []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}),
			},
		},
		{
			name:       "removing endpoints excludes their node IPs",
			updatedEPS: getTestEndpointSlice("eps1", "test", "svc1", "node1"),
			nodePrivateIPs: map[string]*utilsets.IgnoreCaseSet{
				"node1": utilsets.NewString("10.0.0.1"),
			},
			expectedOps: []*loadBalancerBackendPoolUpdateOperation{
				getDesiredStateOperation("test/svc1", "lb1", "test-svc1", []string{"10.0.0.1"}),
			},
		},
		{
			name:       "node with multiple IPs includes all",
			updatedEPS: getTestEndpointSlice("eps1", "test", "svc1", "node1"),
			nodePrivateIPs: map[string]*utilsets.IgnoreCaseSet{
				"node1": utilsets.NewString("10.0.0.1", "10.0.0.2"),
			},
			expectedOps: []*loadBalancerBackendPoolUpdateOperation{
				getDesiredStateOperation("test/svc1", "lb1", "test-svc1", []string{"10.0.0.1", "10.0.0.2"}),
			},
		},
		{
			name:       "scale to zero endpoints enqueues empty desired state",
			updatedEPS: getTestEndpointSlice("eps1", "test", "svc1"),
			nodePrivateIPs: map[string]*utilsets.IgnoreCaseSet{
				"node1": utilsets.NewString("10.0.0.1"),
			},
			expectedOps: []*loadBalancerBackendPoolUpdateOperation{
				getDesiredStateOperation("test/svc1", "lb1", "test-svc1", []string{}),
			},
		},
		{
			name:       "dual-stack service produces one op per pool",
			ipFamily:   consts.IPVersionDualStackString,
			updatedEPS: getTestEndpointSlice("eps1", "test", "svc1", "node1"),
			nodePrivateIPs: map[string]*utilsets.IgnoreCaseSet{
				"node1": utilsets.NewString("10.0.0.1", "fd00::1"),
			},
			expectedOps: []*loadBalancerBackendPoolUpdateOperation{
				getDesiredStateOperation("test/svc1", "lb1", "test-svc1-ipv6", []string{"fd00::1"}),
				getDesiredStateOperation("test/svc1", "lb1", "test-svc1", []string{"10.0.0.1"}),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			cloud := GetTestCloud(ctrl)
			cloud.localServiceNameToServiceInfoMap = sync.Map{}
			ipFamily := tc.ipFamily
			if ipFamily == "" {
				ipFamily = consts.IPVersionIPv4String
			}
			cloud.localServiceNameToServiceInfoMap.Store("test/svc1", newServiceInfo(ipFamily, "lb1"))
			cloud.LoadBalancerBackendPoolUpdateIntervalInSeconds = 1
			cloud.LoadBalancerSKU = consts.LoadBalancerSKUStandard
			cloud.MultipleStandardLoadBalancerConfigurations = []config.MultipleStandardLoadBalancerConfiguration{
				{Name: "lb1"},
			}
			cloud.nodePrivateIPs = tc.nodePrivateIPs

			svc := getTestService("svc1", v1.ProtocolTCP, nil, false)
			svc.Namespace = "test"
			client := fake.NewSimpleClientset(&svc, getTestEndpointSlice("eps1", "test", "svc1"))
			informerFactory := informers.NewSharedInformerFactory(client, 0)
			cloud.serviceLister = informerFactory.Core().V1().Services().Lister()
			for _, eps := range tc.otherEndpointSlices {
				cloud.endpointSlicesCache.Store(
					fmt.Sprintf("%s/%s", eps.Namespace, eps.Name), eps)
			}
			stopCh := make(chan struct{})
			t.Cleanup(func() { close(stopCh) })
			informerFactory.Start(stopCh)
			informerFactory.WaitForCacheSync(stopCh)

			// Create updater without starting run() so we can inspect queued operations.
			u := newLoadBalancerBackendPoolUpdater(cloud, time.Hour)
			cloud.backendPoolUpdater = u

			for _, op := range tc.existingQueueOps {
				u.addOperation(op)
			}

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
			// Sort nodeIPs for stable comparison; order is not significant at runtime.
			for _, op := range actual {
				slices.Sort(op.nodeIPs)
			}
			for _, op := range tc.expectedOps {
				slices.Sort(op.nodeIPs)
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
		description  string
		existingEPS  *discovery_v1.EndpointSlice
		existingIPv4 []string
		existingIPv6 []string
		expectedIPv4 []string
		expectedIPv6 []string
	}{
		{
			description:  "should update backend pool as expected",
			existingEPS:  getTestEndpointSlice("eps1", "default", "svc1", "node2"),
			existingIPv4: []string{"10.0.0.1"},
			existingIPv6: []string{"fd00::1"},
			expectedIPv4: []string{"10.0.0.2"},
			expectedIPv6: []string{"fd00::2"},
		},
		{
			description:  "should not report an error if failed to get the endpointslice",
			existingIPv4: []string{"10.0.0.1"},
			existingIPv6: []string{"fd00::1"},
		},
		{
			description:  "should not update backend pool when IPs already match",
			existingEPS:  getTestEndpointSlice("eps1", "default", "svc1", "node1"),
			existingIPv4: []string{"10.0.0.1"},
			existingIPv6: []string{"fd00::1"},
		},
		{
			description:  "should remove all IPs when endpoints scale to zero",
			existingEPS:  getTestEndpointSlice("eps1", "default", "svc1"),
			existingIPv4: []string{"10.0.0.1"},
			existingIPv6: []string{"fd00::1"},
			expectedIPv4: []string{},
			expectedIPv6: []string{},
		},
		{
			description:  "should not update when empty pool and empty endpoints",
			existingEPS:  getTestEndpointSlice("eps1", "default", "svc1"),
			existingIPv4: []string{},
			existingIPv6: []string{},
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
			stopCh := make(chan struct{})
			t.Cleanup(func() { close(stopCh) })
			informerFactory.Start(stopCh)
			informerFactory.WaitForCacheSync(stopCh)
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

			existingBackendPool := getTestBackendAddressPoolWithIPs("lb1", "default-svc1", tc.existingIPv4)
			existingBackendPoolIPv6 := getTestBackendAddressPoolWithIPs("lb1", "default-svc1-ipv6", tc.existingIPv6)
			existingLB := armnetwork.LoadBalancer{
				Name: ptr.To("lb1"),
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					BackendAddressPools: []*armnetwork.BackendAddressPool{
						existingBackendPool,
						existingBackendPoolIPv6,
					},
				},
			}
			mockLBClient := cloud.NetworkClientFactory.GetBackendAddressPoolClient().(*mock_backendaddresspoolclient.MockInterface)
			if tc.expectedIPv4 != nil {
				expectedBackendPool := getTestBackendAddressPoolWithIPs("lb1", "default-svc1", tc.expectedIPv4)
				mockLBClient.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "default-svc1").Return(existingBackendPool, nil)
				mockLBClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "default-svc1", *expectedBackendPool).Return(nil, nil)
			}
			if tc.expectedIPv6 != nil {
				expectedBackendPoolIPv6 := getTestBackendAddressPoolWithIPs("lb1", "default-svc1-ipv6", tc.expectedIPv6)
				mockLBClient.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "default-svc1-ipv6").Return(existingBackendPoolIPv6, nil)
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

	t.Run("should not panic on nil address properties", func(t *testing.T) {
		cloud := GetTestCloud(ctrl)
		cloud.localServiceNameToServiceInfoMap.Store("default/svc1", newServiceInfo(consts.IPVersionIPv4String, "lb1"))
		cloud.nodePrivateIPs = map[string]*utilsets.IgnoreCaseSet{
			"node1": utilsets.NewString("10.0.0.1"),
		}
		cloud.endpointSlicesCache.Store("eps1/default", getTestEndpointSlice("eps1", "default", "svc1", "node1"))
		cloud.backendPoolUpdater = newLoadBalancerBackendPoolUpdater(cloud, time.Hour)

		svc := getTestService("svc1", v1.ProtocolTCP, nil, false)
		bp := getTestBackendAddressPoolWithIPs("lb1", "default-svc1", []string{"10.0.0.1"})
		bp.Properties.LoadBalancerBackendAddresses = append(
			bp.Properties.LoadBalancerBackendAddresses,
			&armnetwork.LoadBalancerBackendAddress{Properties: nil},
		)
		existingLB := armnetwork.LoadBalancer{
			Name: ptr.To("lb1"),
			Properties: &armnetwork.LoadBalancerPropertiesFormat{
				BackendAddressPools: []*armnetwork.BackendAddressPool{bp},
			},
		}

		err := cloud.checkAndApplyLocalServiceBackendPoolUpdates(existingLB, &svc)
		assert.NoError(t, err)
	})
}

func TestAreNodeIPsEqual(t *testing.T) {
	for _, tc := range []struct {
		desc                    string
		currentIPs, expectedIPs []string
		equal                   bool
	}{
		{
			desc:        "same IPs same order",
			currentIPs:  []string{"10.0.0.1", "10.0.0.2"},
			expectedIPs: []string{"10.0.0.1", "10.0.0.2"},
			equal:       true,
		},
		{
			desc:        "same IPs different order",
			currentIPs:  []string{"10.0.0.2", "10.0.0.1"},
			expectedIPs: []string{"10.0.0.1", "10.0.0.2"},
			equal:       true,
		},
		{
			desc:        "expected IPs need add and delete",
			currentIPs:  []string{"10.0.0.1"},
			expectedIPs: []string{"10.0.0.2"},
			equal:       false,
		},
		{
			desc:        "expected IPs need add",
			currentIPs:  []string{"10.0.0.1"},
			expectedIPs: []string{"10.0.0.1", "10.0.0.2"},
			equal:       false,
		},
		{
			desc:        "expected IPs need delete",
			currentIPs:  []string{"10.0.0.1", "10.0.0.2"},
			expectedIPs: []string{"10.0.0.1"},
			equal:       false,
		},
		{
			desc:  "no IPs",
			equal: true,
		},
		{
			desc:       "expected IPs are empty",
			currentIPs: []string{"10.0.0.1"},
			equal:      false,
		},
		{
			desc:        "nil current with expected IPs",
			currentIPs:  nil,
			expectedIPs: []string{"10.0.0.1"},
			equal:       false,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			assert.Equal(t, tc.equal, areNodeIPsEqual(tc.currentIPs, tc.expectedIPs))
		})
	}
}

func TestAddOperation(t *testing.T) {
	futureTime := time.Now().Add(1 * time.Hour)

	for _, tc := range []struct {
		name        string
		ops         []*loadBalancerBackendPoolUpdateOperation
		expectedOps []*loadBalancerBackendPoolUpdateOperation
	}{
		{
			name: "replaces op for same target with latest IPs",
			ops: []*loadBalancerBackendPoolUpdateOperation{
				{serviceName: "ns1/svc1", loadBalancerName: "lb1", backendPoolName: "pool1", nodeIPs: []string{"10.0.0.1", "10.0.0.2"}},
				{serviceName: "ns1/svc1", loadBalancerName: "lb1", backendPoolName: "pool1", nodeIPs: []string{"10.0.0.3", "10.0.0.4"}},
			},
			expectedOps: []*loadBalancerBackendPoolUpdateOperation{
				{serviceName: "ns1/svc1", loadBalancerName: "lb1", backendPoolName: "pool1", nodeIPs: []string{"10.0.0.3", "10.0.0.4"}},
			},
		},
		{
			name: "different services are independent",
			ops: []*loadBalancerBackendPoolUpdateOperation{
				{serviceName: "ns1/svc1", loadBalancerName: "lb1", backendPoolName: "pool1", nodeIPs: []string{"10.0.0.1"}},
				{serviceName: "ns1/svc2", loadBalancerName: "lb1", backendPoolName: "pool2", nodeIPs: []string{"10.0.0.2"}},
			},
			expectedOps: []*loadBalancerBackendPoolUpdateOperation{
				{serviceName: "ns1/svc1", loadBalancerName: "lb1", backendPoolName: "pool1", nodeIPs: []string{"10.0.0.1"}},
				{serviceName: "ns1/svc2", loadBalancerName: "lb1", backendPoolName: "pool2", nodeIPs: []string{"10.0.0.2"}},
			},
		},
		{
			name: "different pools are independent",
			ops: []*loadBalancerBackendPoolUpdateOperation{
				{serviceName: "ns1/svc1", loadBalancerName: "lb1", backendPoolName: "poolA", nodeIPs: []string{"10.0.0.1"}},
				{serviceName: "ns1/svc1", loadBalancerName: "lb1", backendPoolName: "poolB", nodeIPs: []string{"10.0.0.2"}},
			},
			expectedOps: []*loadBalancerBackendPoolUpdateOperation{
				{serviceName: "ns1/svc1", loadBalancerName: "lb1", backendPoolName: "poolA", nodeIPs: []string{"10.0.0.1"}},
				{serviceName: "ns1/svc1", loadBalancerName: "lb1", backendPoolName: "poolB", nodeIPs: []string{"10.0.0.2"}},
			},
		},
		{
			name: "preserves nextEligibleAt on replace",
			ops: []*loadBalancerBackendPoolUpdateOperation{
				{serviceName: "ns1/svc1", loadBalancerName: "lb1", backendPoolName: "pool1", nodeIPs: []string{"10.0.0.1"}, nextEligibleAt: futureTime},
				{serviceName: "ns1/svc1", loadBalancerName: "lb1", backendPoolName: "pool1", nodeIPs: []string{"10.0.0.3"}},
			},
			expectedOps: []*loadBalancerBackendPoolUpdateOperation{
				{serviceName: "ns1/svc1", loadBalancerName: "lb1", backendPoolName: "pool1", nodeIPs: []string{"10.0.0.3"}, nextEligibleAt: futureTime},
			},
		},
		{
			name: "resets retryAttempts on replace",
			ops: []*loadBalancerBackendPoolUpdateOperation{
				{serviceName: "ns1/svc1", loadBalancerName: "lb1", backendPoolName: "pool1", nodeIPs: []string{"10.0.0.1"}, retryAttempts: 2},
				{serviceName: "ns1/svc1", loadBalancerName: "lb1", backendPoolName: "pool1", nodeIPs: []string{"10.0.0.3"}},
			},
			expectedOps: []*loadBalancerBackendPoolUpdateOperation{
				{serviceName: "ns1/svc1", loadBalancerName: "lb1", backendPoolName: "pool1", nodeIPs: []string{"10.0.0.3"}},
			},
		},
		{
			name: "skips when IPs unchanged",
			ops: []*loadBalancerBackendPoolUpdateOperation{
				{serviceName: "ns1/svc1", loadBalancerName: "lb1", backendPoolName: "pool1", nodeIPs: []string{"10.0.0.1", "10.0.0.2"}, retryAttempts: 2, nextEligibleAt: futureTime},
				{serviceName: "ns1/svc1", loadBalancerName: "lb1", backendPoolName: "pool1", nodeIPs: []string{"10.0.0.1", "10.0.0.2"}},
			},
			expectedOps: []*loadBalancerBackendPoolUpdateOperation{
				{serviceName: "ns1/svc1", loadBalancerName: "lb1", backendPoolName: "pool1", nodeIPs: []string{"10.0.0.1", "10.0.0.2"}, retryAttempts: 2, nextEligibleAt: futureTime},
			},
		},
		{
			name: "replace with empty desired state",
			ops: []*loadBalancerBackendPoolUpdateOperation{
				{serviceName: "ns1/svc1", loadBalancerName: "lb1", backendPoolName: "pool1", nodeIPs: []string{"10.0.0.1", "10.0.0.2"}},
				{serviceName: "ns1/svc1", loadBalancerName: "lb1", backendPoolName: "pool1", nodeIPs: []string{}},
			},
			expectedOps: []*loadBalancerBackendPoolUpdateOperation{
				{serviceName: "ns1/svc1", loadBalancerName: "lb1", backendPoolName: "pool1", nodeIPs: []string{}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cloud := &Cloud{}
			u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)

			for _, op := range tc.ops {
				u.addOperation(op)
			}

			drained := u.drainOperations()
			var actual []*loadBalancerBackendPoolUpdateOperation
			for _, op := range drained {
				actual = append(actual, op.(*loadBalancerBackendPoolUpdateOperation))
			}
			assert.Equal(t, tc.expectedOps, actual)
		})
	}
}

func TestCountOperations(t *testing.T) {
	op1 := getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1"})
	op2 := getDesiredStateOperation("ns1/svc2", "lb1", "pool1", []string{"10.0.0.2"})

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
	op1 := getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1"})
	op2 := getDesiredStateOperation("ns1/svc2", "lb1", "pool2", []string{"10.0.0.2"})

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

func TestHasParkedOperations(t *testing.T) {
	cloud := &Cloud{}
	u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)

	for _, tc := range []struct {
		name     string
		ops      []batchOperation
		expected bool
	}{
		{name: "empty slice", ops: nil, expected: false},
		{name: "zero nextEligibleAt", ops: []batchOperation{
			getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1"}),
		}, expected: false},
		{name: "past nextEligibleAt", ops: func() []batchOperation {
			op := getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1"})
			op.nextEligibleAt = time.Now().Add(-time.Minute)
			return []batchOperation{op}
		}(), expected: false},
		{name: "future nextEligibleAt", ops: func() []batchOperation {
			op := getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1"})
			op.nextEligibleAt = time.Now().Add(5 * time.Minute)
			return []batchOperation{op}
		}(), expected: true},
		{name: "mix of past and future", ops: func() []batchOperation {
			op1 := getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1"})
			op2 := getDesiredStateOperation("ns1/svc2", "lb1", "pool1", []string{"10.0.0.2"})
			op2.nextEligibleAt = time.Now().Add(5 * time.Minute)
			return []batchOperation{op1, op2}
		}(), expected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, u.hasParkedOperations(tc.ops))
		})
	}
}

func TestRequeueOperations(t *testing.T) {
	op1 := getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1"})
	op2 := getDesiredStateOperation("ns1/svc2", "lb1", "pool1", []string{"10.0.0.2"})
	op3 := getDesiredStateOperation("ns1/svc3", "lb1", "pool1", []string{"10.0.0.3"})

	t.Run("requeue to empty queue", func(t *testing.T) {
		cloud := &Cloud{}
		u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)

		u.requeueOperations([]batchOperation{op1, op2})

		ops := u.drainOperations()
		assert.Equal(t, []batchOperation{op1, op2}, ops)
	})

	t.Run("requeue prepends before existing", func(t *testing.T) {
		cloud := &Cloud{}
		u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
		u.addOperation(op3)

		u.requeueOperations([]batchOperation{op1, op2})

		ops := u.drainOperations()
		assert.Equal(t, []batchOperation{op1, op2, op3}, ops)
	})

	t.Run("acquires updater lock", func(t *testing.T) {
		cloud := &Cloud{}
		u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)

		u.lock.Lock()

		done := make(chan struct{})
		go func() {
			defer close(done)
			u.requeueOperations([]batchOperation{op1})
		}()

		select {
		case <-done:
			t.Fatal("requeueOperations returned while lock was held")
		case <-time.After(100 * time.Millisecond):
		}

		u.lock.Unlock()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("requeueOperations did not complete after lock released")
		}
	})

	t.Run("requeue drops op if op for same target already exists", func(t *testing.T) {
		cloud := &Cloud{}
		u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)

		u.addOperation(op1)

		// Stale op being requeued after failure.
		staleOp := getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.5"})
		staleOp.retryAttempts = 1
		u.requeueOperations([]batchOperation{staleOp})

		// Only the fresh op should remain.
		ops := u.drainOperations()
		assert.Equal(t, 1, len(ops))
		assert.Equal(t, op1, ops[0])
	})

	for _, tc := range []struct {
		name                   string
		freshNextEligibleAt    time.Time
		staleNextEligibleAt    time.Time
		expectedNextEligibleAt time.Time
	}{
		{
			name:                   "transfers nextEligibleAt to fresh op",
			staleNextEligibleAt:    time.Now().Add(1 * time.Hour),
			expectedNextEligibleAt: time.Now().Add(1 * time.Hour),
		},
		{
			name:                   "transfers nextEligibleAt to fresh op when its value is earlier",
			freshNextEligibleAt:    time.Now().Add(1 * time.Hour),
			staleNextEligibleAt:    time.Now().Add(2 * time.Hour),
			expectedNextEligibleAt: time.Now().Add(2 * time.Hour),
		},
		{
			name:                   "preserves fresh op nextEligibleAt when it is already later",
			freshNextEligibleAt:    time.Now().Add(2 * time.Hour),
			staleNextEligibleAt:    time.Now().Add(1 * time.Hour),
			expectedNextEligibleAt: time.Now().Add(2 * time.Hour),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cloud := &Cloud{}
			u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)

			freshOp := getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.3"})
			freshOp.nextEligibleAt = tc.freshNextEligibleAt
			u.addOperation(freshOp)

			staleOp := getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1"})
			staleOp.nextEligibleAt = tc.staleNextEligibleAt
			staleOp.retryAttempts = 1
			u.requeueOperations([]batchOperation{staleOp})

			ops := u.drainOperations()
			assert.Equal(t, 1, len(ops))
			op := ops[0].(*loadBalancerBackendPoolUpdateOperation)
			assert.ElementsMatch(t, []string{"10.0.0.3"}, op.nodeIPs, "fresh op IPs should be kept")
			assert.InDelta(t, tc.expectedNextEligibleAt.UnixNano(), op.nextEligibleAt.UnixNano(), float64(time.Second), "nextEligibleAt should use max")
		})
	}
}

func TestGroupOperations(t *testing.T) {
	opPool1 := getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1"})
	opPool2 := getDesiredStateOperation("ns1/svc2", "lb1", "pool2", []string{"10.0.0.2"})

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
			operations:     []batchOperation{opPool1},
			localServices:  map[string]*serviceInfo{"ns1/svc1": {lbName: "lb1"}},
			expectedGroups: map[string][]batchOperation{"lb1:pool1": {opPool1}},
		},
		{
			name:       "groups operations targeting different pools separately",
			operations: []batchOperation{opPool1, opPool2},
			localServices: map[string]*serviceInfo{
				"ns1/svc1": {lbName: "lb1"},
				"ns1/svc2": {lbName: "lb1"},
			},
			expectedGroups: map[string][]batchOperation{"lb1:pool1": {opPool1}, "lb1:pool2": {opPool2}},
		},
		{
			name:           "skips operations for non-local services",
			operations:     []batchOperation{opPool1},
			localServices:  map[string]*serviceInfo{},
			expectedGroups: map[string][]batchOperation{},
		},
		{
			name:           "skips operations targeting stale load balancer",
			operations:     []batchOperation{opPool1},
			localServices:  map[string]*serviceInfo{"ns1/svc1": {lbName: "lb2"}},
			expectedGroups: map[string][]batchOperation{},
		},
		{
			name:          "keeps valid operation when another is filtered",
			operations:    []batchOperation{opPool1, opPool2},
			localServices: map[string]*serviceInfo{"ns1/svc1": {lbName: "lb1"}},
			expectedGroups: map[string][]batchOperation{
				"lb1:pool1": {opPool1},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cloud := &Cloud{}
			var svcObjects []runtime.Object
			for k, v := range tc.localServices {
				cloud.localServiceNameToServiceInfoMap.Store(k, v)
				parts := strings.Split(k, "/")
				svcObjects = append(svcObjects, &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Namespace: parts[0], Name: parts[1]},
				})
			}
			client := fake.NewSimpleClientset(svcObjects...)
			informerFactory := informers.NewSharedInformerFactory(client, 0)
			cloud.serviceLister = informerFactory.Core().V1().Services().Lister()
			stopCh := make(chan struct{})
			t.Cleanup(func() { close(stopCh) })
			informerFactory.Start(stopCh)
			informerFactory.WaitForCacheSync(stopCh)

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

	t.Run("skips operation for service in map but deleted from informer", func(t *testing.T) {
		cloud := &Cloud{}
		cloud.localServiceNameToServiceInfoMap.Store("ns1/svc1", &serviceInfo{lbName: "lb1"})

		// Informer has no Service object (simulates failed deletion leaving stale map entry).
		client := fake.NewSimpleClientset()
		informerFactory := informers.NewSharedInformerFactory(client, 0)
		cloud.serviceLister = informerFactory.Core().V1().Services().Lister()
		stopCh := make(chan struct{})
		t.Cleanup(func() { close(stopCh) })
		informerFactory.Start(stopCh)
		informerFactory.WaitForCacheSync(stopCh)

		u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
		op := getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1"})

		groups := u.groupOperations(context.Background(), []batchOperation{op})

		assert.Equal(t, 0, len(groups))
	})
}

func TestLoadBalancerBackendPoolUpdaterSerializesWithServiceReconcileLock(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	cloud := GetTestCloud(ctrl)
	cloud.localServiceNameToServiceInfoMap = sync.Map{}
	cloud.localServiceNameToServiceInfoMap.Store("ns1/svc1", &serviceInfo{lbName: "lb1"})

	svc := getTestService("svc1", v1.ProtocolTCP, nil, false)
	svc.Namespace = "ns1"
	client := fake.NewSimpleClientset(&svc)
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	cloud.serviceLister = informerFactory.Core().V1().Services().Lister()
	stopCh := make(chan struct{})
	t.Cleanup(func() { close(stopCh) })
	informerFactory.Start(stopCh)
	informerFactory.WaitForCacheSync(stopCh)

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
	u.addOperation(getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1"}))

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
	svc.Namespace = "ns1"
	client := fake.NewSimpleClientset(&svc)
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	cloud.serviceLister = informerFactory.Core().V1().Services().Lister()
	stopCh := make(chan struct{})
	t.Cleanup(func() { close(stopCh) })
	informerFactory.Start(stopCh)
	informerFactory.WaitForCacheSync(stopCh)

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
	u.addOperation(getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1"}))

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
		u.addOperation(getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.2"}))
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
	u.addOperation(getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1"}))

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
	svc.Namespace = "ns1"
	client := fake.NewSimpleClientset(&svc)
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	cloud.serviceLister = informerFactory.Core().V1().Services().Lister()
	stopCh := make(chan struct{})
	t.Cleanup(func() { close(stopCh) })
	informerFactory.Start(stopCh)
	informerFactory.WaitForCacheSync(stopCh)

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
	u.addOperation(getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1"}))

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
	svc.Namespace = "ns1"
	client := fake.NewSimpleClientset(&svc)
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	cloud.serviceLister = informerFactory.Core().V1().Services().Lister()
	stopCh := make(chan struct{})
	t.Cleanup(func() { close(stopCh) })
	informerFactory.Start(stopCh)
	informerFactory.WaitForCacheSync(stopCh)

	// No ARM mock expectations. The operation targets lb1 but the service moves
	// to lb2 while process() is blocked on serviceReconcileLock, so groupOperations
	// filters it.

	u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
	u.addOperation(getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1"}))

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
	svc.Namespace = "ns1"
	client := fake.NewSimpleClientset(&svc)
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	cloud.serviceLister = informerFactory.Core().V1().Services().Lister()
	stopCh := make(chan struct{})
	t.Cleanup(func() { close(stopCh) })
	informerFactory.Start(stopCh)
	informerFactory.WaitForCacheSync(stopCh)

	// No ARM mock expectations. removeOperation cancels the operation
	// before process() drains the queue.

	u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
	u.addOperation(getDesiredStateOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1"}))

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

// setupRetryTest creates a Cloud with a fake event recorder, service lister,
// and mock backend pool client for retry tests. Each call creates a fresh
// gomock.Controller scoped to t (auto-Finish via t.Cleanup). svcNames defaults
// to ["svc1"] when omitted. All services are registered in
// localServiceNameToServiceInfoMap with lbName "lb1".
func setupRetryTest(t *testing.T, maxRetries int, svcNames ...string) (
	*Cloud, *record.FakeRecorder, *mock_backendaddresspoolclient.MockInterface,
) {
	t.Helper()
	ctrl := gomock.NewController(t)

	cloud := GetTestCloud(ctrl)
	cloud.localServiceNameToServiceInfoMap = sync.Map{}

	if len(svcNames) == 0 {
		svcNames = []string{"svc1"}
	}

	var svcObjects []runtime.Object
	for _, name := range svcNames {
		cloud.localServiceNameToServiceInfoMap.Store("default/"+name, &serviceInfo{lbName: "lb1"})
		svc := getTestService(name, v1.ProtocolTCP, nil, false)
		svcObjects = append(svcObjects, &svc)
	}
	cloud.LoadBalancerBackendPoolUpdateMaxRetries = ptr.To(maxRetries)

	fakeRecorder := record.NewFakeRecorder(100)
	cloud.eventRecorder = fakeRecorder

	client := fake.NewSimpleClientset(svcObjects...)
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	cloud.serviceLister = informerFactory.Core().V1().Services().Lister()

	stopCh := make(chan struct{})
	t.Cleanup(func() { close(stopCh) })
	informerFactory.Start(stopCh)
	informerFactory.WaitForCacheSync(stopCh)

	mockBP := cloud.NetworkClientFactory.GetBackendAddressPoolClient().(*mock_backendaddresspoolclient.MockInterface)
	return cloud, fakeRecorder, mockBP
}

// drainEvents reads all buffered events from a FakeRecorder without blocking.
func drainEvents(recorder *record.FakeRecorder) []string {
	var events []string
	for {
		select {
		case e := <-recorder.Events:
			events = append(events, e)
		default:
			return events
		}
	}
}

// assertEventReasons asserts events match the expected reasons in order.
func assertEventReasons(t *testing.T, events []string, expectedReasons ...string) {
	t.Helper()
	if len(events) != len(expectedReasons) {
		t.Fatalf("expected %d events, got %d: %v", len(expectedReasons), len(events), events)
	}
	for i, reason := range expectedReasons {
		assert.Contains(t, events[i], reason)
	}
}

func TestLoadBalancerBackendPoolUpdaterRetry(t *testing.T) {
	// Error Classification
	for _, tc := range []struct {
		name              string
		maxRetries        int
		getErr            error
		createOrUpdateErr error
		expectedQueue     int
		expectedEventType string
		expectedEvent     string
		terminalMsg       string
	}{
		{
			name:              "successful update emits Updated event",
			maxRetries:        3,
			expectedQueue:     0,
			expectedEventType: v1.EventTypeNormal,
			expectedEvent:     consts.LoadBalancerBackendPoolUpdated,
		},
		{
			name:              "500 from SDK retry emits Failed event",
			maxRetries:        3,
			getErr:            &azcore.ResponseError{StatusCode: http.StatusInternalServerError},
			expectedQueue:     0,
			expectedEventType: v1.EventTypeWarning,
			expectedEvent:     consts.LoadBalancerBackendPoolUpdateFailed,
			terminalMsg:       "SDK retries exhausted",
		},
		{
			name:              "408 from SDK retry emits Failed event",
			maxRetries:        3,
			getErr:            &azcore.ResponseError{StatusCode: http.StatusRequestTimeout},
			expectedQueue:     0,
			expectedEventType: v1.EventTypeWarning,
			expectedEvent:     consts.LoadBalancerBackendPoolUpdateFailed,
			terminalMsg:       "SDK retries exhausted",
		},
		{
			name:              "502 from SDK retry emits Failed event",
			maxRetries:        3,
			getErr:            &azcore.ResponseError{StatusCode: http.StatusBadGateway},
			expectedQueue:     0,
			expectedEventType: v1.EventTypeWarning,
			expectedEvent:     consts.LoadBalancerBackendPoolUpdateFailed,
			terminalMsg:       "SDK retries exhausted",
		},
		{
			name:              "503 from SDK retry emits Failed event",
			maxRetries:        3,
			getErr:            &azcore.ResponseError{StatusCode: http.StatusServiceUnavailable},
			expectedQueue:     0,
			expectedEventType: v1.EventTypeWarning,
			expectedEvent:     consts.LoadBalancerBackendPoolUpdateFailed,
			terminalMsg:       "SDK retries exhausted",
		},
		{
			name:              "504 from SDK retry emits Failed event",
			maxRetries:        3,
			getErr:            &azcore.ResponseError{StatusCode: http.StatusGatewayTimeout},
			expectedQueue:     0,
			expectedEventType: v1.EventTypeWarning,
			expectedEvent:     consts.LoadBalancerBackendPoolUpdateFailed,
			terminalMsg:       "SDK retries exhausted",
		},
		{
			name:          "404 not found is dropped without event",
			maxRetries:    3,
			getErr:        &azcore.ResponseError{StatusCode: http.StatusNotFound},
			expectedQueue: 0,
		},
		{
			name:              "non-ARM error emits Failed event",
			maxRetries:        3,
			getErr:            fmt.Errorf("connection reset"),
			expectedQueue:     0,
			expectedEventType: v1.EventTypeWarning,
			expectedEvent:     consts.LoadBalancerBackendPoolUpdateFailed,
			terminalMsg:       "non-retriable",
		},
		{
			name:              "retriable 409 with MaxRetries 0 emits Failed event on first failure",
			maxRetries:        0,
			createOrUpdateErr: &azcore.ResponseError{StatusCode: http.StatusConflict},
			expectedQueue:     0,
			expectedEventType: v1.EventTypeWarning,
			expectedEvent:     consts.LoadBalancerBackendPoolUpdateFailed,
		},
		{
			name:              "409 conflict requeues and emits Retrying event",
			maxRetries:        3,
			createOrUpdateErr: &azcore.ResponseError{StatusCode: http.StatusConflict},
			expectedQueue:     1,
			expectedEventType: v1.EventTypeWarning,
			expectedEvent:     consts.LoadBalancerBackendPoolUpdateRetrying,
		},
		{
			name:              "412 precondition failed requeues and emits Retrying event",
			maxRetries:        3,
			createOrUpdateErr: &azcore.ResponseError{StatusCode: http.StatusPreconditionFailed},
			expectedQueue:     1,
			expectedEventType: v1.EventTypeWarning,
			expectedEvent:     consts.LoadBalancerBackendPoolUpdateRetrying,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cloud, recorder, mockBP := setupRetryTest(t, tc.maxRetries)

			if tc.getErr != nil {
				mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool1").Return(nil, tc.getErr).Times(1)
			} else {
				mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool1").Return(
					getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}), nil,
				).Times(1)
				mockBP.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "pool1", gomock.Any()).Return(nil, tc.createOrUpdateErr).Times(1)
			}

			u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
			u.addOperation(getDesiredStateOperation("default/svc1", "lb1", "pool1", []string{"10.0.0.1"}))

			latencyBefore, failureBefore := getBackendPoolUpdaterMetrics(t)

			u.process(context.Background())

			assert.Equal(t, tc.expectedQueue, u.countOperations())

			latencyAfter, failureAfter := getBackendPoolUpdaterMetrics(t)

			switch tc.expectedEvent {
			case consts.LoadBalancerBackendPoolUpdateFailed:
				assert.Equal(t, latencyBefore+1, latencyAfter, "failed operation should record one metric observation")
				assert.Equal(t, failureBefore+1, failureAfter, "failed operation should increment failure counter")
			case consts.LoadBalancerBackendPoolUpdated:
				assert.Equal(t, latencyBefore+1, latencyAfter, "successful operation should record one metric observation")
				assert.Equal(t, failureBefore, failureAfter, "successful operation should not increment failure counter")
			case consts.LoadBalancerBackendPoolUpdateRetrying:
				assert.Equal(t, latencyBefore, latencyAfter, "retrying operation should not record any metric")
				assert.Equal(t, failureBefore, failureAfter, "retrying operation should not increment failure counter")
			default:
				assert.Equal(t, latencyBefore, latencyAfter, "stale operation should not record any metric")
				assert.Equal(t, failureBefore, failureAfter, "stale operation should not increment failure counter")
			}

			events := drainEvents(recorder)
			if tc.expectedEvent == "" {
				assert.Empty(t, events, "expected no events")
			} else {
				assert.Equal(t, 1, len(events), "expected exactly one event")
				if len(events) > 0 {
					assert.Contains(t, events[0], tc.expectedEventType, "unexpected event type")
					assert.Contains(t, events[0], tc.expectedEvent)

					// Assert message format based on error classification.
					if tc.terminalMsg != "" {
						assert.Contains(t, events[0], tc.terminalMsg)
					} else {
						assert.NotContains(t, events[0], "SDK retries exhausted")
						assert.NotContains(t, events[0], "non-retriable")
					}
					switch tc.expectedEvent {
					case consts.LoadBalancerBackendPoolUpdateFailed:
						if tc.terminalMsg == "" {
							assert.Contains(t, events[0], "retries")
							assert.Contains(t, events[0], "retrigger")
						}
					case consts.LoadBalancerBackendPoolUpdateRetrying:
						assert.Contains(t, events[0], "will retry")
					case consts.LoadBalancerBackendPoolUpdated:
						assert.Contains(t, events[0], "updated successfully")
					}

					// Assert the causal error string appears in the event message.
					errToCheck := tc.getErr
					if errToCheck == nil {
						errToCheck = tc.createOrUpdateErr
					}
					if errToCheck != nil {
						assert.Contains(t, events[0], errToCheck.Error())
					}
				}
			}
		})
	}

	// Retriable Error Paths
	for _, tc := range []struct {
		name              string
		getErr            error
		createOrUpdateErr error
	}{
		{
			name:   "ThrottleError from Get parks until RetryAfter",
			getErr: &retryrepectthrottled.ThrottleError{RetryAfter: time.Now().Add(5 * time.Minute)},
		},
		{
			name:              "ThrottleError from CreateOrUpdate parks until RetryAfter",
			createOrUpdateErr: &retryrepectthrottled.ThrottleError{RetryAfter: time.Now().Add(5 * time.Minute)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cloud, recorder, mockBP := setupRetryTest(t, 3)

			if tc.getErr != nil {
				mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool1").Return(nil, tc.getErr).Times(1)
			} else {
				mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool1").Return(
					getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}), nil,
				).Times(1)
				mockBP.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "pool1", gomock.Any()).Return(nil, tc.createOrUpdateErr).Times(1)
			}

			u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
			u.addOperation(getDesiredStateOperation("default/svc1", "lb1", "pool1", []string{"10.0.0.1"}))

			// Tick 1: ThrottleError, requeues with backoff.
			u.process(context.Background())
			assert.Equal(t, 1, u.countOperations())
			assertEventReasons(t, drainEvents(recorder), consts.LoadBalancerBackendPoolUpdateRetrying)

			latencyBeforePark, failureBeforePark := getBackendPoolUpdaterMetrics(t)

			// Tick 2: still parked, skipped.
			u.process(context.Background())
			assert.Equal(t, 1, u.countOperations())
			assert.Empty(t, drainEvents(recorder), "expected no events")

			latencyAfterPark, failureAfterPark := getBackendPoolUpdaterMetrics(t)
			assert.Equal(t, latencyBeforePark, latencyAfterPark, "parked tick should not record any metric")
			assert.Equal(t, failureBeforePark, failureAfterPark, "parked tick should not increment failure counter")

			ops := u.drainOperations()
			assert.Equal(t, 1, len(ops))
			lbOp := ops[0].(*loadBalancerBackendPoolUpdateOperation)
			assert.Equal(t, 1, lbOp.retryAttempts, "retryAttempts should not increment on parked tick")
			assert.True(t, lbOp.nextEligibleAt.After(time.Now()), "nextEligibleAt should still be in the future")
		})
	}

	for _, tc := range []struct {
		name              string
		createOrUpdateErr error
	}{
		{
			name:              "ThrottleError with past RetryAfter requeues without parking",
			createOrUpdateErr: &retryrepectthrottled.ThrottleError{RetryAfter: time.Now().Add(-time.Second)},
		},
		{
			name:              "409 requeues then succeeds on next tick",
			createOrUpdateErr: &azcore.ResponseError{StatusCode: http.StatusConflict},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cloud, recorder, mockBP := setupRetryTest(t, 3)

			// Tick 1: retriable error, requeues.
			mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool1").Return(
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}), nil,
			).Times(1)
			mockBP.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "pool1", gomock.Any()).Return(nil, tc.createOrUpdateErr).Times(1)
			// Tick 2: succeeds.
			mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool1").Return(
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}), nil,
			).Times(1)
			mockBP.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "pool1", gomock.Any()).Return(nil, nil).Times(1)

			u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
			u.addOperation(getDesiredStateOperation("default/svc1", "lb1", "pool1", []string{"10.0.0.1"}))

			u.process(context.Background())
			assert.Equal(t, 1, u.countOperations())

			u.process(context.Background())
			assert.Equal(t, 0, u.countOperations())

			assertEventReasons(t, drainEvents(recorder), consts.LoadBalancerBackendPoolUpdateRetrying, consts.LoadBalancerBackendPoolUpdated)
		})
	}

	// Parking Behavior
	t.Run("parked op in group blocks entire group from processing", func(t *testing.T) {
		cloud, recorder, _ := setupRetryTest(t, 3, "svc1", "svc2")

		// No ARM mock expectations: parked ops should not make ARM calls.
		u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)

		parkedOp := getDesiredStateOperation("default/svc1", "lb1", "pool1", []string{"10.0.0.1"})
		parkedOp.nextEligibleAt = time.Now().Add(5 * time.Minute)
		parkedOp.retryAttempts = 1
		u.addOperation(parkedOp)

		// Different service, same pool name (fabricated) grouped together.
		u.addOperation(getDesiredStateOperation("default/svc2", "lb1", "pool1", []string{"10.0.0.2"}))

		u.process(context.Background())

		// Both ops should be requeued without ARM calls, retryAttempts unchanged.
		ops := u.drainOperations()
		assert.Equal(t, 2, len(ops))
		for _, op := range ops {
			lbOp := op.(*loadBalancerBackendPoolUpdateOperation)
			if lbOp.nodeIPs[0] == "10.0.0.1" {
				assert.Equal(t, 1, lbOp.retryAttempts, "parked op retryAttempts should be unchanged")
			} else {
				assert.Equal(t, 0, lbOp.retryAttempts, "fresh op retryAttempts should be unchanged")
			}
		}

		assert.Empty(t, drainEvents(recorder), "expected no events")
	})

	// Retry Budget
	t.Run("repeated failures exhaust retry budget and emit failed event with guidance", func(t *testing.T) {
		cloud, recorder, mockBP := setupRetryTest(t, 1)

		conflictErr := &azcore.ResponseError{StatusCode: http.StatusConflict}
		// Tick 1: 409, requeues (retryAttempts 0->1, within budget).
		mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool1").Return(
			getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}), nil,
		).Times(1)
		mockBP.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "pool1", gomock.Any()).Return(
			nil, conflictErr,
		).Times(1)
		// Tick 2: 409, exhausted (retryAttempts=1 >= maxRetries).
		mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool1").Return(
			getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}), nil,
		).Times(1)
		mockBP.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "pool1", gomock.Any()).Return(
			nil, conflictErr,
		).Times(1)

		u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
		u.addOperation(getDesiredStateOperation("default/svc1", "lb1", "pool1", []string{"10.0.0.1"}))

		u.process(context.Background())
		assert.Equal(t, 1, u.countOperations())

		u.process(context.Background())
		assert.Equal(t, 0, u.countOperations())

		events := drainEvents(recorder)
		assertEventReasons(t, events, consts.LoadBalancerBackendPoolUpdateRetrying, consts.LoadBalancerBackendPoolUpdateFailed)
		assert.Contains(t, events[1], "after 1 retries")
		assert.Contains(t, events[1], conflictErr.Error())
		assert.Contains(t, events[1], "retrigger")
		assert.NotContains(t, events[1], "SDK retries exhausted")
		assert.NotContains(t, events[1], "non-retriable")
	})

	t.Run("each op in group maintains its own retry counter across failures", func(t *testing.T) {
		cloud, _, mockBP := setupRetryTest(t, 3, "svc1", "svc2")

		mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool1").Return(
			getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}), nil,
		).Times(1)
		mockBP.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "pool1", gomock.Any()).Return(
			nil, &azcore.ResponseError{StatusCode: http.StatusConflict},
		).Times(1)

		u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)

		opA := getDesiredStateOperation("default/svc1", "lb1", "pool1", []string{"10.0.0.1"})
		opA.retryAttempts = 1
		u.addOperation(opA)

		opB := getDesiredStateOperation("default/svc2", "lb1", "pool1", []string{"10.0.0.2"})
		u.addOperation(opB)

		u.process(context.Background())

		// Both requeued since neither exhausted (maxRetries=3).
		assert.Equal(t, 2, u.countOperations())

		ops := u.drainOperations()
		assert.Equal(t, 2, len(ops))
		for _, op := range ops {
			lbOp := op.(*loadBalancerBackendPoolUpdateOperation)
			if lbOp.nodeIPs[0] == "10.0.0.1" {
				assert.Equal(t, 2, lbOp.retryAttempts) // was 1, now 2
			} else {
				assert.Equal(t, 1, lbOp.retryAttempts) // was 0, now 1
			}
		}
	})

	t.Run("exhausted op is dropped while remaining op with budget is requeued", func(t *testing.T) {
		cloud, _, mockBP := setupRetryTest(t, 2, "svc1", "svc2")

		mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool1").Return(
			getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}), nil,
		).Times(1)
		mockBP.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "pool1", gomock.Any()).Return(
			nil, &azcore.ResponseError{StatusCode: http.StatusConflict},
		).Times(1)

		u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)

		opA := getDesiredStateOperation("default/svc1", "lb1", "pool1", []string{"10.0.0.1"})
		opA.retryAttempts = 2
		u.addOperation(opA)

		opB := getDesiredStateOperation("default/svc2", "lb1", "pool1", []string{"10.0.0.2"})
		u.addOperation(opB)

		u.process(context.Background())

		assert.Equal(t, 1, u.countOperations())
		ops := u.drainOperations()
		assert.Equal(t, "default/svc2", ops[0].(*loadBalancerBackendPoolUpdateOperation).serviceName)
	})

	t.Run("retry followed by success records success metric without failure metric", func(t *testing.T) {
		cloud, _, mockBP := setupRetryTest(t, 3)

		// Tick 1: 409, requeues.
		mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool1").Return(
			getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}), nil,
		).Times(1)
		mockBP.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "pool1", gomock.Any()).Return(
			nil, &azcore.ResponseError{StatusCode: http.StatusConflict},
		).Times(1)
		// Tick 2: succeeds.
		mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool1").Return(
			getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}), nil,
		).Times(1)
		mockBP.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "pool1", gomock.Any()).Return(nil, nil).Times(1)

		u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
		u.addOperation(getDesiredStateOperation("default/svc1", "lb1", "pool1", []string{"10.0.0.1"}))

		latencyBefore, failureBefore := getBackendPoolUpdaterMetrics(t)

		u.process(context.Background())
		assert.Equal(t, 1, u.countOperations())

		u.process(context.Background())
		assert.Equal(t, 0, u.countOperations())

		latencyAfter, failureAfter := getBackendPoolUpdaterMetrics(t)
		assert.Equal(t, failureBefore, failureAfter, "failure metric should not increment for retried-then-succeeded operation")
		assert.Equal(t, latencyBefore+1, latencyAfter, "expected exactly one metric observation (success)")
	})

	// Event Dedup
	type opSpec struct {
		svcKey        string
		ips           []string
		retryAttempts int
	}
	for _, tc := range []struct {
		name            string
		maxRetries      int
		svcNames        []string
		ops             []opSpec
		expectedQueue   int
		expectedReasons []string
	}{
		{
			name:       "multiple services in same group each receive their own event",
			maxRetries: 3,
			svcNames:   []string{"svc1", "svc2"},
			ops: []opSpec{
				{svcKey: "default/svc1", ips: []string{"10.0.0.1"}},
				{svcKey: "default/svc2", ips: []string{"10.0.0.2"}},
			},
			expectedQueue:   2,
			expectedReasons: []string{consts.LoadBalancerBackendPoolUpdateRetrying, consts.LoadBalancerBackendPoolUpdateRetrying},
		},
		{
			name:       "two exhausted ops for same service produce only one failed event",
			maxRetries: 0,
			ops: []opSpec{
				{svcKey: "default/svc1", ips: []string{"10.0.0.1"}},
				{svcKey: "default/svc1", ips: []string{"10.0.0.2"}},
			},
			expectedQueue:   0,
			expectedReasons: []string{consts.LoadBalancerBackendPoolUpdateFailed},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cloud, recorder, mockBP := setupRetryTest(t, tc.maxRetries, tc.svcNames...)

			mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool1").Return(
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}), nil,
			).Times(1)
			mockBP.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "pool1", gomock.Any()).Return(
				nil, &azcore.ResponseError{StatusCode: http.StatusConflict},
			).Times(1)

			u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
			for _, op := range tc.ops {
				o := getDesiredStateOperation(op.svcKey, "lb1", "pool1", op.ips)
				o.retryAttempts = op.retryAttempts
				u.addOperation(o)
			}

			u.process(context.Background())

			assert.Equal(t, tc.expectedQueue, u.countOperations())
			assertEventReasons(t, drainEvents(recorder), tc.expectedReasons...)
		})
	}

	// Desired-state replace interaction with retry
	t.Run("fresh desired state after 409 resets retryAttempts and uses new IPs", func(t *testing.T) {
		cloud, _, mockBP := setupRetryTest(t, 3)

		// Tick 1: 409, requeues.
		mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool1").Return(
			getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}), nil,
		).Times(1)
		mockBP.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "pool1", gomock.Any()).Return(
			nil, &azcore.ResponseError{StatusCode: http.StatusConflict},
		).Times(1)

		u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
		u.addOperation(getDesiredStateOperation("default/svc1", "lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}))

		u.process(context.Background())
		assert.Equal(t, 1, u.countOperations())
		ops := u.drainOperations()
		assert.Equal(t, 1, ops[0].(*loadBalancerBackendPoolUpdateOperation).retryAttempts)
		u.requeueOperations(ops)

		// New event replaces with fresh desired state.
		u.addOperation(getDesiredStateOperation("default/svc1", "lb1", "pool1", []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}))
		assert.Equal(t, 1, u.countOperations())
		ops = u.drainOperations()
		assert.Equal(t, 0, ops[0].(*loadBalancerBackendPoolUpdateOperation).retryAttempts)
		assert.ElementsMatch(t, []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}, ops[0].(*loadBalancerBackendPoolUpdateOperation).nodeIPs)
		u.requeueOperations(ops)

		// Tick 2: succeeds with fresh desired state.
		mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool1").Return(
			getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}), nil,
		).Times(1)
		mockBP.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "pool1",
			*getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}),
		).Return(nil, nil).Times(1)

		u.process(context.Background())
		assert.Equal(t, 0, u.countOperations())
	})

	t.Run("desired state updated during Retry-After preserves nextEligibleAt and uses latest IPs", func(t *testing.T) {
		cloud, _, mockBP := setupRetryTest(t, 3)

		// Tick 1: ThrottleError parks the operation.
		mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool1").Return(
			nil, &retryrepectthrottled.ThrottleError{RetryAfter: time.Now().Add(1 * time.Hour)},
		).Times(1)

		u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
		u.addOperation(getDesiredStateOperation("default/svc1", "lb1", "pool1", []string{"10.0.0.1"}))

		u.process(context.Background())
		assert.Equal(t, 1, u.countOperations())
		ops := u.drainOperations()
		assert.True(t, ops[0].(*loadBalancerBackendPoolUpdateOperation).nextEligibleAt.After(time.Now()))
		u.requeueOperations(ops)

		// Fresh op replaces IPs but keeps nextEligibleAt.
		u.addOperation(getDesiredStateOperation("default/svc1", "lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}))
		assert.Equal(t, 1, u.countOperations())
		ops = u.drainOperations()
		op := ops[0].(*loadBalancerBackendPoolUpdateOperation)
		assert.ElementsMatch(t, []string{"10.0.0.1", "10.0.0.2"}, op.nodeIPs)
		assert.True(t, op.nextEligibleAt.After(time.Now()), "nextEligibleAt should still be in future")

		// Simulate time passing.
		op.nextEligibleAt = time.Now().Add(-1 * time.Second)
		u.requeueOperations(ops)

		// Tick 2: succeeds with latest desired state.
		mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool1").Return(
			getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}), nil,
		).Times(1)
		mockBP.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "pool1",
			*getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
		).Return(nil, nil).Times(1)

		u.process(context.Background())
		assert.Equal(t, 0, u.countOperations())
	})

	t.Run("two services replaced independently", func(t *testing.T) {
		cloud, _, mockBP := setupRetryTest(t, 3, "svc1", "svc2")

		// Both fail with 409.
		mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool1").Return(
			getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}), nil,
		).Times(1)
		mockBP.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "pool1", gomock.Any()).Return(
			nil, &azcore.ResponseError{StatusCode: http.StatusConflict},
		).Times(1)
		mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool2").Return(
			getTestBackendAddressPoolWithIPs("lb1", "pool2", []string{}), nil,
		).Times(1)
		mockBP.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "pool2", gomock.Any()).Return(
			nil, &azcore.ResponseError{StatusCode: http.StatusConflict},
		).Times(1)

		u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
		u.addOperation(getDesiredStateOperation("default/svc1", "lb1", "pool1", []string{"10.0.0.1"}))
		u.addOperation(getDesiredStateOperation("default/svc2", "lb1", "pool2", []string{"10.0.0.2"}))

		u.process(context.Background())
		assert.Equal(t, 2, u.countOperations())

		// Replace svc1 only.
		u.addOperation(getDesiredStateOperation("default/svc1", "lb1", "pool1", []string{"10.0.0.1", "10.0.0.3"}))

		ops := u.drainOperations()
		for _, o := range ops {
			lbOp := o.(*loadBalancerBackendPoolUpdateOperation)
			if lbOp.serviceName == "default/svc1" {
				assert.ElementsMatch(t, []string{"10.0.0.1", "10.0.0.3"}, lbOp.nodeIPs)
				assert.Equal(t, 0, lbOp.retryAttempts)
			} else {
				assert.ElementsMatch(t, []string{"10.0.0.2"}, lbOp.nodeIPs)
				assert.Equal(t, 1, lbOp.retryAttempts)
			}
		}
	})

	t.Run("failed op is dropped when fresher op arrives during ARM call", func(t *testing.T) {
		cloud, _, mockBP := setupRetryTest(t, 3)

		u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
		u.addOperation(getDesiredStateOperation("default/svc1", "lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}))

		// Tick 1: fresh op added during call, ThrottleError parks.
		mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool1").DoAndReturn(
			func(_ context.Context, _, _, _ string) (*armnetwork.BackendAddressPool, error) {
				u.addOperation(getDesiredStateOperation("default/svc1", "lb1", "pool1", []string{"10.0.0.3", "10.0.0.4"}))
				return nil, &retryrepectthrottled.ThrottleError{RetryAfter: time.Now().Add(1 * time.Hour)}
			},
		).Times(1)

		u.process(context.Background())

		// Stale op dropped, fresh op remains with transferred nextEligibleAt.
		ops := u.drainOperations()
		assert.Equal(t, 1, len(ops))
		op := ops[0].(*loadBalancerBackendPoolUpdateOperation)
		assert.Equal(t, 0, op.retryAttempts)
		assert.True(t, op.nextEligibleAt.After(time.Now()), "fresh op should inherit throttle delay")

		// Tick 2: op is parked (nextEligibleAt in future), no ARM calls.
		u.requeueOperations(ops)
		u.process(context.Background())
		assert.Equal(t, 1, u.countOperations())

		// Simulate time passing.
		ops = u.drainOperations()
		ops[0].(*loadBalancerBackendPoolUpdateOperation).nextEligibleAt = time.Now().Add(-1 * time.Second)
		u.requeueOperations(ops)

		// Tick 3: op is eligible, succeeds.
		mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool1").Return(
			getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}), nil,
		).Times(1)
		mockBP.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "pool1",
			*getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.3", "10.0.0.4"}),
		).Return(nil, nil).Times(1)

		u.process(context.Background())
		assert.Equal(t, 0, u.countOperations())
	})

	// Staleness
	for _, tc := range []struct {
		name      string
		mutateMap func(*Cloud)
	}{
		{
			name: "op that becomes stale during ARM call is dropped at requeue",
			mutateMap: func(cloud *Cloud) {
				cloud.localServiceNameToServiceInfoMap.Delete("default/svc1")
			},
		},
		{
			name: "op with changed load balancer during ARM call is dropped at requeue",
			mutateMap: func(cloud *Cloud) {
				cloud.localServiceNameToServiceInfoMap.Store("default/svc1", &serviceInfo{lbName: "lb2"})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cloud, recorder, mockBP := setupRetryTest(t, 3)

			mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool1").Return(
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}), nil,
			).Times(1)
			mockBP.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "pool1", gomock.Any()).DoAndReturn(
				func(_ context.Context, _, _, _ string, _ armnetwork.BackendAddressPool) (*armnetwork.BackendAddressPool, error) {
					tc.mutateMap(cloud)
					return nil, &azcore.ResponseError{StatusCode: http.StatusConflict}
				},
			).Times(1)

			u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
			u.addOperation(getDesiredStateOperation("default/svc1", "lb1", "pool1", []string{"10.0.0.1"}))

			latencyBefore, failureBefore := getBackendPoolUpdaterMetrics(t)

			u.process(context.Background())

			assert.Equal(t, 0, u.countOperations())
			assert.Empty(t, drainEvents(recorder), "expected no events")

			latencyAfter, failureAfter := getBackendPoolUpdaterMetrics(t)
			assert.Equal(t, latencyBefore, latencyAfter, "dropped operation should not record any metric")
			assert.Equal(t, failureBefore, failureAfter, "dropped operation should not increment failure counter")
		})
	}

	t.Run("op that becomes stale between ticks is dropped at next grouping", func(t *testing.T) {
		cloud, recorder, mockBP := setupRetryTest(t, 3)

		mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool1").Return(
			getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}), nil,
		).Times(1)
		mockBP.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "pool1", gomock.Any()).Return(
			nil, &azcore.ResponseError{StatusCode: http.StatusConflict},
		).Times(1)

		u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
		u.addOperation(getDesiredStateOperation("default/svc1", "lb1", "pool1", []string{"10.0.0.1"}))

		latencyBefore, failureBefore := getBackendPoolUpdaterMetrics(t)

		// Tick 1: 409, requeues.
		u.process(context.Background())
		assert.Equal(t, 1, u.countOperations())

		// Delete service from map between ticks.
		cloud.localServiceNameToServiceInfoMap.Delete("default/svc1")

		// Tick 2: stale, filtered without call.
		u.process(context.Background())
		assert.Equal(t, 0, u.countOperations())

		// Only the retrying event from tick 1 should be present.
		assertEventReasons(t, drainEvents(recorder), consts.LoadBalancerBackendPoolUpdateRetrying)

		latencyAfter, failureAfter := getBackendPoolUpdaterMetrics(t)
		assert.Equal(t, latencyBefore, latencyAfter, "dropped operation should not record any metric")
		assert.Equal(t, failureBefore, failureAfter, "dropped operation should not increment failure counter")
	})

	t.Run("parked op that becomes stale is dropped when eligible", func(t *testing.T) {
		cloud, recorder, _ := setupRetryTest(t, 3)

		// No ARM mock expectations: parked ops and stale ops should not make ARM calls.
		u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)

		// Add op with nextEligibleAt in the near future.
		op := getDesiredStateOperation("default/svc1", "lb1", "pool1", []string{"10.0.0.1"})
		op.nextEligibleAt = time.Now().Add(50 * time.Millisecond)
		op.retryAttempts = 1
		u.addOperation(op)

		latencyBefore, failureBefore := getBackendPoolUpdaterMetrics(t)

		// Tick 1: not yet eligible, skipped.
		u.process(context.Background())
		assert.Equal(t, 1, u.countOperations())

		// Sleep past nextEligibleAt and delete service.
		time.Sleep(60 * time.Millisecond)
		cloud.localServiceNameToServiceInfoMap.Delete("default/svc1")

		// Tick 2: eligible but stale, filtered.
		u.process(context.Background())
		assert.Equal(t, 0, u.countOperations())

		// No events on either tick (parked, then stale drop).
		assert.Empty(t, drainEvents(recorder), "expected no events")

		latencyAfter, failureAfter := getBackendPoolUpdaterMetrics(t)
		assert.Equal(t, latencyBefore, latencyAfter, "dropped operation should not record any metric")
		assert.Equal(t, failureBefore, failureAfter, "dropped operation should not increment failure counter")
	})

	// Lifecycle
	for _, tc := range []struct {
		name              string
		createOrUpdateErr error
	}{
		{
			name:              "removeOperation removes parked op from queue",
			createOrUpdateErr: &retryrepectthrottled.ThrottleError{RetryAfter: time.Now().Add(5 * time.Minute)},
		},
		{
			name:              "removeOperation removes requeued op from queue",
			createOrUpdateErr: &azcore.ResponseError{StatusCode: http.StatusConflict},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cloud, _, mockBP := setupRetryTest(t, 3)

			mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool1").Return(
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}), nil,
			).Times(1)
			mockBP.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "pool1", gomock.Any()).Return(
				nil, tc.createOrUpdateErr,
			).Times(1)

			u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
			u.addOperation(getDesiredStateOperation("default/svc1", "lb1", "pool1", []string{"10.0.0.1"}))

			u.process(context.Background())
			assert.Equal(t, 1, u.countOperations())

			u.removeOperation("default/svc1")
			assert.Equal(t, 0, u.countOperations())
		})
	}

	t.Run("retry requeue is serialized with removeOperation under serviceReconcileLock", func(t *testing.T) {
		cloud, _, mockBP := setupRetryTest(t, 3)

		armBlocked := make(chan struct{})
		armUnblock := make(chan struct{})
		mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool1").Return(
			getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}), nil,
		).Times(1)
		mockBP.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "pool1", gomock.Any()).DoAndReturn(
			func(_ context.Context, _, _, _ string, _ armnetwork.BackendAddressPool) (*armnetwork.BackendAddressPool, error) {
				close(armBlocked)
				<-armUnblock
				return nil, &azcore.ResponseError{StatusCode: http.StatusConflict}
			},
		).Times(1)

		u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
		u.addOperation(getDesiredStateOperation("default/svc1", "lb1", "pool1", []string{"10.0.0.1"}))

		// Start process() in goroutine; it acquires serviceReconcileLock.
		processDone := make(chan struct{})
		go func() {
			defer close(processDone)
			u.process(context.Background())
		}()

		// Wait for process() to be mid-ARM-call.
		<-armBlocked

		// Simulate main reconcile path trying to acquire serviceReconcileLock.
		lockAcquired := make(chan struct{})
		lockDone := make(chan struct{})
		go func() {
			cloud.serviceReconcileLock.Lock()
			close(lockAcquired)
			u.removeOperation("default/svc1")
			cloud.serviceReconcileLock.Unlock()
			close(lockDone)
		}()

		// Verify lock is NOT acquired while process() is mid-flight.
		select {
		case <-lockAcquired:
			t.Fatal("serviceReconcileLock acquired while process() is mid-flight")
		case <-time.After(200 * time.Millisecond):
			// Expected: blocked.
		}

		// Unblock ARM; returns 409, requeues, then releases lock.
		close(armUnblock)
		<-processDone

		// Wait for lock goroutine to fully complete.
		select {
		case <-lockDone:
		case <-time.After(5 * time.Second):
			t.Fatal("lock goroutine did not complete")
		}

		// Op was requeued by process, then removed by removeOperation.
		assert.Equal(t, 0, u.countOperations())
	})

	t.Run("retry requeue is serialized with service deletion under serviceReconcileLock", func(t *testing.T) {
		cloud, _, mockBP := setupRetryTest(t, 3)

		armBlocked := make(chan struct{})
		armUnblock := make(chan struct{})
		mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool1").Return(
			getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}), nil,
		).Times(1)
		mockBP.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "pool1", gomock.Any()).DoAndReturn(
			func(_ context.Context, _, _, _ string, _ armnetwork.BackendAddressPool) (*armnetwork.BackendAddressPool, error) {
				close(armBlocked)
				<-armUnblock
				return nil, &azcore.ResponseError{StatusCode: http.StatusConflict}
			},
		).Times(1)

		u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
		u.addOperation(getDesiredStateOperation("default/svc1", "lb1", "pool1", []string{"10.0.0.1"}))

		// Start process() in goroutine; it acquires serviceReconcileLock.
		processDone := make(chan struct{})
		go func() {
			defer close(processDone)
			u.process(context.Background())
		}()

		// Wait for process() to be mid-ARM-call.
		<-armBlocked

		// Simulate main reconcile path trying to delete service from map.
		lockAcquired := make(chan struct{})
		lockDone := make(chan struct{})
		go func() {
			cloud.serviceReconcileLock.Lock()
			close(lockAcquired)
			cloud.localServiceNameToServiceInfoMap.Delete("default/svc1")
			cloud.serviceReconcileLock.Unlock()
			close(lockDone)
		}()

		// Verify lock is NOT acquired while process() is mid-flight.
		select {
		case <-lockAcquired:
			t.Fatal("serviceReconcileLock acquired while process() is mid-flight")
		case <-time.After(200 * time.Millisecond):
			// Expected: blocked.
		}

		// Unblock ARM; returns 409, requeues, then releases lock.
		close(armUnblock)
		<-processDone

		// Wait for lock goroutine to fully complete.
		select {
		case <-lockDone:
		case <-time.After(5 * time.Second):
			t.Fatal("lock goroutine did not complete")
		}

		// Op was requeued by process (queue == 1), map delete happened after.
		assert.Equal(t, 1, u.countOperations())

		// Next tick: groupOperations filters the stale op.
		u.process(context.Background())
		assert.Equal(t, 0, u.countOperations())
	})

	for _, tc := range []struct {
		name              string
		createOrUpdateErr error
	}{
		{
			name:              "context canceled during ARM call drops op without event",
			createOrUpdateErr: &azcore.ResponseError{StatusCode: http.StatusConflict},
		},
		{
			name:              "ARM call returns context.Canceled on shutdown and op is dropped without event",
			createOrUpdateErr: context.Canceled,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cloud, recorder, mockBP := setupRetryTest(t, 3)

			ctx, cancel := context.WithCancel(context.Background())
			mockBP.EXPECT().Get(gomock.Any(), gomock.Any(), "lb1", "pool1").Return(
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}), nil,
			).Times(1)
			mockBP.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), "lb1", "pool1", gomock.Any()).DoAndReturn(
				func(_ context.Context, _, _, _ string, _ armnetwork.BackendAddressPool) (*armnetwork.BackendAddressPool, error) {
					cancel()
					return nil, tc.createOrUpdateErr
				},
			).Times(1)

			u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
			u.addOperation(getDesiredStateOperation("default/svc1", "lb1", "pool1", []string{"10.0.0.1"}))

			latencyBefore, failureBefore := getBackendPoolUpdaterMetrics(t)

			u.process(ctx)

			assert.Equal(t, 0, u.countOperations())
			assert.Empty(t, drainEvents(recorder), "expected no events")

			latencyAfter, failureAfter := getBackendPoolUpdaterMetrics(t)
			assert.Equal(t, latencyBefore, latencyAfter, "dropped operation should not record any metric")
			assert.Equal(t, failureBefore, failureAfter, "dropped operation should not increment failure counter")
		})
	}

	t.Run("shutdown with parked ops drops all without event", func(t *testing.T) {
		cloud, recorder, _ := setupRetryTest(t, 3)

		// No ARM mock expectations: parked ops with canceled context should never reach ARM.
		u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)

		op := getDesiredStateOperation("default/svc1", "lb1", "pool1", []string{"10.0.0.1"})
		op.nextEligibleAt = time.Now().Add(5 * time.Minute)
		op.retryAttempts = 1
		u.addOperation(op)

		// Cancel context before calling process (simulates shutdown).
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		latencyBefore, failureBefore := getBackendPoolUpdaterMetrics(t)

		u.process(ctx)

		// Op was drained and discarded (not requeued).
		assert.Equal(t, 0, u.countOperations())
		assert.Empty(t, drainEvents(recorder), "expected no events on shutdown")

		latencyAfter, failureAfter := getBackendPoolUpdaterMetrics(t)
		assert.Equal(t, latencyBefore, latencyAfter, "dropped operation should not record any metric")
		assert.Equal(t, failureBefore, failureAfter, "dropped operation should not increment failure counter")
	})
}
