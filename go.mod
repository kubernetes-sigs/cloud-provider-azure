module sigs.k8s.io/cloud-provider-azure

go 1.16

require (
	github.com/Azure/azure-sdk-for-go v62.0.0+incompatible
	github.com/Azure/go-autorest/autorest v0.11.24
	github.com/Azure/go-autorest/autorest/adal v0.9.18
	github.com/Azure/go-autorest/autorest/mocks v0.4.1
	github.com/Azure/go-autorest/autorest/to v0.4.0
	github.com/Azure/go-autorest/autorest/validation v0.3.1 // indirect
	github.com/dnaeon/go-vcr v1.2.0 // indirect
	github.com/evanphx/json-patch v5.6.0+incompatible
	github.com/fsnotify/fsnotify v1.5.1
	github.com/gofrs/uuid v4.2.0+incompatible // indirect
	github.com/golang/mock v1.6.0
	github.com/onsi/ginkgo v1.16.5
	github.com/onsi/gomega v1.18.1
	github.com/rubiojr/go-vhd v0.0.0-20200706122120-ccecf6c0760f
	github.com/spf13/cobra v1.3.0
	github.com/spf13/pflag v1.0.5
	github.com/stretchr/testify v1.7.0
	golang.org/x/crypto v0.0.0-20220112180741-5e0467b6c7ce
	k8s.io/api v0.21.10
	k8s.io/apimachinery v0.21.10
	k8s.io/apiserver v0.21.10
	k8s.io/client-go v0.21.10
	k8s.io/cloud-provider v0.21.10
	k8s.io/component-base v0.21.10
	k8s.io/controller-manager v0.21.10
	k8s.io/klog/v2 v2.9.0
	k8s.io/utils v0.0.0-20210521133846-da695404a2bc
	sigs.k8s.io/yaml v1.2.0

)

replace google.golang.org/grpc => google.golang.org/grpc v1.27.1
