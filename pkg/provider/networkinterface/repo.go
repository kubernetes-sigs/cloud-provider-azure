package networkinterface

//go:generate mockgen -destination=./mock_repo.go -package=networkinterface -copyright_file ../../../hack/boilerplate/boilerplate.generatego.txt -source=repo.go Repository

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/interfaceclient"
)

type Repository interface {
	Get(ctx context.Context, resourceGroup, resourceName string) (*armnetwork.Interface, error)

	GetVirtualMachineScaleSetNetworkInterface(
		ctx context.Context,
		resourceGroupName string,
		virtualMachineScaleSetName string,
		virtualMachineIndex string,
		networkInterfaceName string,
	) (*armnetwork.Interface, error)

	ListVirtualMachineScaleSetNetworkInterfaces(
		ctx context.Context,
		resourceGroupName string,
		virtualMachineScaleSetName string,
	) ([]*armnetwork.Interface, error)

	CreateOrUpdate(
		ctx context.Context,
		resourceGroup string,
		resource *armnetwork.Interface,
	) (*armnetwork.Interface, error)

	Delete(ctx context.Context, resourceGroup, resourceName string) error
}

type repo struct {
	client interfaceclient.Interface
}

func NewRepo(client interfaceclient.Interface) (Repository, error) {
	return &repo{
		client: client,
	}, nil
}

func (r *repo) Get(ctx context.Context, resourceGroup, resourceName string) (*armnetwork.Interface, error) {
	//TODO implement me
	panic("implement me")
}

func (r *repo) GetVirtualMachineScaleSetNetworkInterface(ctx context.Context, resourceGroupName string, virtualMachineScaleSetName string, virtualMachineIndex string, networkInterfaceName string) (*armnetwork.Interface, error) {
	//TODO implement me
	panic("implement me")
}

func (r *repo) ListVirtualMachineScaleSetNetworkInterfaces(ctx context.Context, resourceGroupName string, virtualMachineScaleSetName string) ([]*armnetwork.Interface, error) {
	//TODO implement me
	panic("implement me")
}

func (r *repo) CreateOrUpdate(ctx context.Context, resourceGroup string, resource *armnetwork.Interface) (*armnetwork.Interface, error) {
	//TODO implement me
	panic("implement me")
}

func (r *repo) Delete(ctx context.Context, resourceGroup, resourceName string) error {
	//TODO implement me
	panic("implement me")
}
