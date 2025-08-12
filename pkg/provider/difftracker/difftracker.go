package difftracker

import (
	"k8s.io/client-go/util/workqueue"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func InitializeDiffTracker(K8s K8s_State, NRP NRP_State) *DiffTracker {
	// If any field is nil, initialize it
	if K8s.Services == nil {
		K8s.Services = utilsets.NewString()
	}
	if K8s.Egresses == nil {
		K8s.Egresses = utilsets.NewString()
	}
	if K8s.Nodes == nil {
		K8s.Nodes = make(map[string]Node)
	}
	if NRP.LoadBalancers == nil {
		NRP.LoadBalancers = utilsets.NewString()
	}
	if NRP.NATGateways == nil {
		NRP.NATGateways = utilsets.NewString()
	}
	if NRP.Locations == nil {
		NRP.Locations = make(map[string]NRPLocation)
	}

	diffTracker := &DiffTracker{
		K8sResources: K8s,
		NRPResources: NRP,
		PodEgressQueue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[PodCrudEvent](),
			workqueue.TypedRateLimitingQueueConfig[PodCrudEvent]{Name: "PodEgress"},
		),
	}

	return diffTracker
}
