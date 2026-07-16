package provider

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/log"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/servicegateway/difftracker"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func newProviderDiffTracker(t *testing.T, az *Cloud, kubeClient kubernetes.Interface) *difftracker.DiffTracker {
	t.Helper()

	return seededProviderDiffTracker(t, az, kubeClient, difftracker.K8sState{
		Services: utilsets.NewString(),
		Egresses: utilsets.NewString(),
		Nodes:    make(map[string]difftracker.Node),
	}, difftracker.NRPState{
		LoadBalancers: utilsets.NewString(),
		NATGateways:   utilsets.NewString(),
		Locations:     make(map[string]difftracker.NRPLocation),
	})
}

// seededProviderDiffTracker builds a real engine pre-seeded with the given K8s and NRP state, so a
// test can start from live egress pods (New seeds the outbound ref-counter from k8s.Nodes) rather
// than driving the full create lifecycle. Used to exercise the informer's delete/drain paths against
// an engine that already tracks the pods.
func seededProviderDiffTracker(t *testing.T, az *Cloud, kubeClient kubernetes.Interface, k8s difftracker.K8sState, nrp difftracker.NRPState) *difftracker.DiffTracker {
	t.Helper()

	dt, err := difftracker.New(
		log.Noop(),
		k8s,
		nrp,
		difftracker.Config{
			SubscriptionID:             az.SubscriptionID,
			ResourceGroup:              az.ResourceGroup,
			Location:                   az.Location,
			VNetName:                   az.VnetName,
			VNetResourceGroup:          az.VnetResourceGroup,
			ServiceGatewayResourceName: consts.DefaultServiceGatewayResourceName,
		},
		az.NetworkClientFactory,
		kubeClient,
	)
	if !assert.NoError(t, err) {
		t.FailNow()
	}
	return dt
}

func newSGWCloudWithServiceAndRecorder(t *testing.T, ctrl *gomock.Controller, svc v1.Service) (*Cloud, *record.FakeRecorder) {
	t.Helper()

	az := GetTestCloudWithContainerLoadBalancer(ctrl)
	kubeClient := fake.NewSimpleClientset(&svc)
	az.KubeClient = kubeClient
	az.diffTracker = newProviderDiffTracker(t, az, kubeClient)
	rec := record.NewFakeRecorder(10)
	az.eventRecorder = rec

	return az, rec
}

func assertNoEvent(t *testing.T, rec *record.FakeRecorder) {
	t.Helper()

	select {
	case ev := <-rec.Events:
		t.Fatalf("expected no warning event, got: %s", ev)
	case <-time.After(100 * time.Millisecond):
	}
}
