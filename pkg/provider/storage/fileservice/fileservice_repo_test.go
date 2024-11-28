/*
Copyright 2024 The Kubernetes Authors.

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

package fileservice

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storage/armstorage"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/fileservicepropertiesclient/mock_fileservicepropertiesclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
)

func TestGetFileServicePropertiesCache(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fileserviceRepo := &fileServicePropertiesRepo{}

	tests := []struct {
		name                          string
		subsID                        string
		resourceGroup                 string
		account                       string
		setFileClient                 bool
		setFileServicePropertiesCache bool
		expectedErr                   string
	}{
		{
			name:        "[failure] FileClient is nil",
			expectedErr: "clientFactory is nil",
		},
		{
			name:          "[failure] fileServicePropertiesCache is nil",
			setFileClient: true,
			expectedErr:   "fileServicePropertiesCache is nil",
		},
		{
			name:                          "[Success]",
			setFileClient:                 true,
			setFileServicePropertiesCache: true,
			expectedErr:                   "",
		},
	}

	for _, test := range tests {
		if test.setFileClient {
			mockFileClient := mock_fileservicepropertiesclient.NewMockInterface(ctrl)
			fileserviceRepo.clientFactory = mock_azclient.NewMockClientFactory(ctrl)
			fileserviceRepo.clientFactory.(*mock_azclient.MockClientFactory).EXPECT().GetFileServicePropertiesClientForSub(gomock.Any()).Return(mockFileClient, nil).AnyTimes()
			mockFileClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.FileServiceProperties{}, nil).AnyTimes()
		}
		if test.setFileServicePropertiesCache {
			getter := func(_ context.Context, _ string) (*armstorage.FileServiceProperties, error) { return nil, nil }
			fileserviceRepo.fileServicePropertiesCache, _ = cache.NewTimedCache(time.Minute, getter, false)
		}

		_, err := fileserviceRepo.Get(ctx, test.subsID, test.resourceGroup, test.account)
		assert.Equal(t, err == nil, test.expectedErr == "", fmt.Sprintf("returned error: %v", err), test.name)
		if test.expectedErr != "" && err != nil {
			assert.Equal(t, err.Error(), test.expectedErr, err.Error(), test.name)
		}
	}
}
