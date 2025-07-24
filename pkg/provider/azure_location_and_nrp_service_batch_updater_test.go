package provider

import (
	"context"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/loadbalancerclient/mock_loadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/publicipaddressclient/mock_publicipaddressclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/difftracker"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func TestLoadBalancerLocationAndNRPServiceBatchUpdater(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	cloud := GetTestCloudWithContainerLoadBalancer(ctrl)

	client := fake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	cloud.serviceLister = informerFactory.Core().V1().Services().Lister()
	cloud.nodePrivateIPs = map[string]*utilsets.IgnoreCaseSet{
		"node1": utilsets.NewString("10.0.0.1"),
		"node2": utilsets.NewString("10.0.0.2"),
	}

	// lb := &armnetwork.LoadBalancer{
	// 	Name: to.Ptr("test-service-uid"),
	// }

	mockLBsClient := cloud.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
	mockLBsClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	// mockLBsClient.EXPECT().List(gomock.Any(), "rg").Return([]*armnetwork.LoadBalancer{lb}, nil).AnyTimes()
	mockLBsClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	mockLBsClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil)

	mockLBsClient.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	pipClient := cloud.NetworkClientFactory.GetPublicIPAddressClient().(*mock_publicipaddressclient.MockInterface)
	pipClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	pipClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	pipClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armnetwork.PublicIPAddress{Name: ptr.To("testCluster-atest"), Location: ptr.To("eastus")}, nil).AnyTimes()

	mockLBBackendPool := cloud.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ string, _ *v1.Service, lb *armnetwork.LoadBalancer) (bool, bool, *armnetwork.LoadBalancer, error) {
		return false, false, lb, nil
	}).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockLBBackendPool.EXPECT().GetBackendPrivateIPs(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

	k8s := difftracker.K8s_State{
		Services: utilsets.NewString(),
		Egresses: utilsets.NewString(),
		Nodes:    make(map[string]difftracker.Node),
	}
	nrp := difftracker.NRP_State{
		LoadBalancers: utilsets.NewString(),
		NATGateways:   utilsets.NewString(),
		Locations:     make(map[string]difftracker.NRPLocation),
	}

	// Initialize the diff tracker state and get the necessary operations to sync the cluster with NRP
	cloud.diffTracker = difftracker.InitializeDiffTracker(k8s, nrp)

	locationAndNRPServiceBatchUpdater := newLocationAndNRPServiceBatchUpdater(cloud)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cloud.locationAndNRPServiceBatchUpdater = locationAndNRPServiceBatchUpdater
	go cloud.locationAndNRPServiceBatchUpdater.run(ctx)

	cloud.setUpEndpointSlicesInformer(informerFactory)

	stopCh := make(chan struct{})
	defer close(stopCh)
	informerFactory.Start(stopCh)
	time.Sleep(100 * time.Millisecond)

	// _, err := client.CoreV1().Services("default").Create(context.Background(), &v1.Service{
	// 	ObjectMeta: metav1.ObjectMeta{
	// 		Name:      "test-service",
	// 		Namespace: "default",
	// 		UID:       "test-service-uid",
	// 	},
	// }, metav1.CreateOptions{})

	// assert.NoError(t, err)
	// service := getResourceGroupTestService("test-svc", "test", "1.2.3.4", 80)
	service := getResourceGroupTestService("test-svc", "test", "", 80)
	service.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal

	nodes := []*v1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node1",
			},
		},
	}

	lbStatus, err := cloud.EnsureLoadBalancer(context.TODO(), "test", &service, nodes)
	if err != nil {
		t.Fatalf("failed to ensure load balancer: %v", err)
	}
	assert.NotNil(t, lbStatus)

	// _, err = client.DiscoveryV1().EndpointSlices("default").Create(context.Background(), &discovery_v1.EndpointSlice{
	// 	ObjectMeta: metav1.ObjectMeta{
	// 		Name:      "test-slice",
	// 		Namespace: "default",
	// 		OwnerReferences: []metav1.OwnerReference{
	// 			{
	// 				Kind: "Service",
	// 				Name: "test-service",
	// 				UID:  "test-service-uid",
	// 			},
	// 		},
	// 	},
	// 	Endpoints: []discovery_v1.Endpoint{
	// 		{
	// 			NodeName:  func(s string) *string { return &s }("node1"),
	// 			Addresses: []string{"198.168.1.1"},
	// 		},
	// 	},
	// }, metav1.CreateOptions{})
	// assert.NoError(t, err)

	// Add a sleep to keep the test running
	time.Sleep(100 * time.Second)

}

func TestLocationAndNRPServiceBatchUpdaterRunCancel(t *testing.T) {
	cloud := &Cloud{}
	updater := newLocationAndNRPServiceBatchUpdater(cloud)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		updater.run(ctx)
		close(done)
	}()

	// Cancel the context to trigger shutdown
	cancel()

	// Wait for run to exit
	select {
	case <-done:
		// Success, run has exited
	case <-time.After(3 * time.Second):
		t.Fatal("updater.run didn't exit after context cancellation")
	}
}

func TestMergeMaps(t *testing.T) {
	tests := []struct {
		name     string
		maps     []map[string]string
		expected map[string]string
	}{
		{
			name:     "empty maps",
			maps:     []map[string]string{},
			expected: map[string]string{},
		},
		{
			name:     "single map",
			maps:     []map[string]string{{"192.168.1.1": "10.0.0.1", "192.168.1.2": "10.0.0.2"}},
			expected: map[string]string{"192.168.1.1": "10.0.0.1", "192.168.1.2": "10.0.0.2"},
		},
		{
			name: "multiple maps",
			maps: []map[string]string{
				{"192.168.1.1": "10.0.0.1", "192.168.1.2": "10.0.0.2"},
				{"192.168.1.3": "10.0.0.3"},
				{"192.168.1.2": "10.0.0.4"}, // Should override the value from the first map
			},
			expected: map[string]string{"192.168.1.1": "10.0.0.1", "192.168.1.2": "10.0.0.4", "192.168.1.3": "10.0.0.3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mergeMaps(tt.maps...)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// func TestProcess(t *testing.T) {
// 	ctx := context.Background()

// 	t.Run("process with additions", func(t *testing.T) {
// 		// Setup mock data
// 		additions := sets.New[string]("service1")

// 		// Create cloud with mock data
// 		cloud := &Cloud{
// 			diffTracker: &mockDiffTracker{
// 				syncLoadBalancerServices: difftracker.SyncServicesReturnType{
// 					Additions: additions,
// 					Removals:  sets.New[string](),
// 				},
// 				syncLocationsAddresses: difftracker.LocationsDataMapType{
// 					Locations: map[string][]string{"location1": {"address1"}},
// 				},
// 			},
// 			localServiceNameToNRPServiceMap: NewSyncMapLookUp[struct{}](),
// 			endpointSlicesCache:             NewSyncMapLookUp[*discovery_v1.EndpointSlice](),
// 		}

// 		// Create endpoint slice and add to cache
// 		endpointSlice := &discovery_v1.EndpointSlice{
// 			ObjectMeta: metav1.ObjectMeta{
// 				Labels: map[string]string{
// 					discovery_v1.LabelServiceName: "service1",
// 					discovery_v1.LabelManagedBy:   "endpointslice-controller.k8s.io",
// 				},
// 				OwnerReferences: []metav1.OwnerReference{
// 					{
// 						Kind: "Service",
// 						UID:  "service1",
// 					},
// 				},
// 			},
// 		}
// 		cloud.endpointSlicesCache.Store("slice1", endpointSlice)

// 		// Create and run the updater
// 		updater := newLocationAndNRPServiceBatchUpdater(cloud)
// 		updater.process(ctx)

// 		// Verify localServiceNameToNRPServiceMap was updated
// 		_, exists := cloud.localServiceNameToNRPServiceMap.Load("service1")
// 		assert.True(t, exists)
// 	})

// 	t.Run("process with removals", func(t *testing.T) {
// 		// Setup mock data
// 		removals := sets.New[string]("service2")

// 		// Create cloud with mock data
// 		cloud := &Cloud{
// 			diffTracker: &mockDiffTracker{
// 				syncLoadBalancerServices: difftracker.SyncServicesReturnType{
// 					Additions: sets.New[string](),
// 					Removals:  removals,
// 				},
// 				syncLocationsAddresses: difftracker.LocationsDataMapType{
// 					Locations: map[string][]string{},
// 				},
// 			},
// 			localServiceNameToNRPServiceMap: NewSyncMapLookUp[struct{}](),
// 			endpointSlicesCache:             NewSyncMapLookUp[*discovery_v1.EndpointSlice](),
// 		}

// 		// Create endpoint slice and add to cache
// 		endpointSlice := &discovery_v1.EndpointSlice{
// 			ObjectMeta: metav1.ObjectMeta{
// 				Labels: map[string]string{
// 					discovery_v1.LabelServiceName: "service2",
// 					discovery_v1.LabelManagedBy:   "endpointslice-controller.k8s.io",
// 				},
// 				OwnerReferences: []metav1.OwnerReference{
// 					{
// 						Kind: "Service",
// 						UID:  "service2",
// 					},
// 				},
// 			},
// 		}
// 		cloud.endpointSlicesCache.Store("slice2", endpointSlice)

// 		// Create and run the updater
// 		updater := newLocationAndNRPServiceBatchUpdater(cloud)
// 		updater.process(ctx)
// 	})
// }

// func TestRun(t *testing.T) {
// 	cloud := &Cloud{
// 		diffTracker: &mockDiffTracker{
// 			syncLoadBalancerServices: difftracker.SyncServicesReturnType{
// 				Additions: sets.New[string](),
// 				Removals:  sets.New[string](),
// 			},
// 		},
// 		localServiceNameToNRPServiceMap: NewSyncMapLookUp[struct{}](),
// 		endpointSlicesCache:             NewSyncMapLookUp[*discovery_v1.EndpointSlice](),
// 	}
// 	updater := newLocationAndNRPServiceBatchUpdater(cloud)

// 	ctx, cancel := context.WithCancel(context.Background())
// 	defer cancel()

// 	go updater.run(ctx)

// 	// Send update trigger
// 	updater.channelUpdateTrigger <- true

// 	// Give time for processing to complete
// 	wait.Poll(100*time.Millisecond, 1*time.Second, func() (bool, error) {
// 		return true, nil
// 	})
// }
