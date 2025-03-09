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
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	privatedns "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/privatedns/armprivatedns"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storage/armstorage"
	"github.com/google/uuid"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/accountclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	azureconfig "sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/storage/fileservice"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/subnet"
	"sigs.k8s.io/cloud-provider-azure/pkg/util/lockmap"
)

// SkipMatchingTag skip account matching tag
const SkipMatchingTag = "skip-matching"
const LocationGlobal = "global"
const privateDNSZoneNameFmt = "%s.%s.%s"
const defaultPrivateDNSZoneName = "privatelink"
const DefaultTokenAudience = "api://AzureADTokenExchange" //nolint:gosec // G101 ignore this!

type Type string

const (
	StorageTypeBlob Type = "blob"
	StorageTypeFile Type = "file"
	blobNameSuffix       = "-blob"
)

// AccountOptions contains the fields which are used to create storage account.
type AccountOptions struct {
	SubscriptionID                            string
	Name, Type, Kind, ResourceGroup, Location string
	EnableHTTPSTrafficOnly                    bool
	// indicate whether create new account when Name is empty or when account does not exists
	CreateAccount                           bool
	CreatePrivateEndpoint                   *bool
	StorageType                             Type
	StorageEndpointSuffix                   string
	DisableFileServiceDeleteRetentionPolicy *bool
	EnableLargeFileShare                    *bool
	IsHnsEnabled                            *bool
	EnableNfsV3                             *bool
	AllowBlobPublicAccess                   *bool
	RequireInfrastructureEncryption         *bool
	AllowSharedKeyAccess                    *bool
	IsMultichannelEnabled                   *bool
	KeyName                                 *string
	KeyVersion                              *string
	KeyVaultURI                             *string
	Tags                                    map[string]string
	VirtualNetworkResourceIDs               []string
	VNetResourceGroup                       string
	VNetName                                string
	SubnetName                              string
	AccessTier                              string
	MatchTags                               bool
	GetLatestAccountKey                     bool
	EnableBlobVersioning                    *bool
	SoftDeleteBlobs                         int32
	SoftDeleteContainers                    int32
	// indicate whether to get a random matching account, if false, will get the first matching account
	PickRandomMatchingAccount bool
	// provide the source account name in snapshot restore and volume clone scenarios
	SourceAccountName string
	// default is "privatelink"
	PrivateDNSZoneName string
}

type accountWithLocation struct {
	Name, StorageType, Location string
}
type AccountRepo struct {
	azureconfig.Config
	Environment          *azclient.Environment
	ComputeClientFactory azclient.ClientFactory
	NetworkClientFactory azclient.ClientFactory
	subnetRepo           subnet.Repository
	fileServiceRepo      fileservice.Repository
	storageAccountCache  cache.Resource[armstorage.Account]
	lockMap              *lockmap.LockMap
}

func NewRepository(config azureconfig.Config, env *azclient.Environment, computeClientFactory azclient.ClientFactory, networkClientFactory azclient.ClientFactory) (*AccountRepo, error) {
	getter := func(_ context.Context, _ string) (*armstorage.Account, error) { return nil, nil }
	storageAccountCache, err := cache.NewTimedCache(time.Minute, getter, config.DisableAPICallCache)
	if err != nil {
		return nil, err
	}
	fileserviceRepo, err := fileservice.NewRepository(config, computeClientFactory)
	if err != nil {
		return nil, err
	}
	subnetRepo, err := subnet.NewRepo(networkClientFactory.GetSubnetClient())
	if err != nil {
		return nil, err
	}
	return &AccountRepo{
		Config:               config,
		Environment:          env,
		fileServiceRepo:      fileserviceRepo,
		ComputeClientFactory: computeClientFactory,
		NetworkClientFactory: networkClientFactory,
		subnetRepo:           subnetRepo,
		storageAccountCache:  storageAccountCache,
		lockMap:              lockmap.NewLockMap(),
	}, nil
}

// getStorageAccounts get matching storage accounts
func (az *AccountRepo) getStorageAccounts(ctx context.Context, storageAccountClient accountclient.Interface, accountOptions *AccountOptions) ([]accountWithLocation, error) {
	result, err := storageAccountClient.List(ctx, accountOptions.ResourceGroup)
	if err != nil {
		return nil, err
	}

	accounts := []accountWithLocation{}
	for _, acct := range result {
		if acct.Name != nil && acct.Location != nil && acct.SKU != nil {
			if !(isStorageTypeEqual(acct, accountOptions) &&
				isAccountKindEqual(acct, accountOptions) &&
				isLocationEqual(acct, accountOptions) &&
				isLargeFileSharesPropertyEqual(acct, accountOptions) &&
				isTagsEqual(acct, accountOptions) &&
				isTaggedWithSkip(acct) &&
				isHnsPropertyEqual(acct, accountOptions) &&
				isEnableNfsV3PropertyEqual(acct, accountOptions) &&
				isEnableHTTPSTrafficOnlyEqual(acct, accountOptions) &&
				isAllowBlobPublicAccessEqual(acct, accountOptions) &&
				isRequireInfrastructureEncryptionEqual(acct, accountOptions) &&
				isAllowSharedKeyAccessEqual(acct, accountOptions) &&
				isAccessTierEqual(acct, accountOptions) &&
				AreVNetRulesEqual(acct, accountOptions) &&
				isPrivateEndpointAsExpected(acct, accountOptions)) {
				continue
			}

			equal, err := az.isMultichannelEnabledEqual(ctx, acct, accountOptions)
			if err != nil {
				return nil, err
			}
			if !equal {
				continue
			}

			if equal, err = az.isDisableFileServiceDeleteRetentionPolicyEqual(ctx, acct, accountOptions); err != nil {
				return nil, err
			}
			if !equal {
				continue
			}

			if equal, err = az.isEnableBlobDataProtectionEqual(ctx, acct, accountOptions); err != nil {
				return nil, err
			}
			if !equal {
				continue
			}

			accounts = append(accounts, accountWithLocation{Name: *acct.Name, StorageType: string(*acct.SKU.Name), Location: *acct.Location})
			if !accountOptions.PickRandomMatchingAccount && accountOptions.SourceAccountName == "" {
				// return the first matching account if it's not required to pick a random one
				// or does not need to find a matching account with source account name
				break
			}
			if accountOptions.SourceAccountName == *acct.Name {
				return accounts, nil
			}
		}
	}
	return accounts, nil
}

// serviceAccountToken represents the service account token sent from NodePublishVolume Request.
// ref: https://kubernetes-csi.github.io/docs/token-requests.html
type serviceAccountToken struct {
	APIAzureADTokenExchange struct {
		Token               string    `json:"token"`
		ExpirationTimestamp time.Time `json:"expirationTimestamp"`
	} `json:"api://AzureADTokenExchange"`
}

// parseServiceAccountToken parses the bound service account token from the token passed from NodePublishVolume Request.
func parseServiceAccountToken(tokenStr string) (string, error) {
	if len(tokenStr) == 0 {
		return "", fmt.Errorf("service account token is empty")
	}
	token := serviceAccountToken{}
	if err := json.Unmarshal([]byte(tokenStr), &token); err != nil {
		return "", fmt.Errorf("failed to unmarshal service account tokens, error: %w", err)
	}
	if token.APIAzureADTokenExchange.Token == "" {
		return "", fmt.Errorf("token for audience %s not found", DefaultTokenAudience)
	}
	return token.APIAzureADTokenExchange.Token, nil
}

func (az *AccountRepo) getStorageAccountWithCache(ctx context.Context, subsID, resourceGroup, account string) (armstorage.Account, error) {
	if az.ComputeClientFactory == nil {
		return armstorage.Account{}, fmt.Errorf("ComputeClientFactory is nil")
	}
	if az.storageAccountCache == nil {
		return armstorage.Account{}, fmt.Errorf("storage account cache is nil")
	}
	// search in cache first
	cache, err := az.storageAccountCache.Get(ctx, account, cache.CacheReadTypeDefault)
	if err != nil {
		return armstorage.Account{}, err
	}
	if cache != nil {
		klog.V(2).Infof("Get storage account(%s) from cache", account)
		return *cache, nil
	}

	accountClient, err := az.ComputeClientFactory.GetAccountClientForSub(subsID)
	if err != nil {
		return armstorage.Account{}, err
	}
	result, err := accountClient.GetProperties(ctx, resourceGroup, account, nil)
	if err != nil {
		return armstorage.Account{}, err
	}
	az.storageAccountCache.Set(account, result)
	return *result, nil
}

func (az *AccountRepo) GetStorageAccesskeyFromServiceAccountToken(ctx context.Context, subsID, accountName, rgName, clientID, tenantID, serviceAccountToken string) (string, error) {
	cred, err := azidentity.NewClientAssertionCredential(tenantID, clientID, func(context.Context) (string, error) {
		return parseServiceAccountToken(serviceAccountToken)
	}, &azidentity.ClientAssertionCredentialOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to create client assertion credential, error: %w", err)
	}

	client, err := accountclient.New(subsID, cred, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create storage account client, error: %w", err)
	}

	keys, err := client.ListKeys(ctx, rgName, accountName)
	if err != nil {
		return "", fmt.Errorf("failed to list keys, error: %w", err)
	}

	for _, k := range keys {
		if k != nil && k.Value != nil && *k.Value != "" {
			v := *k.Value
			if ind := strings.LastIndex(v, " "); ind >= 0 {
				v = v[(ind + 1):]
			}
			// get first key
			return v, nil
		}
	}

	return "", fmt.Errorf("failed to list keys, found no key")
}

// GetStorageAccesskey gets the storage account access key
// getLatestAccountKey: get the latest account key per CreationTime if true, otherwise get the first account key
func (az *AccountRepo) GetStorageAccesskey(ctx context.Context, accountClient accountclient.Interface, account, resourceGroup string, getLatestAccountKey bool) (string, error) {
	result, err := accountClient.ListKeys(ctx, resourceGroup, account)
	if err != nil {
		return "", err
	}
	if len(result) == 0 {
		return "", fmt.Errorf("empty keys")
	}

	var key string
	var creationTime time.Time

	for _, k := range result {
		if k.Value != nil && *k.Value != "" {
			v := *k.Value
			if ind := strings.LastIndex(v, " "); ind >= 0 {
				v = v[(ind + 1):]
			}
			if !getLatestAccountKey {
				// get first key
				return v, nil
			}
			// get account key with latest CreationTime
			if key == "" {
				key = v
				if k.CreationTime != nil {
					creationTime = *k.CreationTime
				}
				klog.V(2).Infof("got storage account key with creation time: %v", creationTime)
			} else {
				if k.CreationTime != nil && creationTime.Before(*k.CreationTime) {
					key = v
					creationTime = *k.CreationTime
					klog.V(2).Infof("got storage account key with latest creation time: %v", creationTime)
				}
			}
		}
	}

	if key == "" {
		return "", fmt.Errorf("no valid keys")
	}
	return key, nil
}

// EnsureStorageAccount search storage account, create one storage account(with genAccountNamePrefix) if not found, return accountName, accountKey
func (az *AccountRepo) EnsureStorageAccount(ctx context.Context, accountOptions *AccountOptions, genAccountNamePrefix string) (string, string, error) {
	if accountOptions == nil {
		return "", "", fmt.Errorf("account options is nil")
	}

	accountName := accountOptions.Name
	accountType := accountOptions.Type
	accountKind := accountOptions.Kind
	resourceGroup := accountOptions.ResourceGroup
	location := accountOptions.Location
	enableHTTPSTrafficOnly := accountOptions.EnableHTTPSTrafficOnly
	vnetResourceGroup := accountOptions.VNetResourceGroup
	vnetName := accountOptions.VNetName
	if vnetName == "" {
		vnetName = az.VnetName
	}

	subnetName := accountOptions.SubnetName
	if subnetName == "" {
		subnetName = az.SubnetName
	}

	if accountOptions.SubscriptionID != "" && !strings.EqualFold(accountOptions.SubscriptionID, az.Config.SubscriptionID) && accountOptions.ResourceGroup == "" {
		return "", "", fmt.Errorf("resourceGroup must be specified when subscriptionID(%s) is not empty", accountOptions.SubscriptionID)
	}

	subsID := az.Config.SubscriptionID
	if accountOptions.SubscriptionID != "" {
		subsID = accountOptions.SubscriptionID
	}

	if location == "" {
		location = az.Location
	}

	privateDNSZoneName := defaultPrivateDNSZoneName
	if accountOptions.PrivateDNSZoneName != "" {
		privateDNSZoneName = accountOptions.PrivateDNSZoneName
	}
	if ptr.Deref(accountOptions.CreatePrivateEndpoint, false) {
		if accountOptions.StorageType == "" {
			klog.V(2).Info("set StorageType as file when not specified")
			accountOptions.StorageType = StorageTypeFile
		}

		if len(accountOptions.StorageEndpointSuffix) == 0 && az.Environment != nil {
			accountOptions.StorageEndpointSuffix = az.Environment.StorageEndpointSuffix
		}
		privateDNSZoneName = fmt.Sprintf(privateDNSZoneNameFmt, privateDNSZoneName, accountOptions.StorageType, accountOptions.StorageEndpointSuffix)
	}

	if len(accountOptions.Tags) == 0 {
		accountOptions.Tags = make(map[string]string)
	}
	// set built-in tags
	accountOptions.Tags[consts.CreatedByTag] = "azure"

	var createNewAccount bool
	clientFactory := az.ComputeClientFactory
	if clientFactory == nil {
		return "", "", fmt.Errorf("clientFactory is nil")
	}
	storageAccountClient, err := clientFactory.GetAccountClientForSub(accountOptions.SubscriptionID)
	if err != nil {
		return "", "", err
	}

	if storageAccountClient == nil {
		return "", "", fmt.Errorf("StorageAccountClient is nil")
	}
	if len(accountName) == 0 {
		createNewAccount = true
		if !accountOptions.CreateAccount {
			// find a storage account that matches accountType
			accounts, err := az.getStorageAccounts(ctx, storageAccountClient, accountOptions)
			if err != nil {
				return "", "", fmt.Errorf("could not list storage accounts for account type %s: %w", accountType, err)
			}

			if len(accounts) > 0 {
				klog.V(4).Infof("found %d matching accounts", len(accounts))
				index := 0
				if accountOptions.PickRandomMatchingAccount {
					// randomly pick one matching account
					n, err := rand.Int(rand.Reader, big.NewInt(int64(len(accounts))))
					if err != nil || n == nil {
						return "", "", err
					}
					index = int(n.Int64())
					klog.V(4).Infof("randomly pick one matching account, index: %d, matching accounts: %s", index, accounts)
				}
				accountName = accounts[index].Name
				createNewAccount = false
				if accountOptions.SourceAccountName != "" {
					klog.V(4).Infof("source account name(%s) is provided, try to find a matching account with source account name", accountOptions.SourceAccountName)
					for _, acct := range accounts {
						if acct.Name == accountOptions.SourceAccountName {
							klog.V(2).Infof("found a matching account %s type %s location %s with source account name", acct.Name, acct.StorageType, acct.Location)
							accountName = acct.Name
							break
						}
					}
				}
				klog.V(4).Infof("found a matching account %s with account index %d", accountName, index)
			}
		}

		if len(accountName) == 0 {
			accountName = generateStorageAccountName(genAccountNamePrefix)
		}
	} else {
		createNewAccount = false
		if accountOptions.CreateAccount {
			// check whether account exists
			if _, err := az.GetStorageAccesskey(ctx, storageAccountClient, accountName, resourceGroup, accountOptions.GetLatestAccountKey); err != nil {
				klog.V(2).Infof("get storage key for storage account %s returned with %v", accountName, err)
				createNewAccount = true
			}
		}
	}

	if vnetResourceGroup == "" {
		vnetResourceGroup = az.ResourceGroup
		if len(az.VnetResourceGroup) > 0 {
			vnetResourceGroup = az.VnetResourceGroup
		}
	}

	if ptr.Deref(accountOptions.CreatePrivateEndpoint, false) {
		clientFactory := az.NetworkClientFactory
		if clientFactory == nil {
			// multi-tenant support
			clientFactory = az.ComputeClientFactory
		}
		if _, err := clientFactory.GetPrivateZoneClient().Get(ctx, vnetResourceGroup, privateDNSZoneName); err != nil {
			if strings.Contains(err.Error(), consts.ResourceNotFoundMessageCode) {
				// Create DNS zone first, this could make sure driver has write permission on vnetResourceGroup
				if err := az.createPrivateDNSZone(ctx, vnetResourceGroup, privateDNSZoneName); err != nil {
					return "", "", fmt.Errorf("create private DNS zone(%s) in resourceGroup(%s): %w", privateDNSZoneName, vnetResourceGroup, err)
				}
			} else {
				return "", "", fmt.Errorf("get private dns zone %s returned with %v", privateDNSZoneName, err.Error())
			}
		}

		// Create virtual link to the private DNS zone
		vNetLinkName := vnetName + "-vnetlink"
		if _, err := clientFactory.GetVirtualNetworkLinkClient().Get(ctx, vnetResourceGroup, privateDNSZoneName, vNetLinkName); err != nil {
			if strings.Contains(err.Error(), consts.ResourceNotFoundMessageCode) {
				if err := az.createVNetLink(ctx, vNetLinkName, vnetResourceGroup, vnetName, privateDNSZoneName); err != nil {
					return "", "", fmt.Errorf("create virtual link for vnet(%s) and DNS Zone(%s) in resourceGroup(%s): %w", vnetName, privateDNSZoneName, vnetResourceGroup, err)
				}
			} else {
				return "", "", fmt.Errorf("get virtual link for vnet(%s) and DNS Zone(%s) in resourceGroup(%s) returned with %w", vnetName, privateDNSZoneName, vnetResourceGroup, err)
			}
		}
	}

	if createNewAccount {
		// set network rules for storage account
		var networkRuleSet *armstorage.NetworkRuleSet
		virtualNetworkRules := []*armstorage.VirtualNetworkRule{}
		for i, subnetID := range accountOptions.VirtualNetworkResourceIDs {
			vnetRule := &armstorage.VirtualNetworkRule{
				VirtualNetworkResourceID: &accountOptions.VirtualNetworkResourceIDs[i],
				Action:                   to.Ptr(string(armstorage.DefaultActionAllow)),
			}
			virtualNetworkRules = append(virtualNetworkRules, vnetRule)
			klog.V(4).Infof("subnetID(%s) has been set", subnetID)
		}
		if len(virtualNetworkRules) > 0 {
			networkRuleSet = &armstorage.NetworkRuleSet{
				VirtualNetworkRules: virtualNetworkRules,
				DefaultAction:       to.Ptr(armstorage.DefaultActionDeny),
			}
		}

		if ptr.Deref(accountOptions.CreatePrivateEndpoint, false) {
			networkRuleSet = &armstorage.NetworkRuleSet{
				DefaultAction: to.Ptr(armstorage.DefaultActionDeny),
			}
		}

		if accountType == "" {
			accountType = DefaultStorageAccountType
		}

		// use StorageV2 by default per https://docs.microsoft.com/en-us/azure/storage/common/storage-account-options
		kind := DefaultStorageAccountKind
		if accountKind != "" {
			kind = armstorage.Kind(accountKind)
		}
		tags := convertMapToMapPointer(accountOptions.Tags)

		klog.V(2).Infof("azure - no matching account found, begin to create a new account %s in resource group %s, location: %s, accountType: %s, accountKind: %s, tags: %+v",
			accountName, resourceGroup, location, accountType, kind, accountOptions.Tags)

		cp := &armstorage.AccountCreateParameters{
			SKU:  &armstorage.SKU{Name: to.Ptr(armstorage.SKUName(accountType))},
			Kind: to.Ptr(kind),
			Properties: &armstorage.AccountPropertiesCreateParameters{
				EnableHTTPSTrafficOnly: &enableHTTPSTrafficOnly,
				NetworkRuleSet:         networkRuleSet,
				IsHnsEnabled:           accountOptions.IsHnsEnabled,
				EnableNfsV3:            accountOptions.EnableNfsV3,
				MinimumTLSVersion:      to.Ptr(armstorage.MinimumTLSVersionTLS12),
			},
			Tags:     tags,
			Location: &location}

		if accountOptions.EnableLargeFileShare != nil {
			state := armstorage.LargeFileSharesStateDisabled
			if *accountOptions.EnableLargeFileShare {
				state = armstorage.LargeFileSharesStateEnabled
			}
			klog.V(2).Infof("enable LargeFileShare(%s) for storage account(%s)", state, accountName)
			cp.Properties.LargeFileSharesState = to.Ptr(state)
		}
		if accountOptions.AllowBlobPublicAccess != nil {
			klog.V(2).Infof("set AllowBlobPublicAccess(%v) for storage account(%s)", *accountOptions.AllowBlobPublicAccess, accountName)
			cp.Properties.AllowBlobPublicAccess = accountOptions.AllowBlobPublicAccess
		}
		if accountOptions.RequireInfrastructureEncryption != nil {
			klog.V(2).Infof("set RequireInfrastructureEncryption(%v) for storage account(%s)", *accountOptions.RequireInfrastructureEncryption, accountName)
			cp.Properties.Encryption = &armstorage.Encryption{
				RequireInfrastructureEncryption: accountOptions.RequireInfrastructureEncryption,
				KeySource:                       to.Ptr(armstorage.KeySourceMicrosoftStorage),
				Services: &armstorage.EncryptionServices{
					File: &armstorage.EncryptionService{Enabled: ptr.To(true)},
					Blob: &armstorage.EncryptionService{Enabled: ptr.To(true)},
				},
			}
		}
		if accountOptions.AllowSharedKeyAccess != nil {
			klog.V(2).Infof("set Allow SharedKeyAccess (%v) for storage account (%s)", *accountOptions.AllowSharedKeyAccess, accountName)
			cp.Properties.AllowSharedKeyAccess = accountOptions.AllowSharedKeyAccess
		}
		if accountOptions.KeyVaultURI != nil {
			klog.V(2).Infof("set KeyVault(%v) for storage account(%s)", accountOptions.KeyVaultURI, accountName)
			cp.Properties.Encryption = &armstorage.Encryption{
				KeyVaultProperties: &armstorage.KeyVaultProperties{
					KeyName:     accountOptions.KeyName,
					KeyVersion:  accountOptions.KeyVersion,
					KeyVaultURI: accountOptions.KeyVaultURI,
				},
				KeySource: to.Ptr(armstorage.KeySourceMicrosoftKeyvault),
				Services: &armstorage.EncryptionServices{
					File: &armstorage.EncryptionService{Enabled: ptr.To(true)},
					Blob: &armstorage.EncryptionService{Enabled: ptr.To(true)},
				},
			}
		}

		if _, rerr := storageAccountClient.Create(ctx, resourceGroup, accountName, cp); rerr != nil {
			return "", "", fmt.Errorf("failed to create storage account %s, error: %w", accountName, rerr)
		}

		if ptr.Deref(accountOptions.EnableBlobVersioning, false) ||
			accountOptions.SoftDeleteBlobs > 0 ||
			accountOptions.SoftDeleteContainers > 0 {
			var blobPolicy, containerPolicy *armstorage.DeleteRetentionPolicy
			var enableBlobVersioning *bool

			if accountOptions.SoftDeleteContainers > 0 {
				containerPolicy = &armstorage.DeleteRetentionPolicy{
					Enabled: ptr.To(accountOptions.SoftDeleteContainers > 0),
					Days:    ptr.To(accountOptions.SoftDeleteContainers),
				}
			}
			if accountOptions.SoftDeleteBlobs > 0 {
				blobPolicy = &armstorage.DeleteRetentionPolicy{
					Enabled: ptr.To(accountOptions.SoftDeleteBlobs > 0),
					Days:    ptr.To(accountOptions.SoftDeleteBlobs),
				}
			}

			if accountOptions.EnableBlobVersioning != nil {
				enableBlobVersioning = ptr.To(*accountOptions.EnableBlobVersioning)
			}

			property := armstorage.BlobServiceProperties{
				BlobServiceProperties: &armstorage.BlobServicePropertiesProperties{
					IsVersioningEnabled:            enableBlobVersioning,
					ContainerDeleteRetentionPolicy: containerPolicy,
					DeleteRetentionPolicy:          blobPolicy,
				},
			}
			blobserviceClient, err := clientFactory.GetBlobServicePropertiesClientForSub(subsID)
			if err != nil {
				return "", "", fmt.Errorf("failed to get blob service properties client, error: %w", err)
			}

			if _, err := blobserviceClient.Set(ctx, resourceGroup, accountName, property); err != nil {
				return "", "", fmt.Errorf("failed to set blob service properties for storage account %s, error: %w", accountName, err)
			}
		}

		if accountOptions.DisableFileServiceDeleteRetentionPolicy != nil || accountOptions.IsMultichannelEnabled != nil {
			prop, err := az.fileServiceRepo.Get(ctx, accountOptions.SubscriptionID, accountOptions.ResourceGroup, accountName)
			if err != nil {
				return "", "", err
			}
			if prop.FileServiceProperties == nil {
				return "", "", fmt.Errorf("FileServicePropertiesProperties of account(%s), subscription(%s), resource group(%s) is nil", accountName, subsID, resourceGroup)
			}
			prop.FileServiceProperties.ProtocolSettings = nil
			prop.FileServiceProperties.Cors = nil
			if accountOptions.DisableFileServiceDeleteRetentionPolicy != nil {
				enable := !*accountOptions.DisableFileServiceDeleteRetentionPolicy
				klog.V(2).Infof("set ShareDeleteRetentionPolicy(%v) on account(%s), subscription(%s), resource group(%s)",
					enable, accountName, subsID, resourceGroup)
				prop.FileServiceProperties.ShareDeleteRetentionPolicy = &armstorage.DeleteRetentionPolicy{Enabled: &enable}
			}
			if accountOptions.IsMultichannelEnabled != nil {
				klog.V(2).Infof("enable SMB Multichannel setting on account(%s), subscription(%s), resource group(%s)", accountName, subsID, resourceGroup)
				enabled := *accountOptions.IsMultichannelEnabled
				prop.FileServiceProperties.ProtocolSettings = &armstorage.ProtocolSettings{Smb: &armstorage.SmbSetting{Multichannel: &armstorage.Multichannel{Enabled: &enabled}}}
			}

			if err := az.fileServiceRepo.Set(ctx, subsID, resourceGroup, accountName, prop); err != nil {
				return "", "", err
			}
		}

		if accountOptions.AccessTier != "" {
			klog.V(2).Infof("set AccessTier(%s) on account(%s), subscription(%s), resource group(%s)", accountOptions.AccessTier, accountName, subsID, resourceGroup)
			cp.Properties.AccessTier = to.Ptr(armstorage.AccessTier(accountOptions.AccessTier))
		}
	}

	if ptr.Deref(accountOptions.CreatePrivateEndpoint, false) {
		// Get properties of the storageAccount
		clientFactory := az.ComputeClientFactory
		if clientFactory == nil {
			return "", "", fmt.Errorf("clientFactory is nil")
		}

		storageAccount, err := storageAccountClient.GetProperties(ctx, resourceGroup, accountName, nil)
		if err != nil {
			return "", "", fmt.Errorf("failed to get the properties of storage account(%s), resourceGroup(%s), error: %w", accountName, resourceGroup, err)
		}

		// Create private endpoint
		privateEndpointName := accountName + "-pvtendpoint"
		if accountOptions.StorageType == StorageTypeBlob {
			privateEndpointName = privateEndpointName + blobNameSuffix
		}
		if err := az.createPrivateEndpoint(ctx, accountName, storageAccount.ID, privateEndpointName, vnetResourceGroup, vnetName, subnetName, location, accountOptions.StorageType); err != nil {
			return "", "", fmt.Errorf("create private endpoint for storage account(%s), resourceGroup(%s): %w", accountName, vnetResourceGroup, err)
		}

		// Create dns zone group
		dnsZoneGroupName := accountName + "-dnszonegroup"
		if accountOptions.StorageType == StorageTypeBlob {
			dnsZoneGroupName = dnsZoneGroupName + blobNameSuffix
		}
		if err := az.createPrivateDNSZoneGroup(ctx, dnsZoneGroupName, privateEndpointName, vnetResourceGroup, vnetName, privateDNSZoneName); err != nil {
			return "", "", fmt.Errorf("create private DNS zone group - privateEndpoint(%s), vNetName(%s), resourceGroup(%s): %w", privateEndpointName, vnetName, vnetResourceGroup, err)
		}
	}

	// find the access key with this account
	accountKey, err := az.GetStorageAccesskey(ctx, storageAccountClient, accountName, resourceGroup, accountOptions.GetLatestAccountKey)
	if err != nil {
		return "", "", fmt.Errorf("could not get storage key for storage account %s: %w", accountName, err)
	}

	return accountName, accountKey, nil
}

func (az *AccountRepo) createPrivateEndpoint(ctx context.Context, accountName string, accountID *string, privateEndpointName, vnetResourceGroup, vnetName, subnetName, location string, storageType Type) error {
	klog.V(2).Infof("Creating private endpoint(%s) for account (%s)", privateEndpointName, accountName)

	subnet, err := az.subnetRepo.Get(ctx, vnetResourceGroup, vnetName, subnetName)
	if err != nil {
		return err
	}
	if subnet.Properties == nil {
		klog.Errorf("Properties of (%s, %s) is nil", vnetName, subnetName)
	} else {
		// Disable the private endpoint network policies before creating private endpoint
		subnet.Properties.PrivateEndpointNetworkPolicies = to.Ptr(armnetwork.VirtualNetworkPrivateEndpointNetworkPoliciesDisabled)
	}

	if err := az.subnetRepo.CreateOrUpdate(ctx, vnetResourceGroup, vnetName, subnetName, *subnet); err != nil {
		return err
	}

	//Create private endpoint
	privateLinkServiceConnectionName := accountName + "-pvtsvcconn"
	if storageType == StorageTypeBlob {
		privateLinkServiceConnectionName = privateLinkServiceConnectionName + blobNameSuffix
	}
	privateLinkServiceConnection := &armnetwork.PrivateLinkServiceConnection{
		Name: &privateLinkServiceConnectionName,
		Properties: &armnetwork.PrivateLinkServiceConnectionProperties{
			GroupIDs:             to.SliceOfPtrs(string(storageType)),
			PrivateLinkServiceID: accountID,
		},
	}
	privateLinkServiceConnections := []*armnetwork.PrivateLinkServiceConnection{privateLinkServiceConnection}
	privateEndpoint := armnetwork.PrivateEndpoint{
		Location:   &location,
		Properties: &armnetwork.PrivateEndpointProperties{Subnet: subnet, PrivateLinkServiceConnections: privateLinkServiceConnections},
	}
	clientFactory := az.NetworkClientFactory
	if clientFactory == nil {
		// multi-tenant support
		clientFactory = az.ComputeClientFactory
	}

	_, err = clientFactory.GetPrivateEndpointClient().CreateOrUpdate(ctx, vnetResourceGroup, privateEndpointName, privateEndpoint)
	return err
}

func (az *AccountRepo) createPrivateDNSZone(ctx context.Context, vnetResourceGroup, privateDNSZoneName string) error {
	klog.V(2).Infof("Creating private dns zone(%s) in resourceGroup (%s)", privateDNSZoneName, vnetResourceGroup)
	location := LocationGlobal
	privateDNSZone := privatedns.PrivateZone{Location: &location}
	clientFactory := az.NetworkClientFactory
	if clientFactory == nil {
		// multi-tenant support
		clientFactory = az.ComputeClientFactory
	}
	privatednsclient := clientFactory.GetPrivateZoneClient()

	if _, err := privatednsclient.CreateOrUpdate(ctx, vnetResourceGroup, privateDNSZoneName, privateDNSZone); err != nil {
		if strings.Contains(err.Error(), "exists already") {
			klog.V(2).Infof("private dns zone(%s) in resourceGroup (%s) already exists", privateDNSZoneName, vnetResourceGroup)
			return nil
		}
		return err
	}
	return nil
}

func (az *AccountRepo) createVNetLink(ctx context.Context, vNetLinkName, vnetResourceGroup, vnetName, privateDNSZoneName string) error {
	klog.V(2).Infof("Creating virtual link for vnet(%s) and DNS Zone(%s) in resourceGroup(%s)", vNetLinkName, privateDNSZoneName, vnetResourceGroup)
	clientFactory := az.NetworkClientFactory
	if clientFactory == nil {
		// multi-tenant support
		clientFactory = az.ComputeClientFactory
	}
	vnetLinkClient := clientFactory.GetVirtualNetworkLinkClient()

	location := LocationGlobal
	vnetID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/virtualNetworks/%s", az.SubscriptionID, vnetResourceGroup, vnetName)
	parameters := privatedns.VirtualNetworkLink{
		Location: &location,
		Properties: &privatedns.VirtualNetworkLinkProperties{
			VirtualNetwork:      &privatedns.SubResource{ID: &vnetID},
			RegistrationEnabled: ptr.To(false)},
	}
	_, err := vnetLinkClient.CreateOrUpdate(ctx, vnetResourceGroup, privateDNSZoneName, vNetLinkName, parameters)
	return err
}

func (az *AccountRepo) createPrivateDNSZoneGroup(ctx context.Context, dnsZoneGroupName, privateEndpointName, vnetResourceGroup, vnetName, privateDNSZoneName string) error {
	klog.V(2).Infof("Creating private DNS zone group(%s) with privateEndpoint(%s), vNetName(%s), resourceGroup(%s)", dnsZoneGroupName, privateEndpointName, vnetName, vnetResourceGroup)
	privateDNSZoneGroup := &armnetwork.PrivateDNSZoneGroup{
		Properties: &armnetwork.PrivateDNSZoneGroupPropertiesFormat{
			PrivateDNSZoneConfigs: []*armnetwork.PrivateDNSZoneConfig{
				{
					Name: &privateDNSZoneName,
					Properties: &armnetwork.PrivateDNSZonePropertiesFormat{
						PrivateDNSZoneID: to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/privateDnsZones/%s", az.SubscriptionID, vnetResourceGroup, privateDNSZoneName)),
					},
				},
			},
		},
	}
	clientFactory := az.NetworkClientFactory
	if clientFactory == nil {
		clientFactory = az.ComputeClientFactory
	}
	privatednsgroupClient := clientFactory.GetPrivateDNSZoneGroupClient()
	_, err := privatednsgroupClient.CreateOrUpdate(ctx, vnetResourceGroup, privateEndpointName, dnsZoneGroupName, *privateDNSZoneGroup)
	return err
}

// AddStorageAccountTags add tags to storage account
func (az *AccountRepo) AddStorageAccountTags(ctx context.Context, subsID, resourceGroup, account string, tags map[string]*string) error {
	// add lock to avoid concurrent update on the cache
	az.lockMap.LockEntry(account)
	defer az.lockMap.UnlockEntry(account)

	result, err := az.getStorageAccountWithCache(ctx, subsID, resourceGroup, account)
	if err != nil {
		return err
	}

	// merge two tag map into one
	newTags := make(map[string]*string)
	for k, v := range tags {
		newTags[k] = v
	}
	for k, v := range result.Tags {
		newTags[k] = v
	}

	if len(newTags) > len(result.Tags) {
		// only update when newTags is different from old tags
		_ = az.storageAccountCache.Delete(account) // clean cache
		updateParams := &armstorage.AccountUpdateParameters{Tags: newTags}
		klog.V(2).Infof("add storage account(%s) with tags(%+v)", account, newTags)
		accountClient, err := az.ComputeClientFactory.GetAccountClientForSub(subsID)
		if err != nil {
			return err
		}
		_, err = accountClient.Update(ctx, resourceGroup, account, updateParams)
		return err
	}
	return nil
}

// RemoveStorageAccountTag remove tag from storage account
func (az *AccountRepo) RemoveStorageAccountTag(ctx context.Context, subsID, resourceGroup, account, key string) error {
	// add lock to avoid concurrent update on the cache
	az.lockMap.LockEntry(account)
	defer az.lockMap.UnlockEntry(account)

	result, rerr := az.getStorageAccountWithCache(ctx, subsID, resourceGroup, account)
	if rerr != nil {
		return rerr
	}

	if len(result.Tags) == 0 {
		return nil
	}

	originalLen := len(result.Tags)
	delete(result.Tags, key)
	if originalLen != len(result.Tags) {
		// only update when newTags is different from old tags
		_ = az.storageAccountCache.Delete(account) // clean cache
		updateParams := &armstorage.AccountUpdateParameters{Tags: result.Tags}
		klog.V(2).Infof("remove tag(%s) from storage account(%s)", key, account)
		accountClient, err := az.ComputeClientFactory.GetAccountClientForSub(subsID)
		if err != nil {
			return err
		}
		_, err = accountClient.Update(ctx, resourceGroup, account, updateParams)
		return err
	}
	return nil
}

func isStorageTypeEqual(account *armstorage.Account, accountOptions *AccountOptions) bool {
	if accountOptions.Type != "" && !strings.EqualFold(accountOptions.Type, string(*account.SKU.Name)) {
		return false
	}
	return true
}

func isAccountKindEqual(account *armstorage.Account, accountOptions *AccountOptions) bool {
	if accountOptions.Kind != "" && !strings.EqualFold(accountOptions.Kind, string(*account.Kind)) {
		return false
	}
	return true
}

func isLocationEqual(account *armstorage.Account, accountOptions *AccountOptions) bool {
	if accountOptions.Location != "" && !strings.EqualFold(accountOptions.Location, *account.Location) {
		return false
	}
	return true
}

func AreVNetRulesEqual(account *armstorage.Account, accountOptions *AccountOptions) bool {
	if len(accountOptions.VirtualNetworkResourceIDs) > 0 {
		if account.Properties == nil || account.Properties.NetworkRuleSet == nil ||
			account.Properties.NetworkRuleSet.VirtualNetworkRules == nil {
			return false
		}

		for _, subnetID := range accountOptions.VirtualNetworkResourceIDs {
			found := false
			for _, rule := range account.Properties.NetworkRuleSet.VirtualNetworkRules {
				if strings.EqualFold(ptr.Deref(rule.VirtualNetworkResourceID, ""), subnetID) && strings.EqualFold(*rule.Action, string(armstorage.DefaultActionAllow)) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		klog.V(2).Infof("found all vnet rules(%v) in account %s", accountOptions.VirtualNetworkResourceIDs, ptr.Deref(account.Name, ""))
	}
	return true
}

func isLargeFileSharesPropertyEqual(account *armstorage.Account, accountOptions *AccountOptions) bool {
	if accountOptions.EnableLargeFileShare == nil {
		return true
	}
	if *accountOptions.EnableLargeFileShare {
		return account.Properties.LargeFileSharesState != nil && *account.Properties.LargeFileSharesState == armstorage.LargeFileSharesStateEnabled
	}
	return account.Properties.LargeFileSharesState == nil || *account.Properties.LargeFileSharesState == armstorage.LargeFileSharesStateDisabled
}

func isTaggedWithSkip(account *armstorage.Account) bool {
	if account.Tags != nil {
		// skip account with SkipMatchingTag tag
		if _, ok := account.Tags[SkipMatchingTag]; ok {
			klog.V(2).Infof("found %s tag for account %s, skip matching", SkipMatchingTag, ptr.Deref(account.Name, ""))
			return false
		}
	}
	return true
}

func isTagsEqual(account *armstorage.Account, accountOptions *AccountOptions) bool {
	if !accountOptions.MatchTags {
		// always return true when tags matching is false (by default)
		return true
	}

	// nil and empty map should be regarded as equal
	if len(account.Tags) == 0 && len(accountOptions.Tags) == 0 {
		return true
	}

	if len(account.Tags) < len(accountOptions.Tags) {
		return false
	}

	// ensure all tags in accountOptions are in account
	for k, v := range accountOptions.Tags {
		if ptr.Deref(account.Tags[k], "") != v {
			return false
		}
	}
	return true
}

func isHnsPropertyEqual(account *armstorage.Account, accountOptions *AccountOptions) bool {
	return ptr.Deref(accountOptions.IsHnsEnabled, false) == ptr.Deref(account.Properties.IsHnsEnabled, false)
}

func isEnableNfsV3PropertyEqual(account *armstorage.Account, accountOptions *AccountOptions) bool {
	return ptr.Deref(accountOptions.EnableNfsV3, false) == ptr.Deref(account.Properties.EnableNfsV3, false)
}

func isEnableHTTPSTrafficOnlyEqual(account *armstorage.Account, accountOptions *AccountOptions) bool {
	return accountOptions.EnableHTTPSTrafficOnly == ptr.Deref(account.Properties.EnableHTTPSTrafficOnly, true)
}

func isPrivateEndpointAsExpected(account *armstorage.Account, accountOptions *AccountOptions) bool {
	if accountOptions.CreatePrivateEndpoint == nil {
		// CreatePrivateEndpoint is not set, match current account
		return true
	}

	if ptr.Deref(accountOptions.CreatePrivateEndpoint, false) && account.Properties.PrivateEndpointConnections != nil && len(account.Properties.PrivateEndpointConnections) > 0 {
		return true
	}
	if !ptr.Deref(accountOptions.CreatePrivateEndpoint, false) && len(account.Properties.PrivateEndpointConnections) == 0 {
		return true
	}
	return false
}

func isAllowBlobPublicAccessEqual(account *armstorage.Account, accountOptions *AccountOptions) bool {
	return ptr.Deref(accountOptions.AllowBlobPublicAccess, true) == ptr.Deref(account.Properties.AllowBlobPublicAccess, true)
}

func isRequireInfrastructureEncryptionEqual(account *armstorage.Account, accountOptions *AccountOptions) bool {
	requireInfraEncryption := ptr.Deref(accountOptions.RequireInfrastructureEncryption, false)
	if account.Properties.Encryption == nil {
		return !requireInfraEncryption
	}
	return requireInfraEncryption == ptr.Deref(account.Properties.Encryption.RequireInfrastructureEncryption, false)
}

func isAllowSharedKeyAccessEqual(account *armstorage.Account, accountOptions *AccountOptions) bool {
	return ptr.Deref(accountOptions.AllowSharedKeyAccess, true) == ptr.Deref(account.Properties.AllowSharedKeyAccess, true)
}

func isAccessTierEqual(account *armstorage.Account, accountOptions *AccountOptions) bool {
	if accountOptions.AccessTier == "" {
		return true
	}
	return account != nil && account.Properties != nil && account.Properties.AccessTier != nil && accountOptions.AccessTier == string(*account.Properties.AccessTier)
}

func (az *AccountRepo) isMultichannelEnabledEqual(ctx context.Context, account *armstorage.Account, accountOptions *AccountOptions) (bool, error) {
	if accountOptions.IsMultichannelEnabled == nil {
		return true, nil
	}

	if account.Name == nil {
		klog.Warningf("account.Name under resource group(%s) is nil", accountOptions.ResourceGroup)
		return false, nil
	}

	prop, err := az.fileServiceRepo.Get(ctx, accountOptions.SubscriptionID, accountOptions.ResourceGroup, ptr.Deref(account.Name, ""))
	if err != nil {
		return false, err
	}

	if prop.FileServiceProperties == nil ||
		prop.FileServiceProperties.ProtocolSettings == nil ||
		prop.FileServiceProperties.ProtocolSettings.Smb == nil ||
		prop.FileServiceProperties.ProtocolSettings.Smb.Multichannel == nil {
		return !*accountOptions.IsMultichannelEnabled, nil
	}

	return *accountOptions.IsMultichannelEnabled == ptr.Deref(prop.FileServiceProperties.ProtocolSettings.Smb.Multichannel.Enabled, false), nil
}

func (az *AccountRepo) isDisableFileServiceDeleteRetentionPolicyEqual(ctx context.Context, account *armstorage.Account, accountOptions *AccountOptions) (bool, error) {
	if accountOptions.DisableFileServiceDeleteRetentionPolicy == nil {
		return true, nil
	}

	if account.Name == nil {
		klog.Warningf("account.Name under resource group(%s) is nil", accountOptions.ResourceGroup)
		return false, nil
	}
	prop, err := az.fileServiceRepo.Get(ctx, accountOptions.SubscriptionID, accountOptions.ResourceGroup, ptr.Deref(account.Name, ""))
	if err != nil {
		return false, err
	}

	if prop.FileServiceProperties == nil ||
		prop.FileServiceProperties.ShareDeleteRetentionPolicy == nil ||
		prop.FileServiceProperties.ShareDeleteRetentionPolicy.Enabled == nil {
		// by default, ShareDeleteRetentionPolicy.Enabled is true if it's nil
		return !*accountOptions.DisableFileServiceDeleteRetentionPolicy, nil
	}

	return *accountOptions.DisableFileServiceDeleteRetentionPolicy != *prop.FileServiceProperties.ShareDeleteRetentionPolicy.Enabled, nil
}

func (az *AccountRepo) isEnableBlobDataProtectionEqual(ctx context.Context, account *armstorage.Account, accountOptions *AccountOptions) (bool, error) {
	if accountOptions.SoftDeleteBlobs == 0 &&
		accountOptions.SoftDeleteContainers == 0 &&
		accountOptions.EnableBlobVersioning == nil {
		return true, nil
	}
	blobserviceClient, err := az.ComputeClientFactory.GetBlobServicePropertiesClientForSub(accountOptions.SubscriptionID)
	if err != nil {
		return false, err
	}
	property, err := blobserviceClient.Get(ctx, accountOptions.ResourceGroup, ptr.Deref(account.Name, ""))
	if err != nil {
		return false, err
	}

	return isSoftDeleteBlobsEqual(property, accountOptions) &&
		isSoftDeleteContainersEqual(property, accountOptions) &&
		isEnableBlobVersioningEqual(property, accountOptions), nil
}

func isSoftDeleteBlobsEqual(property *armstorage.BlobServiceProperties, accountOptions *AccountOptions) bool {
	wantEnable := accountOptions.SoftDeleteBlobs > 0
	actualEnable := property.BlobServiceProperties != nil && property.BlobServiceProperties.DeleteRetentionPolicy != nil &&
		ptr.Deref(property.BlobServiceProperties.DeleteRetentionPolicy.Enabled, false)
	if wantEnable != actualEnable {
		return false
	}
	if !actualEnable {
		return true
	}

	return accountOptions.SoftDeleteBlobs == ptr.Deref(property.BlobServiceProperties.DeleteRetentionPolicy.Days, 0)
}

func isSoftDeleteContainersEqual(property *armstorage.BlobServiceProperties, accountOptions *AccountOptions) bool {
	wantEnable := accountOptions.SoftDeleteContainers > 0
	actualEnable := property.BlobServiceProperties != nil && property.BlobServiceProperties.ContainerDeleteRetentionPolicy != nil &&
		ptr.Deref(property.BlobServiceProperties.ContainerDeleteRetentionPolicy.Enabled, false)
	if wantEnable != actualEnable {
		return false
	}
	if !actualEnable {
		return true
	}

	return accountOptions.SoftDeleteContainers == ptr.Deref(property.BlobServiceProperties.ContainerDeleteRetentionPolicy.Days, 0)
}

func isEnableBlobVersioningEqual(property *armstorage.BlobServiceProperties, accountOptions *AccountOptions) bool {
	return ptr.Deref(accountOptions.EnableBlobVersioning, false) == ptr.Deref(property.BlobServiceProperties.IsVersioningEnabled, false)
}

// get a storage account by UUID
func generateStorageAccountName(accountNamePrefix string) string {
	uniqueID := strings.Replace(uuid.NewString(), "-", "", -1)
	accountName := strings.ToLower(accountNamePrefix + uniqueID)
	if len(accountName) > consts.StorageAccountNameMaxLength {
		return accountName[:consts.StorageAccountNameMaxLength-1]
	}
	return accountName
}

func convertMapToMapPointer(origin map[string]string) map[string]*string {
	newly := make(map[string]*string)
	for k, v := range origin {
		value := v
		newly[k] = &value
	}
	return newly
}
