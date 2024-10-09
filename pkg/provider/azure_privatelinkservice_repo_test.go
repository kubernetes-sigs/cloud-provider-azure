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
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/pointer"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/privatelinkserviceclient/mockprivatelinkserviceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/cache"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

func TestCreateOrUpdatePLS(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tests := []struct {
		clientErr   *retry.Error
		expectedErr error
	}{
		{
			clientErr:   &retry.Error{HTTPStatusCode: http.StatusPreconditionFailed},
			expectedErr: fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 412, RawError: %w", error(nil)),
		},
		{
			clientErr:   &retry.Error{RawError: fmt.Errorf(consts.OperationCanceledErrorMessage)},
			expectedErr: fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: %w", fmt.Errorf("canceledandsupersededduetoanotheroperation")),
		},
	}

	for _, test := range tests {
		az := GetTestCloud(ctrl)
		az.pipCache.Set("rg*frontendID", "test")

		mockPLSClient := az.PrivateLinkServiceClient.(*mockprivatelinkserviceclient.MockInterface)
		mockPLSClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", gomock.Any(), gomock.Any(), gomock.Any()).Return(test.clientErr)
		mockPLSClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return([]network.PrivateLinkService{}, nil)

		err := az.CreateOrUpdatePLS(&v1.Service{}, "rg", network.PrivateLinkService{
			Name: pointer.String("pls"),
			Etag: pointer.String("etag"),
			PrivateLinkServiceProperties: &network.PrivateLinkServiceProperties{
				LoadBalancerFrontendIPConfigurations: &[]network.FrontendIPConfiguration{
					{
						ID: pointer.String("frontendID"),
					},
				},
			},
		})
		assert.EqualError(t, test.expectedErr, err.Error())

		// loadbalancer should be removed from cache if the etag is mismatch or the operation is canceled
		pls, err := az.plsCache.GetWithDeepCopy(context.TODO(), "rg*frontendID", cache.CacheReadTypeDefault)
		assert.NoError(t, err)
		assert.Equal(t, consts.PrivateLinkServiceNotExistID, *pls.(*network.PrivateLinkService).ID)
	}
}

func TestDeletePLS(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	mockPLSClient := az.PrivateLinkServiceClient.(*mockprivatelinkserviceclient.MockInterface)
	mockPLSClient.EXPECT().Delete(gomock.Any(), "rg", "pls").Return(&retry.Error{HTTPStatusCode: http.StatusInternalServerError})

	err := az.DeletePLS(&v1.Service{}, "rg", "pls", "frontendID")
	assert.EqualError(t, fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 500, RawError: %w", error(nil)), fmt.Sprintf("%s", err.Error()))
}

func TestDeletePEConn(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	mockPLSClient := az.PrivateLinkServiceClient.(*mockprivatelinkserviceclient.MockInterface)
	mockPLSClient.EXPECT().DeletePEConnection(gomock.Any(), "rg", "pls", "peConn").Return(&retry.Error{HTTPStatusCode: http.StatusInternalServerError})

	err := az.DeletePEConn(&v1.Service{}, "rg", "pls", "peConn")
	assert.EqualError(t, fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 500, RawError: %w", error(nil)), fmt.Sprintf("%s", err.Error()))
}

func TestGetPrivateLinkService(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	az.plsCache.Set("rg*frontendID", &network.PrivateLinkService{Name: pointer.String("pls")})

	// cache hit
	pls, err := az.getPrivateLinkService(context.TODO(), "rg", ptr.To("frontendID"), azcache.CacheReadTypeDefault)
	assert.NoError(t, err)
	assert.Equal(t, "pls", *pls.Name)

	// cache miss
	mockPLSClient := az.PrivateLinkServiceClient.(*mockprivatelinkserviceclient.MockInterface)
	mockPLSClient.EXPECT().List(gomock.Any(), "rg1").Return([]network.PrivateLinkService{
		{
			Name: pointer.String("pls1"),
			PrivateLinkServiceProperties: &network.PrivateLinkServiceProperties{
				LoadBalancerFrontendIPConfigurations: &[]network.FrontendIPConfiguration{
					{
						ID: pointer.String("frontendID1"),
					},
				},
			},
		},
	}, nil)
	pls, err = az.getPrivateLinkService(context.TODO(), "rg1", ptr.To("frontendID1"), azcache.CacheReadTypeDefault)
	assert.NoError(t, err)
	assert.Equal(t, "pls1", *pls.Name)
}

func TestGetPLSCacheKey(t *testing.T) {
	rg, frontendID := "rg", "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/frontendIPConfigurations/ipconfig"
	assert.Equal(t, "rg*/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/frontendIPConfigurations/ipconfig", getPLSCacheKey(rg, frontendID))
}

func TestParsePLSCacheKey(t *testing.T) {
	key := "rg*/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/frontendIPConfigurations/ipconfig"
	rg, frontendID := parsePLSCacheKey(key)
	assert.Equal(t, "rg", rg)
	assert.Equal(t, "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/frontendIPConfigurations/ipconfig", frontendID)
}
