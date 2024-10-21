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
	"k8s.io/utils/pointer"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/routetableclient/mockroutetableclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

func TestCreateOrUpdateRouteTable(t *testing.T) {
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
		az.rtCache.Set("rt", "test")

		mockRTClient := az.RouteTablesClient.(*mockroutetableclient.MockInterface)
		mockRTClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(test.clientErr)
		mockRTClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "rt", gomock.Any()).Return(network.RouteTable{}, nil)

		err := az.CreateOrUpdateRouteTable(network.RouteTable{
			Name: pointer.String("rt"),
			Etag: pointer.String("etag"),
		})
		assert.EqualError(t, test.expectedErr, err.Error())

		// route table should be removed from cache if the etag is mismatch or the operation is canceled
		shouldBeEmpty, err := az.rtCache.GetWithDeepCopy(context.TODO(), "rt", cache.CacheReadTypeDefault)
		assert.NoError(t, err)
		assert.Empty(t, shouldBeEmpty)
	}
}
