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

package securitygroup

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/stretchr/testify/assert"

	"go.uber.org/mock/gomock"

	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/securitygroupclient/mock_securitygroupclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

func TestCreateOrUpdateSecurityGroupCanceled(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockSGClient := mock_securitygroupclient.NewMockInterface(ctrl)
	az, err := NewSecurityGroupRepo("rg", "sg", 120, false, mockSGClient)
	assert.NoError(t, err)
	az.(*securityGroupRepo).nsgCache.Set("sg", "test")

	mockSGClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", "sg", gomock.Any()).Return(nil, &azcore.ResponseError{
		RawResponse: &http.Response{
			Body: io.NopCloser(strings.NewReader(consts.OperationCanceledErrorMessage)),
		},
	})
	mockSGClient.EXPECT().Get(gomock.Any(), "rg", "sg").Return(&armnetwork.SecurityGroup{}, nil)

	err = az.CreateOrUpdateSecurityGroup(context.TODO(), &armnetwork.SecurityGroup{Name: ptr.To("sg")})
	assert.Contains(t, err.Error(), "canceledandsupersededduetoanotheroperation")

	// security group should be removed from cache if the operation is canceled
	shouldBeEmpty, err := az.(*securityGroupRepo).nsgCache.GetWithDeepCopy(context.TODO(), "sg", cache.CacheReadTypeDefault)
	assert.NoError(t, err)
	assert.Empty(t, shouldBeEmpty)
}
