/*
Copyright 2020 The Kubernetes Authors.

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

package storage_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storage/armstorage"
	"go.uber.org/mock/gomock"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/accountclient/mock_accountclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/storage"
)

func TestStorage(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	value := "foo bar"
	oldTime := time.Now().Add(-time.Hour)
	newValue := "newkey"
	newTime := time.Now()

	tests := []struct {
		results             []*armstorage.AccountKey
		getLatestAccountKey bool
		expectedKey         string
		expectErr           bool
		err                 error
	}{
		{[]*armstorage.AccountKey{}, false, "", true, nil},
		{
			[]*armstorage.AccountKey{
				{Value: &value},
			},
			false,
			"bar",
			false,
			nil,
		},
		{
			[]*armstorage.AccountKey{
				{},
				{Value: &value},
			},
			false,
			"bar",
			false,
			nil,
		},
		{
			[]*armstorage.AccountKey{
				{Value: &value, CreationTime: &oldTime},
				{Value: &newValue, CreationTime: &newTime},
			},

			true,
			"newkey",
			false,
			nil,
		},
		{
			[]*armstorage.AccountKey{
				{Value: &value, CreationTime: &oldTime},
				{Value: &newValue, CreationTime: &newTime},
			},

			false,
			"bar",
			false,
			nil,
		},
		{[]*armstorage.AccountKey{}, false, "", true, fmt.Errorf("test error")},
	}

	for _, test := range tests {
		mockStorageAccountsClient := mock_accountclient.NewMockInterface(ctrl)
		mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), "rg", gomock.Any()).Return(test.results, nil).AnyTimes()
		key, err := storage.GetStorageAccesskey(ctx, mockStorageAccountsClient, "acct", "rg", test.getLatestAccountKey)
		if test.expectErr && err == nil {
			t.Errorf("Unexpected non-error")
			continue
		}
		if !test.expectErr && err != nil {
			t.Errorf("Unexpected error: %v", err)
			continue
		}
		if key != test.expectedKey {
			t.Errorf("expected: %s, saw %s", test.expectedKey, key)
		}
	}
}
