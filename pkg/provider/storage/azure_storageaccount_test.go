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

package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	privatedns "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/privatedns/armprivatedns"
	armstorage "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storage/armstorage/v2"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/accountclient/mock_accountclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/blobservicepropertiesclient/mock_blobservicepropertiesclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/privatednszonegroupclient/mock_privatednszonegroupclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/privateendpointclient/mock_privateendpointclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/privatezoneclient/mock_privatezoneclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualnetworklinkclient/mock_virtualnetworklinkclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	azureconfig "sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/storage/fileservice/mock_fileservice"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/subnet"
	"sigs.k8s.io/cloud-provider-azure/pkg/util/lockmap"
)

const TestLocation = "testLocation"

func TestGetStorageAccessKeys(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storageAccountRepo := &AccountRepo{
		ComputeClientFactory: mock_azclient.NewMockClientFactory(ctrl),
	}
	value := "foo bar"
	oldTime := time.Now().Add(-time.Hour)
	newValue := "newkey"
	newTime := time.Now()

	tests := []struct {
		results             *armstorage.AccountListKeysResult
		getLatestAccountKey bool
		expectedKey         string
		expectErr           bool
		err                 error
	}{
		{&armstorage.AccountListKeysResult{}, false, "", true, nil},
		{
			&armstorage.AccountListKeysResult{
				Keys: []*armstorage.AccountKey{
					{Value: &value},
				},
			},
			false,
			"bar",
			false,
			nil,
		},
		{
			&armstorage.AccountListKeysResult{
				Keys: []*armstorage.AccountKey{
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
			&armstorage.AccountListKeysResult{
				Keys: []*armstorage.AccountKey{
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
			&armstorage.AccountListKeysResult{
				Keys: []*armstorage.AccountKey{
					{Value: &value, CreationTime: &oldTime},
					{Value: &newValue, CreationTime: &newTime},
				},
			},
			false,
			"bar",
			false,
			nil,
		},
		{&armstorage.AccountListKeysResult{}, false, "", true, fmt.Errorf("test error")},
	}

	for _, test := range tests {
		mockStorageAccountsClient := mock_accountclient.NewMockInterface(ctrl)
		mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), "rg", gomock.Any()).Return(test.results.Keys, nil).AnyTimes()
		key, err := storageAccountRepo.GetStorageAccesskey(ctx, mockStorageAccountsClient, "acct", "rg", test.getLatestAccountKey)
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storageAccountRepo := &AccountRepo{
		ComputeClientFactory: mock_azclient.NewMockClientFactory(ctrl),
	}
	name := "testAccount"
	location := TestLocation
	networkID := "networkID"
	accountProperties := armstorage.AccountProperties{
		NetworkRuleSet: &armstorage.NetworkRuleSet{
			VirtualNetworkRules: []*armstorage.VirtualNetworkRule{
				{
					VirtualNetworkResourceID: &networkID,
					Action:                   to.Ptr(string(armstorage.DefaultActionAllow)),
					State:                    to.Ptr(armstorage.State("state")),
				},
			},
		}}

	account := &armstorage.Account{
		SKU: &armstorage.SKU{
			Name: to.Ptr(armstorage.SKUName("testSku")),
			Tier: to.Ptr(armstorage.SKUTier("testSkuTier")),
		},
		Kind:       to.Ptr(armstorage.Kind("testKind")),
		Location:   &location,
		Name:       &name,
		Properties: &accountProperties,
	}

	testResourceGroups := []*armstorage.Account{account}

	accountOptions := &AccountOptions{
		ResourceGroup:             "rg",
		VirtualNetworkResourceIDs: []string{networkID},
		EnableHTTPSTrafficOnly:    true,
	}

	mockStorageAccountsClient := mock_accountclient.NewMockInterface(ctrl)
	mockStorageAccountsClient.EXPECT().List(gomock.Any(), "rg").Return(testResourceGroups, nil).Times(1)

	accountsWithLocations, err := storageAccountRepo.getStorageAccounts(ctx, mockStorageAccountsClient, accountOptions)
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storageAccountRepo := &AccountRepo{
		ComputeClientFactory: mock_azclient.NewMockClientFactory(ctrl),
	}
	// default account with name, location, sku, kind
	name := "testAccount"
	location := TestLocation
	sku := &armstorage.SKU{
		Name: to.Ptr(armstorage.SKUName("testSku")),
		Tier: to.Ptr(armstorage.SKUTier("testSkuTier")),
	}
	account := &armstorage.Account{
		SKU:      sku,
		Kind:     to.Ptr(armstorage.Kind("testKind")),
		Location: &location,
		Name:     &name,
	}

	accountPropertiesWithoutNetworkRuleSet := armstorage.AccountProperties{NetworkRuleSet: nil}
	accountPropertiesWithoutVirtualNetworkRules := armstorage.AccountProperties{
		NetworkRuleSet: &armstorage.NetworkRuleSet{
			VirtualNetworkRules: nil,
		}}

	tests := []struct {
		testCase           string
		testAccountOptions *AccountOptions
		testResourceGroups []*armstorage.Account
		expectedResult     []accountWithLocation
		expectedError      error
	}{
		{
			testCase: "account name is nil",
			testAccountOptions: &AccountOptions{
				ResourceGroup: "rg",
			},
			testResourceGroups: []*armstorage.Account{},
			expectedResult:     []accountWithLocation{},
			expectedError:      nil,
		},
		{
			testCase: "account location is nil",
			testAccountOptions: &AccountOptions{
				ResourceGroup: "rg",
			},
			testResourceGroups: []*armstorage.Account{{Name: &name}},
			expectedResult:     []accountWithLocation{},
			expectedError:      nil,
		},
		{
			testCase: "account sku is nil",
			testAccountOptions: &AccountOptions{
				ResourceGroup: "rg",
			},
			testResourceGroups: []*armstorage.Account{{Name: &name, Location: &location}},
			expectedResult:     []accountWithLocation{},
			expectedError:      nil,
		},
		{
			testCase: "account options type is not empty and not equal account storage type",
			testAccountOptions: &AccountOptions{
				ResourceGroup: "rg",
				Type:          "testAccountOptionsType",
			},
			testResourceGroups: []*armstorage.Account{account},
			expectedResult:     []accountWithLocation{},
			expectedError:      nil,
		},
		{
			testCase: "account options kind is not empty and not equal account type",
			testAccountOptions: &AccountOptions{
				ResourceGroup: "rg",
				Kind:          "testAccountOptionsKind",
			},
			testResourceGroups: []*armstorage.Account{account},
			expectedResult:     []accountWithLocation{},
			expectedError:      nil,
		},
		{
			testCase: "account options location is not empty and not equal account location",
			testAccountOptions: &AccountOptions{
				ResourceGroup: "rg",
				Location:      "testAccountOptionsLocation",
			},
			testResourceGroups: []*armstorage.Account{account},
			expectedResult:     []accountWithLocation{},
			expectedError:      nil,
		},
		{
			testCase: "account options account properties are nil",
			testAccountOptions: &AccountOptions{
				ResourceGroup:             "rg",
				VirtualNetworkResourceIDs: []string{"id"},
			},
			testResourceGroups: []*armstorage.Account{},
			expectedResult:     []accountWithLocation{},
			expectedError:      nil,
		},
		{
			testCase: "account options account properties network rule set is nil",
			testAccountOptions: &AccountOptions{
				ResourceGroup:             "rg",
				VirtualNetworkResourceIDs: []string{"id"},
			},
			testResourceGroups: []*armstorage.Account{{Name: &name, Kind: to.Ptr(armstorage.Kind("kind")), Location: &location, SKU: sku, Properties: &accountPropertiesWithoutNetworkRuleSet}},
			expectedResult:     []accountWithLocation{},
			expectedError:      nil,
		},
		{
			testCase: "account options account properties virtual network rule is nil",
			testAccountOptions: &AccountOptions{
				ResourceGroup:             "rg",
				VirtualNetworkResourceIDs: []string{"id"},
			},
			testResourceGroups: []*armstorage.Account{{Name: &name, Kind: to.Ptr(armstorage.Kind("kind")), Location: &location, SKU: sku, Properties: &accountPropertiesWithoutVirtualNetworkRules}},
			expectedResult:     []accountWithLocation{},
			expectedError:      nil,
		},
		{
			testCase: "account options CreatePrivateEndpoint is true and no private endpoint exists",
			testAccountOptions: &AccountOptions{
				ResourceGroup:         "rg",
				CreatePrivateEndpoint: ptr.To(true),
			},
			testResourceGroups: []*armstorage.Account{{Name: &name, Kind: to.Ptr(armstorage.Kind("kind")), Location: &location, SKU: sku, Properties: &armstorage.AccountProperties{}}},
			expectedResult:     []accountWithLocation{},
			expectedError:      nil,
		},
	}

	for _, test := range tests {
		t.Logf("running test case: %s", test.testCase)
		mockStorageAccountsClient := mock_accountclient.NewMockInterface(ctrl)
		mockStorageAccountsClient.EXPECT().List(gomock.Any(), "rg").Return(test.testResourceGroups, nil).AnyTimes()
		accountsWithLocations, err := storageAccountRepo.getStorageAccounts(ctx, mockStorageAccountsClient, test.testAccountOptions)
		if !errors.Is(err, test.expectedError) {
			t.Errorf("unexpected error: %v", err)
		}

		if len(accountsWithLocations) != len(test.expectedResult) {
			t.Error("unexpected error as returned accounts slice is not empty")
		}
	}
}

func TestEnsureStorageAccount(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resourceGroup := "ResourceGroup"
	vnetName := "VnetName"
	vnetResourceGroup := "VnetResourceGroup"
	subnetName := "SubnetName"
	location := TestLocation

	config := azureconfig.Config{
		AzureClientConfig: azureconfig.AzureClientConfig{
			SubscriptionID:                "testSub",
			NetworkResourceSubscriptionID: "testNetSub",
		},
		ResourceGroup:     resourceGroup,
		VnetResourceGroup: vnetResourceGroup,
		SubnetName:        subnetName,
		VnetName:          vnetName,
		Location:          location,
	}

	sku := &armstorage.SKU{
		Name: to.Ptr(armstorage.SKUName("testSku")),
		Tier: to.Ptr(armstorage.SKUTier("testSkuTier")),
	}
	testStorageAccounts :=
		[]*armstorage.Account{
			{Name: ptr.To("testStorageAccount"), Kind: to.Ptr(armstorage.Kind("kind")), Location: &location, SKU: sku, Properties: &armstorage.AccountProperties{NetworkRuleSet: &armstorage.NetworkRuleSet{}}},
			{Name: ptr.To("wantedAccount"), Kind: to.Ptr(armstorage.Kind("kind")), Location: &location, SKU: sku, Properties: &armstorage.AccountProperties{NetworkRuleSet: &armstorage.NetworkRuleSet{}}},
			{Name: ptr.To("otherAccount"), Kind: to.Ptr(armstorage.Kind("kind")), Location: &location, SKU: sku, Properties: &armstorage.AccountProperties{NetworkRuleSet: &armstorage.NetworkRuleSet{}}},
		}

	value := "foo bar"
	storageAccountListKeys := []*armstorage.AccountKey{
		{Value: &value},
	}

	tests := []struct {
		name                            string
		createAccount                   bool
		createPrivateEndpoint           *bool
		vNetLinkName                    string
		publicNetworkAccess             string
		SubnetPropertiesFormatNil       bool
		mockStorageAccountsClient       bool
		setAccountOptions               bool
		pickRandomMatchingAccount       bool
		accessTier                      string
		storageType                     Type
		requireInfrastructureEncryption *bool
		isSmbOAuthEnabled               *bool
		keyVaultURL                     *string
		sourceAccountName               string
		accountName                     string
		subscriptionID                  string
		resourceGroup                   string
		expectedErr                     string
		expectedAccountName             string
	}{
		{
			name:                            "[Success] EnsureStorageAccount with createPrivateEndpoint and storagetype blob",
			createAccount:                   true,
			createPrivateEndpoint:           ptr.To(true),
			mockStorageAccountsClient:       true,
			setAccountOptions:               true,
			pickRandomMatchingAccount:       true,
			storageType:                     StorageTypeBlob,
			requireInfrastructureEncryption: ptr.To(true),
			keyVaultURL:                     ptr.To("keyVaultURL"),
			isSmbOAuthEnabled:               ptr.To(true),
			resourceGroup:                   "rg",
			accessTier:                      "AccessTierHot",
			accountName:                     "",
			expectedErr:                     "",
		},
		{
			name:                            "[Success] EnsureStorageAccount with createPrivateEndpoint",
			createAccount:                   true,
			createPrivateEndpoint:           ptr.To(true),
			vNetLinkName:                    "vnetLinkName",
			publicNetworkAccess:             string(armstorage.PublicNetworkAccessDisabled),
			mockStorageAccountsClient:       true,
			setAccountOptions:               true,
			requireInfrastructureEncryption: ptr.To(true),
			keyVaultURL:                     ptr.To("keyVaultURL"),
			resourceGroup:                   "rg",
			accessTier:                      "AccessTierHot",
			accountName:                     "",
			expectedErr:                     "",
		},
		{
			name:                            "[Success] EnsureStorageAccount returns with source account",
			mockStorageAccountsClient:       true,
			setAccountOptions:               true,
			requireInfrastructureEncryption: ptr.To(true),
			resourceGroup:                   "rg",
			sourceAccountName:               "wantedAccount",
			accountName:                     "wantedAccount",
			expectedErr:                     "",
		},
		{
			name:                      "[Failed] EnsureStorageAccount with createPrivateEndpoint: get storage key failed",
			createAccount:             true,
			createPrivateEndpoint:     ptr.To(true),
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
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			StorageAccountRepo := &AccountRepo{
				ComputeClientFactory: mock_azclient.NewMockClientFactory(ctrl),
				NetworkClientFactory: mock_azclient.NewMockClientFactory(ctrl),
				subnetRepo:           subnet.NewMockRepository(ctrl),
				Config:               config,
				Environment: &azclient.Environment{
					StorageEndpointSuffix: "storagesuffix",
				},
			}
			mockBlobClient := mock_blobservicepropertiesclient.NewMockInterface(ctrl)
			StorageAccountRepo.ComputeClientFactory.(*mock_azclient.MockClientFactory).EXPECT().GetBlobServicePropertiesClientForSub(gomock.Any()).Return(mockBlobClient, nil).AnyTimes()
			mockBlobClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.BlobServiceProperties{
				BlobServiceProperties: &armstorage.BlobServicePropertiesProperties{}}, nil).AnyTimes()
			mockBlobClient.EXPECT().Set(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.BlobServiceProperties{}, nil).AnyTimes()

			mockStorageAccountsClient := mock_accountclient.NewMockInterface(ctrl)

			if test.mockStorageAccountsClient {
				StorageAccountRepo.ComputeClientFactory.(*mock_azclient.MockClientFactory).EXPECT().GetAccountClientForSub(gomock.Any()).Return(mockStorageAccountsClient, nil).AnyTimes()
			}

			if ptr.Deref(test.createPrivateEndpoint, false) {
				mockStorageAccountsClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testStorageAccounts, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().Create(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().GetProperties(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(testStorageAccounts[0], nil).AnyTimes()
				if test.accountName == "" {
					mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), gomock.Any()).Return(storageAccountListKeys, nil).AnyTimes()
				} else {
					mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), gomock.Any()).Return(storageAccountListKeys, errors.New("")).AnyTimes()
				}

				subnetPropertiesFormat := &armnetwork.SubnetPropertiesFormat{}
				if test.SubnetPropertiesFormatNil {
					subnetPropertiesFormat = nil
				}
				mockedSubnet := &armnetwork.Subnet{Properties: subnetPropertiesFormat}

				subnetRepo := StorageAccountRepo.subnetRepo.(*subnet.MockRepository)
				subnetRepo.EXPECT().Get(gomock.Any(), vnetResourceGroup, vnetName, subnetName).Return(mockedSubnet, nil).Times(1)
				subnetRepo.EXPECT().CreateOrUpdate(gomock.Any(), vnetResourceGroup, vnetName, subnetName, gomock.Any()).Return(nil).Times(1)

				mockPrivateDNSClient := mock_privatezoneclient.NewMockInterface(ctrl)
				StorageAccountRepo.NetworkClientFactory.(*mock_azclient.MockClientFactory).EXPECT().GetPrivateZoneClient().Return(mockPrivateDNSClient).AnyTimes()
				mockPrivateDNSClient.EXPECT().Get(gomock.Any(), vnetResourceGroup, gomock.Any()).Return(&privatedns.PrivateZone{}, errors.New("ResourceNotFound")).Times(1)
				mockPrivateDNSClient.EXPECT().CreateOrUpdate(gomock.Any(), vnetResourceGroup, gomock.Any(), gomock.Any()).Return(nil, nil).Times(1)

				mockPrivateDNSZoneGroup := mock_privatednszonegroupclient.NewMockInterface(ctrl)
				StorageAccountRepo.NetworkClientFactory.(*mock_azclient.MockClientFactory).EXPECT().GetPrivateDNSZoneGroupClient().Return(mockPrivateDNSZoneGroup).AnyTimes()
				mockPrivateDNSZoneGroup.EXPECT().CreateOrUpdate(gomock.Any(), vnetResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).Times(1)
				mockPrivateEndpointClient := mock_privateendpointclient.NewMockInterface(ctrl)
				StorageAccountRepo.NetworkClientFactory.(*mock_azclient.MockClientFactory).EXPECT().GetPrivateEndpointClient().Return(mockPrivateEndpointClient).AnyTimes()
				mockPrivateEndpointClient.EXPECT().CreateOrUpdate(gomock.Any(), vnetResourceGroup, gomock.Any(), gomock.Any()).Return(nil, nil).Times(1)
				mockVirtualNetworkLinksClient := mock_virtualnetworklinkclient.NewMockInterface(ctrl)
				StorageAccountRepo.NetworkClientFactory.(*mock_azclient.MockClientFactory).EXPECT().GetVirtualNetworkLinkClient().Return(mockVirtualNetworkLinksClient).AnyTimes()
				mockVirtualNetworkLinksClient.EXPECT().Get(gomock.Any(), vnetResourceGroup, gomock.Any(), gomock.Any()).Return(&privatedns.VirtualNetworkLink{}, errors.New("ResourceNotFound")).Times(1)
				mockVirtualNetworkLinksClient.EXPECT().CreateOrUpdate(gomock.Any(), vnetResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).Times(1)
			}

			if test.sourceAccountName != "" {
				mockStorageAccountsClient.EXPECT().ListKeys(gomock.Any(), gomock.Any(), gomock.Any()).Return(storageAccountListKeys, nil).AnyTimes()
			}

			var testAccountOptions *AccountOptions
			if test.setAccountOptions {
				testAccountOptions = &AccountOptions{
					ResourceGroup:                   test.resourceGroup,
					CreatePrivateEndpoint:           test.createPrivateEndpoint,
					VNetLinkName:                    test.vNetLinkName,
					PublicNetworkAccess:             test.publicNetworkAccess,
					Name:                            test.accountName,
					CreateAccount:                   test.createAccount,
					SubscriptionID:                  test.subscriptionID,
					AccessTier:                      test.accessTier,
					StorageType:                     test.storageType,
					EnableBlobVersioning:            ptr.To(true),
					IsSmbOAuthEnabled:               test.isSmbOAuthEnabled,
					KeyVaultURI:                     test.keyVaultURL,
					RequireInfrastructureEncryption: test.requireInfrastructureEncryption,
					SoftDeleteBlobs:                 7,
					SoftDeleteContainers:            7,
					PickRandomMatchingAccount:       test.pickRandomMatchingAccount,
					SourceAccountName:               test.sourceAccountName,
				}
			}

			accountName, _, err := StorageAccountRepo.EnsureStorageAccount(ctx, testAccountOptions, "test")
			if test.expectedAccountName != "" {
				assert.Equal(t, accountName, test.expectedAccountName, test.name)
			}
			assert.Equal(t, err == nil, test.expectedErr == "", fmt.Sprintf("returned error: %v", err), test.name)
			if test.expectedErr != "" {
				assert.Equal(t, err != nil, strings.Contains(err.Error(), test.expectedErr), err.Error(), test.name)
			}
			ctrl.Finish()
		})
	}
}

func TestGetStorageAccountWithCache(t *testing.T) {
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
			expectedErr: "ComputeClientFactory is nil",
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
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			ctrl := gomock.NewController(t)
			storageAccountRepo := &AccountRepo{}
			if test.setStorageAccountClient {
				storageAccountRepo.ComputeClientFactory = mock_azclient.NewMockClientFactory(ctrl)
				mockStorageAccountsClient := mock_accountclient.NewMockInterface(ctrl)
				storageAccountRepo.ComputeClientFactory.(*mock_azclient.MockClientFactory).EXPECT().GetAccountClientForSub("").Return(mockStorageAccountsClient, nil).AnyTimes()
				mockStorageAccountsClient.EXPECT().GetProperties(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.Account{}, nil).AnyTimes()
			}

			if test.setStorageAccountCache {
				getter := func(_ context.Context, _ string) (*armstorage.Account, error) { return nil, nil }
				storageAccountRepo.storageAccountCache, _ = cache.NewTimedCache(time.Minute, getter, false)
			}

			_, err := storageAccountRepo.getStorageAccountWithCache(ctx, test.subsID, test.resourceGroup, test.account)
			assert.Equal(t, err == nil, test.expectedErr == "", fmt.Sprintf("returned error: %v", err), test.name)
			ctrl.Finish()
			cancel()
		})
	}
}

func TestAddStorageAccountTags(t *testing.T) {

	tests := []struct {
		name           string
		subsID         string
		resourceGroup  string
		account        string
		tags           map[string]*string
		parallelThread int
		expectedErr    error
	}{
		{
			name:        "no tags update",
			account:     "account",
			expectedErr: nil,
		},
		{
			name:        "tags update",
			account:     "account",
			tags:        map[string]*string{"key": ptr.To("value")},
			expectedErr: nil,
		},
		{
			name:           "tags update in parallel",
			account:        "account",
			tags:           map[string]*string{"key": ptr.To("value")},
			parallelThread: 10,
			expectedErr:    nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)

			ctx, cancel := context.WithCancel(context.Background())

			storageAccountRepo := &AccountRepo{
				ComputeClientFactory: mock_azclient.NewMockClientFactory(ctrl),
				lockMap:              lockmap.NewLockMap(),
			}
			getter := func(_ context.Context, _ string) (*armstorage.Account, error) { return nil, nil }
			storageAccountRepo.storageAccountCache, _ = cache.NewTimedCache(time.Minute, getter, false)
			mockStorageAccountsClient := mock_accountclient.NewMockInterface(ctrl)

			storageAccountRepo.ComputeClientFactory.(*mock_azclient.MockClientFactory).EXPECT().GetAccountClientForSub("").Return(mockStorageAccountsClient, nil).AnyTimes()

			mockStorageAccountsClient.EXPECT().GetProperties(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.Account{}, nil).AnyTimes()

			parallelThread := 1
			if test.parallelThread > 1 {
				parallelThread = test.parallelThread
			}
			if len(test.tags) > 0 {
				mockStorageAccountsClient.EXPECT().Update(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).Times(parallelThread)
			}

			if parallelThread > 1 {
				var wg sync.WaitGroup
				wg.Add(parallelThread)
				for i := 0; i < parallelThread; i++ {
					go func() {
						defer wg.Done()
						err := storageAccountRepo.AddStorageAccountTags(ctx, test.subsID, test.resourceGroup, test.account, test.tags)
						assert.Equal(t, err, test.expectedErr, fmt.Sprintf("returned error: %v", err), test.name)
					}()
				}
				wg.Wait()
			} else {
				err := storageAccountRepo.AddStorageAccountTags(ctx, test.subsID, test.resourceGroup, test.account, test.tags)
				assert.Equal(t, err, test.expectedErr, fmt.Sprintf("returned error: %v", err), test.name)
			}
			cancel()
			ctrl.Finish()
		})
	}
}

func TestRemoveStorageAccountTags(t *testing.T) {

	tests := []struct {
		name           string
		subsID         string
		resourceGroup  string
		account        string
		key            string
		parallelThread int
		expectedErr    error
	}{
		{
			name:        "no tag removal",
			account:     "account",
			expectedErr: nil,
		},
		{
			name:        "one tag removal",
			account:     "account",
			key:         "key",
			expectedErr: nil,
		},
		{
			name:           "one tag removal in parallel",
			account:        "account",
			key:            "key",
			parallelThread: 10,
			expectedErr:    nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			ctx, cancel := context.WithCancel(context.Background())

			storageAccountRepo := &AccountRepo{
				ComputeClientFactory: mock_azclient.NewMockClientFactory(ctrl),
			}
			getter := func(_ context.Context, _ string) (*armstorage.Account, error) { return nil, nil }
			storageAccountRepo.storageAccountCache, _ = cache.NewTimedCache(time.Minute, getter, false)
			storageAccountRepo.lockMap = lockmap.NewLockMap()
			mockStorageAccountsClient := mock_accountclient.NewMockInterface(ctrl)

			storageAccountRepo.ComputeClientFactory.(*mock_azclient.MockClientFactory).EXPECT().GetAccountClientForSub("").Return(mockStorageAccountsClient, nil).AnyTimes()

			mockStorageAccountsClient.EXPECT().GetProperties(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armstorage.Account{Tags: map[string]*string{"key": ptr.To("value")}}, nil).AnyTimes()

			parallelThread := 1
			if test.parallelThread > 1 {
				parallelThread = test.parallelThread
			}
			if len(test.key) > 0 {
				mockStorageAccountsClient.EXPECT().Update(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).Times(1)
			}

			if parallelThread > 1 {
				var wg sync.WaitGroup
				wg.Add(parallelThread)
				for i := 0; i < parallelThread; i++ {
					go func() {
						defer wg.Done()
						err := storageAccountRepo.RemoveStorageAccountTag(ctx, test.subsID, test.resourceGroup, test.account, test.key)
						assert.Equal(t, err, test.expectedErr, fmt.Sprintf("returned error: %v", err), test.name)
					}()
				}
				wg.Wait()
			} else {
				err := storageAccountRepo.RemoveStorageAccountTag(ctx, test.subsID, test.resourceGroup, test.account, test.key)
				if test.expectedErr != nil {
					assert.Equal(t, err, test.expectedErr, fmt.Sprintf("returned error: %v", err), test.name)
				} else {
					assert.Nil(t, err, fmt.Sprintf("returned error: %v", err), test.name)
				}
			}
			ctrl.Finish()
			cancel()
		})
	}

}

func TestIsPrivateEndpointAsExpected(t *testing.T) {
	tests := []struct {
		account        *armstorage.Account
		accountOptions *AccountOptions
		expectedResult bool
	}{
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					PrivateEndpointConnections: []*armstorage.PrivateEndpointConnection{{}},
				},
			},
			accountOptions: &AccountOptions{
				CreatePrivateEndpoint: ptr.To(true),
			},
			expectedResult: true,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					PrivateEndpointConnections: nil,
				},
			},
			accountOptions: &AccountOptions{
				CreatePrivateEndpoint: ptr.To(false),
			},
			expectedResult: true,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					PrivateEndpointConnections: []*armstorage.PrivateEndpointConnection{{}},
				},
			},
			accountOptions: &AccountOptions{
				CreatePrivateEndpoint: ptr.To(false),
			},
			expectedResult: false,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					PrivateEndpointConnections: []*armstorage.PrivateEndpointConnection{{}},
				},
			},
			accountOptions: &AccountOptions{
				CreatePrivateEndpoint: nil,
			},
			expectedResult: true,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					PrivateEndpointConnections: nil,
				},
			},
			accountOptions: &AccountOptions{
				CreatePrivateEndpoint: ptr.To(true),
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
		account        *armstorage.Account
		accountOptions *AccountOptions
		expectedResult bool
	}{
		{
			desc: "nil tags",
			account: &armstorage.Account{
				Tags: nil,
			},
			accountOptions: &AccountOptions{},
			expectedResult: true,
		},
		{
			desc: "empty tags",
			account: &armstorage.Account{
				Tags: map[string]*string{},
			},
			accountOptions: &AccountOptions{},
			expectedResult: true,
		},
		{
			desc: "identitical tags",
			account: &armstorage.Account{
				Tags: map[string]*string{
					"key":  ptr.To("value"),
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
			account: &armstorage.Account{
				Tags: map[string]*string{
					"key":  ptr.To("value"),
					"key2": ptr.To("value2"),
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
			account: &armstorage.Account{
				Tags: map[string]*string{
					"key": ptr.To("value2"),
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
			account: &armstorage.Account{
				Tags: map[string]*string{
					"key": ptr.To("value2"),
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
		{
			desc: "non-identitical tags with different keys",
			account: &armstorage.Account{
				Tags: map[string]*string{
					"key1": ptr.To("value2"),
				},
			},
			accountOptions: &AccountOptions{
				MatchTags: true,
				Tags: map[string]string{
					"key2": "value",
				},
			},
			expectedResult: false,
		},
		{
			desc: "account tags is empty",
			account: &armstorage.Account{
				Tags: map[string]*string{},
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
		account        *armstorage.Account
		accountOptions *AccountOptions
		expectedResult bool
	}{
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					IsHnsEnabled: ptr.To(true),
				},
			},
			accountOptions: &AccountOptions{},
			expectedResult: false,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				IsHnsEnabled: ptr.To(false),
			},
			expectedResult: true,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					IsHnsEnabled: ptr.To(true),
				},
			},
			accountOptions: &AccountOptions{
				IsHnsEnabled: ptr.To(true),
			},
			expectedResult: true,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				IsHnsEnabled: ptr.To(true),
			},
			expectedResult: false,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					IsHnsEnabled: ptr.To(true),
				},
			},
			accountOptions: &AccountOptions{
				IsHnsEnabled: ptr.To(false),
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
		account        *armstorage.Account
		accountOptions *AccountOptions
		expectedResult bool
	}{
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					EnableNfsV3: ptr.To(true),
				},
			},
			accountOptions: &AccountOptions{},
			expectedResult: false,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				EnableNfsV3: ptr.To(false),
			},
			expectedResult: true,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					EnableNfsV3: ptr.To(true),
				},
			},
			accountOptions: &AccountOptions{
				EnableNfsV3: ptr.To(true),
			},
			expectedResult: true,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				EnableNfsV3: ptr.To(true),
			},
			expectedResult: false,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					EnableNfsV3: ptr.To(true),
				},
			},
			accountOptions: &AccountOptions{
				EnableNfsV3: ptr.To(false),
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
		account        *armstorage.Account
		accountOptions *AccountOptions
		expectedResult bool
	}{
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					EnableHTTPSTrafficOnly: ptr.To(true),
				},
			},
			accountOptions: &AccountOptions{},
			expectedResult: false,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				EnableHTTPSTrafficOnly: false,
			},
			expectedResult: false,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					EnableHTTPSTrafficOnly: ptr.To(true),
				},
			},
			accountOptions: &AccountOptions{
				EnableHTTPSTrafficOnly: true,
			},
			expectedResult: true,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				EnableHTTPSTrafficOnly: true,
			},
			expectedResult: true,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					EnableHTTPSTrafficOnly: ptr.To(true),
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
		account        *armstorage.Account
		accountOptions *AccountOptions
		expectedResult bool
	}{
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					AllowBlobPublicAccess: ptr.To(true),
				},
			},
			accountOptions: &AccountOptions{},
			expectedResult: true,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				AllowBlobPublicAccess: ptr.To(false),
			},
			expectedResult: false,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					AllowBlobPublicAccess: ptr.To(true),
				},
			},
			accountOptions: &AccountOptions{
				AllowBlobPublicAccess: ptr.To(true),
			},
			expectedResult: true,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				AllowBlobPublicAccess: ptr.To(true),
			},
			expectedResult: true,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					AllowBlobPublicAccess: ptr.To(true),
				},
			},
			accountOptions: &AccountOptions{
				AllowBlobPublicAccess: ptr.To(false),
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
		account        *armstorage.Account
		accountOptions *AccountOptions
		expectedResult bool
	}{
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					AllowSharedKeyAccess: ptr.To(true),
				},
			},
			accountOptions: &AccountOptions{},
			expectedResult: true,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				AllowSharedKeyAccess: ptr.To(false),
			},
			expectedResult: false,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					AllowSharedKeyAccess: ptr.To(true),
				},
			},
			accountOptions: &AccountOptions{
				AllowSharedKeyAccess: ptr.To(true),
			},
			expectedResult: true,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				AllowSharedKeyAccess: ptr.To(true),
			},
			expectedResult: true,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					AllowSharedKeyAccess: ptr.To(true),
				},
			},
			accountOptions: &AccountOptions{
				AllowSharedKeyAccess: ptr.To(false),
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
		account        *armstorage.Account
		accountOptions *AccountOptions
		expectedResult bool
	}{
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					Encryption: &armstorage.Encryption{
						RequireInfrastructureEncryption: ptr.To(true),
					},
				},
			},
			accountOptions: &AccountOptions{},
			expectedResult: false,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					Encryption: &armstorage.Encryption{
						RequireInfrastructureEncryption: ptr.To(true),
					},
				},
			},
			accountOptions: &AccountOptions{
				RequireInfrastructureEncryption: ptr.To(true),
			},
			expectedResult: true,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					Encryption: &armstorage.Encryption{
						RequireInfrastructureEncryption: ptr.To(false),
					},
				},
			},
			accountOptions: &AccountOptions{
				RequireInfrastructureEncryption: ptr.To(false),
			},
			expectedResult: true,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				RequireInfrastructureEncryption: ptr.To(false),
			},
			expectedResult: true,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					Encryption: &armstorage.Encryption{
						RequireInfrastructureEncryption: ptr.To(true),
					},
				},
			},
			accountOptions: &AccountOptions{
				RequireInfrastructureEncryption: ptr.To(false),
			},
			expectedResult: false,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					Encryption: &armstorage.Encryption{
						RequireInfrastructureEncryption: ptr.To(false),
					},
				},
			},
			accountOptions: &AccountOptions{
				RequireInfrastructureEncryption: ptr.To(true),
			},
			expectedResult: false,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				RequireInfrastructureEncryption: ptr.To(true),
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
		account        *armstorage.Account
		accountOptions *AccountOptions
		expectedResult bool
	}{
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					LargeFileSharesState: to.Ptr(armstorage.LargeFileSharesStateEnabled),
				},
			},
			accountOptions: &AccountOptions{},
			expectedResult: true,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				EnableLargeFileShare: ptr.To(false),
			},
			expectedResult: true,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					LargeFileSharesState: to.Ptr(armstorage.LargeFileSharesStateEnabled),
				},
			},
			accountOptions: &AccountOptions{
				EnableLargeFileShare: ptr.To(true),
			},
			expectedResult: true,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				EnableLargeFileShare: ptr.To(true),
			},
			expectedResult: false,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					LargeFileSharesState: to.Ptr(armstorage.LargeFileSharesStateEnabled),
				},
			},
			accountOptions: &AccountOptions{
				EnableLargeFileShare: ptr.To(false),
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
		account        *armstorage.Account
		accountOptions *AccountOptions
		expectedResult bool
	}{
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					AccessTier: to.Ptr(armstorage.AccessTierCool),
				},
			},
			accountOptions: &AccountOptions{},
			expectedResult: true,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					AccessTier: to.Ptr(armstorage.AccessTierHot),
				},
			},
			accountOptions: &AccountOptions{
				AccessTier: "Hot",
			},
			expectedResult: true,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					AccessTier: to.Ptr(armstorage.AccessTierPremium),
				},
			},
			accountOptions: &AccountOptions{
				AccessTier: "Premium",
			},
			expectedResult: true,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				AccessTier: "Hot",
			},
			expectedResult: false,
		},
		{
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{
					AccessTier: to.Ptr(armstorage.AccessTierPremium),
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	accountName := "account2"

	StorageAccountRepo := &AccountRepo{
		fileServiceRepo: mock_fileservice.NewMockRepository(ctrl),
	}

	multichannelEnabled := armstorage.FileServiceProperties{
		FileServiceProperties: &armstorage.FileServicePropertiesProperties{
			ProtocolSettings: &armstorage.ProtocolSettings{
				Smb: &armstorage.SmbSetting{Multichannel: &armstorage.Multichannel{Enabled: ptr.To(true)}},
			},
		},
	}

	multichannelDisabled := armstorage.FileServiceProperties{
		FileServiceProperties: &armstorage.FileServicePropertiesProperties{
			ProtocolSettings: &armstorage.ProtocolSettings{
				Smb: &armstorage.SmbSetting{Multichannel: &armstorage.Multichannel{Enabled: ptr.To(false)}},
			},
		},
	}

	incompleteServiceProperties := armstorage.FileServiceProperties{
		FileServiceProperties: &armstorage.FileServicePropertiesProperties{
			ProtocolSettings: &armstorage.ProtocolSettings{},
		},
	}

	tests := []struct {
		desc                      string
		account                   *armstorage.Account
		accountOptions            *AccountOptions
		serviceProperties         *armstorage.FileServiceProperties
		servicePropertiesRetError error
		expectedResult            bool
	}{
		{
			desc: "IsMultichannelEnabled is nil",
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{},
			expectedResult: true,
		},
		{
			desc: "account.Name is nil",
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				IsMultichannelEnabled: ptr.To(false),
			},
			expectedResult: false,
		},
		{
			desc: "IsMultichannelEnabled not equal #1",
			account: &armstorage.Account{
				Name:       &accountName,
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				IsMultichannelEnabled: ptr.To(false),
			},
			serviceProperties: &multichannelEnabled,
			expectedResult:    false,
		},
		{
			desc: "GetServiceProperties return error",
			account: &armstorage.Account{
				Name:       &accountName,
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				IsMultichannelEnabled: ptr.To(false),
			},
			serviceProperties:         &multichannelEnabled,
			servicePropertiesRetError: fmt.Errorf("GetServiceProperties return error"),
			expectedResult:            false,
		},
		{
			desc: "IsMultichannelEnabled not equal #2",
			account: &armstorage.Account{
				Name:       &accountName,
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				IsMultichannelEnabled: ptr.To(true),
			},
			serviceProperties: &multichannelDisabled,
			expectedResult:    false,
		},
		{
			desc: "IsMultichannelEnabled is equal #1",
			account: &armstorage.Account{
				Name:       &accountName,
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				IsMultichannelEnabled: ptr.To(true),
			},
			serviceProperties: &multichannelEnabled,
			expectedResult:    true,
		},
		{
			desc: "IsMultichannelEnabled is equal #2",
			account: &armstorage.Account{
				Name:       &accountName,
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				IsMultichannelEnabled: ptr.To(false),
			},
			serviceProperties: &multichannelDisabled,
			expectedResult:    true,
		},
		{
			desc: "incompleteServiceProperties should be regarded as IsMultichannelDisabled",
			account: &armstorage.Account{
				Name:       &accountName,
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				IsMultichannelEnabled: ptr.To(false),
			},
			serviceProperties: &incompleteServiceProperties,
			expectedResult:    true,
		},
	}

	for _, test := range tests {
		if test.serviceProperties != nil {
			StorageAccountRepo.fileServiceRepo.(*mock_fileservice.MockRepository).EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(test.serviceProperties, test.servicePropertiesRetError).Times(1)
		}

		result, _ := StorageAccountRepo.isMultichannelEnabledEqual(ctx, test.account, test.accountOptions)
		assert.Equal(t, test.expectedResult, result, test.desc)
	}
}

func TestIsDisableFileServiceDeleteRetentionPolicyEqual(t *testing.T) {

	accountName := "account"

	deleteRetentionPolicyEnabled := armstorage.FileServiceProperties{
		FileServiceProperties: &armstorage.FileServicePropertiesProperties{
			ShareDeleteRetentionPolicy: &armstorage.DeleteRetentionPolicy{
				Enabled: ptr.To(true),
			},
		},
	}

	deleteRetentionPolicyDisabled := armstorage.FileServiceProperties{
		FileServiceProperties: &armstorage.FileServicePropertiesProperties{
			ShareDeleteRetentionPolicy: &armstorage.DeleteRetentionPolicy{
				Enabled: ptr.To(false),
			},
		},
	}

	incompleteServiceProperties := armstorage.FileServiceProperties{
		FileServiceProperties: &armstorage.FileServicePropertiesProperties{},
	}

	tests := []struct {
		desc                      string
		account                   *armstorage.Account
		accountOptions            *AccountOptions
		serviceProperties         *armstorage.FileServiceProperties
		servicePropertiesRetError error
		expectedResult            bool
	}{
		{
			desc: "DisableFileServiceDeleteRetentionPolicy is nil",
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{},
			expectedResult: true,
		},
		{
			desc: "account.Name is nil",
			account: &armstorage.Account{
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				DisableFileServiceDeleteRetentionPolicy: ptr.To(false),
			},
			expectedResult: false,
		},
		{
			desc: "DisableFileServiceDeleteRetentionPolicy not equal",
			account: &armstorage.Account{
				Name:       &accountName,
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				DisableFileServiceDeleteRetentionPolicy: ptr.To(true),
			},
			serviceProperties: &deleteRetentionPolicyEnabled,
			expectedResult:    false,
		},
		{
			desc: "GetServiceProperties return error",
			account: &armstorage.Account{
				Name:       &accountName,
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				DisableFileServiceDeleteRetentionPolicy: ptr.To(false),
			},
			serviceProperties:         &deleteRetentionPolicyEnabled,
			servicePropertiesRetError: fmt.Errorf("GetServiceProperties return error"),
			expectedResult:            false,
		},
		{
			desc: "DisableFileServiceDeleteRetentionPolicy not equal",
			account: &armstorage.Account{
				Name:       &accountName,
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				DisableFileServiceDeleteRetentionPolicy: ptr.To(false),
			},
			serviceProperties: &deleteRetentionPolicyDisabled,
			expectedResult:    false,
		},
		{
			desc: "DisableFileServiceDeleteRetentionPolicy is equal",
			account: &armstorage.Account{
				Name:       &accountName,
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				DisableFileServiceDeleteRetentionPolicy: ptr.To(true),
			},
			serviceProperties: &deleteRetentionPolicyDisabled,
			expectedResult:    true,
		},
		{
			desc: "DisableFileServiceDeleteRetentionPolicy is equal",
			account: &armstorage.Account{
				Name:       &accountName,
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				DisableFileServiceDeleteRetentionPolicy: ptr.To(false),
			},
			serviceProperties: &deleteRetentionPolicyEnabled,
			expectedResult:    true,
		},
		{
			desc: "incompleteServiceProperties should be regarded as not DisableFileServiceDeleteRetentionPolicy",
			account: &armstorage.Account{
				Name:       &accountName,
				Properties: &armstorage.AccountProperties{},
			},
			accountOptions: &AccountOptions{
				DisableFileServiceDeleteRetentionPolicy: ptr.To(false),
			},
			serviceProperties: &incompleteServiceProperties,
			expectedResult:    true,
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			ctx, cancel := context.WithCancel(context.Background())
			StorageAccountRepo := &AccountRepo{
				fileServiceRepo: mock_fileservice.NewMockRepository(ctrl),
			}
			if test.serviceProperties != nil {
				StorageAccountRepo.fileServiceRepo.(*mock_fileservice.MockRepository).EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(test.serviceProperties, test.servicePropertiesRetError).Times(1)
			}
			result, _ := StorageAccountRepo.isDisableFileServiceDeleteRetentionPolicyEqual(ctx, test.account, test.accountOptions)
			assert.Equal(t, test.expectedResult, result, test.desc)
			ctrl.Finish()
			cancel()
		})
	}
}

func TestIsSoftDeleteBlobsEqual(t *testing.T) {
	type args struct {
		property       *armstorage.BlobServiceProperties
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
				property: &armstorage.BlobServiceProperties{
					BlobServiceProperties: &armstorage.BlobServicePropertiesProperties{},
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
				property: &armstorage.BlobServiceProperties{
					BlobServiceProperties: &armstorage.BlobServicePropertiesProperties{
						DeleteRetentionPolicy: &armstorage.DeleteRetentionPolicy{
							Enabled: ptr.To(false),
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
				property: &armstorage.BlobServiceProperties{
					BlobServiceProperties: &armstorage.BlobServicePropertiesProperties{
						DeleteRetentionPolicy: &armstorage.DeleteRetentionPolicy{
							Enabled: ptr.To(true),
							Days:    ptr.To(int32(7)),
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
				property: &armstorage.BlobServiceProperties{
					BlobServiceProperties: &armstorage.BlobServicePropertiesProperties{
						DeleteRetentionPolicy: &armstorage.DeleteRetentionPolicy{
							Enabled: ptr.To(true),
							Days:    ptr.To(int32(7)),
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
		property       *armstorage.BlobServiceProperties
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
				property: &armstorage.BlobServiceProperties{
					BlobServiceProperties: &armstorage.BlobServicePropertiesProperties{},
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
				property: &armstorage.BlobServiceProperties{
					BlobServiceProperties: &armstorage.BlobServicePropertiesProperties{
						ContainerDeleteRetentionPolicy: &armstorage.DeleteRetentionPolicy{
							Enabled: ptr.To(false),
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
				property: &armstorage.BlobServiceProperties{
					BlobServiceProperties: &armstorage.BlobServicePropertiesProperties{
						ContainerDeleteRetentionPolicy: &armstorage.DeleteRetentionPolicy{
							Enabled: ptr.To(true),
							Days:    ptr.To(int32(7)),
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
				property: &armstorage.BlobServiceProperties{
					BlobServiceProperties: &armstorage.BlobServicePropertiesProperties{
						ContainerDeleteRetentionPolicy: &armstorage.DeleteRetentionPolicy{
							Enabled: ptr.To(true),
							Days:    ptr.To(int32(7)),
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
		property       *armstorage.BlobServiceProperties
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
				property: &armstorage.BlobServiceProperties{
					BlobServiceProperties: &armstorage.BlobServicePropertiesProperties{},
				},
				accountOptions: &AccountOptions{
					EnableBlobVersioning: ptr.To(false),
				},
			},
			want: true,
		},
		{
			name: "not equal",
			args: args{
				property: &armstorage.BlobServiceProperties{
					BlobServiceProperties: &armstorage.BlobServicePropertiesProperties{
						IsVersioningEnabled: ptr.To(true),
					},
				},
				accountOptions: &AccountOptions{
					EnableBlobVersioning: ptr.To(false),
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

func TestParseServiceAccountTokenError(t *testing.T) {
	cases := []struct {
		desc     string
		saTokens string
	}{
		{
			desc:     "empty serviceaccount tokens",
			saTokens: "",
		},
		{
			desc:     "invalid serviceaccount tokens",
			saTokens: "invalid",
		},
		{
			desc:     "token for audience not found",
			saTokens: `{"aud1":{"token":"eyJhbGciOiJSUzI1NiIsImtpZCI6InRhVDBxbzhQVEZ1ajB1S3BYUUxIclRsR01XakxjemJNOTlzWVMxSlNwbWcifQ.eyJhdWQiOlsiYXBpOi8vQXp1cmVBRGlUb2tlbkV4Y2hhbmdlIl0sImV4cCI6MTY0MzIzNDY0NywiaWF0IjoxNjQzMjMxMDQ3LCJpc3MiOiJodHRwczovL2t1YmVybmV0ZXMuZGVmYXVsdC5zdmMuY2x1c3Rlci5sb2NhbCIsImt1YmVybmV0ZXMuaW8iOnsibmFtZXNwYWNlIjoidGVzdC12MWFscGhhMSIsInBvZCI6eyJuYW1lIjoic2VjcmV0cy1zdG9yZS1pbmxpbmUtY3JkIiwidWlkIjoiYjBlYmZjMzUtZjEyNC00ZTEyLWI3N2UtYjM0MjM2N2IyMDNmIn0sInNlcnZpY2VhY2NvdW50Ijp7Im5hbWUiOiJkZWZhdWx0IiwidWlkIjoiMjViNGY1NzgtM2U4MC00NTczLWJlOGQtZTdmNDA5ZDI0MmI2In19LCJuYmYiOjE2NDMyMzEwNDcsInN1YiI6InN5c3RlbTpzZXJ2aWNlYWNjb3VudDp0ZXN0LXYxYWxwaGExOmRlZmF1bHQifQ.ALE46aKmtTV7dsuFOwDZqvEjdHFUTNP-JVjMxexTemmPA78fmPTUZF0P6zANumA03fjX3L-MZNR3PxmEZgKA9qEGIDsljLsUWsVBEquowuBh8yoBYkGkMJmRfmbfS3y7_4Q7AU3D9Drw4iAHcn1GwedjOQC0i589y3dkNNqf8saqHfXkbSSLtSE0f2uzI-PjuTKvR1kuojEVNKlEcA4wsKfoiRpkua17sHkHU0q9zxCMDCr_1f8xbigRnRx0wscU3vy-8KhF3zQtpcWkk3r4C5YSXut9F3xjz5J9DUQn2vNMfZg4tOdcR-9Xv9fbY5iujiSlS58GEktSEa3SE9wrCw\",\"expirationTimestamp\":\"2022-01-26T22:04:07Z\"},\"gcp\":{\"token\":\"eyJhbGciOiJSUzI1NiIsImtpZCI6InRhVDBxbzhQVEZ1ajB1S3BYUUxIclRsR01XakxjemJNOTlzWVMxSlNwbWcifQ.eyJhdWQiOlsiZ2NwIl0sImV4cCI6MTY0MzIzNDY0NywiaWF0IjoxNjQzMjMxMDQ3LCJpc3MiOiJodHRwczovL2t1YmVybmV0ZXMuZGVmYXVsdC5zdmMuY2x1c3Rlci5sb2NhbCIsImt1YmVybmV0ZXMuaW8iOnsibmFtZXNwYWNlIjoidGVzdC12MWFscGhhMSIsInBvZCI6eyJuYW1lIjoic2VjcmV0cy1zdG9yZS1pbmxpbmUtY3JkIiwidWlkIjoiYjBlYmZjMzUtZjEyNC00ZTEyLWI3N2UtYjM0MjM2N2IyMDNmIn0sInNlcnZpY2VhY2NvdW50Ijp7Im5hbWUiOiJkZWZhdWx0IiwidWlkIjoiMjViNGY1NzgtM2U4MC00NTczLWJlOGQtZTdmNDA5ZDI0MmI2In19LCJuYmYiOjE2NDMyMzEwNDcsInN1YiI6InN5c3RlbTpzZXJ2aWNlYWNjb3VudDp0ZXN0LXYxYWxwaGExOmRlZmF1bHQifQ.BT0YGI7bGdSNaIBqIEnVL0Ky5t-fynaemSGxjGdKOPl0E22UIVGDpAMUhaS19i20c-Dqs-Kn0N-R5QyDNpZg8vOL5KIFqu2kSYNbKxtQW7TPYIsV0d9wUZjLSr54DKrmyXNMGRoT2bwcF4yyfmO46eMmZSaXN8Y4lgapeabg6CBVVQYHD-GrgXf9jVLeJfCQkTuojK1iXOphyD6NqlGtVCaY1jWxbBMibN0q214vKvQboub8YMuvclGdzn_l_ZQSTjvhBj9I-W1t-JArVjqHoIb8_FlR9BSgzgL7V3Jki55vmiOdEYqMErJWrIZPP3s8qkU5hhO9rSVEd3LJHponvQ","expirationTimestamp":"2022-01-26T22:04:07Z"}}`, //nolint
		},
		{
			desc:     "token incorrect format",
			saTokens: `{"api://AzureADTokenExchange":{"tokens":"eyJhbGciOiJSUzI1NiIsImtpZCI6InRhVDBxbzhQVEZ1ajB1S3BYUUxIclRsR01XakxjemJNOTlzWVMxSlNwbWcifQ.eyJhdWQiOlsiYXBpOi8vQXp1cmVBRGlUb2tlbkV4Y2hhbmdlIl0sImV4cCI6MTY0MzIzNDY0NywiaWF0IjoxNjQzMjMxMDQ3LCJpc3MiOiJodHRwczovL2t1YmVybmV0ZXMuZGVmYXVsdC5zdmMuY2x1c3Rlci5sb2NhbCIsImt1YmVybmV0ZXMuaW8iOnsibmFtZXNwYWNlIjoidGVzdC12MWFscGhhMSIsInBvZCI6eyJuYW1lIjoic2VjcmV0cy1zdG9yZS1pbmxpbmUtY3JkIiwidWlkIjoiYjBlYmZjMzUtZjEyNC00ZTEyLWI3N2UtYjM0MjM2N2IyMDNmIn0sInNlcnZpY2VhY2NvdW50Ijp7Im5hbWUiOiJkZWZhdWx0IiwidWlkIjoiMjViNGY1NzgtM2U4MC00NTczLWJlOGQtZTdmNDA5ZDI0MmI2In19LCJuYmYiOjE2NDMyMzEwNDcsInN1YiI6InN5c3RlbTpzZXJ2aWNlYWNjb3VudDp0ZXN0LXYxYWxwaGExOmRlZmF1bHQifQ.ALE46aKmtTV7dsuFOwDZqvEjdHFUTNP-JVjMxexTemmPA78fmPTUZF0P6zANumA03fjX3L-MZNR3PxmEZgKA9qEGIDsljLsUWsVBEquowuBh8yoBYkGkMJmRfmbfS3y7_4Q7AU3D9Drw4iAHcn1GwedjOQC0i589y3dkNNqf8saqHfXkbSSLtSE0f2uzI-PjuTKvR1kuojEVNKlEcA4wsKfoiRpkua17sHkHU0q9zxCMDCr_1f8xbigRnRx0wscU3vy-8KhF3zQtpcWkk3r4C5YSXut9F3xjz5J9DUQn2vNMfZg4tOdcR-9Xv9fbY5iujiSlS58GEktSEa3SE9wrCw\",\"expirationTimestamp\":\"2022-01-26T22:04:07Z\"},\"gcp\":{\"token\":\"eyJhbGciOiJSUzI1NiIsImtpZCI6InRhVDBxbzhQVEZ1ajB1S3BYUUxIclRsR01XakxjemJNOTlzWVMxSlNwbWcifQ.eyJhdWQiOlsiZ2NwIl0sImV4cCI6MTY0MzIzNDY0NywiaWF0IjoxNjQzMjMxMDQ3LCJpc3MiOiJodHRwczovL2t1YmVybmV0ZXMuZGVmYXVsdC5zdmMuY2x1c3Rlci5sb2NhbCIsImt1YmVybmV0ZXMuaW8iOnsibmFtZXNwYWNlIjoidGVzdC12MWFscGhhMSIsInBvZCI6eyJuYW1lIjoic2VjcmV0cy1zdG9yZS1pbmxpbmUtY3JkIiwidWlkIjoiYjBlYmZjMzUtZjEyNC00ZTEyLWI3N2UtYjM0MjM2N2IyMDNmIn0sInNlcnZpY2VhY2NvdW50Ijp7Im5hbWUiOiJkZWZhdWx0IiwidWlkIjoiMjViNGY1NzgtM2U4MC00NTczLWJlOGQtZTdmNDA5ZDI0MmI2In19LCJuYmYiOjE2NDMyMzEwNDcsInN1YiI6InN5c3RlbTpzZXJ2aWNlYWNjb3VudDp0ZXN0LXYxYWxwaGExOmRlZmF1bHQifQ.BT0YGI7bGdSNaIBqIEnVL0Ky5t-fynaemSGxjGdKOPl0E22UIVGDpAMUhaS19i20c-Dqs-Kn0N-R5QyDNpZg8vOL5KIFqu2kSYNbKxtQW7TPYIsV0d9wUZjLSr54DKrmyXNMGRoT2bwcF4yyfmO46eMmZSaXN8Y4lgapeabg6CBVVQYHD-GrgXf9jVLeJfCQkTuojK1iXOphyD6NqlGtVCaY1jWxbBMibN0q214vKvQboub8YMuvclGdzn_l_ZQSTjvhBj9I-W1t-JArVjqHoIb8_FlR9BSgzgL7V3Jki55vmiOdEYqMErJWrIZPP3s8qkU5hhO9rSVEd3LJHponvQ","expirationTimestamp":"2022-01-26T22:04:07Z"}}`, //nolint

		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			if _, err := parseServiceAccountToken(tc.saTokens); err == nil {
				t.Errorf("ParseServiceAccountToken(%s) = nil, want error", tc.saTokens)
			}
		})
	}
}

func TestParseServiceAccountToken(t *testing.T) {
	saTokens := `{"api://AzureADTokenExchange":{"token":"eyJhbGciOiJSUzI1NiIsImtpZCI6InRhVDBxbzhQVEZ1ajB1S3BYUUxIclRsR01XakxjemJNOTlzWVMxSlNwbWcifQ.eyJhdWQiOlsiYXBpOi8vQXp1cmVBRGlUb2tlbkV4Y2hhbmdlIl0sImV4cCI6MTY0MzIzNDY0NywiaWF0IjoxNjQzMjMxMDQ3LCJpc3MiOiJodHRwczovL2t1YmVybmV0ZXMuZGVmYXVsdC5zdmMuY2x1c3Rlci5sb2NhbCIsImt1YmVybmV0ZXMuaW8iOnsibmFtZXNwYWNlIjoidGVzdC12MWFscGhhMSIsInBvZCI6eyJuYW1lIjoic2VjcmV0cy1zdG9yZS1pbmxpbmUtY3JkIiwidWlkIjoiYjBlYmZjMzUtZjEyNC00ZTEyLWI3N2UtYjM0MjM2N2IyMDNmIn0sInNlcnZpY2VhY2NvdW50Ijp7Im5hbWUiOiJkZWZhdWx0IiwidWlkIjoiMjViNGY1NzgtM2U4MC00NTczLWJlOGQtZTdmNDA5ZDI0MmI2In19LCJuYmYiOjE2NDMyMzEwNDcsInN1YiI6InN5c3RlbTpzZXJ2aWNlYWNjb3VudDp0ZXN0LXYxYWxwaGExOmRlZmF1bHQifQ.ALE46aKmtTV7dsuFOwDZqvEjdHFUTNP-JVjMxexTemmPA78fmPTUZF0P6zANumA03fjX3L-MZNR3PxmEZgKA9qEGIDsljLsUWsVBEquowuBh8yoBYkGkMJmRfmbfS3y7_4Q7AU3D9Drw4iAHcn1GwedjOQC0i589y3dkNNqf8saqHfXkbSSLtSE0f2uzI-PjuTKvR1kuojEVNKlEcA4wsKfoiRpkua17sHkHU0q9zxCMDCr_1f8xbigRnRx0wscU3vy-8KhF3zQtpcWkk3r4C5YSXut9F3xjz5J9DUQn2vNMfZg4tOdcR-9Xv9fbY5iujiSlS58GEktSEa3SE9wrCw","expirationTimestamp":"2022-01-26T22:04:07Z"},"aud2":{"token":"eyJhbGciOiJSUzI1NiIsImtpZCI6InRhVDBxbzhQVEZ1ajB1S3BYUUxIclRsR01XakxjemJNOTlzWVMxSlNwbWcifQ.eyJhdWQiOlsiZ2NwIl0sImV4cCI6MTY0MzIzNDY0NywiaWF0IjoxNjQzMjMxMDQ3LCJpc3MiOiJodHRwczovL2t1YmVybmV0ZXMuZGVmYXVsdC5zdmMuY2x1c3Rlci5sb2NhbCIsImt1YmVybmV0ZXMuaW8iOnsibmFtZXNwYWNlIjoidGVzdC12MWFscGhhMSIsInBvZCI6eyJuYW1lIjoic2VjcmV0cy1zdG9yZS1pbmxpbmUtY3JkIiwidWlkIjoiYjBlYmZjMzUtZjEyNC00ZTEyLWI3N2UtYjM0MjM2N2IyMDNmIn0sInNlcnZpY2VhY2NvdW50Ijp7Im5hbWUiOiJkZWZhdWx0IiwidWlkIjoiMjViNGY1NzgtM2U4MC00NTczLWJlOGQtZTdmNDA5ZDI0MmI2In19LCJuYmYiOjE2NDMyMzEwNDcsInN1YiI6InN5c3RlbTpzZXJ2aWNlYWNjb3VudDp0ZXN0LXYxYWxwaGExOmRlZmF1bHQifQ.BT0YGI7bGdSNaIBqIEnVL0Ky5t-fynaemSGxjGdKOPl0E22UIVGDpAMUhaS19i20c-Dqs-Kn0N-R5QyDNpZg8vOL5KIFqu2kSYNbKxtQW7TPYIsV0d9wUZjLSr54DKrmyXNMGRoT2bwcF4yyfmO46eMmZSaXN8Y4lgapeabg6CBVVQYHD-GrgXf9jVLeJfCQkTuojK1iXOphyD6NqlGtVCaY1jWxbBMibN0q214vKvQboub8YMuvclGdzn_l_ZQSTjvhBj9I-W1t-JArVjqHoIb8_FlR9BSgzgL7V3Jki55vmiOdEYqMErJWrIZPP3s8qkU5hhO9rSVEd3LJHponvQ","expirationTimestamp":"2022-01-26T22:04:07Z"}}` //nolint
	expectedToken := `eyJhbGciOiJSUzI1NiIsImtpZCI6InRhVDBxbzhQVEZ1ajB1S3BYUUxIclRsR01XakxjemJNOTlzWVMxSlNwbWcifQ.eyJhdWQiOlsiYXBpOi8vQXp1cmVBRGlUb2tlbkV4Y2hhbmdlIl0sImV4cCI6MTY0MzIzNDY0NywiaWF0IjoxNjQzMjMxMDQ3LCJpc3MiOiJodHRwczovL2t1YmVybmV0ZXMuZGVmYXVsdC5zdmMuY2x1c3Rlci5sb2NhbCIsImt1YmVybmV0ZXMuaW8iOnsibmFtZXNwYWNlIjoidGVzdC12MWFscGhhMSIsInBvZCI6eyJuYW1lIjoic2VjcmV0cy1zdG9yZS1pbmxpbmUtY3JkIiwidWlkIjoiYjBlYmZjMzUtZjEyNC00ZTEyLWI3N2UtYjM0MjM2N2IyMDNmIn0sInNlcnZpY2VhY2NvdW50Ijp7Im5hbWUiOiJkZWZhdWx0IiwidWlkIjoiMjViNGY1NzgtM2U4MC00NTczLWJlOGQtZTdmNDA5ZDI0MmI2In19LCJuYmYiOjE2NDMyMzEwNDcsInN1YiI6InN5c3RlbTpzZXJ2aWNlYWNjb3VudDp0ZXN0LXYxYWxwaGExOmRlZmF1bHQifQ.ALE46aKmtTV7dsuFOwDZqvEjdHFUTNP-JVjMxexTemmPA78fmPTUZF0P6zANumA03fjX3L-MZNR3PxmEZgKA9qEGIDsljLsUWsVBEquowuBh8yoBYkGkMJmRfmbfS3y7_4Q7AU3D9Drw4iAHcn1GwedjOQC0i589y3dkNNqf8saqHfXkbSSLtSE0f2uzI-PjuTKvR1kuojEVNKlEcA4wsKfoiRpkua17sHkHU0q9zxCMDCr_1f8xbigRnRx0wscU3vy-8KhF3zQtpcWkk3r4C5YSXut9F3xjz5J9DUQn2vNMfZg4tOdcR-9Xv9fbY5iujiSlS58GEktSEa3SE9wrCw`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         //nolint

	token, err := parseServiceAccountToken(saTokens)
	if err != nil {
		t.Fatalf("ParseServiceAccountToken(%s) = %v, want nil", saTokens, err)
	}
	if token != expectedToken {
		t.Errorf("ParseServiceAccountToken(%s) = %s, want %s", saTokens, token, expectedToken)
	}
}

func TestAreVNetRulesEqual(t *testing.T) {
	type args struct {
		account       *armstorage.Account
		accountOption *AccountOptions
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "account option is empty",
			args: args{
				account: &armstorage.Account{
					Properties: &armstorage.AccountProperties{},
				},
				accountOption: &AccountOptions{
					VirtualNetworkResourceIDs: []string{},
				},
			},
			want: true,
		},
		{
			name: "VirtualNetworkRules are equal",
			args: args{
				account: &armstorage.Account{
					Properties: &armstorage.AccountProperties{
						NetworkRuleSet: &armstorage.NetworkRuleSet{
							VirtualNetworkRules: []*armstorage.VirtualNetworkRule{
								{
									VirtualNetworkResourceID: ptr.To("id"),
									Action:                   to.Ptr(string(armstorage.DefaultActionAllow)),
									State:                    to.Ptr(armstorage.State("state")),
								},
							},
						},
					},
				},
				accountOption: &AccountOptions{
					VirtualNetworkResourceIDs: []string{"id"},
				},
			},
			want: true,
		},
		{
			name: "VirtualNetworkRules are equal with multiple NetworkRules",
			args: args{
				account: &armstorage.Account{
					Properties: &armstorage.AccountProperties{
						NetworkRuleSet: &armstorage.NetworkRuleSet{
							VirtualNetworkRules: []*armstorage.VirtualNetworkRule{
								{
									VirtualNetworkResourceID: ptr.To("id1"),
									Action:                   to.Ptr(string(armstorage.DefaultActionAllow)),
								},
								{
									VirtualNetworkResourceID: ptr.To("id2"),
									Action:                   to.Ptr(string(armstorage.DefaultActionAllow)),
								},
							},
						},
					},
				},
				accountOption: &AccountOptions{
					VirtualNetworkResourceIDs: []string{"id2"},
				},
			},
			want: true,
		},
		{
			name: "VirtualNetworkRules not equal",
			args: args{
				account: &armstorage.Account{
					Properties: &armstorage.AccountProperties{
						NetworkRuleSet: &armstorage.NetworkRuleSet{
							VirtualNetworkRules: []*armstorage.VirtualNetworkRule{
								{
									VirtualNetworkResourceID: ptr.To("id1"),
									Action:                   to.Ptr(string(armstorage.DefaultActionAllow)),
									State:                    to.Ptr(armstorage.State("state")),
								},
							},
						},
					},
				},
				accountOption: &AccountOptions{
					VirtualNetworkResourceIDs: []string{"id2"},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AreVNetRulesEqual(tt.args.account, tt.args.accountOption); got != tt.want {
				t.Errorf("areVNetRulesEqual() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGenerateStorageAccountName(t *testing.T) {
	tests := []struct {
		prefix string
	}{
		{
			prefix: "",
		},
		{
			prefix: "pvc",
		},
		{
			prefix: "1234512345123451234512345",
		},
	}

	for _, test := range tests {
		accountName := generateStorageAccountName(test.prefix)
		if len(accountName) > consts.StorageAccountNameMaxLength || len(accountName) < 3 {
			t.Errorf("input prefix: %s, output account name: %s, length not in [3,%d]", test.prefix, accountName, consts.StorageAccountNameMaxLength)
		}

		for _, char := range accountName {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') {
				t.Errorf("input prefix: %s, output account name: %s, there is non-digit or non-letter(%q)", test.prefix, accountName, char)
				break
			}
		}
	}
}
