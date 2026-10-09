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
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/loadbalancerclient/mock_loadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/natgatewayclient/mock_natgatewayclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/publicipaddressclient/mock_publicipaddressclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/servicegatewayclient/mock_servicegatewayclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
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
		Tags:       egressIdentityTags("leftover"),
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

// TestInitializeFromCluster_KeepsPublicIPsServicesChoose pins that startup hands the Public IPs Services
// choose to the orphan Public IP sweep. The Services are rejected by admission, so nothing is provisioned and
// their chosen addresses are detached, which is exactly when a sweep could mistake them for leaks.
func TestInitializeFromCluster_CompletesWhenNoAdditionIsDispatched(t *testing.T) {
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
	mockSGW.EXPECT().GetServices(gomock.Any(), "rg", "sgw").Return(nil, nil).AnyTimes()
	mockSGW.EXPECT().GetAddressLocations(gomock.Any(), "rg", "sgw").Return(nil, nil).AnyTimes()
	mockSGW.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockSGW.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockLB.EXPECT().List(gomock.Any(), "rg").Return(nil, nil).AnyTimes()
	mockNAT.EXPECT().List(gomock.Any(), "rg").Return(nil, nil).AnyTimes()
	mockPIP.EXPECT().List(gomock.Any(), "rg").Return(nil, nil).AnyTimes()

	otherClass := "example.com/other"
	kube := fake.NewSimpleClientset(
		&v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "rejected", Namespace: "ns", UID: types.UID("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeee4")},
			Spec: v1.ServiceSpec{
				Type:            v1.ServiceTypeLoadBalancer,
				SessionAffinity: v1.ServiceAffinityClientIP,
				Ports:           []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP}},
			},
		},
		&v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "other-class", Namespace: "ns", UID: types.UID("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeee5")},
			Spec: v1.ServiceSpec{
				Type:              v1.ServiceTypeLoadBalancer,
				LoadBalancerClass: &otherClass,
				Ports:             []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP}},
			},
		},
		// Both units of a rejected dual-stack Service are skipped.
		&v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "rejected-dual-stack", Namespace: "ns", UID: types.UID("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeee6")},
			Spec: v1.ServiceSpec{
				Type:            v1.ServiceTypeLoadBalancer,
				IPFamilies:      []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
				SessionAffinity: v1.ServiceAffinityClientIP,
				Ports:           []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP}},
			},
		},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dt, err := InitializeFromCluster(ctx, testConfig(), mockFactory, kube)
	assert.NoError(t, err, "initialization must not wait for creations that were never dispatched")
	assert.NotNil(t, dt)
}

func TestInitializeFromCluster_KeepsPublicIPsServicesChoose(t *testing.T) {
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
	mockSGW.EXPECT().GetServices(gomock.Any(), "rg", "sgw").Return(nil, nil).AnyTimes()
	mockSGW.EXPECT().GetAddressLocations(gomock.Any(), "rg", "sgw").Return(nil, nil).AnyTimes()
	mockSGW.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockSGW.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	// An orphaned load balancer gives initialization work to complete.
	const orphanLB = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	mockLB.EXPECT().List(gomock.Any(), "rg").Return([]*armnetwork.LoadBalancer{{Name: ptr.To(orphanLB)}}, nil).AnyTimes()
	mockLB.EXPECT().Get(gomock.Any(), "rg", orphanLB, gomock.Any()).Return(nil, notFoundError()).AnyTimes()
	mockLB.EXPECT().Delete(gomock.Any(), "rg", orphanLB).Return(nil).AnyTimes()
	mockPIP.EXPECT().Delete(gomock.Any(), "rg", PublicIPName(orphanLB)).Return(nil).AnyTimes()
	mockNAT.EXPECT().List(gomock.Any(), "rg").Return(nil, nil).AnyTimes()

	const byName, byAddress, orphan = "web-pip", "reserved-pip", "leftover-pip"
	const movedUID = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeee3"
	// Tagged as egress addresses so that only a Service's choice keeps the chosen ones.
	detached := func(name, address string) *armnetwork.PublicIPAddress {
		identity, _ := identityFromPublicIPName(name)
		return &armnetwork.PublicIPAddress{Name: ptr.To(name), Tags: egressIdentityTags(identity), Properties: &armnetwork.PublicIPAddressPropertiesFormat{IPAddress: ptr.To(address)}}
	}
	mockPIP.EXPECT().List(gomock.Any(), "rg").Return([]*armnetwork.PublicIPAddress{
		detached(byName, "20.0.0.7"), detached(byAddress, "20.0.0.9"), detached(orphan, "20.0.0.5"),
		{Name: ptr.To(PublicIPName(movedUID)), Properties: &armnetwork.PublicIPAddressPropertiesFormat{IPAddress: ptr.To("20.0.0.11")}},
	}, nil).Times(1)
	mockPIP.EXPECT().Delete(gomock.Any(), "rg", orphan).Return(nil).Times(1)
	mockPIP.EXPECT().Delete(gomock.Any(), "rg", PublicIPName(movedUID)).Return(nil).Times(1)

	rejected := func(name, uid string, mutate func(*v1.Service)) *v1.Service {
		svc := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID(uid)},
			Spec: v1.ServiceSpec{
				Type:            v1.ServiceTypeLoadBalancer,
				SessionAffinity: v1.ServiceAffinityClientIP,
				Ports:           []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP}},
			},
		}
		mutate(svc)
		return svc
	}
	kube := fake.NewSimpleClientset(
		rejected("by-name", "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeee1", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationPIPNameDualStack[false]: byName}
		}),
		rejected("by-address", "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeee2", func(s *v1.Service) { s.Spec.LoadBalancerIP = "20.0.0.9" }),
		rejected("moved", movedUID, func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationPIPNameDualStack[false]: "user-y"}
		}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The mock fails on any delete but the orphan's: the Public IPs the Services choose are kept.
	dt, err := InitializeFromCluster(ctx, testConfig(), mockFactory, kube)
	assert.NoError(t, err)
	assert.NotNil(t, dt)
}

// A Service deleted while the controller was down, whose Public IP was created under the name it chose but
// whose load balancer never was, still has that Public IP released at startup.
func TestInitializeFromCluster_ReleasesNamedPublicIPOfServiceDeletedWhileDown(t *testing.T) {
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
	mockSGW.EXPECT().GetServices(gomock.Any(), "rg", "sgw").Return(nil, nil).AnyTimes()
	mockSGW.EXPECT().GetAddressLocations(gomock.Any(), "rg", "sgw").Return(nil, nil).AnyTimes()
	mockSGW.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockSGW.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockLB.EXPECT().List(gomock.Any(), "rg").Return(nil, nil).AnyTimes()
	mockLB.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, notFoundError()).AnyTimes()
	mockLB.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockNAT.EXPECT().List(gomock.Any(), "rg").Return(nil, nil).AnyTimes()

	const uid = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeee1"
	named := &armnetwork.PublicIPAddress{Name: ptr.To("web-ip"), Properties: &armnetwork.PublicIPAddressPropertiesFormat{},
		Tags: map[string]*string{consts.ServiceTagKey: ptr.To("ns/web"), consts.ClusterNameKey: ptr.To("cluster")}}
	mockPIP.EXPECT().List(gomock.Any(), "rg").Return([]*armnetwork.PublicIPAddress{named}, nil).AnyTimes()
	mockPIP.EXPECT().Get(gomock.Any(), "rg", "web-ip", gomock.Any()).Return(named, nil).AnyTimes()
	mockPIP.EXPECT().Delete(gomock.Any(), "rg", PublicIPName(uid)).Return(nil).AnyTimes()
	released := make(chan struct{}, 1)
	mockPIP.EXPECT().Delete(gomock.Any(), "rg", "web-ip").DoAndReturn(func(context.Context, string, string) error {
		released <- struct{}{}
		return nil
	}).Times(1)

	now := metav1.Now()
	kube := fake.NewSimpleClientset(&v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns", UID: types.UID(uid), DeletionTimestamp: &now,
			Finalizers:  []string{ServiceGatewayServiceCleanupFinalizer},
			Annotations: map[string]string{consts.ServiceAnnotationPIPNameDualStack[false]: "web-ip"}},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer, Ports: []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP}}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dt, err := InitializeFromCluster(ctx, testConfig(), mockFactory, kube)
	assert.NoError(t, err)
	assert.NotNil(t, dt)
	select {
	case <-released:
	case <-ctx.Done():
		t.Fatal("the named Public IP the controller created was not released")
	}
	assert.Eventually(t, func() bool {
		svc, err := kube.CoreV1().Services("ns").Get(context.Background(), "web", metav1.GetOptions{})
		return err == nil && !hasServiceGatewayFinalizer(svc)
	}, 10*time.Second, 50*time.Millisecond, "the finalizer is removed once the Public IP is released")
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

// TestInitializeFromCluster_RecordsTheIPFamiliesOfServices pins that a restart knows each Service's IP
// families before any endpoint event arrives, so addresses keep going to the unit of their family.
func TestInitializeFromCluster_RecordsTheIPFamiliesOfServices(t *testing.T) {
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
	mockSGW.EXPECT().GetServices(gomock.Any(), "rg", "sgw").Return(nil, nil).AnyTimes()
	mockSGW.EXPECT().GetAddressLocations(gomock.Any(), "rg", "sgw").Return(nil, nil).AnyTimes()
	mockSGW.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	// An orphaned load balancer gives initialization work to complete; without it, a cluster whose
	// only Services are rejected waits for an initial sync nothing triggers.
	const orphanLB = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	mockLB.EXPECT().List(gomock.Any(), "rg").Return([]*armnetwork.LoadBalancer{{Name: ptr.To(orphanLB)}}, nil).AnyTimes()
	mockLB.EXPECT().Get(gomock.Any(), "rg", orphanLB, gomock.Any()).Return(nil, notFoundError()).AnyTimes()
	mockLB.EXPECT().Delete(gomock.Any(), "rg", orphanLB).Return(nil).AnyTimes()
	mockPIP.EXPECT().Delete(gomock.Any(), "rg", PublicIPName(orphanLB)).Return(nil).AnyTimes()
	mockNAT.EXPECT().List(gomock.Any(), "rg").Return(nil, nil).AnyTimes()
	mockPIP.EXPECT().List(gomock.Any(), "rg").Return(nil, nil).AnyTimes()

	// Rejected by admission, so nothing is provisioned; its families are still known.
	const uid = "11111111-2222-3333-4444-555555555555"
	kube := fake.NewSimpleClientset(&v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns", UID: uid},
		Spec: v1.ServiceSpec{
			Type:            v1.ServiceTypeLoadBalancer,
			SessionAffinity: v1.ServiceAffinityClientIP,
			IPFamilies:      []v1.IPFamily{v1.IPv6Protocol, v1.IPv4Protocol},
			Ports:           []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP}},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dt, err := InitializeFromCluster(ctx, testConfig(), mockFactory, kube)
	assert.NoError(t, err)
	if assert.NotNil(t, dt) {
		dt.mu.Lock()
		defer dt.mu.Unlock()
		assert.Equal(t, []v1.IPFamily{v1.IPv6Protocol, v1.IPv4Protocol}, dt.inboundFamilies[uid])
		assert.True(t, dt.K8sResources.Services.Has(uid))
		assert.True(t, dt.K8sResources.Services.Has(uid+"-v4"))
	}
}

// A deleting dual-stack Service whose primary unit still has a load balancer and whose IPv6 family chose a
// Public IP by name: the secondary unit releases that Public IP before the primary unit is deleted and the
// finalizer removed, so a restart in between still finds the Service.
func TestInitializeFromCluster_ReleasesTheSecondaryUnitsChosenPublicIPBeforeThePrimary(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const uid = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeee2"
	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
	mockLB := mock_loadbalancerclient.NewMockInterface(ctrl)
	mockNAT := mock_natgatewayclient.NewMockInterface(ctrl)
	mockPIP := mock_publicipaddressclient.NewMockInterface(ctrl)
	mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
	mockFactory.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()
	mockFactory.EXPECT().GetNatGatewayClient().Return(mockNAT).AnyTimes()
	mockFactory.EXPECT().GetPublicIPAddressClient().Return(mockPIP).AnyTimes()
	mockSGW.EXPECT().GetServices(gomock.Any(), "rg", "sgw").Return(nil, nil).AnyTimes()
	mockSGW.EXPECT().GetAddressLocations(gomock.Any(), "rg", "sgw").Return(nil, nil).AnyTimes()
	mockSGW.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockSGW.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockNAT.EXPECT().List(gomock.Any(), "rg").Return(nil, nil).AnyTimes()
	mockLB.EXPECT().List(gomock.Any(), "rg").Return([]*armnetwork.LoadBalancer{{Name: ptr.To(uid)}}, nil).AnyTimes()
	mockLB.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, notFoundError()).AnyTimes()

	var mu sync.Mutex
	var order []string
	record := func(event string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, event)
	}
	mockLB.EXPECT().Delete(gomock.Any(), "rg", gomock.Any()).DoAndReturn(func(_ context.Context, _, name string) error {
		record("lb " + name)
		return nil
	}).AnyTimes()
	chosen := &armnetwork.PublicIPAddress{Name: ptr.To("mine-v6"), Properties: &armnetwork.PublicIPAddressPropertiesFormat{},
		Tags: map[string]*string{consts.ServiceTagKey: ptr.To("ns/web"), consts.ClusterNameKey: ptr.To("cluster")}}
	mockPIP.EXPECT().List(gomock.Any(), "rg").Return([]*armnetwork.PublicIPAddress{chosen}, nil).AnyTimes()
	mockPIP.EXPECT().Get(gomock.Any(), "rg", "mine-v6", gomock.Any()).Return(chosen, nil).AnyTimes()
	mockPIP.EXPECT().Delete(gomock.Any(), "rg", gomock.Any()).DoAndReturn(func(_ context.Context, _, name string) error {
		record("pip " + name)
		return nil
	}).AnyTimes()

	now := metav1.Now()
	kube := fake.NewSimpleClientset(&v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns", UID: types.UID(uid), DeletionTimestamp: &now,
			Finalizers:  []string{ServiceGatewayServiceCleanupFinalizer},
			Annotations: map[string]string{consts.ServiceAnnotationPIPNameDualStack[true]: "mine-v6"}},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer, IPFamilies: []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
			Ports: []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP}}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dt, err := InitializeFromCluster(ctx, testConfig(), mockFactory, kube)
	assert.NoError(t, err)
	assert.NotNil(t, dt)
	assert.Eventually(t, func() bool {
		svc, err := kube.CoreV1().Services("ns").Get(context.Background(), "web", metav1.GetOptions{})
		return err == nil && !hasServiceGatewayFinalizer(svc)
	}, 10*time.Second, 50*time.Millisecond, "the finalizer is removed once both units are gone")

	mu.Lock()
	defer mu.Unlock()
	released, primaryDeleted := slices.Index(order, "pip mine-v6"), slices.Index(order, "lb "+uid)
	assert.True(t, released >= 0 && primaryDeleted > released, "the chosen Public IP must be released before the primary unit is deleted: %v", order)
}

// A Service whose primary IP family changed to IPv6 while the controller was down: startup sees its own
// Public IP is IPv4, and the Service's next reconcile recreates the primary unit in IPv6.
func TestInitializeFromCluster_RecreatesThePrimaryUnitOfAChangedFamily(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const uid = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeee3"
	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
	mockLB := mock_loadbalancerclient.NewMockInterface(ctrl)
	mockNAT := mock_natgatewayclient.NewMockInterface(ctrl)
	mockPIP := mock_publicipaddressclient.NewMockInterface(ctrl)
	mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
	mockFactory.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()
	mockFactory.EXPECT().GetNatGatewayClient().Return(mockNAT).AnyTimes()
	mockFactory.EXPECT().GetPublicIPAddressClient().Return(mockPIP).AnyTimes()
	mockSGW.EXPECT().GetServices(gomock.Any(), "rg", "sgw").Return([]*armnetwork.ServiceGatewayService{{
		Name: ptr.To(uid), Properties: &armnetwork.ServiceGatewayServicePropertiesFormat{ServiceType: ptr.To(armnetwork.ServiceTypeInbound)},
	}}, nil).AnyTimes()
	mockSGW.EXPECT().GetAddressLocations(gomock.Any(), "rg", "sgw").Return(nil, nil).AnyTimes()
	mockSGW.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockSGW.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockNAT.EXPECT().List(gomock.Any(), "rg").Return(nil, nil).AnyTimes()
	mockLB.EXPECT().List(gomock.Any(), "rg").Return([]*armnetwork.LoadBalancer{{Name: ptr.To(uid)}}, nil).AnyTimes()
	mockLB.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, notFoundError()).AnyTimes()
	mockLB.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _, _ string, lb armnetwork.LoadBalancer) (*armnetwork.LoadBalancer, error) {
			lb.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateSucceeded)
			return &lb, nil
		}).AnyTimes()
	deleted := make(chan string, 4)
	mockLB.EXPECT().Delete(gomock.Any(), "rg", gomock.Any()).DoAndReturn(func(_ context.Context, _, name string) error {
		deleted <- name
		return nil
	}).AnyTimes()
	oldPIP := &armnetwork.PublicIPAddress{Name: ptr.To(PublicIPName(uid)), Properties: &armnetwork.PublicIPAddressPropertiesFormat{
		PublicIPAddressVersion: ptr.To(armnetwork.IPVersionIPv4), IPAddress: ptr.To("20.1.2.3")}}
	mockPIP.EXPECT().List(gomock.Any(), "rg").Return([]*armnetwork.PublicIPAddress{oldPIP}, nil).AnyTimes()
	mockPIP.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, notFoundError()).AnyTimes()
	mockPIP.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockPIP.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _, _ string, pip armnetwork.PublicIPAddress) (*armnetwork.PublicIPAddress, error) {
			pip.Properties.IPAddress = ptr.To("2603:1030::7")
			pip.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateSucceeded)
			return &pip, nil
		}).AnyTimes()

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns", UID: types.UID(uid), Finalizers: []string{ServiceGatewayServiceCleanupFinalizer}},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer, IPFamilies: []v1.IPFamily{v1.IPv6Protocol},
			Ports: []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP}}},
		Status: v1.ServiceStatus{LoadBalancer: v1.LoadBalancerStatus{Ingress: []v1.LoadBalancerIngress{{IP: "20.1.2.3"}}}},
	}
	kube := fake.NewSimpleClientset(svc)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dt, err := InitializeFromCluster(ctx, testConfig(), mockFactory, kube)
	assert.NoError(t, err)
	if !assert.NotNil(t, dt) {
		return
	}
	select {
	case name := <-deleted:
		assert.Equal(t, uid, name, "startup deletes the IPv4 primary unit")
	case <-ctx.Done():
		t.Fatal("startup did not delete the primary unit of the old family")
	}
	assert.NotContains(t, dt.provisionedFamilies, uid, "startup must consume the stale provisioned family")

	// The service controller reconciles every LoadBalancer Service after a restart.
	assert.NoError(t, dt.ReconcileInboundService(svc))
	assert.Never(t, func() bool {
		select {
		case <-deleted:
			return true
		default:
			return false
		}
	}, 500*time.Millisecond, 50*time.Millisecond, "reconcile must not schedule a second primary-unit delete")
	assert.Eventually(t, func() bool {
		svc, err := kube.CoreV1().Services("ns").Get(context.Background(), "web", metav1.GetOptions{})
		return err == nil && len(svc.Status.LoadBalancer.Ingress) == 1 && svc.Status.LoadBalancer.Ingress[0].IP == "2603:1030::7"
	}, 10*time.Second, 50*time.Millisecond, "the primary unit is recreated in IPv6 and the IPv4 address leaves the status")
}

func TestInitializeFromCluster_RecreatePrimaryPropagatesStatusFailure(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const uid = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeee4"
	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
	mockLB := mock_loadbalancerclient.NewMockInterface(ctrl)
	mockNAT := mock_natgatewayclient.NewMockInterface(ctrl)
	mockPIP := mock_publicipaddressclient.NewMockInterface(ctrl)
	mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
	mockFactory.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()
	mockFactory.EXPECT().GetNatGatewayClient().Return(mockNAT).AnyTimes()
	mockFactory.EXPECT().GetPublicIPAddressClient().Return(mockPIP).AnyTimes()
	mockSGW.EXPECT().GetServices(gomock.Any(), "rg", "sgw").Return([]*armnetwork.ServiceGatewayService{{
		Name: ptr.To(uid), Properties: &armnetwork.ServiceGatewayServicePropertiesFormat{ServiceType: ptr.To(armnetwork.ServiceTypeInbound)},
	}}, nil).AnyTimes()
	mockSGW.EXPECT().GetAddressLocations(gomock.Any(), "rg", "sgw").Return(nil, nil).AnyTimes()
	mockSGW.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockSGW.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockNAT.EXPECT().List(gomock.Any(), "rg").Return(nil, nil).AnyTimes()
	mockLB.EXPECT().List(gomock.Any(), "rg").Return([]*armnetwork.LoadBalancer{{Name: ptr.To(uid)}}, nil).AnyTimes()
	mockLB.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, notFoundError()).AnyTimes()
	mockLB.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _, _ string, lb armnetwork.LoadBalancer) (*armnetwork.LoadBalancer, error) {
			lb.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateSucceeded)
			return &lb, nil
		}).AnyTimes()
	mockLB.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	oldPIP := &armnetwork.PublicIPAddress{Name: ptr.To(PublicIPName(uid)), Properties: &armnetwork.PublicIPAddressPropertiesFormat{
		PublicIPAddressVersion: ptr.To(armnetwork.IPVersionIPv4), IPAddress: ptr.To("20.1.2.3")}}
	mockPIP.EXPECT().List(gomock.Any(), "rg").Return([]*armnetwork.PublicIPAddress{oldPIP}, nil).AnyTimes()
	mockPIP.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, notFoundError()).AnyTimes()
	mockPIP.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockPIP.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _, _ string, pip armnetwork.PublicIPAddress) (*armnetwork.PublicIPAddress, error) {
			pip.Properties.IPAddress = ptr.To("2603:1030::7")
			pip.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateSucceeded)
			return &pip, nil
		}).AnyTimes()

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns", UID: types.UID(uid), Finalizers: []string{ServiceGatewayServiceCleanupFinalizer}},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer, IPFamilies: []v1.IPFamily{v1.IPv6Protocol},
			Ports: []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP}}},
		Status: v1.ServiceStatus{LoadBalancer: v1.LoadBalancerStatus{Ingress: []v1.LoadBalancerIngress{{IP: "20.1.2.3"}}}},
	}
	kube := fake.NewSimpleClientset(svc)
	kube.PrependReactor("patch", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "status" {
			return true, nil, errors.New("apiserver unavailable")
		}
		return false, nil, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dt, err := InitializeFromCluster(ctx, testConfig(), mockFactory, kube)

	assert.ErrorContains(t, err, "apiserver unavailable")
	assert.Nil(t, dt)
}
