module sigs.k8s.io/cloud-provider-azure

go 1.15

require (
	github.com/Azure/azure-sdk-for-go v43.0.0+incompatible
	github.com/Azure/go-autorest/autorest v0.11.1
	github.com/Azure/go-autorest/autorest/adal v0.9.5
	github.com/Azure/go-autorest/autorest/mocks v0.4.1
	github.com/Azure/go-autorest/autorest/to v0.2.0
	github.com/dnaeon/go-vcr v1.1.0 // indirect
	github.com/evanphx/json-patch v4.9.0+incompatible
	github.com/golang/mock v1.4.1
	github.com/golangci/golangci-lint v1.25.0 // indirect
	github.com/onsi/ginkgo v1.11.0
	github.com/onsi/gomega v1.8.1
	github.com/rubiojr/go-vhd v0.0.0-20200706105327-02e210299021
	github.com/spf13/cobra v1.1.1
	github.com/spf13/pflag v1.0.5
	github.com/stretchr/testify v1.6.1
	golang.org/x/crypto v0.0.0-20201002170205-7f63de1d35b0
	k8s.io/api v0.0.0
	k8s.io/apimachinery v0.0.0
	k8s.io/apiserver v0.0.0
	k8s.io/client-go v0.0.0
	k8s.io/cloud-provider v0.0.0
	k8s.io/component-base v0.0.0
	k8s.io/controller-manager v0.0.0
	k8s.io/klog v1.0.0
	k8s.io/klog/v2 v2.4.0
	k8s.io/kubernetes v1.21.0-alpha.0.0.20201210005053-f58c4d8cd725
	k8s.io/utils v0.0.0-20201110183641-67b214c5f920
	sigs.k8s.io/yaml v1.2.0
)

replace (
	github.com/Azure/azure-sdk-for-go => github.com/Azure/azure-sdk-for-go v44.0.1-0.20200710185145-8277be38f5a2+incompatible
	github.com/Azure/go-ansiterm => github.com/Azure/go-ansiterm v0.0.0-20170929234023-d6e3b3328b78
	github.com/Azure/go-autorest/autorest => github.com/Azure/go-autorest/autorest v0.9.6
	github.com/Azure/go-autorest/autorest/adal => github.com/Azure/go-autorest/autorest/adal v0.8.2
	github.com/Azure/go-autorest/autorest/date => github.com/Azure/go-autorest/autorest/date v0.2.0
	github.com/Azure/go-autorest/autorest/mocks => github.com/Azure/go-autorest/autorest/mocks v0.3.0
	github.com/Azure/go-autorest/autorest/to => github.com/Azure/go-autorest/autorest/to v0.2.0
	github.com/Azure/go-autorest/autorest/validation => github.com/Azure/go-autorest/autorest/validation v0.1.0
	github.com/Azure/go-autorest/logger => github.com/Azure/go-autorest/logger v0.1.0
	github.com/Azure/go-autorest/tracing => github.com/Azure/go-autorest/tracing v0.5.0
	github.com/NYTimes/gziphandler => github.com/NYTimes/gziphandler v0.0.0-20170623195520-56545f4a5d46
	github.com/PuerkitoBio/purell => github.com/PuerkitoBio/purell v1.1.1
	github.com/PuerkitoBio/urlesc => github.com/PuerkitoBio/urlesc v0.0.0-20170810143723-de5bf2ad4578
	github.com/beorn7/perks => github.com/beorn7/perks v1.0.0
	github.com/blang/semver => github.com/blang/semver v3.5.0+incompatible
	github.com/coreos/go-systemd => github.com/coreos/go-systemd v0.0.0-20190321100706-95778dfbb74e
	github.com/coreos/pkg => github.com/coreos/pkg v0.0.0-20180108230652-97fdf19511ea
	github.com/davecgh/go-spew => github.com/davecgh/go-spew v1.1.1
	github.com/dgrijalva/jwt-go => github.com/dgrijalva/jwt-go v3.2.0+incompatible
	github.com/docker/distribution => github.com/docker/distribution v2.7.1+incompatible
	github.com/docker/spdystream => github.com/docker/spdystream v0.0.0-20160310174837-449fdfce4d96
	github.com/emicklei/go-restful => github.com/emicklei/go-restful v2.9.5+incompatible
	github.com/evanphx/json-patch => github.com/evanphx/json-patch v4.2.0+incompatible
	github.com/go-logr/logr => github.com/go-logr/logr v0.2.0
	github.com/go-openapi/jsonpointer => github.com/go-openapi/jsonpointer v0.19.3
	github.com/go-openapi/jsonreference => github.com/go-openapi/jsonreference v0.19.3
	github.com/go-openapi/spec => github.com/go-openapi/spec v0.19.3
	github.com/go-openapi/swag => github.com/go-openapi/swag v0.19.5
	github.com/gogo/protobuf => github.com/gogo/protobuf v1.3.1
	github.com/golang/groupcache => github.com/golang/groupcache v0.0.0-20160516000752-02826c3e7903
	github.com/golang/mock => github.com/golang/mock v1.3.1
	github.com/golang/protobuf => github.com/golang/protobuf v1.3.2
	github.com/google/go-cmp => github.com/google/go-cmp v0.3.0
	github.com/google/gofuzz => github.com/google/gofuzz v1.1.0
	github.com/google/uuid => github.com/google/uuid v1.1.1
	github.com/googleapis/gnostic => github.com/googleapis/gnostic v0.4.1
	github.com/grpc-ecosystem/go-grpc-prometheus => github.com/grpc-ecosystem/go-grpc-prometheus v1.2.0
	github.com/hashicorp/golang-lru => github.com/hashicorp/golang-lru v0.5.1
	github.com/hpcloud/tail => github.com/hpcloud/tail v1.0.0
	github.com/imdario/mergo => github.com/imdario/mergo v0.3.5
	github.com/inconshreveable/mousetrap => github.com/inconshreveable/mousetrap v1.0.0
	github.com/json-iterator/go => github.com/json-iterator/go v1.1.8
	github.com/konsorten/go-windows-terminal-sequences => github.com/konsorten/go-windows-terminal-sequences v1.0.1
	github.com/mailru/easyjson => github.com/mailru/easyjson v0.7.0
	github.com/matttproud/golang_protobuf_extensions => github.com/matttproud/golang_protobuf_extensions v1.0.1
	github.com/moby/term => github.com/moby/term v0.0.0-20200312100748-672ec06f55cd
	github.com/modern-go/concurrent => github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd
	github.com/modern-go/reflect2 => github.com/modern-go/reflect2 v1.0.1
	github.com/munnerz/goautoneg => github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822
	github.com/onsi/ginkgo => github.com/onsi/ginkgo v1.11.0
	github.com/onsi/gomega => github.com/onsi/gomega v1.7.0
	github.com/opencontainers/go-digest => github.com/opencontainers/go-digest v1.0.0-rc1
	github.com/pkg/errors => github.com/pkg/errors v0.8.1
	github.com/pmezard/go-difflib => github.com/pmezard/go-difflib v1.0.0
	github.com/prometheus/client_golang => github.com/prometheus/client_golang v1.7.1
	github.com/prometheus/client_model => github.com/prometheus/client_model v0.2.0
	github.com/prometheus/common => github.com/prometheus/common v0.10.0
	github.com/prometheus/procfs => github.com/prometheus/procfs v0.0.2
	github.com/rubiojr/go-vhd => github.com/rubiojr/go-vhd v0.0.0-20160810183302-0bfd3b39853c
	github.com/satori/go.uuid => github.com/satori/go.uuid v1.2.0
	github.com/sirupsen/logrus => github.com/sirupsen/logrus v1.4.2
	github.com/spf13/cobra => github.com/spf13/cobra v0.0.5
	github.com/spf13/pflag => github.com/spf13/pflag v1.0.5
	github.com/stretchr/objx => github.com/stretchr/objx v0.2.0
	github.com/stretchr/testify => github.com/stretchr/testify v1.4.0
	go.etcd.io/etcd => go.etcd.io/etcd v0.0.0-20191023171146-3cf2f69b5738
	go.uber.org/atomic => go.uber.org/atomic v1.3.2
	go.uber.org/multierr => go.uber.org/multierr v1.1.0
	go.uber.org/zap => go.uber.org/zap v1.10.0
	golang.org/x/crypto => golang.org/x/crypto v0.0.0-20200220183623-bac4c82f6975
	golang.org/x/net => golang.org/x/net v0.0.0-20201209123823-ac852fbbde11
	golang.org/x/oauth2 => golang.org/x/oauth2 v0.0.0-20190604053449-0f29369cfe45
	golang.org/x/sync => golang.org/x/sync v0.0.0-20190911185100-cd5d95a43a6e
	golang.org/x/sys => golang.org/x/sys v0.0.0-20191022100944-742c48ecaeb7
	golang.org/x/text => golang.org/x/text v0.3.2
	golang.org/x/time => golang.org/x/time v0.0.0-20190308202827-9d24e82272b4
	google.golang.org/appengine => google.golang.org/appengine v1.5.0
	google.golang.org/genproto => google.golang.org/genproto v0.0.0-20190819201941-24fa4b261c55
	google.golang.org/grpc => google.golang.org/grpc v1.26.0
	gopkg.in/fsnotify.v1 => gopkg.in/fsnotify.v1 v1.4.7
	gopkg.in/inf.v0 => gopkg.in/inf.v0 v0.9.1
	gopkg.in/natefinch/lumberjack.v2 => gopkg.in/natefinch/lumberjack.v2 v2.0.0
	gopkg.in/tomb.v1 => gopkg.in/tomb.v1 v1.0.0-20141024135613-dd632973f1e7
	gopkg.in/yaml.v2 => gopkg.in/yaml.v2 v2.2.8
	k8s.io/api => k8s.io/api v0.0.0-20201209045733-fcac651617f2
	k8s.io/apiextensions-apiserver => k8s.io/apiextensions-apiserver v0.0.0-20201114091224-a7ee1efe41fc
	k8s.io/apimachinery => k8s.io/apimachinery v0.21.0-alpha.0.0.20201209085528-15c5dba13c59
	k8s.io/apiserver => k8s.io/apiserver v0.0.0-20201209130508-aed7ab078321
	k8s.io/cli-runtime => k8s.io/cli-runtime v0.0.0-20201209051923-2e4b259e04ba
	k8s.io/client-go => k8s.io/client-go v0.0.0-20201209050023-e24efdc77f15
	k8s.io/cloud-provider => k8s.io/cloud-provider v0.0.0-20201021002512-82fca6d2b013
	k8s.io/cluster-bootstrap => k8s.io/cluster-bootstrap v0.0.0-20201114092228-614b98eee358
	k8s.io/code-generator => k8s.io/code-generator v0.21.0-alpha.0
	k8s.io/component-base => k8s.io/component-base v0.0.0-20201114090208-1e84b325f5ba
	k8s.io/component-helpers => k8s.io/component-helpers v0.20.0-alpha.2.0.20201114090304-7cb42b694587
	k8s.io/controller-manager => k8s.io/controller-manager v0.20.0-alpha.1.0.20201209052538-b2c380a1dc86
	k8s.io/cri-api => k8s.io/cri-api v0.21.0-alpha.0
	k8s.io/csi-translation-lib => k8s.io/csi-translation-lib v0.0.0-20201114092327-833303372de1
	k8s.io/gengo => k8s.io/gengo v0.0.0-20200205140755-e0e292d8aa12
	k8s.io/klog => k8s.io/klog v1.0.0
	k8s.io/kube-aggregator => k8s.io/kube-aggregator v0.0.0-20201126170540-6c47de442a82
	k8s.io/kube-controller-manager => k8s.io/kube-controller-manager v0.0.0-20201114092129-18c28a4120de
	k8s.io/kube-openapi => k8s.io/kube-openapi v0.0.0-20201113171705-d219536bb9fd
	k8s.io/kube-proxy => k8s.io/kube-proxy v0.0.0-20201114091637-deb12d4b202f
	k8s.io/kube-scheduler => k8s.io/kube-scheduler v0.0.0-20201114091838-0f62d3991af1
	k8s.io/kubectl => k8s.io/kubectl v0.0.0-20201210013108-5cfbd4019670
	k8s.io/kubelet => k8s.io/kubelet v0.0.0-20201114091737-92ded5ee6b96
	k8s.io/legacy-cloud-providers => k8s.io/legacy-cloud-providers v0.0.0-20201211092501-716c3daa6bc8
	k8s.io/metrics => k8s.io/metrics v0.0.0-20201114091333-d70c0e0c6aa5
	k8s.io/mount-utils => k8s.io/mount-utils v0.21.0-alpha.0
	k8s.io/sample-apiserver => k8s.io/sample-apiserver v0.0.0-20201114090814-1f4e6a92d4b8
	k8s.io/utils => k8s.io/utils v0.0.0-20200619165400-6e3d28b6ed19
	sigs.k8s.io/apiserver-network-proxy/konnectivity-client => sigs.k8s.io/apiserver-network-proxy/konnectivity-client v0.0.9
	sigs.k8s.io/yaml => sigs.k8s.io/yaml v1.2.0
)
