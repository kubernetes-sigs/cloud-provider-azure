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

package provider

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/Azure/azure-sdk-for-go/services/privatedns/mgmt/2018-09-01/privatedns"
	"github.com/Azure/azure-sdk-for-go/services/storage/mgmt/2021-09-01/storage"
	"github.com/Azure/go-autorest/autorest/date"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"k8s.io/utils/pointer"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/blobclient/mockblobclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/fileclient/mockfileclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/privatednsclient/mockprivatednsclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/privatednszonegroupclient/mockprivatednszonegroupclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/privateendpointclient/mockprivateendpointclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/storageaccountclient/mockstorageaccountclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/subnetclient/mocksubnetclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/virtualnetworklinksclient/mockvirtualnetworklinksclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

const TestLocation = "testLocation"

func TestGetStorageAccessKeys(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	cloud := &Cloud{}
	value := "foo bar"
	oldTime := date.Time{}
	oldTime.Time = time.Now().Add(-time.Hour)
	newValue := "newkey"
	newTime := date.Time{}
	newTime.Time = time.Now()

	tests := []struct {
		results             storage.AccountListKeysResult
		getLatestAccountKey bool
		expectedKey         string
		expectErr           bool
		err                 error
	}{
		{storage.AccountListKeysResult{}, false, "", true, nil},
		{
			storage.AccountListKeysResult{
				Keys: &[]storage.AccountKey{
					{Value: &value},
				},
			},
			false,
			"bar",
			false,
			nil,
		},
		{
			storage.AccountListKeysResult{
				Keys: &[]storage.AccountKey{
					{},
					{Value: &value},
				},
			},
			false,
			"bar",
			false,
			nil,
		},
		{
			storage.AccountListKeysResult{
				Keys: &[]storage.AccountKey{
					{Value: &value, CreationTime: &oldTime},
					{Value: &newValue, CreationTime: &newTime},
				},
			},
			true,
			"newkey",
			false,
			nil,
		},
		{
			storage.AccountListKeysResult{
				Keys: &[]storage.AccountKey{
					{Value: &value, CreationTime: &oldTime},
					{Value: &newValue, CreationTime: &newTime},
				},
			},
			false,
			"bar",
			false,
			nil,
		},
		{storage.AccountListKeysResult{}, false, "", true, fmt.Errorf("test error")},
	}

	for _, test := range tests {
		mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
		cloud.StorageAccountClient = mockStorageAccountsClient
		mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), "", "rg", gomock.Any()).Return(test.results, nil).AnyTimes()
		key, err := cloud.GetStorageAccesskey(ctx, "", "acct", "rg", test.getLatestAccountKey)
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

func TestGetStorageAccounts(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	cloud := &Cloud{}

	name := "testAccount"
	location := TestLocation
	networkID := "networkID"
	accountProperties := storage.AccountProperties{
		NetworkRuleSet: &storage.NetworkRuleSet{
			VirtualNetworkRules: &[]storage.VirtualNetworkRule{
				{
					VirtualNetworkResourceID: &networkID,
					Action:                   storage.ActionAllow,
					State:                    "state",
				},
			},
		}}

	account := storage.Account{
		Sku: &storage.Sku{
			Name: "testSku",
			Tier: "testSkuTier",
		},
		Kind:              "testKind",
		Location:          &location,
		Name:              &name,
		AccountProperties: &accountProperties,
	}

	testResourceGroups := []storage.Account{account}

	accountOptions := &AccountOptions{
		ResourceGroup:             "rg",
		VirtualNetworkResourceIDs: []string{networkID},
		EnableHTTPSTrafficOnly:    true,
	}

	mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
	cloud.StorageAccountClient = mockStorageAccountsClient

	mockStorageAccountsClient.EXPECT().ListByResourceGroup(gomock.Any(), "", "rg").Return(testResourceGroups, nil).Times(1)

	accountsWithLocations, err := cloud.getStorageAccounts(ctx, accountOptions)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if accountsWithLocations == nil {
		t.Error("unexpected error as returned accounts are nil")
	}

	if len(accountsWithLocations) == 0 {
		t.Error("unexpected error as returned accounts slice is empty")
	}

	expectedAccountWithLocation := accountWithLocation{
		Name:        "testAccount",
		StorageType: "testSku",
		Location:    TestLocation,
	}

	accountWithLocation := accountsWithLocations[0]
	if accountWithLocation.Name != expectedAccountWithLocation.Name {
		t.Errorf("expected %s, but was %s", accountWithLocation.Name, expectedAccountWithLocation.Name)
	}

	if accountWithLocation.StorageType != expectedAccountWithLocation.StorageType {
		t.Errorf("expected %s, but was %s", accountWithLocation.StorageType, expectedAccountWithLocation.StorageType)
	}

	if accountWithLocation.Location != expectedAccountWithLocation.Location {
		t.Errorf("expected %s, but was %s", accountWithLocation.Location, expectedAccountWithLocation.Location)
	}
}

func TestGetStorageAccountEdgeCases(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	cloud := &Cloud{}

	// default account with name, location, sku, kind
	name := "testAccount"
	location := TestLocation
	sku := &storage.Sku{
		Name: "testSku",
		Tier: "testSkuTier",
	}
	account := storage.Account{
		Sku:      sku,
		Kind:     "testKind",
		Location: &location,
		Name:     &name,
	}

	accountPropertiesWithoutNetworkRuleSet := storage.AccountProperties{NetworkRuleSet: nil}
	accountPropertiesWithoutVirtualNetworkRules := storage.AccountProperties{
		NetworkRuleSet: &storage.NetworkRuleSet{
			VirtualNetworkRules: nil,
		}}

	tests := []struct {
		testCase           string
		testAccountOptions *AccountOptions
		testResourceGroups []storage.Account
		expectedResult     []accountWithLocation
		expectedError      error
	}{
		{
			testCase: "account name is nil",
			testAccountOptions: &AccountOptions{
				ResourceGroup: "rg",
			},
			testResourceGroups: []storage.Account{},
			expectedResult:     []accountWithLocation{},
			expectedError:      nil,
		},
		{
			testCase: "account location is nil",
			testAccountOptions: &AccountOptions{
				ResourceGroup: "rg",
			},
			testResourceGroups: []storage.Account{{Name: &name}},
			expectedResult:     []accountWithLocation{},
			expectedError:      nil,
		},
		{
			testCase: "account sku is nil",
			testAccountOptions: &AccountOptions{
				ResourceGroup: "rg",
			},
			testResourceGroups: []storage.Account{{Name: &name, Location: &location}},
			expectedResult:     []accountWithLocation{},
			expectedError:      nil,
		},
		{
			testCase: "account options type is not empty and not equal account storage type",
			testAccountOptions: &AccountOptions{
				ResourceGroup: "rg",
				Type:          "testAccountOptionsType",
			},
			testResourceGroups: []storage.Account{account},
			expectedResult:     []accountWithLocation{},
			expectedError:      nil,
		},
		{
			testCase: "account options kind is not empty and not equal account type",
			testAccountOptions: &AccountOptions{
				ResourceGroup: "rg",
				Kind:          "testAccountOptionsKind",
			},
			testResourceGroups: []storage.Account{account},
			expectedResult:     []accountWithLocation{},
			expectedError:      nil,
		},
		{
			testCase: "account options location is not empty and not equal account location",
			testAccountOptions: &AccountOptions{
				ResourceGroup: "rg",
				Location:      "testAccountOptionsLocation",
			},
			testResourceGroups: []storage.Account{account},
			expectedResult:     []accountWithLocation{},
			expectedError:      nil,
		},
		{
			testCase: "account options account properties are nil",
			testAccountOptions: &AccountOptions{
				ResourceGroup:             "rg",
				VirtualNetworkResourceIDs: []string{"id"},
			},
			testResourceGroups: []storage.Account{},
			expectedResult:     []accountWithLocation{},
			expectedError:      nil,
		},
		{
			testCase: "account options account properties network rule set is nil",
			testAccountOptions: &AccountOptions{
				ResourceGroup:             "rg",
				VirtualNetworkResourceIDs: []string{"id"},
			},
			testResourceGroups: []storage.Account{{Name: &name, Kind: "kind", Location: &location, Sku: sku, AccountProperties: &accountPropertiesWithoutNetworkRuleSet}},
			expectedResult:     []accountWithLocation{},
			expectedError:      nil,
		},
		{
			testCase: "account options account properties virtual network rule is nil",
			testAccountOptions: &AccountOptions{
				ResourceGroup:             "rg",
				VirtualNetworkResourceIDs: []string{"id"},
			},
			testResourceGroups: []storage.Account{{Name: &name, Kind: "kind", Location: &location, Sku: sku, AccountProperties: &accountPropertiesWithoutVirtualNetworkRules}},
			expectedResult:     []accountWithLocation{},
			expectedError:      nil,
		},
		{
			testCase: "account options CreatePrivateEndpoint is true and no private endpoint exists",
			testAccountOptions: &AccountOptions{
				ResourceGroup:         "rg",
				CreatePrivateEndpoint: true,
			},
			testResourceGroups: []storage.Account{{Name: &name, Kind: "kind", Location: &location, Sku: sku, AccountProperties: &storage.AccountProperties{}}},
			expectedResult:     []accountWithLocation{},
			expectedError:      nil,
		},
	}

	for _, test := range tests {
		t.Logf("running test case: %s", test.testCase)
		mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
		cloud.StorageAccountClient = mockStorageAccountsClient

		mockStorageAccountsClient.EXPECT().ListByResourceGroup(gomock.Any(), "", "rg").Return(test.testResourceGroups, nil).AnyTimes()

		accountsWithLocations, err := cloud.getStorageAccounts(ctx, test.testAccountOptions)
		if !errors.Is(err, test.expectedError) {
			t.Errorf("unexpected error: %v", err)
		}

		if len(accountsWithLocations) != len(test.expectedResult) {
			t.Error("unexpected error as returned accounts slice is not empty")
		}
	}
}

func TestEnsureStorageAccount(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	resourceGroup := "ResourceGroup"
	vnetName := "VnetName"
	vnetResourceGroup := "VnetResourceGroup"
	subnetName := "SubnetName"
	location := TestLocation

	cloud := GetTestCloud(ctrl)
	cloud.ResourceGroup = resourceGroup
	cloud.VnetResourceGroup = vnetResourceGroup
	cloud.VnetName = vnetName
	cloud.SubnetName = subnetName
	cloud.Location = location
	cloud.SubscriptionID = "testSub"

	name := "testStorageAccount"
	sku := &storage.Sku{
		Name: "testSku",
		Tier: "testSkuTier",
	}
	testStorageAccounts :=
		[]storage.Account{
			{Name: &name, Kind: "kind", Location: &location, Sku: sku, AccountProperties: &storage.AccountProperties{NetworkRuleSet: &storage.NetworkRuleSet{}}}}

	value := "foo bar"
	storageAccountListKeys := storage.AccountListKeysResult{
		Keys: &[]storage.AccountKey{
			{Value: &value},
		},
	}

	tests := []struct {
		name                            string
		createAccount                   bool
		createPrivateEndpoint           bool
		SubnetPropertiesFormatNil       bool
		mockStorageAccountsClient       bool
		setAccountOptions               bool
		pickRandomMatchingAccount       bool
		accessTier                      string
		storageType                     StorageType
		requireInfrastructureEncryption *bool
		keyVaultURL                     *string
		accountName                     string
		subscriptionID                  string
		resourceGroup                   string
		expectedErr                     string
	}{
		{
			name:                            "[Success] EnsureStorageAccount with createPrivateEndpoint and storagetype blob",
			createAccount:                   true,
			createPrivateEndpoint:           true,
			mockStorageAccountsClient:       true,
			setAccountOptions:               true,
			pickRandomMatchingAccount:       true,
			storageType:                     StorageTypeBlob,
			requireInfrastructureEncryption: pointer.Bool(true),
			keyVaultURL:                     pointer.String("keyVaultURL"),
			resourceGroup:                   "rg",
			accessTier:                      "AccessTierHot",
			accountName:                     "",
			expectedErr:                     "",
		},
		{
			name:                            "[Success] EnsureStorageAccount with createPrivateEndpoint",
			createAccount:                   true,
			createPrivateEndpoint:           true,
			mockStorageAccountsClient:       true,
			setAccountOptions:               true,
			requireInfrastructureEncryption: pointer.Bool(true),
			keyVaultURL:                     pointer.String("keyVaultURL"),
			resourceGroup:                   "rg",
			accessTier:                      "AccessTierHot",
			accountName:                     "",
			expectedErr:                     "",
		},
		{
			name:                      "[Failed] EnsureStorageAccount with createPrivateEndpoint: get storage key failed",
			createAccount:             true,
			createPrivateEndpoint:     true,
			SubnetPropertiesFormatNil: true,
			mockStorageAccountsClient: true,
			setAccountOptions:         true,
			resourceGroup:             "rg",
			accountName:               "accountname",
			expectedErr:               "could not get storage key for storage account",
		},
		{
			name:                      "[Failed] account options is nil",
			mockStorageAccountsClient: false,
			setAccountOptions:         false,
			expectedErr:               "account options is nil",
		},
		{
			name:              "[Failed] resourceGroup must be specified when subscriptionID is not empty",
			subscriptionID:    "abc",
			resourceGroup:     "",
			setAccountOptions: true,
			expectedErr:       "resourceGroup must be specified when subscriptionID(abc) is not empty",
		},
		{
			name:              "[Failed] could not get storage key for storage account",
			subscriptionID:    "",
			resourceGroup:     "",
			setAccountOptions: true,
			expectedErr:       "could not get storage key for storage account",
		},
	}

	for _, test := range tests {
		mockBlobClient := mockblobclient.NewMockInterface(ctrl)
		cloud.BlobClient = mockBlobClient
		mockBlobClient.EXPECT().GetServiceProperties(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(storage.BlobServiceProperties{
			BlobServicePropertiesProperties: &storage.BlobServicePropertiesProperties{}}, nil).AnyTimes()
		mockBlobClient.EXPECT().SetServiceProperties(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(storage.BlobServiceProperties{}, nil).AnyTimes()

		mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
		if test.mockStorageAccountsClient {
			cloud.StorageAccountClient = mockStorageAccountsClient
		}

		if test.createPrivateEndpoint {
			mockStorageAccountsClient.EXPECT().ListByResourceGroup(gomock.Any(), gomock.Any(), gomock.Any()).Return(testStorageAccounts, nil).AnyTimes()
			mockStorageAccountsClient.EXPECT().Create(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
			mockStorageAccountsClient.EXPECT().GetProperties(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(testStorageAccounts[0], nil).AnyTimes()
			if test.accountName == "" {
				mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(storageAccountListKeys, nil).AnyTimes()
			} else {
				mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(storageAccountListKeys, &retry.Error{}).AnyTimes()
			}

			subnetPropertiesFormat := &network.SubnetPropertiesFormat{}
			if test.SubnetPropertiesFormatNil {
				subnetPropertiesFormat = nil
			}
			subnet := network.Subnet{SubnetPropertiesFormat: subnetPropertiesFormat}

			mockSubnetsClient := mocksubnetclient.NewMockInterface(ctrl)
			mockSubnetsClient.EXPECT().Get(gomock.Any(), vnetResourceGroup, vnetName, subnetName, gomock.Any()).Return(subnet, nil).Times(1)
			mockSubnetsClient.EXPECT().CreateOrUpdate(gomock.Any(), vnetResourceGroup, vnetName, subnetName, gomock.Any()).Return(nil).Times(1)
			cloud.SubnetsClient = mockSubnetsClient

			mockPrivateDNSClient := mockprivatednsclient.NewMockInterface(ctrl)
			mockPrivateDNSClient.EXPECT().Get(gomock.Any(), vnetResourceGroup, gomock.Any()).Return(privatedns.PrivateZone{}, &retry.Error{}).Times(1)
			mockPrivateDNSClient.EXPECT().CreateOrUpdate(gomock.Any(), vnetResourceGroup, gomock.Any(), gomock.Any(), "", true).Return(nil).Times(1)
			cloud.privatednsclient = mockPrivateDNSClient

			mockPrivateDNSZoneGroup := mockprivatednszonegroupclient.NewMockInterface(ctrl)
			mockPrivateDNSZoneGroup.EXPECT().CreateOrUpdate(gomock.Any(), vnetResourceGroup, gomock.Any(), gomock.Any(), gomock.Any(), "", false).Return(nil).Times(1)
			cloud.privatednszonegroupclient = mockPrivateDNSZoneGroup

			mockPrivateEndpointClient := mockprivateendpointclient.NewMockInterface(ctrl)
			mockPrivateEndpointClient.EXPECT().CreateOrUpdate(gomock.Any(), vnetResourceGroup, gomock.Any(), gomock.Any(), "", true).Return(nil).Times(1)
			cloud.privateendpointclient = mockPrivateEndpointClient

			mockVirtualNetworkLinksClient := mockvirtualnetworklinksclient.NewMockInterface(ctrl)
			mockVirtualNetworkLinksClient.EXPECT().Get(gomock.Any(), vnetResourceGroup, gomock.Any(), gomock.Any()).Return(privatedns.VirtualNetworkLink{}, &retry.Error{}).Times(1)
			mockVirtualNetworkLinksClient.EXPECT().CreateOrUpdate(gomock.Any(), vnetResourceGroup, gomock.Any(), gomock.Any(), gomock.Any(), "", false).Return(nil).Times(1)
			cloud.virtualNetworkLinksClient = mockVirtualNetworkLinksClient
		}

		var testAccountOptions *AccountOptions
		if test.setAccountOptions {
			testAccountOptions = &AccountOptions{
				ResourceGroup:             test.resourceGroup,
				CreatePrivateEndpoint:     test.createPrivateEndpoint,
				Name:                      test.accountName,
				CreateAccount:             test.createAccount,
				SubscriptionID:            test.subscriptionID,
				AccessTier:                test.accessTier,
				StorageType:               test.storageType,
				EnableBlobVersioning:      pointer.Bool(true),
				SoftDeleteBlobs:           7,
				SoftDeleteContainers:      7,
				PickRandomMatchingAccount: test.pickRandomMatchingAccount,
			}
		}

		_, _, err := cloud.EnsureStorageAccount(ctx, testAccountOptions, "test")
		assert.Equal(t, err == nil, test.expectedErr == "", fmt.Sprintf("returned error: %v", err), test.name)
		if test.expectedErr != "" {
			assert.Equal(t, err != nil, strings.Contains(err.Error(), test.expectedErr), err.Error(), test.name)
		}
	}
}

func TestGetStorageAccountWithCache(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	cloud := &Cloud{}

	tests := []struct {
		name                    string
		subsID                  string
		resourceGroup           string
		account                 string
		setStorageAccountClient bool
		setStorageAccountCache  bool
		expectedErr             string
	}{
		{
			name:        "[failure] StorageAccountClient is nil",
			expectedErr: "StorageAccountClient is nil",
		},
		{
			name:                    "[failure] storageAccountCache is nil",
			setStorageAccountClient: true,
			expectedErr:             "storageAccountCache is nil",
		},
		{
			name:                    "[Success]",
			setStorageAccountClient: true,
			setStorageAccountCache:  true,
			expectedErr:             "",
		},
	}

	for _, test := range tests {
		if test.setStorageAccountClient {
			mockStorageAccountsClient := mockstorageaccountclient.NewMockInterface(ctrl)
			cloud.StorageAccountClient = mockStorageAccountsClient
			mockStorageAccountsClient.EXPECT().GetProperties(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(storage.Account{}, nil).AnyTimes()
		}

		if test.setStorageAccountCache {
			getter := func(key string) (interface{}, error) { return nil, nil }
			cloud.storageAccountCache, _ = cache.NewTimedCache(time.Minute, getter, false)
		}

		_, err := cloud.getStorageAccountWithCache(ctx, test.subsID, test.resourceGroup, test.account)
		assert.Equal(t, err == nil, test.expectedErr == "", fmt.Sprintf("returned error: %v", err), test.name)
		if test.expectedErr != "" && err != nil {
			assert.Equal(t, err.RawError.Error(), test.expectedErr, err.RawError.Error(), test.name)
		}
	}
}

func TestIsPrivateEndpointAsExpected(t *testing.T) {
	tests := []struct {
		account        storage.Account
		accountOptions *AccountOptions
		expectedResult bool
	}{
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					PrivateEndpointConnections: &[]storage.PrivateEndpointConnection{{}},
				},
			},
			accountOptions: &AccountOptions{
				CreatePrivateEndpoint: true,
			},
			expectedResult: true,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					PrivateEndpointConnections: nil,
				},
			},
			accountOptions: &AccountOptions{
				CreatePrivateEndpoint: false,
			},
			expectedResult: true,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					PrivateEndpointConnections: &[]storage.PrivateEndpointConnection{{}},
				},
			},
			accountOptions: &AccountOptions{
				CreatePrivateEndpoint: false,
			},
			expectedResult: false,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					PrivateEndpointConnections: nil,
				},
			},
			accountOptions: &AccountOptions{
				CreatePrivateEndpoint: true,
			},
			expectedResult: false,
		},
	}

	for _, test := range tests {
		result := isPrivateEndpointAsExpected(test.account, test.accountOptions)
		assert.Equal(t, test.expectedResult, result)
	}
}

func TestIsTagsEqual(t *testing.T) {
	tests := []struct {
		desc           string
		account        storage.Account
		accountOptions *AccountOptions
		expectedResult bool
	}{
		{
			desc: "nil tags",
			account: storage.Account{
				Tags: nil,
			},
			accountOptions: &AccountOptions{},
			expectedResult: true,
		},
		{
			desc: "empty tags",
			account: storage.Account{
				Tags: map[string]*string{},
			},
			accountOptions: &AccountOptions{},
			expectedResult: true,
		},
		{
			desc: "identitical tags",
			account: storage.Account{
				Tags: map[string]*string{
					"key":  pointer.String("value"),
					"key2": nil,
				},
			},
			accountOptions: &AccountOptions{
				Tags: map[string]string{
					"key":  "value",
					"key2": "",
				},
			},
			expectedResult: true,
		},
		{
			desc: "identitical tags",
			account: storage.Account{
				Tags: map[string]*string{
					"key":  pointer.String("value"),
					"key2": pointer.String("value2"),
				},
			},
			accountOptions: &AccountOptions{
				Tags: map[string]string{
					"key2": "value2",
					"key":  "value",
				},
			},
			expectedResult: true,
		},
		{
			desc: "non-identitical tags while MatchTags is false",
			account: storage.Account{
				Tags: map[string]*string{
					"key": pointer.String("value2"),
				},
			},
			accountOptions: &AccountOptions{
				MatchTags: false,
				Tags: map[string]string{
					"key": "value",
				},
			},
			expectedResult: true,
		},
		{
			desc: "non-identitical tags",
			account: storage.Account{
				Tags: map[string]*string{
					"key": pointer.String("value2"),
				},
			},
			accountOptions: &AccountOptions{
				MatchTags: true,
				Tags: map[string]string{
					"key": "value",
				},
			},
			expectedResult: false,
		},
	}

	for _, test := range tests {
		result := isTagsEqual(test.account, test.accountOptions)
		assert.Equal(t, test.expectedResult, result)
	}
}

func TestIsHnsPropertyEqual(t *testing.T) {
	tests := []struct {
		account        storage.Account
		accountOptions *AccountOptions
		expectedResult bool
	}{
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					IsHnsEnabled: pointer.Bool(true),
				},
			},
			accountOptions: &AccountOptions{},
			expectedResult: false,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				IsHnsEnabled: pointer.Bool(false),
			},
			expectedResult: true,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					IsHnsEnabled: pointer.Bool(true),
				},
			},
			accountOptions: &AccountOptions{
				IsHnsEnabled: pointer.Bool(true),
			},
			expectedResult: true,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				IsHnsEnabled: pointer.Bool(true),
			},
			expectedResult: false,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					IsHnsEnabled: pointer.Bool(true),
				},
			},
			accountOptions: &AccountOptions{
				IsHnsEnabled: pointer.Bool(false),
			},
			expectedResult: false,
		},
	}

	for _, test := range tests {
		result := isHnsPropertyEqual(test.account, test.accountOptions)
		assert.Equal(t, test.expectedResult, result)
	}
}

func TestIsEnableNfsV3PropertyEqual(t *testing.T) {
	tests := []struct {
		account        storage.Account
		accountOptions *AccountOptions
		expectedResult bool
	}{
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					EnableNfsV3: pointer.Bool(true),
				},
			},
			accountOptions: &AccountOptions{},
			expectedResult: false,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				EnableNfsV3: pointer.Bool(false),
			},
			expectedResult: true,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					EnableNfsV3: pointer.Bool(true),
				},
			},
			accountOptions: &AccountOptions{
				EnableNfsV3: pointer.Bool(true),
			},
			expectedResult: true,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				EnableNfsV3: pointer.Bool(true),
			},
			expectedResult: false,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					EnableNfsV3: pointer.Bool(true),
				},
			},
			accountOptions: &AccountOptions{
				EnableNfsV3: pointer.Bool(false),
			},
			expectedResult: false,
		},
	}

	for _, test := range tests {
		result := isEnableNfsV3PropertyEqual(test.account, test.accountOptions)
		assert.Equal(t, test.expectedResult, result)
	}
}

func TestIsEnableHTTPSTrafficOnly(t *testing.T) {
	tests := []struct {
		account        storage.Account
		accountOptions *AccountOptions
		expectedResult bool
	}{
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					EnableHTTPSTrafficOnly: pointer.Bool(true),
				},
			},
			accountOptions: &AccountOptions{},
			expectedResult: false,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				EnableHTTPSTrafficOnly: false,
			},
			expectedResult: false,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					EnableHTTPSTrafficOnly: pointer.Bool(true),
				},
			},
			accountOptions: &AccountOptions{
				EnableHTTPSTrafficOnly: true,
			},
			expectedResult: true,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				EnableHTTPSTrafficOnly: true,
			},
			expectedResult: true,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					EnableHTTPSTrafficOnly: pointer.Bool(true),
				},
			},
			accountOptions: &AccountOptions{
				EnableHTTPSTrafficOnly: false,
			},
			expectedResult: false,
		},
	}

	for _, test := range tests {
		result := isEnableHTTPSTrafficOnlyEqual(test.account, test.accountOptions)
		assert.Equal(t, test.expectedResult, result)
	}
}

func TestIsAllowBlobPublicAccessEqual(t *testing.T) {
	tests := []struct {
		account        storage.Account
		accountOptions *AccountOptions
		expectedResult bool
	}{
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					AllowBlobPublicAccess: pointer.Bool(true),
				},
			},
			accountOptions: &AccountOptions{},
			expectedResult: true,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				AllowBlobPublicAccess: pointer.Bool(false),
			},
			expectedResult: false,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					AllowBlobPublicAccess: pointer.Bool(true),
				},
			},
			accountOptions: &AccountOptions{
				AllowBlobPublicAccess: pointer.Bool(true),
			},
			expectedResult: true,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				AllowBlobPublicAccess: pointer.Bool(true),
			},
			expectedResult: true,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					AllowBlobPublicAccess: pointer.Bool(true),
				},
			},
			accountOptions: &AccountOptions{
				AllowBlobPublicAccess: pointer.Bool(false),
			},
			expectedResult: false,
		},
	}

	for _, test := range tests {
		result := isAllowBlobPublicAccessEqual(test.account, test.accountOptions)
		assert.Equal(t, test.expectedResult, result)
	}
}

func TestIsAllowSharedKeyAccessEqual(t *testing.T) {
	tests := []struct {
		account        storage.Account
		accountOptions *AccountOptions
		expectedResult bool
	}{
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					AllowSharedKeyAccess: pointer.Bool(true),
				},
			},
			accountOptions: &AccountOptions{},
			expectedResult: true,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				AllowSharedKeyAccess: pointer.Bool(false),
			},
			expectedResult: false,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					AllowSharedKeyAccess: pointer.Bool(true),
				},
			},
			accountOptions: &AccountOptions{
				AllowSharedKeyAccess: pointer.Bool(true),
			},
			expectedResult: true,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				AllowSharedKeyAccess: pointer.Bool(true),
			},
			expectedResult: true,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					AllowSharedKeyAccess: pointer.Bool(true),
				},
			},
			accountOptions: &AccountOptions{
				AllowSharedKeyAccess: pointer.Bool(false),
			},
			expectedResult: false,
		},
	}

	for _, test := range tests {
		result := isAllowSharedKeyAccessEqual(test.account, test.accountOptions)
		assert.Equal(t, test.expectedResult, result)
	}
}

func TestIsRequireInfrastructureEncryptionEqual(t *testing.T) {
	tests := []struct {
		account        storage.Account
		accountOptions *AccountOptions
		expectedResult bool
	}{
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					Encryption: &storage.Encryption{
						RequireInfrastructureEncryption: pointer.Bool(true),
					},
				},
			},
			accountOptions: &AccountOptions{},
			expectedResult: false,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					Encryption: &storage.Encryption{
						RequireInfrastructureEncryption: pointer.Bool(true),
					},
				},
			},
			accountOptions: &AccountOptions{
				RequireInfrastructureEncryption: pointer.Bool(true),
			},
			expectedResult: true,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					Encryption: &storage.Encryption{
						RequireInfrastructureEncryption: pointer.Bool(false),
					},
				},
			},
			accountOptions: &AccountOptions{
				RequireInfrastructureEncryption: pointer.Bool(false),
			},
			expectedResult: true,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				RequireInfrastructureEncryption: pointer.Bool(false),
			},
			expectedResult: true,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					Encryption: &storage.Encryption{
						RequireInfrastructureEncryption: pointer.Bool(true),
					},
				},
			},
			accountOptions: &AccountOptions{
				RequireInfrastructureEncryption: pointer.Bool(false),
			},
			expectedResult: false,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					Encryption: &storage.Encryption{
						RequireInfrastructureEncryption: pointer.Bool(false),
					},
				},
			},
			accountOptions: &AccountOptions{
				RequireInfrastructureEncryption: pointer.Bool(true),
			},
			expectedResult: false,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				RequireInfrastructureEncryption: pointer.Bool(true),
			},
			expectedResult: false,
		},
	}

	for _, test := range tests {
		result := isRequireInfrastructureEncryptionEqual(test.account, test.accountOptions)
		assert.Equal(t, test.expectedResult, result)
	}
}

func TestIsLargeFileSharesPropertyEqual(t *testing.T) {
	tests := []struct {
		account        storage.Account
		accountOptions *AccountOptions
		expectedResult bool
	}{
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					LargeFileSharesState: storage.LargeFileSharesStateEnabled,
				},
			},
			accountOptions: &AccountOptions{},
			expectedResult: true,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				EnableLargeFileShare: pointer.Bool(false),
			},
			expectedResult: true,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					LargeFileSharesState: storage.LargeFileSharesStateEnabled,
				},
			},
			accountOptions: &AccountOptions{
				EnableLargeFileShare: pointer.Bool(true),
			},
			expectedResult: true,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				EnableLargeFileShare: pointer.Bool(true),
			},
			expectedResult: false,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					LargeFileSharesState: storage.LargeFileSharesStateEnabled,
				},
			},
			accountOptions: &AccountOptions{
				EnableLargeFileShare: pointer.Bool(false),
			},
			expectedResult: false,
		},
	}

	for _, test := range tests {
		result := isLargeFileSharesPropertyEqual(test.account, test.accountOptions)
		assert.Equal(t, test.expectedResult, result)
	}
}

func TestIsAccessTierEqual(t *testing.T) {
	tests := []struct {
		account        storage.Account
		accountOptions *AccountOptions
		expectedResult bool
	}{
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					AccessTier: storage.AccessTierCool,
				},
			},
			accountOptions: &AccountOptions{},
			expectedResult: true,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					AccessTier: storage.AccessTierHot,
				},
			},
			accountOptions: &AccountOptions{
				AccessTier: "Hot",
			},
			expectedResult: true,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					AccessTier: storage.AccessTierPremium,
				},
			},
			accountOptions: &AccountOptions{
				AccessTier: "Premium",
			},
			expectedResult: true,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				AccessTier: "Hot",
			},
			expectedResult: false,
		},
		{
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{
					AccessTier: storage.AccessTierPremium,
				},
			},
			accountOptions: &AccountOptions{
				AccessTier: "Hot",
			},
			expectedResult: false,
		},
	}

	for _, test := range tests {
		result := isAccessTierEqual(test.account, test.accountOptions)
		assert.Equal(t, test.expectedResult, result)
	}
}

func TestIsMultichannelEnabledEqual(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	accountName := "account2"

	cloud := GetTestCloud(ctrl)

	multichannelEnabled := storage.FileServiceProperties{
		FileServicePropertiesProperties: &storage.FileServicePropertiesProperties{
			ProtocolSettings: &storage.ProtocolSettings{
				Smb: &storage.SmbSetting{Multichannel: &storage.Multichannel{Enabled: pointer.Bool(true)}},
			},
		},
	}

	multichannelDisabled := storage.FileServiceProperties{
		FileServicePropertiesProperties: &storage.FileServicePropertiesProperties{
			ProtocolSettings: &storage.ProtocolSettings{
				Smb: &storage.SmbSetting{Multichannel: &storage.Multichannel{Enabled: pointer.Bool(false)}},
			},
		},
	}

	incompleteServiceProperties := storage.FileServiceProperties{
		FileServicePropertiesProperties: &storage.FileServicePropertiesProperties{
			ProtocolSettings: &storage.ProtocolSettings{},
		},
	}

	mockFileClient := mockfileclient.NewMockInterface(ctrl)
	cloud.FileClient = mockFileClient
	mockFileClient.EXPECT().WithSubscriptionID(gomock.Any()).Return(mockFileClient).AnyTimes()

	tests := []struct {
		desc                      string
		account                   storage.Account
		accountOptions            *AccountOptions
		serviceProperties         *storage.FileServiceProperties
		servicePropertiesRetError error
		expectedResult            bool
	}{
		{
			desc: "IsMultichannelEnabled is nil",
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{},
			expectedResult: true,
		},
		{
			desc: "account.Name is nil",
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				IsMultichannelEnabled: pointer.Bool(false),
			},
			expectedResult: false,
		},
		{
			desc: "IsMultichannelEnabled not equal",
			account: storage.Account{
				Name:              &accountName,
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				IsMultichannelEnabled: pointer.Bool(false),
			},
			serviceProperties: &multichannelEnabled,
			expectedResult:    false,
		},
		{
			desc: "GetServiceProperties return error",
			account: storage.Account{
				Name:              &accountName,
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				IsMultichannelEnabled: pointer.Bool(false),
			},
			serviceProperties:         &multichannelEnabled,
			servicePropertiesRetError: fmt.Errorf("GetServiceProperties return error"),
			expectedResult:            false,
		},
		{
			desc: "IsMultichannelEnabled not equal",
			account: storage.Account{
				Name:              &accountName,
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				IsMultichannelEnabled: pointer.Bool(true),
			},
			serviceProperties: &multichannelDisabled,
			expectedResult:    false,
		},
		{
			desc: "IsMultichannelEnabled is equal",
			account: storage.Account{
				Name:              &accountName,
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				IsMultichannelEnabled: pointer.Bool(true),
			},
			serviceProperties: &multichannelEnabled,
			expectedResult:    true,
		},
		{
			desc: "IsMultichannelEnabled is equal",
			account: storage.Account{
				Name:              &accountName,
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				IsMultichannelEnabled: pointer.Bool(false),
			},
			serviceProperties: &multichannelDisabled,
			expectedResult:    true,
		},
		{
			desc: "incompleteServiceProperties should be regarded as IsMultichannelDisabled",
			account: storage.Account{
				Name:              &accountName,
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				IsMultichannelEnabled: pointer.Bool(false),
			},
			serviceProperties: &incompleteServiceProperties,
			expectedResult:    true,
		},
	}

	for _, test := range tests {
		if test.serviceProperties != nil {
			mockFileClient.EXPECT().GetServiceProperties(gomock.Any(), gomock.Any(), gomock.Any()).Return(*test.serviceProperties, test.servicePropertiesRetError).Times(1)
		}

		result := cloud.isMultichannelEnabledEqual(ctx, test.account, test.accountOptions)
		assert.Equal(t, test.expectedResult, result, test.desc)
	}
}

func TestIsDisableFileServiceDeleteRetentionPolicyEqual(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	accountName := "account"
	cloud := GetTestCloud(ctrl)

	deleteRetentionPolicyEnabled := storage.FileServiceProperties{
		FileServicePropertiesProperties: &storage.FileServicePropertiesProperties{
			ShareDeleteRetentionPolicy: &storage.DeleteRetentionPolicy{
				Enabled: pointer.Bool(true),
			},
		},
	}

	deleteRetentionPolicyDisabled := storage.FileServiceProperties{
		FileServicePropertiesProperties: &storage.FileServicePropertiesProperties{
			ShareDeleteRetentionPolicy: &storage.DeleteRetentionPolicy{
				Enabled: pointer.Bool(false),
			},
		},
	}

	incompleteServiceProperties := storage.FileServiceProperties{
		FileServicePropertiesProperties: &storage.FileServicePropertiesProperties{},
	}

	mockFileClient := mockfileclient.NewMockInterface(ctrl)
	cloud.FileClient = mockFileClient
	mockFileClient.EXPECT().WithSubscriptionID(gomock.Any()).Return(mockFileClient).AnyTimes()

	tests := []struct {
		desc                      string
		account                   storage.Account
		accountOptions            *AccountOptions
		serviceProperties         *storage.FileServiceProperties
		servicePropertiesRetError error
		expectedResult            bool
	}{
		{
			desc: "DisableFileServiceDeleteRetentionPolicy is nil",
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{},
			expectedResult: true,
		},
		{
			desc: "account.Name is nil",
			account: storage.Account{
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				DisableFileServiceDeleteRetentionPolicy: pointer.Bool(false),
			},
			expectedResult: false,
		},
		{
			desc: "DisableFileServiceDeleteRetentionPolicy not equal",
			account: storage.Account{
				Name:              &accountName,
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				DisableFileServiceDeleteRetentionPolicy: pointer.Bool(true),
			},
			serviceProperties: &deleteRetentionPolicyEnabled,
			expectedResult:    false,
		},
		{
			desc: "GetServiceProperties return error",
			account: storage.Account{
				Name:              &accountName,
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				DisableFileServiceDeleteRetentionPolicy: pointer.Bool(false),
			},
			serviceProperties:         &deleteRetentionPolicyEnabled,
			servicePropertiesRetError: fmt.Errorf("GetServiceProperties return error"),
			expectedResult:            false,
		},
		{
			desc: "DisableFileServiceDeleteRetentionPolicy not equal",
			account: storage.Account{
				Name:              &accountName,
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				DisableFileServiceDeleteRetentionPolicy: pointer.Bool(false),
			},
			serviceProperties: &deleteRetentionPolicyDisabled,
			expectedResult:    false,
		},
		{
			desc: "DisableFileServiceDeleteRetentionPolicy is equal",
			account: storage.Account{
				Name:              &accountName,
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				DisableFileServiceDeleteRetentionPolicy: pointer.Bool(true),
			},
			serviceProperties: &deleteRetentionPolicyDisabled,
			expectedResult:    true,
		},
		{
			desc: "DisableFileServiceDeleteRetentionPolicy is equal",
			account: storage.Account{
				Name:              &accountName,
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				DisableFileServiceDeleteRetentionPolicy: pointer.Bool(false),
			},
			serviceProperties: &deleteRetentionPolicyEnabled,
			expectedResult:    true,
		},
		{
			desc: "incompleteServiceProperties should be regarded as not DisableFileServiceDeleteRetentionPolicy",
			account: storage.Account{
				Name:              &accountName,
				AccountProperties: &storage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				DisableFileServiceDeleteRetentionPolicy: pointer.Bool(false),
			},
			serviceProperties: &incompleteServiceProperties,
			expectedResult:    true,
		},
	}

	for _, test := range tests {
		if test.serviceProperties != nil {
			mockFileClient.EXPECT().GetServiceProperties(gomock.Any(), gomock.Any(), gomock.Any()).Return(*test.serviceProperties, test.servicePropertiesRetError).Times(1)
		}

		result := cloud.isDisableFileServiceDeleteRetentionPolicyEqual(ctx, test.account, test.accountOptions)
		assert.Equal(t, test.expectedResult, result, test.desc)
	}
}

func Test_isSoftDeleteBlobsEqual(t *testing.T) {
	type args struct {
		property       storage.BlobServiceProperties
		accountOptions *AccountOptions
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "not equal for property nil",
			args: args{
				property: storage.BlobServiceProperties{
					BlobServicePropertiesProperties: &storage.BlobServicePropertiesProperties{},
				},
				accountOptions: &AccountOptions{
					SoftDeleteBlobs: 7,
				},
			},
			want: false,
		},
		{
			name: "not equal for property not enable",
			args: args{
				property: storage.BlobServiceProperties{
					BlobServicePropertiesProperties: &storage.BlobServicePropertiesProperties{
						DeleteRetentionPolicy: &storage.DeleteRetentionPolicy{
							Enabled: pointer.Bool(false),
						},
					},
				},
				accountOptions: &AccountOptions{
					SoftDeleteBlobs: 7,
				},
			},
			want: false,
		},
		{
			name: "not equal for accountOptions nil",
			args: args{
				property: storage.BlobServiceProperties{
					BlobServicePropertiesProperties: &storage.BlobServicePropertiesProperties{
						DeleteRetentionPolicy: &storage.DeleteRetentionPolicy{
							Enabled: pointer.Bool(true),
							Days:    pointer.Int32(7),
						},
					},
				},
				accountOptions: &AccountOptions{},
			},
			want: false,
		},
		{
			name: "qual",
			args: args{
				property: storage.BlobServiceProperties{
					BlobServicePropertiesProperties: &storage.BlobServicePropertiesProperties{
						DeleteRetentionPolicy: &storage.DeleteRetentionPolicy{
							Enabled: pointer.Bool(true),
							Days:    pointer.Int32(7),
						},
					},
				},
				accountOptions: &AccountOptions{
					SoftDeleteBlobs: 7,
				},
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSoftDeleteBlobsEqual(tt.args.property, tt.args.accountOptions); got != tt.want {
				t.Errorf("isSoftDeleteBlobsEqual() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_isSoftDeleteContainersEqual(t *testing.T) {
	type args struct {
		property       storage.BlobServiceProperties
		accountOptions *AccountOptions
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "not equal for property nil",
			args: args{
				property: storage.BlobServiceProperties{
					BlobServicePropertiesProperties: &storage.BlobServicePropertiesProperties{},
				},
				accountOptions: &AccountOptions{
					SoftDeleteContainers: 7,
				},
			},
			want: false,
		},
		{
			name: "not equal for property not enable",
			args: args{
				property: storage.BlobServiceProperties{
					BlobServicePropertiesProperties: &storage.BlobServicePropertiesProperties{
						ContainerDeleteRetentionPolicy: &storage.DeleteRetentionPolicy{
							Enabled: pointer.Bool(false),
						},
					},
				},
				accountOptions: &AccountOptions{
					SoftDeleteContainers: 7,
				},
			},
			want: false,
		},
		{
			name: "not equal for accountOptions nil",
			args: args{
				property: storage.BlobServiceProperties{
					BlobServicePropertiesProperties: &storage.BlobServicePropertiesProperties{
						ContainerDeleteRetentionPolicy: &storage.DeleteRetentionPolicy{
							Enabled: pointer.Bool(true),
							Days:    pointer.Int32(7),
						},
					},
				},
				accountOptions: &AccountOptions{},
			},
			want: false,
		},
		{
			name: "qual",
			args: args{
				property: storage.BlobServiceProperties{
					BlobServicePropertiesProperties: &storage.BlobServicePropertiesProperties{
						ContainerDeleteRetentionPolicy: &storage.DeleteRetentionPolicy{
							Enabled: pointer.Bool(true),
							Days:    pointer.Int32(7),
						},
					},
				},
				accountOptions: &AccountOptions{
					SoftDeleteContainers: 7,
				},
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSoftDeleteContainersEqual(tt.args.property, tt.args.accountOptions); got != tt.want {
				t.Errorf("isSoftDeleteContainersEqual() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_isEnableBlobVersioningEqual(t *testing.T) {
	type args struct {
		property       storage.BlobServiceProperties
		accountOptions *AccountOptions
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "equal",
			args: args{
				property: storage.BlobServiceProperties{
					BlobServicePropertiesProperties: &storage.BlobServicePropertiesProperties{},
				},
				accountOptions: &AccountOptions{
					EnableBlobVersioning: pointer.Bool(false),
				},
			},
			want: true,
		},
		{
			name: "not equal",
			args: args{
				property: storage.BlobServiceProperties{
					BlobServicePropertiesProperties: &storage.BlobServicePropertiesProperties{
						IsVersioningEnabled: pointer.Bool(true),
					},
				},
				accountOptions: &AccountOptions{
					EnableBlobVersioning: pointer.Bool(false),
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isEnableBlobVersioningEqual(tt.args.property, tt.args.accountOptions); got != tt.want {
				t.Errorf("isEnableBlobVersioningEqual() = %v, want %v", got, tt.want)
			}
		})
	}
}
