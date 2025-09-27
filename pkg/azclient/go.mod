module sigs.k8s.io/cloud-provider-azure/pkg/azclient

go 1.24.6

require (
	github.com/Azure/azure-sdk-for-go/sdk/azcore v1.19.1
	github.com/Azure/azure-sdk-for-go/sdk/azidentity v1.12.0
	github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v2 v2.2.0
	github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6 v6.4.0
	github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerregistry/armcontainerregistry v1.2.0
	github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v6 v6.6.0
	github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/keyvault/armkeyvault v1.5.0
	github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/msi/armmsi v1.2.0
	github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6 v6.2.0
	github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/privatedns/armprivatedns v1.3.0
	github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources v1.2.0
	github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storage/armstorage/v2 v2.0.0
	github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets v1.4.0
	github.com/Azure/msi-dataplane v0.4.3
	github.com/go-faker/faker/v4 v4.6.1
	github.com/google/uuid v1.6.0
	github.com/onsi/ginkgo/v2 v2.23.4
	github.com/onsi/gomega v1.37.0
	github.com/stretchr/testify v1.11.1
	go.opentelemetry.io/otel v1.37.0
	go.opentelemetry.io/otel/metric v1.37.0
	go.uber.org/mock v0.6.0
	golang.org/x/crypto v0.42.0
	golang.org/x/net v0.44.0
	golang.org/x/sync v0.17.0
	golang.org/x/time v0.12.0
	gopkg.in/dnaeon/go-vcr.v3 v3.2.0
	k8s.io/klog/v2 v2.130.1
	k8s.io/utils v0.0.0-20241210054802-24370beab758
)

require (
	github.com/Azure/azure-sdk-for-go/sdk/internal v1.11.2 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/internal v1.2.0 // indirect
	github.com/AzureAD/microsoft-authentication-library-for-go v1.5.0 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/fsnotify/fsnotify v1.8.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-task/slim-sprig/v3 v3.0.0 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.0 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/pprof v0.0.0-20250403155104-27863c87afa6 // indirect
	github.com/kylelemons/godebug v1.1.0 // indirect
	github.com/pkg/browser v0.0.0-20240102092130-5ac0b6a4141c // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/rogpeppe/go-internal v1.13.1 // indirect
	go.uber.org/automaxprocs v1.6.0 // indirect
	golang.org/x/exp v0.0.0-20240506185415-9bf2ced13842 // indirect
	golang.org/x/sys v0.36.0 // indirect
	golang.org/x/text v0.29.0 // indirect
	golang.org/x/tools v0.36.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
