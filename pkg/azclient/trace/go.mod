module sigs.k8s.io/cloud-provider-azure/pkg/azclient/trace

go 1.21.1

require (
	github.com/Azure/azure-sdk-for-go/sdk/azcore v1.11.1
	go.opentelemetry.io/otel v1.26.0
	go.opentelemetry.io/otel/metric v1.26.0
	go.opentelemetry.io/otel/trace v1.26.0
	k8s.io/klog/v2 v2.120.1
	sigs.k8s.io/cloud-provider-azure/pkg/azclient v0.0.20
)

require (
	github.com/Azure/azure-sdk-for-go/sdk/internal v1.6.0 // indirect
	github.com/go-logr/logr v1.4.1 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	golang.org/x/net v0.24.0 // indirect
	golang.org/x/text v0.15.0 // indirect
)
