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
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/loadbalancerclient/mock_loadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/natgatewayclient/mock_natgatewayclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/publicipaddressclient/mock_publicipaddressclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/servicegatewayclient/mock_servicegatewayclient"
)

func testConfig() Config {
	return Config{
		SubscriptionID:             "sub",
		ResourceGroup:              "rg",
		Location:                   "eastus",
		VNetName:                   "vnet",
		ServiceGatewayResourceName: "sgw",
		ServiceGatewayID:           "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/serviceGateways/sgw",
	}
}

func notFoundError() error {
	return &azcore.ResponseError{StatusCode: http.StatusNotFound}
}

func TestCreateOrUpdatePIP_Mock(t *testing.T) {
	pip := &armnetwork.PublicIPAddress{Name: ptr.To("svc-pip")}

	t.Run("success", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockFactory := mock_azclient.NewMockClientFactory(ctrl)
		mockPIP := mock_publicipaddressclient.NewMockInterface(ctrl)
		mockFactory.EXPECT().GetPublicIPAddressClient().Return(mockPIP).AnyTimes()
		mockPIP.EXPECT().CreateOrUpdate(gomock.Any(), "rg", "svc-pip", gomock.Any()).Return(pip, nil)

		dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
		assert.NoError(t, dt.createOrUpdatePIP(context.Background(), "rg", pip))
	})

	t.Run("error", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockFactory := mock_azclient.NewMockClientFactory(ctrl)
		mockPIP := mock_publicipaddressclient.NewMockInterface(ctrl)
		mockFactory.EXPECT().GetPublicIPAddressClient().Return(mockPIP).AnyTimes()
		mockPIP.EXPECT().CreateOrUpdate(gomock.Any(), "rg", "svc-pip", gomock.Any()).Return(nil, errors.New("boom"))

		dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
		assert.Error(t, dt.createOrUpdatePIP(context.Background(), "rg", pip))
	})
}

func TestDeletePublicIP_Mock(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockFactory := mock_azclient.NewMockClientFactory(ctrl)
		mockPIP := mock_publicipaddressclient.NewMockInterface(ctrl)
		mockFactory.EXPECT().GetPublicIPAddressClient().Return(mockPIP).AnyTimes()
		mockPIP.EXPECT().Delete(gomock.Any(), "rg", "svc-pip").Return(nil)

		dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
		assert.NoError(t, dt.deletePublicIP(context.Background(), "rg", "svc-pip"))
	})

	t.Run("not-found is success", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockFactory := mock_azclient.NewMockClientFactory(ctrl)
		mockPIP := mock_publicipaddressclient.NewMockInterface(ctrl)
		mockFactory.EXPECT().GetPublicIPAddressClient().Return(mockPIP).AnyTimes()
		mockPIP.EXPECT().Delete(gomock.Any(), "rg", "svc-pip").Return(notFoundError())

		dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
		assert.NoError(t, dt.deletePublicIP(context.Background(), "rg", "svc-pip"))
	})

	t.Run("error", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockFactory := mock_azclient.NewMockClientFactory(ctrl)
		mockPIP := mock_publicipaddressclient.NewMockInterface(ctrl)
		mockFactory.EXPECT().GetPublicIPAddressClient().Return(mockPIP).AnyTimes()
		mockPIP.EXPECT().Delete(gomock.Any(), "rg", "svc-pip").Return(errors.New("boom"))

		dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
		assert.Error(t, dt.deletePublicIP(context.Background(), "rg", "svc-pip"))
	})

	t.Run("empty name", func(t *testing.T) {
		dt := &DiffTracker{config: testConfig()}
		assert.Error(t, dt.deletePublicIP(context.Background(), "rg", ""))
	})
}

func TestCreateOrUpdateLB_Mock(t *testing.T) {
	lb := armnetwork.LoadBalancer{Name: ptr.To("svc")}

	t.Run("success", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockFactory := mock_azclient.NewMockClientFactory(ctrl)
		mockLB := mock_loadbalancerclient.NewMockInterface(ctrl)
		mockFactory.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()
		mockLB.EXPECT().CreateOrUpdate(gomock.Any(), "rg", "svc", gomock.Any()).Return(nil, nil)

		dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
		assert.NoError(t, dt.createOrUpdateLB(context.Background(), lb))
	})

	t.Run("error", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockFactory := mock_azclient.NewMockClientFactory(ctrl)
		mockLB := mock_loadbalancerclient.NewMockInterface(ctrl)
		mockFactory.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()
		mockLB.EXPECT().CreateOrUpdate(gomock.Any(), "rg", "svc", gomock.Any()).Return(nil, errors.New("boom"))

		dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
		assert.Error(t, dt.createOrUpdateLB(context.Background(), lb))
	})

	t.Run("empty name", func(t *testing.T) {
		dt := &DiffTracker{config: testConfig()}
		assert.Error(t, dt.createOrUpdateLB(context.Background(), armnetwork.LoadBalancer{}))
	})
}

func TestDeleteLB_Mock(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockFactory := mock_azclient.NewMockClientFactory(ctrl)
		mockLB := mock_loadbalancerclient.NewMockInterface(ctrl)
		mockFactory.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()
		mockLB.EXPECT().Delete(gomock.Any(), "rg", "uid").Return(nil)

		dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
		assert.NoError(t, dt.deleteLB(context.Background(), "uid"))
	})

	t.Run("error", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockFactory := mock_azclient.NewMockClientFactory(ctrl)
		mockLB := mock_loadbalancerclient.NewMockInterface(ctrl)
		mockFactory.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()
		mockLB.EXPECT().Delete(gomock.Any(), "rg", "uid").Return(errors.New("boom"))

		dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
		assert.Error(t, dt.deleteLB(context.Background(), "uid"))
	})
}

func TestCreateOrUpdateNatGateway_Mock(t *testing.T) {
	natGW := armnetwork.NatGateway{Name: ptr.To("svc")}

	t.Run("success", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockFactory := mock_azclient.NewMockClientFactory(ctrl)
		mockNAT := mock_natgatewayclient.NewMockInterface(ctrl)
		mockFactory.EXPECT().GetNatGatewayClient().Return(mockNAT).AnyTimes()
		mockNAT.EXPECT().CreateOrUpdate(gomock.Any(), "rg", "svc", gomock.Any()).Return(nil, nil)

		dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
		assert.NoError(t, dt.createOrUpdateNatGateway(context.Background(), "rg", natGW))
	})

	t.Run("error", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockFactory := mock_azclient.NewMockClientFactory(ctrl)
		mockNAT := mock_natgatewayclient.NewMockInterface(ctrl)
		mockFactory.EXPECT().GetNatGatewayClient().Return(mockNAT).AnyTimes()
		mockNAT.EXPECT().CreateOrUpdate(gomock.Any(), "rg", "svc", gomock.Any()).Return(nil, errors.New("boom"))

		dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
		assert.Error(t, dt.createOrUpdateNatGateway(context.Background(), "rg", natGW))
	})

	t.Run("empty name", func(t *testing.T) {
		dt := &DiffTracker{config: testConfig()}
		assert.Error(t, dt.createOrUpdateNatGateway(context.Background(), "rg", armnetwork.NatGateway{}))
	})
}

func TestDeleteNatGateway_Mock(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockFactory := mock_azclient.NewMockClientFactory(ctrl)
		mockNAT := mock_natgatewayclient.NewMockInterface(ctrl)
		mockFactory.EXPECT().GetNatGatewayClient().Return(mockNAT).AnyTimes()
		mockNAT.EXPECT().Delete(gomock.Any(), "rg", "svc").Return(nil)

		dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
		assert.NoError(t, dt.deleteNatGateway(context.Background(), "rg", "svc"))
	})

	t.Run("error", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockFactory := mock_azclient.NewMockClientFactory(ctrl)
		mockNAT := mock_natgatewayclient.NewMockInterface(ctrl)
		mockFactory.EXPECT().GetNatGatewayClient().Return(mockNAT).AnyTimes()
		mockNAT.EXPECT().Delete(gomock.Any(), "rg", "svc").Return(errors.New("boom"))

		dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
		assert.Error(t, dt.deleteNatGateway(context.Background(), "rg", "svc"))
	})

	t.Run("empty name", func(t *testing.T) {
		dt := &DiffTracker{config: testConfig()}
		assert.Error(t, dt.deleteNatGateway(context.Background(), "rg", ""))
	})
}

func TestUpdateNRPSGWServices_Mock(t *testing.T) {
	servicesDTO := ServicesDataDTO{
		Action: PartialUpdate,
		Services: []ServiceDTO{
			{Service: "svc", ServiceType: Inbound, IsDelete: true},
		},
	}

	t.Run("success", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockFactory := mock_azclient.NewMockClientFactory(ctrl)
		mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
		mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
		mockSGW.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).Return(nil)

		dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
		assert.NoError(t, dt.updateNRPSGWServices(context.Background(), "sgw", servicesDTO))
	})

	t.Run("error", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockFactory := mock_azclient.NewMockClientFactory(ctrl)
		mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
		mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
		mockSGW.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).Return(errors.New("boom"))

		dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
		assert.Error(t, dt.updateNRPSGWServices(context.Background(), "sgw", servicesDTO))
	})

	t.Run("no-op when empty and not full update", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockFactory := mock_azclient.NewMockClientFactory(ctrl)
		// No client call expected.
		dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
		assert.NoError(t, dt.updateNRPSGWServices(context.Background(), "sgw", ServicesDataDTO{Action: PartialUpdate}))
	})
}

func TestUpdateNRPSGWAddressLocations_Mock(t *testing.T) {
	locationsDTO := LocationsDataDTO{
		Action: PartialUpdate,
		Locations: []LocationDTO{
			{Location: "node1", AddressUpdateAction: PartialUpdate, Addresses: []AddressDTO{}},
		},
	}

	t.Run("success", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockFactory := mock_azclient.NewMockClientFactory(ctrl)
		mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
		mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
		mockSGW.EXPECT().UpdateAddressLocations(gomock.Any(), "rg", "sgw", gomock.Any()).Return(nil)

		dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
		assert.NoError(t, dt.updateNRPSGWAddressLocations(context.Background(), "sgw", locationsDTO))
	})

	t.Run("error", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockFactory := mock_azclient.NewMockClientFactory(ctrl)
		mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
		mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
		mockSGW.EXPECT().UpdateAddressLocations(gomock.Any(), "rg", "sgw", gomock.Any()).Return(errors.New("boom"))

		dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
		assert.Error(t, dt.updateNRPSGWAddressLocations(context.Background(), "sgw", locationsDTO))
	})
}

func TestDisassociateNatGatewayFromServiceGateway_Mock(t *testing.T) {
	// Simplest reconcile path: no matching SGW service to clear, and the NAT
	// gateway is already gone (404) -> method returns nil.
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
	mockNAT := mock_natgatewayclient.NewMockInterface(ctrl)
	mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
	mockFactory.EXPECT().GetNatGatewayClient().Return(mockNAT).AnyTimes()

	mockSGW.EXPECT().GetServices(gomock.Any(), "rg", "sgw").Return([]*armnetwork.ServiceGatewayService{}, nil)
	mockNAT.EXPECT().Get(gomock.Any(), "rg", "svc", gomock.Any()).Return(nil, notFoundError())

	dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
	assert.NoError(t, dt.disassociateNatGatewayFromServiceGateway(context.Background(), "sgw", "svc"))
}

func TestConvertServicesUpdateActionToARM(t *testing.T) {
	assert.Equal(t, armnetwork.ServiceUpdateActionPartialUpdate, *convertServicesUpdateActionToARM(PartialUpdate))
	assert.Equal(t, armnetwork.ServiceUpdateActionFullUpdate, *convertServicesUpdateActionToARM(FullUpdate))
	// Unknown defaults to PartialUpdate.
	assert.Equal(t, armnetwork.ServiceUpdateActionPartialUpdate, *convertServicesUpdateActionToARM(UnknownUpdateAction))
}

func TestConvertLocationsUpdateActionToARM(t *testing.T) {
	assert.Equal(t, armnetwork.UpdateActionPartialUpdate, *convertLocationsUpdateActionToARM(PartialUpdate))
	assert.Equal(t, armnetwork.UpdateActionFullUpdate, *convertLocationsUpdateActionToARM(FullUpdate))
	// Unknown defaults to PartialUpdate.
	assert.Equal(t, armnetwork.UpdateActionPartialUpdate, *convertLocationsUpdateActionToARM(UnknownUpdateAction))
}

func TestConvertLocationDTOsToAddressLocations(t *testing.T) {
	t.Run("drained node keeps non-nil empty Addresses", func(t *testing.T) {
		locs := convertLocationDTOsToAddressLocations([]LocationDTO{
			{Location: "node1", AddressUpdateAction: FullUpdate, Addresses: []AddressDTO{}},
		})
		assert.Len(t, locs, 1)
		assert.NotNil(t, locs[0].Addresses)
		assert.Empty(t, locs[0].Addresses)
		assert.Equal(t, armnetwork.AddressUpdateActionFullUpdate, *locs[0].AddressUpdateAction)
	})

	t.Run("address with empty ServiceNames keeps non-nil empty Services", func(t *testing.T) {
		locs := convertLocationDTOsToAddressLocations([]LocationDTO{
			{Location: "node1", AddressUpdateAction: PartialUpdate, Addresses: []AddressDTO{
				{Address: "10.0.0.5", ServiceNames: nil},
			}},
		})
		assert.Len(t, locs, 1)
		assert.Equal(t, armnetwork.AddressUpdateActionPartialUpdate, *locs[0].AddressUpdateAction)
		assert.Len(t, locs[0].Addresses, 1)
		assert.NotNil(t, locs[0].Addresses[0].Services)
		assert.Empty(t, locs[0].Addresses[0].Services)
		assert.Equal(t, "10.0.0.5", *locs[0].Addresses[0].Address)
	})

	t.Run("unknown AddressUpdateAction defaults to PartialUpdate", func(t *testing.T) {
		// A LocationDTO whose AddressUpdateAction is left unset (zero value
		// UnknownUpdateAction) must still produce an explicit action, matching the
		// service/location action converters, rather than a nil AddressUpdateAction.
		locs := convertLocationDTOsToAddressLocations([]LocationDTO{
			{Location: "node1", Addresses: []AddressDTO{}},
		})
		assert.Len(t, locs, 1)
		assert.NotNil(t, locs[0].AddressUpdateAction)
		assert.Equal(t, armnetwork.AddressUpdateActionPartialUpdate, *locs[0].AddressUpdateAction)
	})
}

// TestARMPrimitivesDoNotHoldStateLock enforces the package concurrency invariant: ARM
// primitives never hold dt.mu across I/O. It blocks inside an in-flight ARM call and
// asserts dt.mu is still acquirable; a regression that took the state lock around an ARM
// call would serialize state access behind network latency and deadlock this test.
func TestARMPrimitivesDoNotHoldStateLock(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	inARM := make(chan struct{})
	releaseARM := make(chan struct{})

	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockPIP := mock_publicipaddressclient.NewMockInterface(ctrl)
	mockFactory.EXPECT().GetPublicIPAddressClient().Return(mockPIP).AnyTimes()
	mockPIP.EXPECT().
		CreateOrUpdate(gomock.Any(), "rg", "svc-pip", gomock.Any()).
		DoAndReturn(func(context.Context, string, string, armnetwork.PublicIPAddress) (*armnetwork.PublicIPAddress, error) {
			close(inARM)
			<-releaseARM
			return &armnetwork.PublicIPAddress{Name: ptr.To("svc-pip")}, nil
		})

	dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}

	armDone := make(chan struct{})
	go func() {
		_ = dt.createOrUpdatePIP(context.Background(), "rg", &armnetwork.PublicIPAddress{Name: ptr.To("svc-pip")})
		close(armDone)
	}()

	<-inARM // ARM call is now in flight

	locked := make(chan struct{})
	go func() {
		dt.mu.Lock()
		_ = dt.NRPResources // touch lock-guarded state
		dt.mu.Unlock()
		close(locked)
	}()

	select {
	case <-locked:
	case <-time.After(2 * time.Second):
		close(releaseARM)
		<-armDone
		t.Fatal("dt.mu was held during an in-flight ARM call; ARM primitives must not hold the state lock across I/O")
	}

	close(releaseARM)
	<-armDone
}
