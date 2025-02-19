package fixture

import (
	armcontainerservice "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v6"
	"k8s.io/utils/ptr"
)

func (f *AzureFixture) ManagedCluster() *AzureManagedClusterFixture {
	return &AzureManagedClusterFixture{
		mc: &armcontainerservice.ManagedCluster{
<<<<<<< HEAD
			Name: ptr.To("mangaedcluster"),
=======
			Name: ptr.To("managedcluster"),
>>>>>>> 9141356e07a8826ff5e44af7f048cb52def705be
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
