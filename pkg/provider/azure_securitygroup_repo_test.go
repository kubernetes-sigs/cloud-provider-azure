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
	"fmt"
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	"k8s.io/utils/pointer"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/securitygroupclient/mocksecuritygroupclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

func TestCreateOrUpdateSecurityGroupCanceled(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	az.nsgCache.Set("sg", "test")

	mockSGClient := az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
	mockSGClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(&retry.Error{
		RawError: fmt.Errorf(consts.OperationCanceledErrorMessage),
	})
	mockSGClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "sg", gomock.Any()).Return(network.SecurityGroup{}, nil)

	err := az.CreateOrUpdateSecurityGroup(network.SecurityGroup{Name: pointer.String("sg")})
	assert.EqualError(t, fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: %w", fmt.Errorf("canceledandsupersededduetoanotheroperation")), err.Error())

	// security group should be removed from cache if the operation is canceled
	shouldBeEmpty, err := az.nsgCache.GetWithDeepCopy("sg", cache.CacheReadTypeDefault)
	assert.NoError(t, err)
	assert.Empty(t, shouldBeEmpty)
}
