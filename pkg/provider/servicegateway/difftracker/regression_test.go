/*
Copyright 2026 The Kubernetes Authors.

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

package difftracker

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	"k8s.io/client-go/kubernetes/fake"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/loadbalancerclient/mock_loadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/publicipaddressclient/mock_publicipaddressclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/servicegatewayclient/mock_servicegatewayclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// TestInitConcurrencyHandshakeRace stresses the increment-before-send / undo-on-coalesce design
// in triggerLocationsUpdater / triggerServiceUpdater together with the sync.Once close in
// checkInitializationCompleteLocked. It asserts that pendingUpdaterTriggers never goes negative
// and that the init completion channel is closed at most once. Run under -race.
func TestInitConcurrencyHandshakeRace(t *testing.T) {
	dt := newTestDiffTracker()
	dt.initCompletionChecker = make(chan struct{})
	atomic.StoreInt32(&dt.isInitializing, 1)

	// Seed one service in StateCreated so the pendingOps check inside
	// checkInitializationCompleteLocked sees zero outstanding work.
	uid := "svc-init-race"
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:      StateCreated,
	}

	const goroutines = 200
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			dt.triggerLocationsUpdater()
			dt.triggerServiceUpdater()
		}()
	}

	// Consumers mirror the real updaters: the decrement is conditional on isInitializing==1, so a
	// consumer cannot decrement after checkInitializationCompleteLocked sets isInitializing=0.
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			select {
			case <-dt.locationsUpdaterTrigger:
				if atomic.LoadInt32(&dt.isInitializing) == 1 {
					atomic.AddInt32(&dt.pendingUpdaterTriggers, -1)
				}
				dt.checkInitializationComplete()
			default:
			}
			select {
			case <-dt.serviceUpdaterTrigger:
				if atomic.LoadInt32(&dt.isInitializing) == 1 {
					atomic.AddInt32(&dt.pendingUpdaterTriggers, -1)
				}
				dt.checkInitializationComplete()
			default:
			}
		}()
	}

	close(start)
	wg.Wait()

	counter := atomic.LoadInt32(&dt.pendingUpdaterTriggers)
	assert.GreaterOrEqual(t, counter, int32(0),
		"pendingUpdaterTriggers must not go negative: negative implies double-decrement, which causes WaitForInitialSync to hang indefinitely")

	select {
	case <-dt.initCompletionChecker:
	default:
	}
}

// TestNRPDrainVsRemoveWireShapes verifies the two distinct wire shapes emitted depending on
// whether a whole node disappeared or only a single pod address was unbound: a gone node emits a
// location with an empty Addresses array (NRP deletes the node), and a per-address removal emits
// an address entry with an empty ServiceRef (NRP unbinds the address). Both are checked at the
// sync level and through DTO->ARM conversion.
func TestNRPDrainVsRemoveWireShapes(t *testing.T) {
	t.Run("GoneNodeEmitsEmptyAddressesArray", func(t *testing.T) {
		dt := newTestDiffTracker()
		uid := "svc-gone-node"
		dt.NRPResources.LoadBalancers.Insert(uid)
		dt.NRPResources.Locations["10.0.0.99"] = NRPLocation{
			Addresses: map[string]NRPAddress{
				"10.244.9.1": {Services: utilsets.NewString(uid)},
			},
		}

		result := dt.GetSyncLocationsAddresses()

		loc, ok := result.Locations["10.0.0.99"]
		if !assert.True(t, ok, "gone node must produce a location entry") {
			return
		}
		assert.Equal(t, PartialUpdate, loc.AddressUpdateAction)
		assert.NotNil(t, loc.Addresses, "Addresses map must be non-nil (wire is [] not null)")
		assert.Empty(t, loc.Addresses, "an empty Addresses array tells NRP to delete the whole location/node")

		dto := MapLocationDataToDTO(result)
		arm := convertLocationDTOsToAddressLocations(dto.Locations)
		if !assert.Len(t, arm, 1) {
			return
		}
		assert.NotNil(t, arm[0].Addresses, "ARM Addresses slice must be non-nil")
		assert.Empty(t, arm[0].Addresses, "ARM Addresses slice must be empty to delete the node/location")
	})

	t.Run("PerAddressRemovalEmitsEmptyServiceRef", func(t *testing.T) {
		dt := newTestDiffTracker()
		uid := "svc-addr-removed"
		dt.NRPResources.LoadBalancers.Insert(uid)
		dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{
			Addresses: map[string]NRPAddress{
				"10.244.0.5": {Services: utilsets.NewString(uid)},
			},
		}
		node := newNode()
		pod := newPod()
		node.Pods["10.244.0.5"] = pod
		dt.K8sResources.Nodes["10.0.0.1"] = node

		result := dt.GetSyncLocationsAddresses()

		loc, ok := result.Locations["10.0.0.1"]
		if !assert.True(t, ok, "per-address removal: location must be present") {
			return
		}
		assert.Equal(t, PartialUpdate, loc.AddressUpdateAction)
		addr, ok := loc.Addresses["10.244.0.5"]
		if !assert.True(t, ok, "per-address removal: address entry must be emitted") {
			return
		}
		assert.NotNil(t, addr.ServiceRef, "ServiceRef must be a non-nil empty set")
		assert.Equal(t, 0, addr.ServiceRef.Len(), "an empty ServiceRef tells NRP to unbind this address from all services")

		dto := MapLocationDataToDTO(result)
		arm := convertLocationDTOsToAddressLocations(dto.Locations)
		if !assert.Len(t, arm, 1) {
			return
		}
		if !assert.NotEmpty(t, arm[0].Addresses) {
			return
		}
		assert.NotNil(t, arm[0].Addresses[0].Services, "ARM Services slice must be non-nil")
		assert.Empty(t, arm[0].Addresses[0].Services, "ARM Services slice must be empty for per-address removal")
	})
}

// TestEgressRefCountSoundness guards the outboundIdentityPodRefCount counter: a duplicate
// DeletePod cannot double-decrement, the counter never goes negative, and removing the last pod
// drives the count to zero so NAT gateway teardown is scheduled.
func TestEgressRefCountSoundness(t *testing.T) {
	t.Run("DuplicateDeletePodIsNoOp", func(t *testing.T) {
		dt := newTestDiffTracker()
		uid := "egress-dup-guard"
		dt.NRPResources.NATGateways.Insert(uid)
		dt.pendingServiceOps[uid] = &ServiceOperationState{
			ServiceUID: uid, Config: NewOutboundServiceConfig(uid, nil), State: StateCreated,
		}
		dt.AddPod(uid, "ns/pod-a", "10.0.0.1", "10.244.0.5")

		first := dt.DeletePod(uid, "10.0.0.1", []string{"10.244.0.5"}, "ns", "pod-a", "")
		assert.True(t, first.IsLastPod, "first delete of the sole pod must report IsLastPod=true")

		second := dt.DeletePod(uid, "10.0.0.1", []string{"10.244.0.5"}, "ns", "pod-a", "")
		assert.False(t, second.IsLastPod, "duplicate delete must be a no-op")

		if v, ok := dt.outboundIdentityPodRefCount.Load(strings.ToLower(uid)); ok {
			assert.GreaterOrEqual(t, v.(int), 0, "ref-counter must not be negative after duplicate delete")
		}
	})

	t.Run("LastPodRemovalDrivesRefCountToZero", func(t *testing.T) {
		dt := newTestDiffTracker()
		uid := "egress-last-zero"
		dt.NRPResources.NATGateways.Insert(uid)
		dt.pendingServiceOps[uid] = &ServiceOperationState{
			ServiceUID: uid, Config: NewOutboundServiceConfig(uid, nil), State: StateCreated,
		}
		dt.AddPod(uid, "ns/p", "10.0.0.1", "10.244.0.1")
		dt.AddPod(uid, "ns/q", "10.0.0.1", "10.244.0.2")

		notLast := dt.DeletePod(uid, "10.0.0.1", []string{"10.244.0.1"}, "ns", "p", "")
		assert.False(t, notLast.IsLastPod, "first delete of two pods must not be last-pod")

		last := dt.DeletePod(uid, "10.0.0.1", []string{"10.244.0.2"}, "ns", "q", "")
		assert.True(t, last.IsLastPod, "deleting the final pod must report IsLastPod=true")

		_, ok := dt.outboundIdentityPodRefCount.Load(strings.ToLower(uid))
		assert.False(t, ok, "last-pod removal must delete the ref-count key (counter 1 to 0)")
	})
}

// TestDeleteInboundServiceLastErrMonotonic verifies that lastErr is monotonic: if a LoadBalancer
// deletion fails but a subsequent public IP deletion succeeds, onComplete is still called with
// success=false carrying the LB error, so the LB is not leaked while tracking is cleared.
func TestDeleteInboundServiceLastErrMonotonic(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	f := mock_azclient.NewMockClientFactory(ctrl)
	sgw := mock_servicegatewayclient.NewMockInterface(ctrl)
	lb := mock_loadbalancerclient.NewMockInterface(ctrl)
	pip := mock_publicipaddressclient.NewMockInterface(ctrl)
	f.EXPECT().GetServiceGatewayClient().Return(sgw).AnyTimes()
	f.EXPECT().GetLoadBalancerClient().Return(lb).AnyTimes()
	f.EXPECT().GetPublicIPAddressClient().Return(pip).AnyTimes()
	sgw.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	lb.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(errors.New("simulated-lb-delete-failure"))
	pip.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)

	kube := fake.NewSimpleClientset(deletionTestService())
	dt := deletionTestDiffTracker(kube, f)

	var completedSuccess *bool
	var completedErr error
	su := deletionTestUpdater(dt, func(uid string, ok bool, err error) {
		b := ok
		completedSuccess = &b
		completedErr = err
		dt.OnServiceCreationComplete(uid, ok, err)
	})

	su.deleteInboundService("uid-1", "corr-lastErr-monotonic")

	if !assert.NotNil(t, completedSuccess, "onComplete must be called") {
		return
	}
	assert.False(t, *completedSuccess, "an LB-delete failure must not be masked by a later successful PIP-delete")
	assert.Error(t, completedErr, "onComplete must receive the error from the failed LB deletion step")
}

// TestSDKV9WireValues asserts the case-sensitive ARM enum and SKU values that the v6 to v9 SDK
// port must preserve. A case flip here causes silent NRP API rejections with no retry path.
func TestSDKV9WireValues(t *testing.T) {
	dtConfig := testConfig()

	t.Run("InboundLBSKUIsExactlyService", func(t *testing.T) {
		cfg := &InboundConfig{
			FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}},
			BackendPorts:  []PortMapping{{Port: 8080, Protocol: "TCP"}},
		}
		_, lb, _, err := buildInboundServiceResources("svc-sku-wire", cfg, dtConfig)
		assert.NoError(t, err)
		if !assert.NotNil(t, lb.SKU) || !assert.NotNil(t, lb.SKU.Name) {
			return
		}
		assert.Equal(t, armnetwork.LoadBalancerSKUName("Service"), *lb.SKU.Name,
			"SGW inbound LB SKU must serialize as exactly \"Service\"")
		assert.Equal(t, "Service", consts.LoadBalancerARMSKUService,
			"consts.LoadBalancerARMSKUService must remain \"Service\"")
	})

	t.Run("AddressUpdateActionEnumWireValues", func(t *testing.T) {
		dtos := []LocationDTO{
			{Location: "10.0.0.1", AddressUpdateAction: PartialUpdate, Addresses: []AddressDTO{}},
			{Location: "10.0.0.2", AddressUpdateAction: FullUpdate, Addresses: []AddressDTO{}},
		}
		armLocs := convertLocationDTOsToAddressLocations(dtos)
		if !assert.Len(t, armLocs, 2) {
			return
		}
		if assert.NotNil(t, armLocs[0].AddressUpdateAction) {
			assert.Equal(t, armnetwork.AddressUpdateActionPartialUpdate, *armLocs[0].AddressUpdateAction)
		}
		if assert.NotNil(t, armLocs[1].AddressUpdateAction) {
			assert.Equal(t, armnetwork.AddressUpdateActionFullUpdate, *armLocs[1].AddressUpdateAction)
		}
		assert.Equal(t, armnetwork.AddressUpdateAction("PartialUpdate"), armnetwork.AddressUpdateActionPartialUpdate)
		assert.Equal(t, armnetwork.AddressUpdateAction("FullUpdate"), armnetwork.AddressUpdateActionFullUpdate)
	})

	t.Run("TransportProtocolWireValues", func(t *testing.T) {
		cfg := &InboundConfig{
			FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}, {Port: 53, Protocol: "UDP"}},
			BackendPorts:  []PortMapping{{Port: 8080, Protocol: "TCP"}, {Port: 5353, Protocol: "UDP"}},
		}
		_, lb, _, err := buildInboundServiceResources("svc-proto-wire", cfg, dtConfig)
		assert.NoError(t, err)
		if !assert.Len(t, lb.Properties.LoadBalancingRules, 2) {
			return
		}
		tcpRule := lb.Properties.LoadBalancingRules[0]
		udpRule := lb.Properties.LoadBalancingRules[1]
		if assert.NotNil(t, tcpRule.Properties) && assert.NotNil(t, tcpRule.Properties.Protocol) {
			assert.Equal(t, armnetwork.TransportProtocolTCP, *tcpRule.Properties.Protocol)
		}
		if assert.NotNil(t, udpRule.Properties) && assert.NotNil(t, udpRule.Properties.Protocol) {
			assert.Equal(t, armnetwork.TransportProtocolUDP, *udpRule.Properties.Protocol)
		}
		assert.Equal(t, armnetwork.TransportProtocol("Tcp"), armnetwork.TransportProtocolTCP)
		assert.Equal(t, armnetwork.TransportProtocol("Udp"), armnetwork.TransportProtocolUDP)
	})
}
