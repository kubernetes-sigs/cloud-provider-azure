package publicip

//go:generate mockgen -destination=./mock_repo.go -package=publicip -copyright_file ../../../hack/boilerplate/boilerplate.generatego.txt -source=repo.go Repository

import (
	"context"
	"fmt"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/publicipaddressclient"

	"sigs.k8s.io/cloud-provider-azure/pkg/cache"
)

type Repository interface {
	Get(
		ctx context.Context,
		resourceGroup string,
		resourceName string,
		crt cache.AzureCacheReadType,
	) (*armnetwork.PublicIPAddress, error)

	GetVirtualMachineScaleSetPublicIPAddress(
		ctx context.Context,
		resourceGroupName string,
		virtualMachineScaleSetName string,
		virtualMachineIndex string,
		networkInterfaceName string,
		ipConfigurationName string,
		publicIPAddressName string,
	) (*armnetwork.PublicIPAddress, error)

	ListByResourceGroup(
		ctx context.Context,
		resourceGroup string,
		crt cache.AzureCacheReadType,
	) ([]*armnetwork.PublicIPAddress, error)

	CreateOrUpdate(
		ctx context.Context,
		resourceGroup string,
		resource *armnetwork.PublicIPAddress,
	) (*armnetwork.PublicIPAddress, error)

	Delete(
		ctx context.Context,
		resourceGroup string,
		resourceName string,
	) error
}

type repo struct {
	client publicipaddressclient.Interface
	cache  cache.Resource
}

func NewRepo(
	client publicipaddressclient.Interface,
	cacheTTL time.Duration,
	disableAPICallCache bool,
) (Repository, error) {
	c, err := NewCache(client, cacheTTL, disableAPICallCache)
	if err != nil {
		return nil, fmt.Errorf("new PublicIP cache: %w", err)
	}

	return &repo{
		client: client,
		cache:  c,
	}, nil
}

func (r *repo) Get(ctx context.Context, resourceGroup string, resourceName string, crt cache.AzureCacheReadType) (*armnetwork.PublicIPAddress, error) {
	cacheKey := getCacheKey(resourceGroup, resourceName)
	rv, err := r.cache.GetWithDeepCopy(ctx, cacheKey, crt)
	if err != nil {
		return nil, err
	}
	return rv.(*armnetwork.PublicIPAddress), nil
}

func (r *repo) GetVirtualMachineScaleSetPublicIPAddress(
	ctx context.Context,
	resourceGroupName string,
	virtualMachineScaleSetName string,
	virtualMachineIndex string,
	networkInterfaceName string,
	ipConfigurationName string,
	publicIPAddressName string,
) (*armnetwork.PublicIPAddress, error) {
	resp, err := r.client.GetVirtualMachineScaleSetPublicIPAddress(
		ctx, resourceGroupName,
		virtualMachineScaleSetName, virtualMachineIndex,
		networkInterfaceName, ipConfigurationName,
		publicIPAddressName, nil,
	)
	if err != nil {
		return nil, err
	}
	return &resp.PublicIPAddress, nil
}

func (r *repo) ListByResourceGroup(ctx context.Context, resourceGroup string, crt cache.AzureCacheReadType) ([]*armnetwork.PublicIPAddress, error) {
	rv, err := r.client.List(ctx, resourceGroup)
	if err != nil {
		return nil, err
	}
	return rv, nil
}

func (r *repo) CreateOrUpdate(ctx context.Context, resourceGroup string, resource *armnetwork.PublicIPAddress) (*armnetwork.PublicIPAddress, error) {
	rv, err := r.client.CreateOrUpdate(ctx, resourceGroup, *resource.Name, *resource)
	if err != nil {
		return nil, err
	}
	_ = r.cache.Delete(*resource.Name)
	return rv, nil
}

func (r *repo) Delete(ctx context.Context, resourceGroup string, resourceName string) error {
	err := r.client.Delete(ctx, resourceGroup, resourceName)
	if err == nil {
		_ = r.cache.Delete(resourceName)
	}
	return err
}
