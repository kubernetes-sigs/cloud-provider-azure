package fixture

import (
	armcontainerservice "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v6"
	"k8s.io/utils/ptr"
)

func (f *AzureFixture) ManagedCluster() *AzureManagedClusterFixture {
	return &AzureManagedClusterFixture{
		mc: &armcontainerservice.ManagedCluster{
			Name: ptr.To("managedcluster"),
			Properties: &armcontainerservice.ManagedClusterProperties{
				NetworkProfile: &armcontainerservice.NetworkProfile{
					PodCidrs: []*string{ptr.To("192.1.0.0/16")},
				},
			},
		},
	}
}

type AzureManagedClusterFixture struct {
	mc *armcontainerservice.ManagedCluster
}

func (f *AzureManagedClusterFixture) Build() *armcontainerservice.ManagedCluster {
	return f.mc
}
