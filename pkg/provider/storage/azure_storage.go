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
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/Azure/azure-sdk-for-go/services/storage/mgmt/2021-09-01/storage"
	"k8s.io/apimachinery/pkg/util/uuid"

	"k8s.io/klog/v2"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/blobclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/diskclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/fileclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/privatednsclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/privatednszonegroupclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/privateendpointclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/snapshotclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/storageaccountclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/subnetclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/virtualnetworklinksclient"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/util"
)

type Provider struct {
	// a timed cache storing storage account properties to avoid querying storage account frequently
	storageAccountCache       azcache.Resource
	StorageAccountClient      storageaccountclient.Interface
	DisksClient               diskclient.Interface
	SnapshotsClient           snapshotclient.Interface
	FileClient                fileclient.Interface
	BlobClient                blobclient.Interface
	privatednsclient          privatednsclient.Interface
	virtualNetworkLinksClient virtualnetworklinksclient.Interface
	SubnetsClient             subnetclient.Interface
	privatednszonegroupclient privatednszonegroupclient.Interface
	privateendpointclient     privateendpointclient.Interface

	ResourceGroup     string
	SubscriptionID    string
	VnetName          string
	SubnetName        string
	VnetResourceGroup string
	Location          string
}

// CreateFileShare creates a file share, using a matching storage account type, account kind, etc.
// storage account will be created if specified account is not found
func (az *Provider) CreateFileShare(ctx context.Context, accountOptions *AccountOptions, shareOptions *fileclient.ShareOptions) (string, string, error) {
	if accountOptions == nil {
		return "", "", fmt.Errorf("account options is nil")
	}
	if shareOptions == nil {
		return "", "", fmt.Errorf("share options is nil")
	}
	if accountOptions.ResourceGroup == "" {
		accountOptions.ResourceGroup = az.ResourceGroup
	}
	if accountOptions.SubscriptionID == "" {
		accountOptions.SubscriptionID = az.SubscriptionID
	}

	accountOptions.EnableHTTPSTrafficOnly = true
	if shareOptions.Protocol == storage.EnabledProtocolsNFS {
		accountOptions.EnableHTTPSTrafficOnly = false
	}

	accountName, accountKey, err := az.EnsureStorageAccount(ctx, accountOptions, consts.FileShareAccountNamePrefix)
	if err != nil {
		return "", "", fmt.Errorf("could not get storage key for storage account %s: %w", accountOptions.Name, err)
	}

	if err := az.createFileShare(ctx, accountOptions.SubscriptionID, accountOptions.ResourceGroup, accountName, shareOptions); err != nil {
		return "", "", fmt.Errorf("failed to create share %s in account %s: %w", shareOptions.Name, accountName, err)
	}
	klog.V(4).Infof("created share %s in account %s", shareOptions.Name, accountOptions.Name)
	return accountName, accountKey, nil
}

// DeleteFileShare deletes a file share using storage account name and key
func (az *Provider) DeleteFileShare(ctx context.Context, subsID, resourceGroup, accountName, shareName string) error {
	if err := az.deleteFileShare(ctx, subsID, resourceGroup, accountName, shareName); err != nil {
		return err
	}
	klog.V(4).Infof("share %s deleted", shareName)
	return nil
}

// ResizeFileShare resizes a file share
func (az *Provider) ResizeFileShare(ctx context.Context, subsID, resourceGroup, accountName, name string, sizeGiB int) error {
	return az.resizeFileShare(ctx, subsID, resourceGroup, accountName, name, sizeGiB)
}

// GetFileShare gets a file share
func (az *Provider) GetFileShare(ctx context.Context, subsID, resourceGroupName, accountName, name string) (storage.FileShare, error) {
	return az.getFileShare(ctx, subsID, resourceGroupName, accountName, name)
}

// get a storage account by UUID
func generateStorageAccountName(accountNamePrefix string) string {
	uniqueID := strings.Replace(string(uuid.NewUUID()), "-", "", -1)
	accountName := strings.ToLower(accountNamePrefix + uniqueID)
	if len(accountName) > consts.StorageAccountNameMaxLength {
		return accountName[:consts.StorageAccountNameMaxLength-1]
	}
	return accountName
}

func (az *Provider) createPrivateEndpoint(ctx context.Context, accountName string, accountID *string, privateEndpointName, vnetResourceGroup, vnetName, subnetName, location string, storageType Type) error {
	klog.V(2).Infof("Creating private endpoint(%s) for account (%s)", privateEndpointName, accountName)

	subnet, _, err := az.getSubnet(vnetName, subnetName)
	if err != nil {
		return err
	}
	if subnet.SubnetPropertiesFormat == nil {
		klog.Errorf("SubnetPropertiesFormat of (%s, %s) is nil", vnetName, subnetName)
	} else {
		// Disable the private endpoint network policies before creating private endpoint
		subnet.SubnetPropertiesFormat.PrivateEndpointNetworkPolicies = network.VirtualNetworkPrivateEndpointNetworkPoliciesDisabled
	}
	if rerr := az.SubnetsClient.CreateOrUpdate(ctx, vnetResourceGroup, vnetName, subnetName, subnet); rerr != nil {
		return rerr.Error()
	}

	//Create private endpoint
	privateLinkServiceConnectionName := accountName + "-pvtsvcconn"
	if storageType == StorageTypeBlob {
		privateLinkServiceConnectionName = privateLinkServiceConnectionName + "-blob"
	}
	privateLinkServiceConnection := network.PrivateLinkServiceConnection{
		Name: &privateLinkServiceConnectionName,
		PrivateLinkServiceConnectionProperties: &network.PrivateLinkServiceConnectionProperties{
			GroupIds:             &[]string{string(storageType)},
			PrivateLinkServiceID: accountID,
		},
	}
	privateLinkServiceConnections := []network.PrivateLinkServiceConnection{privateLinkServiceConnection}
	privateEndpoint := network.PrivateEndpoint{
		Location:                  &location,
		PrivateEndpointProperties: &network.PrivateEndpointProperties{Subnet: &subnet, PrivateLinkServiceConnections: &privateLinkServiceConnections},
	}

	return az.privateendpointclient.CreateOrUpdate(ctx, vnetResourceGroup, privateEndpointName, privateEndpoint, "", true).Error()
}

func (az *Provider) getSubnet(virtualNetworkName string, subnetName string) (network.Subnet, bool, error) {
	var rg string
	if len(az.VnetResourceGroup) > 0 {
		rg = az.VnetResourceGroup
	} else {
		rg = az.ResourceGroup
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subnet, err := az.SubnetsClient.Get(ctx, rg, virtualNetworkName, subnetName, "")
	exists, rerr := util.CheckResourceExistsFromError(err)
	if rerr != nil {
		return subnet, false, rerr.Error()
	}

	if !exists {
		klog.V(2).Infof("Subnet %q not found", subnetName)
	}
	return subnet, exists, nil
}
