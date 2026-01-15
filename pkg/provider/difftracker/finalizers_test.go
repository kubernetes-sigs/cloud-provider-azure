package difftracker

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// ================================================================================================
// HELPER FUNCTION TESTS
// ================================================================================================

func TestHasFinalizer(t *testing.T) {
	tests := []struct {
		name       string
		finalizers []string
		finalizer  string
		expected   bool
	}{
		{
			name:       "finalizer exists",
			finalizers: []string{"finalizer1", "finalizer2", "finalizer3"},
			finalizer:  "finalizer2",
			expected:   true,
		},
		{
			name:       "finalizer does not exist",
			finalizers: []string{"finalizer1", "finalizer2"},
			finalizer:  "finalizer3",
			expected:   false,
		},
		{
			name:       "empty slice",
			finalizers: []string{},
			finalizer:  "finalizer1",
			expected:   false,
		},
		{
			name:       "nil slice",
			finalizers: nil,
			finalizer:  "finalizer1",
			expected:   false,
		},
		{
			name:       "first element",
			finalizers: []string{"finalizer1", "finalizer2"},
			finalizer:  "finalizer1",
			expected:   true,
		},
		{
			name:       "last element",
			finalizers: []string{"finalizer1", "finalizer2"},
			finalizer:  "finalizer2",
			expected:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := hasFinalizer(tt.finalizers, tt.finalizer)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestRemoveFinalizerString(t *testing.T) {
	tests := []struct {
		name        string
		slice       []string
		toRemove    string
		expected    []string
		expectEmpty bool // Use this instead of nil comparison since slices.DeleteFunc returns []string{}
	}{
		{
			name:     "remove existing finalizer",
			slice:    []string{"finalizer1", "finalizer2", "finalizer3"},
			toRemove: "finalizer2",
			expected: []string{"finalizer1", "finalizer3"},
		},
		{
			name:     "remove non-existing finalizer",
			slice:    []string{"finalizer1", "finalizer2"},
			toRemove: "finalizer3",
			expected: []string{"finalizer1", "finalizer2"},
		},
		{
			name:        "remove from empty slice",
			slice:       []string{},
			toRemove:    "finalizer1",
			expectEmpty: true,
		},
		{
			name:        "remove only element",
			slice:       []string{"finalizer1"},
			toRemove:    "finalizer1",
			expectEmpty: true,
		},
		{
			name:     "remove first element",
			slice:    []string{"finalizer1", "finalizer2", "finalizer3"},
			toRemove: "finalizer1",
			expected: []string{"finalizer2", "finalizer3"},
		},
		{
			name:     "remove last element",
			slice:    []string{"finalizer1", "finalizer2", "finalizer3"},
			toRemove: "finalizer3",
			expected: []string{"finalizer1", "finalizer2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := removeFinalizerString(tt.slice, tt.toRemove)
			if tt.expectEmpty {
				assert.Empty(t, result)
			} else {
				assert.Equal(t, tt.expected, result)
			}
		})
	}
}

// ================================================================================================
// SERVICE FINALIZER TESTS
// ================================================================================================

func TestHasServiceGatewayFinalizer(t *testing.T) {
	tests := []struct {
		name     string
		service  *v1.Service
		expected bool
	}{
		{
			name: "service has finalizer",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Finalizers: []string{"other-finalizer", ServiceGatewayServiceCleanupFinalizer},
				},
			},
			expected: true,
		},
		{
			name: "service does not have finalizer",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Finalizers: []string{"other-finalizer"},
				},
			},
			expected: false,
		},
		{
			name: "service has no finalizers",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Finalizers: nil,
				},
			},
			expected: false,
		},
		{
			name: "service has only our finalizer",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Finalizers: []string{ServiceGatewayServiceCleanupFinalizer},
				},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := hasServiceGatewayFinalizer(tt.service)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestAddServiceGatewayFinalizer(t *testing.T) {
	ctx := context.Background()

	t.Run("adds finalizer to service without finalizer", func(t *testing.T) {
		svc := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-service",
				Namespace: "default",
				UID:       types.UID("test-uid"),
			},
		}

		kubeClient := fake.NewSimpleClientset(svc)
		dt := &DiffTracker{
			kubeClient: kubeClient,
		}

		err := dt.addServiceGatewayFinalizer(ctx, svc)
		assert.NoError(t, err)

		// Verify finalizer was added
		updatedSvc, err := kubeClient.CoreV1().Services("default").Get(ctx, "test-service", metav1.GetOptions{})
		assert.NoError(t, err)
		assert.True(t, hasServiceGatewayFinalizer(updatedSvc))
	})

	t.Run("does not duplicate finalizer", func(t *testing.T) {
		svc := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-service",
				Namespace:  "default",
				UID:        types.UID("test-uid"),
				Finalizers: []string{ServiceGatewayServiceCleanupFinalizer},
			},
		}

		kubeClient := fake.NewSimpleClientset(svc)
		dt := &DiffTracker{
			kubeClient: kubeClient,
		}

		err := dt.addServiceGatewayFinalizer(ctx, svc)
		assert.NoError(t, err)

		// Verify finalizer count is still 1
		updatedSvc, err := kubeClient.CoreV1().Services("default").Get(ctx, "test-service", metav1.GetOptions{})
		assert.NoError(t, err)
		count := 0
		for _, f := range updatedSvc.Finalizers {
			if f == ServiceGatewayServiceCleanupFinalizer {
				count++
			}
		}
		assert.Equal(t, 1, count)
	})

	// This test verifies the fix for a race condition where services could get stuck
	// during deletion if only our finalizer was added (without the K8s LB finalizer).
	// The K8s service controller's needsCleanup() checks for the K8s LB finalizer,
	// so without it, the controller tries to add it (which fails on a deleting service)
	// and never calls EnsureLoadBalancerDeleted.
	t.Run("also adds K8s LoadBalancer finalizer for needsCleanup compatibility", func(t *testing.T) {
		svc := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-service",
				Namespace: "default",
				UID:       types.UID("test-uid"),
			},
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
		}

		kubeClient := fake.NewSimpleClientset(svc)
		dt := &DiffTracker{
			kubeClient: kubeClient,
		}

		err := dt.addServiceGatewayFinalizer(ctx, svc)
		assert.NoError(t, err)

		// Verify BOTH finalizers were added
		updatedSvc, err := kubeClient.CoreV1().Services("default").Get(ctx, "test-service", metav1.GetOptions{})
		assert.NoError(t, err)

		// Check our finalizer
		assert.True(t, hasServiceGatewayFinalizer(updatedSvc), "ServiceGateway finalizer should be present")

		// Check K8s LB finalizer (service.kubernetes.io/load-balancer-cleanup)
		hasK8sLBFinalizer := false
		for _, f := range updatedSvc.Finalizers {
			if f == "service.kubernetes.io/load-balancer-cleanup" {
				hasK8sLBFinalizer = true
				break
			}
		}
		assert.True(t, hasK8sLBFinalizer, "K8s LoadBalancer cleanup finalizer should also be present to ensure needsCleanup() returns true during deletion")
	})

	t.Run("does not duplicate K8s LB finalizer if already present", func(t *testing.T) {
		svc := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-service",
				Namespace:  "default",
				UID:        types.UID("test-uid"),
				Finalizers: []string{"service.kubernetes.io/load-balancer-cleanup"},
			},
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer,
			},
		}

		kubeClient := fake.NewSimpleClientset(svc)
		dt := &DiffTracker{
			kubeClient: kubeClient,
		}

		err := dt.addServiceGatewayFinalizer(ctx, svc)
		assert.NoError(t, err)

		// Verify K8s finalizer is not duplicated
		updatedSvc, err := kubeClient.CoreV1().Services("default").Get(ctx, "test-service", metav1.GetOptions{})
		assert.NoError(t, err)
		count := 0
		for _, f := range updatedSvc.Finalizers {
			if f == "service.kubernetes.io/load-balancer-cleanup" {
				count++
			}
		}
		assert.Equal(t, 1, count, "K8s LB finalizer should not be duplicated")
	})
}

func TestRemoveServiceGatewayFinalizer(t *testing.T) {
	ctx := context.Background()

	t.Run("removes finalizer from service", func(t *testing.T) {
		svc := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-service",
				Namespace:  "default",
				UID:        types.UID("test-uid"),
				Finalizers: []string{"other-finalizer", ServiceGatewayServiceCleanupFinalizer},
			},
		}

		kubeClient := fake.NewSimpleClientset(svc)
		dt := &DiffTracker{
			kubeClient: kubeClient,
		}

		err := dt.removeServiceGatewayFinalizer(ctx, svc)
		assert.NoError(t, err)

		// Verify finalizer was removed
		updatedSvc, err := kubeClient.CoreV1().Services("default").Get(ctx, "test-service", metav1.GetOptions{})
		assert.NoError(t, err)
		assert.False(t, hasServiceGatewayFinalizer(updatedSvc))
		assert.Contains(t, updatedSvc.Finalizers, "other-finalizer")
	})

	t.Run("handles service without finalizer gracefully", func(t *testing.T) {
		svc := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-service",
				Namespace:  "default",
				UID:        types.UID("test-uid"),
				Finalizers: []string{"other-finalizer"},
			},
		}

		kubeClient := fake.NewSimpleClientset(svc)
		dt := &DiffTracker{
			kubeClient: kubeClient,
		}

		err := dt.removeServiceGatewayFinalizer(ctx, svc)
		assert.NoError(t, err)
	})

	// This test verifies that when removing our finalizer, we also remove the K8s LB
	// finalizer that we added in addServiceGatewayFinalizer.
	t.Run("also removes K8s LoadBalancer finalizer", func(t *testing.T) {
		svc := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-service",
				Namespace: "default",
				UID:       types.UID("test-uid"),
				Finalizers: []string{
					"other-finalizer",
					ServiceGatewayServiceCleanupFinalizer,
					"service.kubernetes.io/load-balancer-cleanup",
				},
			},
		}

		kubeClient := fake.NewSimpleClientset(svc)
		dt := &DiffTracker{
			kubeClient: kubeClient,
		}

		err := dt.removeServiceGatewayFinalizer(ctx, svc)
		assert.NoError(t, err)

		// Verify both finalizers were removed
		updatedSvc, err := kubeClient.CoreV1().Services("default").Get(ctx, "test-service", metav1.GetOptions{})
		assert.NoError(t, err)
		assert.False(t, hasServiceGatewayFinalizer(updatedSvc), "ServiceGateway finalizer should be removed")

		// Check K8s LB finalizer was also removed
		hasK8sLBFinalizer := false
		for _, f := range updatedSvc.Finalizers {
			if f == "service.kubernetes.io/load-balancer-cleanup" {
				hasK8sLBFinalizer = true
				break
			}
		}
		assert.False(t, hasK8sLBFinalizer, "K8s LoadBalancer cleanup finalizer should also be removed")

		// But other finalizers should remain
		assert.Contains(t, updatedSvc.Finalizers, "other-finalizer")
	})
}

// ================================================================================================
// POD FINALIZER TESTS
// ================================================================================================

func TestHasPodFinalizer(t *testing.T) {
	tests := []struct {
		name     string
		pod      *v1.Pod
		expected bool
	}{
		{
			name: "pod has finalizer",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Finalizers: []string{"other-finalizer", ServiceGatewayPodCleanupFinalizer},
				},
			},
			expected: true,
		},
		{
			name: "pod does not have finalizer",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Finalizers: []string{"other-finalizer"},
				},
			},
			expected: false,
		},
		{
			name: "pod has no finalizers",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Finalizers: nil,
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := hasPodFinalizer(tt.pod)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestAddPodFinalizer(t *testing.T) {
	ctx := context.Background()

	t.Run("adds finalizer to pod", func(t *testing.T) {
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: "default",
			},
		}

		kubeClient := fake.NewSimpleClientset(pod)
		dt := &DiffTracker{
			kubeClient: kubeClient,
		}

		err := dt.AddPodFinalizer(ctx, pod)
		assert.NoError(t, err)

		// Verify finalizer was added
		updatedPod, err := kubeClient.CoreV1().Pods("default").Get(ctx, "test-pod", metav1.GetOptions{})
		assert.NoError(t, err)
		assert.True(t, hasPodFinalizer(updatedPod))
	})

	t.Run("does not duplicate finalizer", func(t *testing.T) {
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-pod",
				Namespace:  "default",
				Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
			},
		}

		kubeClient := fake.NewSimpleClientset(pod)
		dt := &DiffTracker{
			kubeClient: kubeClient,
		}

		err := dt.AddPodFinalizer(ctx, pod)
		assert.NoError(t, err)
	})
}

func TestRemovePodFinalizer(t *testing.T) {
	ctx := context.Background()

	t.Run("removes finalizer from pod", func(t *testing.T) {
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-pod",
				Namespace:  "default",
				Finalizers: []string{"other-finalizer", ServiceGatewayPodCleanupFinalizer},
			},
		}

		kubeClient := fake.NewSimpleClientset(pod)
		dt := &DiffTracker{
			kubeClient: kubeClient,
		}

		err := dt.removePodFinalizer(ctx, pod)
		assert.NoError(t, err)

		// Verify finalizer was removed
		updatedPod, err := kubeClient.CoreV1().Pods("default").Get(ctx, "test-pod", metav1.GetOptions{})
		assert.NoError(t, err)
		assert.False(t, hasPodFinalizer(updatedPod))
		assert.Contains(t, updatedPod.Finalizers, "other-finalizer")
	})
}

// ================================================================================================
// PENDING POD DELETION TESTS
// ================================================================================================

func TestCheckPendingPodDeletions(t *testing.T) {
	ctx := context.Background()

	t.Run("removes finalizer when address not in NRP", func(t *testing.T) {
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-pod",
				Namespace:  "default",
				Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
			},
		}

		kubeClient := fake.NewSimpleClientset(pod)
		dt := &DiffTracker{
			kubeClient:          kubeClient,
			pendingPodDeletions: make(map[string]*PendingPodDeletion),
			NRPResources: NRP_State{
				Locations: make(map[string]NRPLocation),
			},
		}

		// Add pending deletion - address NOT in NRP
		dt.pendingPodDeletions["default/test-pod"] = &PendingPodDeletion{
			Namespace:  "default",
			Name:       "test-pod",
			ServiceUID: "egress-1",
			Address:    "10.0.0.1",
			Location:   "192.168.1.1",
			IsLastPod:  false,
			Timestamp:  time.Now().Format(time.RFC3339),
		}

		dt.CheckPendingPodDeletions(ctx)

		// Verify finalizer was removed
		updatedPod, err := kubeClient.CoreV1().Pods("default").Get(ctx, "test-pod", metav1.GetOptions{})
		assert.NoError(t, err)
		assert.False(t, hasPodFinalizer(updatedPod))

		// Verify tracking was cleaned up
		assert.Empty(t, dt.pendingPodDeletions)
	})

	t.Run("keeps finalizer when address still in NRP", func(t *testing.T) {
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-pod",
				Namespace:  "default",
				Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
			},
		}

		kubeClient := fake.NewSimpleClientset(pod)
		dt := &DiffTracker{
			kubeClient:          kubeClient,
			pendingPodDeletions: make(map[string]*PendingPodDeletion),
			NRPResources: NRP_State{
				Locations: map[string]NRPLocation{
					"192.168.1.1": {
						Addresses: map[string]NRPAddress{
							"10.0.0.1": {
								Services: utilsets.NewString("egress-1"),
							},
						},
					},
				},
			},
		}

		// Add pending deletion - address IS in NRP
		dt.pendingPodDeletions["default/test-pod"] = &PendingPodDeletion{
			Namespace:  "default",
			Name:       "test-pod",
			ServiceUID: "egress-1",
			Address:    "10.0.0.1",
			Location:   "192.168.1.1",
			IsLastPod:  false,
			Timestamp:  time.Now().Format(time.RFC3339),
		}

		dt.CheckPendingPodDeletions(ctx)

		// Verify finalizer was NOT removed
		updatedPod, err := kubeClient.CoreV1().Pods("default").Get(ctx, "test-pod", metav1.GetOptions{})
		assert.NoError(t, err)
		assert.True(t, hasPodFinalizer(updatedPod))

		// Verify tracking is still present
		assert.Len(t, dt.pendingPodDeletions, 1)
	})

	t.Run("skips last pod - handled by deleteOutboundService", func(t *testing.T) {
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-pod",
				Namespace:  "default",
				Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
			},
		}

		kubeClient := fake.NewSimpleClientset(pod)
		dt := &DiffTracker{
			kubeClient:          kubeClient,
			pendingPodDeletions: make(map[string]*PendingPodDeletion),
			NRPResources: NRP_State{
				Locations: make(map[string]NRPLocation), // Address not in NRP
			},
		}

		// Add pending deletion for LAST pod
		dt.pendingPodDeletions["default/test-pod"] = &PendingPodDeletion{
			Namespace:  "default",
			Name:       "test-pod",
			ServiceUID: "egress-1",
			Address:    "10.0.0.1",
			Location:   "192.168.1.1",
			IsLastPod:  true, // Last pod!
			Timestamp:  time.Now().Format(time.RFC3339),
		}

		dt.CheckPendingPodDeletions(ctx)

		// Verify finalizer was NOT removed (last pod is handled elsewhere)
		updatedPod, err := kubeClient.CoreV1().Pods("default").Get(ctx, "test-pod", metav1.GetOptions{})
		assert.NoError(t, err)
		assert.True(t, hasPodFinalizer(updatedPod))

		// Verify tracking is still present
		assert.Len(t, dt.pendingPodDeletions, 1)
	})

	t.Run("handles pod not found - cleans up tracking", func(t *testing.T) {
		kubeClient := fake.NewSimpleClientset() // No pod
		dt := &DiffTracker{
			kubeClient:          kubeClient,
			pendingPodDeletions: make(map[string]*PendingPodDeletion),
			NRPResources: NRP_State{
				Locations: make(map[string]NRPLocation),
			},
		}

		// Add pending deletion for non-existent pod
		dt.pendingPodDeletions["default/missing-pod"] = &PendingPodDeletion{
			Namespace:  "default",
			Name:       "missing-pod",
			ServiceUID: "egress-1",
			Address:    "10.0.0.1",
			Location:   "192.168.1.1",
			IsLastPod:  false,
			Timestamp:  time.Now().Format(time.RFC3339),
		}

		dt.CheckPendingPodDeletions(ctx)

		// Verify tracking was cleaned up
		assert.Empty(t, dt.pendingPodDeletions)
	})

	t.Run("handles empty pending deletions", func(t *testing.T) {
		kubeClient := fake.NewSimpleClientset()
		dt := &DiffTracker{
			kubeClient:          kubeClient,
			pendingPodDeletions: make(map[string]*PendingPodDeletion),
			NRPResources: NRP_State{
				Locations: make(map[string]NRPLocation),
			},
		}

		// Should not panic
		dt.CheckPendingPodDeletions(ctx)
	})
}

// ================================================================================================
// isAddressInNRPLocked TESTS
// ================================================================================================

func TestIsAddressInNRPLocked(t *testing.T) {
	tests := []struct {
		name       string
		nrpState   NRP_State
		serviceUID string
		location   string
		address    string
		expected   bool
	}{
		{
			name: "address exists with service",
			nrpState: NRP_State{
				Locations: map[string]NRPLocation{
					"192.168.1.1": {
						Addresses: map[string]NRPAddress{
							"10.0.0.1": {
								Services: utilsets.NewString("service-1", "service-2"),
							},
						},
					},
				},
			},
			serviceUID: "service-1",
			location:   "192.168.1.1",
			address:    "10.0.0.1",
			expected:   true,
		},
		{
			name: "address exists but different service",
			nrpState: NRP_State{
				Locations: map[string]NRPLocation{
					"192.168.1.1": {
						Addresses: map[string]NRPAddress{
							"10.0.0.1": {
								Services: utilsets.NewString("service-2"),
							},
						},
					},
				},
			},
			serviceUID: "service-1",
			location:   "192.168.1.1",
			address:    "10.0.0.1",
			expected:   false,
		},
		{
			name: "location does not exist",
			nrpState: NRP_State{
				Locations: map[string]NRPLocation{},
			},
			serviceUID: "service-1",
			location:   "192.168.1.1",
			address:    "10.0.0.1",
			expected:   false,
		},
		{
			name: "address does not exist in location",
			nrpState: NRP_State{
				Locations: map[string]NRPLocation{
					"192.168.1.1": {
						Addresses: map[string]NRPAddress{
							"10.0.0.2": { // Different address
								Services: utilsets.NewString("service-1"),
							},
						},
					},
				},
			},
			serviceUID: "service-1",
			location:   "192.168.1.1",
			address:    "10.0.0.1",
			expected:   false,
		},
		{
			name: "address exists but services is nil",
			nrpState: NRP_State{
				Locations: map[string]NRPLocation{
					"192.168.1.1": {
						Addresses: map[string]NRPAddress{
							"10.0.0.1": {
								Services: nil,
							},
						},
					},
				},
			},
			serviceUID: "service-1",
			location:   "192.168.1.1",
			address:    "10.0.0.1",
			expected:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dt := &DiffTracker{
				NRPResources: tt.nrpState,
			}

			result := dt.isAddressInNRPLocked(tt.serviceUID, tt.location, tt.address)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// ================================================================================================
// FINALIZER CONSTANT TESTS
// ================================================================================================

func TestFinalizerConstants(t *testing.T) {
	// Verify finalizer constants have expected values
	assert.Equal(t, "servicegateway.azure.com/service-cleanup", ServiceGatewayServiceCleanupFinalizer)
	assert.Equal(t, "servicegateway.azure.com/pod-cleanup", ServiceGatewayPodCleanupFinalizer)

	// Verify they are all unique
	finalizers := []string{
		ServiceGatewayServiceCleanupFinalizer,
		ServiceGatewayPodCleanupFinalizer,
	}
	seen := make(map[string]bool)
	for _, f := range finalizers {
		assert.False(t, seen[f], "Duplicate finalizer: %s", f)
		seen[f] = true
	}
}

// ================================================================================================
// PENDING DELETION TYPE TESTS
// ================================================================================================

func TestPendingPodDeletionType(t *testing.T) {
	pending := &PendingPodDeletion{
		Namespace:  "default",
		Name:       "test-pod",
		ServiceUID: "egress-service",
		Address:    "10.0.0.1",
		Location:   "192.168.1.1",
		IsLastPod:  true,
		Timestamp:  "2026-01-12T10:00:00Z",
	}

	assert.Equal(t, "default", pending.Namespace)
	assert.Equal(t, "test-pod", pending.Name)
	assert.Equal(t, "egress-service", pending.ServiceUID)
	assert.Equal(t, "10.0.0.1", pending.Address)
	assert.Equal(t, "192.168.1.1", pending.Location)
	assert.True(t, pending.IsLastPod)
	assert.Equal(t, "2026-01-12T10:00:00Z", pending.Timestamp)
}

// ================================================================================================
// CONCURRENT ACCESS TESTS
// ================================================================================================

func TestCheckPendingPodDeletions_Concurrent(t *testing.T) {
	ctx := context.Background()

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-pod",
			Namespace:  "default",
			Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
		},
	}

	kubeClient := fake.NewSimpleClientset(pod)
	dt := &DiffTracker{
		kubeClient:          kubeClient,
		pendingPodDeletions: make(map[string]*PendingPodDeletion),
		NRPResources: NRP_State{
			Locations: make(map[string]NRPLocation),
		},
	}

	dt.pendingPodDeletions["default/test-pod"] = &PendingPodDeletion{
		Namespace:  "default",
		Name:       "test-pod",
		ServiceUID: "egress-1",
		Address:    "10.0.0.1",
		Location:   "192.168.1.1",
		IsLastPod:  false,
		Timestamp:  time.Now().Format(time.RFC3339),
	}

	// Run multiple concurrent checks
	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func() {
			dt.CheckPendingPodDeletions(ctx)
			done <- true
		}()
	}

	// Wait for all to complete
	for i := 0; i < 10; i++ {
		<-done
	}

	// Should not panic and tracking should be cleaned up
	assert.Empty(t, dt.pendingPodDeletions)
}
