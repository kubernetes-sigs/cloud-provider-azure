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
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/loadbalancerclient/mock_loadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/natgatewayclient/mock_natgatewayclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/publicipaddressclient/mock_publicipaddressclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/servicegatewayclient/mock_servicegatewayclient"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func testConfig() Config {
	return Config{
		SubscriptionID:             "sub",
		ResourceGroup:              "rg",
		Location:                   "eastus",
		VNetName:                   "vnet",
		ServiceGatewayResourceName: "sgw",
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
		// Pin the builder -> client seam (see TestCreateOrUpdateLB_Mock).
		var sent armnetwork.PublicIPAddress
		mockPIP.EXPECT().CreateOrUpdate(gomock.Any(), "rg", "svc-pip", gomock.Any()).
			DoAndReturn(func(_ context.Context, _, _ string, got armnetwork.PublicIPAddress) (*armnetwork.PublicIPAddress, error) {
				sent = got
				return pip, nil
			})

		dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
		assert.NoError(t, dt.createOrUpdatePIP(context.Background(), "rg", pip))
		assert.Equal(t, *pip, sent, "the Public IP sent to Azure must be the one the caller built")
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
		// Capture the Load Balancer actually handed to Azure. The builder is well covered by
		// TestBuildInboundServiceResources_* and the v9 wire guard, and this wrapper's error
		// handling is covered below - but nothing verified the SEAM between them. Replacing the
		// caller's LB with an empty armnetwork.LoadBalancer{} (no SKU, no frontend IP config, no
		// backend pool, no rules) passed the entire unit suite, because every mock matched the
		// payload with gomock.Any(). Pin that what the caller built is what is sent.
		var sent armnetwork.LoadBalancer
		mockLB.EXPECT().CreateOrUpdate(gomock.Any(), "rg", "svc", gomock.Any()).
			DoAndReturn(func(_ context.Context, _, _ string, got armnetwork.LoadBalancer) (*armnetwork.LoadBalancer, error) {
				sent = got
				return nil, nil
			})

		dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
		assert.NoError(t, dt.createOrUpdateLB(context.Background(), lb))
		assert.Equal(t, lb, sent, "the Load Balancer sent to Azure must be the one the caller built")
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
		// Pin the builder -> client seam (see TestCreateOrUpdateLB_Mock): with gomock.Any() for the
		// payload, replacing the caller's NAT gateway with an empty one was invisible.
		var sent armnetwork.NatGateway
		mockNAT.EXPECT().CreateOrUpdate(gomock.Any(), "rg", "svc", gomock.Any()).
			DoAndReturn(func(_ context.Context, _, _ string, got armnetwork.NatGateway) (*armnetwork.NatGateway, error) {
				sent = got
				return nil, nil
			})

		dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
		assert.NoError(t, dt.createOrUpdateNatGateway(context.Background(), "rg", natGW))
		assert.Equal(t, natGW, sent, "the NAT Gateway sent to Azure must be the one the caller built")
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

		// Capture and assert the WIRE PAYLOAD, not just that a call happened. With gomock.Any()
		// for the request the DTO -> ARM conversion is entirely unverified: sending nil service
		// requests, the wrong service name, or dropping IsDelete all left this test green while
		// NRP received something completely different from what the caller asked for.
		var got armnetwork.ServiceGatewayUpdateServicesRequest
		mockSGW.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).
			DoAndReturn(func(_ context.Context, _, _ string, req armnetwork.ServiceGatewayUpdateServicesRequest) error {
				got = req
				return nil
			})

		dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
		assert.NoError(t, dt.updateNRPSGWServices(context.Background(), "sgw", servicesDTO))

		if assert.NotNil(t, got.Action, "the request must carry an explicit update action") {
			assert.Equal(t, armnetwork.ServiceUpdateActionPartialUpdate, *got.Action)
		}
		if assert.Len(t, got.ServiceRequests, 1, "exactly the requested service must be sent") {
			sr := got.ServiceRequests[0]
			if assert.NotNil(t, sr.IsDelete) {
				assert.True(t, *sr.IsDelete, "the deletion flag must survive the DTO conversion")
			}
			if assert.NotNil(t, sr.Service) && assert.NotNil(t, sr.Service.Name) {
				assert.Equal(t, "svc", *sr.Service.Name, "the request must name the service the caller asked for")
			}
		}
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

	// NRP completes UpdateServices inline and answers 200 OK, which the generated armnetwork
	// client rejects as an error before a poller exists. updateNRPServices must apply
	// tolerateSynchronousCompletion so the registration is recorded as the success it is.
	//
	// The predicate is unit-tested directly in TestIsSynchronousCompletion; these cases exist
	// because that is not enough. Dropping the tolerance from updateNRPServices leaves the
	// predicate's own tests green while every inbound and egress Service registration fails
	// against the live provider, which is exactly how that regression once shipped.
	t.Run("tolerates NRP's synchronous 200 completion", func(t *testing.T) {
		for name, header := range map[string]http.Header{
			"bare 200":                   {},
			"200 + Location":             {"Location": []string{"https://poll"}},
			"200 + Azure-AsyncOperation": {"Azure-Asyncoperation": []string{"https://poll"}},
		} {
			t.Run(name, func(t *testing.T) {
				ctrl := gomock.NewController(t)
				defer ctrl.Finish()
				mockFactory := mock_azclient.NewMockClientFactory(ctrl)
				mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
				mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()

				req, _ := http.NewRequest(http.MethodPost, "https://example/sgw", nil)
				mockSGW.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).Return(&azcore.ResponseError{
					StatusCode:  http.StatusOK,
					RawResponse: &http.Response{StatusCode: http.StatusOK, Header: header, Request: req},
				})

				dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
				assert.NoError(t, dt.updateNRPSGWServices(context.Background(), "sgw", servicesDTO),
					"a synchronous 200 from NRP must be tolerated by updateNRPServices, not propagated")
			})
		}
	})

	// The mirror of the case above: azure_operations.go must not tolerate a 200 that is azcore's
	// terminal poll of a FAILED asynchronous operation (always a GET), or a failed NRP write is
	// recorded as a successful registration.
	t.Run("propagates a failed async LRO reported as 200", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockFactory := mock_azclient.NewMockClientFactory(ctrl)
		mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
		mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()

		pollGET, _ := http.NewRequest(http.MethodGet, "https://poll.example/op/1", nil)
		mockSGW.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).Return(&azcore.ResponseError{
			StatusCode:  http.StatusOK,
			RawResponse: &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Request: pollGET},
		})

		dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
		assert.Error(t, dt.updateNRPSGWServices(context.Background(), "sgw", servicesDTO),
			"a failed async LRO must not be recorded as a successful registration")
	})
}

func TestUpdateNRPSGWAddressLocations_Mock(t *testing.T) {
	locationsDTO := LocationsDataDTO{
		Action: PartialUpdate,
		Locations: []LocationDTO{
			{
				Location:            "node1",
				AddressUpdateAction: PartialUpdate,
				Addresses:           []AddressDTO{{Address: "10.244.0.7", ServiceNames: utilsets.NewString("svc")}},
			},
		},
	}

	t.Run("success", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockFactory := mock_azclient.NewMockClientFactory(ctrl)
		mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
		mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()

		// Capture and assert the WIRE PAYLOAD. With gomock.Any() for the request, the DTO -> ARM
		// conversion was unverified: sending the wrong node location or dropping the address list
		// left this test green while NRP received something entirely different. This is the call
		// that registers and drains pod IPs, so a wrong location strands an address under a node
		// that does not own it and a wrong address blackholes live traffic.
		var got armnetwork.ServiceGatewayUpdateAddressLocationsRequest
		mockSGW.EXPECT().UpdateAddressLocations(gomock.Any(), "rg", "sgw", gomock.Any()).
			DoAndReturn(func(_ context.Context, _, _ string, req armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
				got = req
				return nil
			})

		dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
		assert.NoError(t, dt.updateNRPSGWAddressLocations(context.Background(), "sgw", locationsDTO))

		if assert.NotNil(t, got.Action, "the request must carry an explicit update action") {
			assert.Equal(t, armnetwork.UpdateActionPartialUpdate, *got.Action)
		}
		if assert.Len(t, got.AddressLocations, 1, "exactly the requested location must be sent") {
			loc := got.AddressLocations[0]
			if assert.NotNil(t, loc.AddressLocation) {
				assert.Equal(t, "node1", *loc.AddressLocation, "the address must be filed under the requested node")
			}
			if assert.Len(t, loc.Addresses, 1, "the location's address must be sent") {
				if assert.NotNil(t, loc.Addresses[0].Address) {
					assert.Equal(t, "10.244.0.7", *loc.Addresses[0].Address)
				}
			}
		}
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

	// The address-locations path carries the same NRP synchronous-200 contract as UpdateServices:
	// without the tolerance every pod-IP registration fails against the live provider while the
	// predicate's own unit tests stay green.
	t.Run("tolerates NRP's synchronous 200 completion", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockFactory := mock_azclient.NewMockClientFactory(ctrl)
		mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
		mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
		mockSGW.EXPECT().UpdateAddressLocations(gomock.Any(), "rg", "sgw", gomock.Any()).
			Return(responseError(http.StatusOK))

		dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
		assert.NoError(t, dt.updateNRPSGWAddressLocations(context.Background(), "sgw", locationsDTO),
			"a synchronous 200 from NRP must be tolerated by updateNRPAddressLocations, not propagated")
	})

	t.Run("propagates a failed async LRO reported as 200", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockFactory := mock_azclient.NewMockClientFactory(ctrl)
		mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
		mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()

		pollGET, _ := http.NewRequest(http.MethodGet, "https://poll.example/op/1", nil)
		mockSGW.EXPECT().UpdateAddressLocations(gomock.Any(), "rg", "sgw", gomock.Any()).Return(&azcore.ResponseError{
			StatusCode:  http.StatusOK,
			RawResponse: &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Request: pollGET},
		})

		dt := &DiffTracker{networkClientFactory: mockFactory, config: testConfig()}
		assert.Error(t, dt.updateNRPSGWAddressLocations(context.Background(), "sgw", locationsDTO),
			"a failed async LRO must not be recorded as a successful location sync")
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

	t.Run("dedupes locations that differ only by IPv6 representation", func(t *testing.T) {
		// NRP rejects a request listing the same location twice (DuplicateLocationsInRequest).
		// An expanded/uppercase form and the compressed/lowercase form of one IPv6 node must
		// collapse to a single, canonical location.
		locs := convertLocationDTOsToAddressLocations([]LocationDTO{
			{Location: "FD00:0:0:0:0:0:0:A", AddressUpdateAction: PartialUpdate, Addresses: []AddressDTO{}},
			{Location: "fd00::a", AddressUpdateAction: PartialUpdate, Addresses: []AddressDTO{}},
		})
		assert.Len(t, locs, 1)
		assert.Equal(t, "fd00::a", *locs[0].AddressLocation)
	})
}

// TestBuildNRPState_FailsOnPublicIPListError verifies that a transient Public IP List failure fails
// init rather than being swallowed. The PIP enumeration is the only source for backfilling a
// crashed-mid-provisioning Service's ingress IP (recoverServiceExternalIPs runs once at init) and for
// orphan PIP cleanup, so silently continuing with an empty list would permanently drop those
// recoveries until the next restart. Failing lets the CCM retry init when Azure is healthy again.
func TestBuildNRPState_FailsOnPublicIPListError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
	mockLB := mock_loadbalancerclient.NewMockInterface(ctrl)
	mockNAT := mock_natgatewayclient.NewMockInterface(ctrl)
	mockPIP := mock_publicipaddressclient.NewMockInterface(ctrl)

	mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
	mockFactory.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()
	mockFactory.EXPECT().GetNatGatewayClient().Return(mockNAT).AnyTimes()
	mockFactory.EXPECT().GetPublicIPAddressClient().Return(mockPIP).AnyTimes()

	// The four upstream fetches succeed with empty state; only the Public IP List fails.
	mockSGW.EXPECT().GetServices(gomock.Any(), "rg", "sgw").Return(nil, nil)
	mockSGW.EXPECT().GetAddressLocations(gomock.Any(), "rg", "sgw").Return(nil, nil)
	mockLB.EXPECT().List(gomock.Any(), "rg").Return(nil, nil)
	mockNAT.EXPECT().List(gomock.Any(), "rg").Return(nil, nil)
	mockPIP.EXPECT().List(gomock.Any(), "rg").Return(nil, errors.New("transient ARM list failure"))

	_, _, _, _, _, err := buildNRPState(context.Background(), testConfig(), mockFactory)
	assert.Error(t, err, "a Public IP List failure must fail init so recovery is retried, not silently skipped")
}

// TestInitializeFromCluster_ReusesFetchedPIPListForOrphanCleanup verifies init hands the PIP slice
// already fetched by buildNRPState to orphan cleanup instead of issuing a second List. That second
// List is non-fatal, so its transient failure would be swallowed and leak an orphan already visible
// in the first slice; List is asserted to run exactly once.
func TestInitializeFromCluster_ReusesFetchedPIPListForOrphanCleanup(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
	mockLB := mock_loadbalancerclient.NewMockInterface(ctrl)
	mockNAT := mock_natgatewayclient.NewMockInterface(ctrl)
	mockPIP := mock_publicipaddressclient.NewMockInterface(ctrl)

	mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
	mockFactory.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()
	mockFactory.EXPECT().GetNatGatewayClient().Return(mockNAT).AnyTimes()
	mockFactory.EXPECT().GetPublicIPAddressClient().Return(mockPIP).AnyTimes()

	// Empty cluster and empty NRP: no ServiceGateway services/locations, no Azure LBs/NATs.
	mockSGW.EXPECT().GetServices(gomock.Any(), "rg", "sgw").Return(nil, nil).AnyTimes()
	mockSGW.EXPECT().GetAddressLocations(gomock.Any(), "rg", "sgw").Return(nil, nil).AnyTimes()
	mockSGW.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockSGW.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockLB.EXPECT().List(gomock.Any(), "rg").Return(nil, nil).AnyTimes()
	mockNAT.EXPECT().List(gomock.Any(), "rg").Return(nil, nil).AnyTimes()

	// A single detached PIP (no IPConfiguration) that no Kubernetes object or NRP entry claims.
	// List returns a NON-NIL slice: init must reuse it and never call List again, which is what
	// the Times(1) below pins. The PIP is an orphan, so the sweep deletes it.
	const orphanPIP = "leftover-pip"
	pips := []*armnetwork.PublicIPAddress{{
		Name:       ptr.To(orphanPIP),
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{},
	}}
	mockPIP.EXPECT().List(gomock.Any(), "rg").Return(pips, nil).Times(1)
	mockPIP.EXPECT().Delete(gomock.Any(), "rg", orphanPIP).Return(nil).Times(1)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dt, err := InitializeFromCluster(ctx, testConfig(), mockFactory, fake.NewSimpleClientset())
	assert.NoError(t, err)
	assert.NotNil(t, dt)
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
