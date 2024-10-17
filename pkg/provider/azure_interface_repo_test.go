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
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/interfaceclient/mockinterfaceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

func TestCreateOrUpdateInterface(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	mockInterfaceClient := az.InterfacesClient.(*mockinterfaceclient.MockInterface)
	mockInterfaceClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, "nic", gomock.Any()).Return(&retry.Error{HTTPStatusCode: http.StatusInternalServerError})

	err := az.CreateOrUpdateInterface(context.TODO(), &v1.Service{}, network.Interface{Name: ptr.To("nic")})
	assert.EqualError(t, fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 500, RawError: %w", error(nil)), err.Error())
}
