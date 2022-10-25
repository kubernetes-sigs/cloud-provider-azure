module sigs.k8s.io/cloud-provider-azure

go 1.16

require (
	github.com/Azure/azure-sdk-for-go v67.0.0+incompatible
	github.com/Azure/go-autorest/autorest v0.11.28
	github.com/Azure/go-autorest/autorest/adal v0.9.21
	github.com/Azure/go-autorest/autorest/mocks v0.4.2
	github.com/Azure/go-autorest/autorest/to v0.4.0
	github.com/evanphx/json-patch v5.6.0+incompatible
	github.com/fsnotify/fsnotify v1.6.0
	github.com/golang/mock v1.6.0
	github.com/onsi/ginkgo/v2 v2.3.1
	github.com/onsi/gomega v1.22.1
	github.com/rubiojr/go-vhd v0.0.0-20200706122120-ccecf6c0760f
	github.com/spf13/cobra v1.6.1
	github.com/spf13/pflag v1.0.5
	github.com/stretchr/testify v1.8.0
	golang.org/x/crypto v0.0.0-20220722155217-630584e8d5aa
	golang.org/x/text v0.4.0
	k8s.io/api v0.22.12
	k8s.io/apimachinery v0.22.12
	k8s.io/apiserver v0.22.12
	k8s.io/client-go v0.22.12
	k8s.io/cloud-provider v0.22.12
	k8s.io/component-base v0.22.12
	k8s.io/controller-manager v0.22.12
	k8s.io/klog/v2 v2.10.0
	k8s.io/utils v0.0.0-20220210201930-3a6ce19ff2f9
	sigs.k8s.io/yaml v1.3.0
)

require (
	github.com/Azure/go-autorest/autorest/validation v0.1.0 // indirect
	github.com/cespare/xxhash/v2 v2.1.2 // indirect
	github.com/dnaeon/go-vcr v1.1.0 // indirect
	github.com/emicklei/go-restful v2.16.0+incompatible // indirect
	github.com/gofrs/uuid v4.0.0+incompatible // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/prometheus/client_golang v1.11.1 // indirect
	go.etcd.io/etcd/api/v3 v3.5.1 // indirect
	go.etcd.io/etcd/client/pkg/v3 v3.5.1 // indirect
	golang.org/x/net v0.0.0-20220906165146-f3363e06e74c // indirect
	golang.org/x/oauth2 v0.0.0-20211104180415-d3ed0bb246c8 // indirect
	google.golang.org/appengine v1.6.7 // indirect
	google.golang.org/genproto v0.0.0-20211208223120-3a66f561d7aa // indirect
	google.golang.org/grpc v1.42.0 // indirect
)

replace (
	k8s.io/api => k8s.io/api v0.22.12
	k8s.io/apimachinery => k8s.io/apimachinery v0.22.12
	k8s.io/apiserver => k8s.io/apiserver v0.22.12
	k8s.io/client-go => k8s.io/client-go v0.22.12
	k8s.io/cloud-provider => k8s.io/cloud-provider v0.22.12
	k8s.io/component-base => k8s.io/component-base v0.22.12
	k8s.io/component-helpers => k8s.io/component-helpers v0.22.12
	k8s.io/controller-manager => k8s.io/controller-manager v0.22.12
	k8s.io/kubelet => k8s.io/kubelet v0.22.12
)
