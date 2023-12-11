module sigs.k8s.io/cloud-provider-azure/pkg/azclient/trace

go 1.21.1

require (
	github.com/Azure/azure-sdk-for-go/sdk/azcore v1.9.1
	go.opentelemetry.io/otel v1.21.0
	go.opentelemetry.io/otel/metric v1.21.0
	go.opentelemetry.io/otel/trace v1.21.0
	k8s.io/klog/v2 v2.110.1
	sigs.k8s.io/cloud-provider-azure/pkg/azclient v0.0.0-20230912015011-ab8ef32c681c
)

require (
	github.com/Azure/azure-sdk-for-go/sdk/internal v1.5.1 // indirect
	github.com/go-logr/logr v1.3.0 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	golang.org/x/net v0.19.0 // indirect
	golang.org/x/text v0.14.0 // indirect
)
