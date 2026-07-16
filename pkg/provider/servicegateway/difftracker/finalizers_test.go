package difftracker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
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

	t.Run("re-adds finalizer when the live pod lacks it despite the passed object showing it", func(t *testing.T) {
		// The passed informer-cache object still lists the finalizer, but the live pod has had it
		// stripped by an interleaved drain-gated removal during an IP-change reconcile. AddPodFinalizer
		// must decide against a fresh GET and re-add it, otherwise the live pod is left unprotected.
		staleObj := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "test-pod",
				Namespace:  "default",
				UID:        "uid-live",
				Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
			},
		}
		livePod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: "default",
				UID:       "uid-live",
			},
		}

		kubeClient := fake.NewSimpleClientset(livePod)
		dt := &DiffTracker{
			kubeClient: kubeClient,
		}

		err := dt.AddPodFinalizer(ctx, staleObj)
		assert.NoError(t, err)

		updatedPod, err := kubeClient.CoreV1().Pods("default").Get(ctx, "test-pod", metav1.GetOptions{})
		assert.NoError(t, err)
		assert.True(t, hasPodFinalizer(updatedPod),
			"finalizer must be re-added against the fresh pod even though the passed object still showed it")
	})

	t.Run("does not add finalizer to a same-name replacement pod (UID mismatch)", func(t *testing.T) {
		// The intended pod (uid-original) was deleted and a same-name pod (uid-replacement) recreated
		// before AddPodFinalizer's GET. Adding the finalizer to the replacement would strand it in
		// Terminating, because removePodFinalizer is UID-guarded and would refuse to strip it.
		intended := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: "default",
				UID:       "uid-original",
			},
		}
		replacement := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: "default",
				UID:       "uid-replacement",
			},
		}

		kubeClient := fake.NewSimpleClientset(replacement)
		dt := &DiffTracker{
			kubeClient: kubeClient,
		}

		err := dt.AddPodFinalizer(ctx, intended)
		assert.ErrorIs(t, err, ErrPodGoneOrReplaced,
			"a same-name replacement (UID mismatch) must be signalled so the caller skips registering the stale pod")

		got, err := kubeClient.CoreV1().Pods("default").Get(ctx, "test-pod", metav1.GetOptions{})
		assert.NoError(t, err)
		assert.False(t, hasPodFinalizer(got),
			"a same-name replacement pod (different UID) must not be given the cleanup finalizer")
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

// TestCheckPendingPodDeletions_PreservesReplacementPodEntry verifies that finalizer cleanup does not
// remove a same-name replacement pod's tracking entry that was re-added while the API calls ran
// without the lock held.
func TestCheckPendingPodDeletions_PreservesReplacementPodEntry(t *testing.T) {
	ctx := context.Background()
	const key = "default/test-pod"

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-pod",
			Namespace:  "default",
			UID:        "uid-a",
			Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
		},
	}
	kube := fake.NewSimpleClientset(pod)
	dt := &DiffTracker{
		kubeClient:          kube,
		pendingPodDeletions: make(map[string]*PendingPodDeletion),
		NRPResources:        NRPState{Locations: make(map[string]NRPLocation)},
	}
	// Original entry (uid-a); its address is not in NRP, so it is collected for finalizer removal.
	dt.pendingPodDeletions[key] = &PendingPodDeletion{
		Namespace: "default", Name: "test-pod", ServiceUID: "egress-1",
		Addresses: []string{"10.0.0.1"}, IsLastPod: false,
		UID: "uid-a", Timestamp: time.Now().Format(time.RFC3339),
	}

	// During the unlocked phase-2 GET, a same-name replacement pod (uid-b) re-adds a fresh entry.
	swapped := false
	kube.PrependReactor("get", "pods", func(_ ktesting.Action) (bool, runtime.Object, error) {
		if !swapped {
			swapped = true
			dt.mu.Lock()
			dt.pendingPodDeletions[key] = &PendingPodDeletion{
				Namespace: "default", Name: "test-pod", ServiceUID: "egress-1",
				Addresses: []string{"10.0.0.2"}, IsLastPod: false,
				UID: "uid-b", Timestamp: time.Now().Format(time.RFC3339),
			}
			dt.mu.Unlock()
		}
		return false, nil, nil // let the default tracker return the uid-a pod
	})

	dt.CheckPendingPodDeletions(ctx)

	dt.mu.Lock()
	cur, ok := dt.pendingPodDeletions[key]
	dt.mu.Unlock()
	if assert.True(t, ok, "the replacement pod's tracking entry must not be clobbered") {
		assert.Equal(t, "uid-b", cur.UID, "phase 3 must preserve the replacement entry (compare-and-delete on UID)")
	}
}

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
			NRPResources: NRPState{
				Locations: make(map[string]NRPLocation),
			},
		}

		// Add pending deletion - address NOT in NRP
		dt.pendingPodDeletions["default/test-pod"] = &PendingPodDeletion{
			Namespace:  "default",
			Name:       "test-pod",
			ServiceUID: "egress-1",
			Addresses:  []string{"10.0.0.1"},
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
			NRPResources: NRPState{
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
			Addresses:  []string{"10.0.0.1"},
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
			NRPResources: NRPState{
				Locations: make(map[string]NRPLocation), // Address not in NRP
			},
		}

		// Add pending deletion for LAST pod
		dt.pendingPodDeletions["default/test-pod"] = &PendingPodDeletion{
			Namespace:  "default",
			Name:       "test-pod",
			ServiceUID: "egress-1",
			Addresses:  []string{"10.0.0.1"},
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
			NRPResources: NRPState{
				Locations: make(map[string]NRPLocation),
			},
		}

		// Add pending deletion for non-existent pod
		dt.pendingPodDeletions["default/missing-pod"] = &PendingPodDeletion{
			Namespace:  "default",
			Name:       "missing-pod",
			ServiceUID: "egress-1",
			Addresses:  []string{"10.0.0.1"},
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
			NRPResources: NRPState{
				Locations: make(map[string]NRPLocation),
			},
		}

		// Should not panic
		dt.CheckPendingPodDeletions(ctx)
	})

	t.Run("keeps finalizer until every address of a dual-stack pod drains", func(t *testing.T) {
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
			NRPResources: NRPState{
				Locations: map[string]NRPLocation{
					"192.168.1.1": {
						Addresses: map[string]NRPAddress{
							"fd00::1": {Services: utilsets.NewString("egress-1")},
						},
					},
				},
			},
		}

		// A dual-stack pod contributes both its IPv4 and IPv6 addresses; only the IPv6 address is
		// still mapped in NRP.
		dt.pendingPodDeletions["default/test-pod"] = &PendingPodDeletion{
			Namespace:  "default",
			Name:       "test-pod",
			ServiceUID: "egress-1",
			Addresses:  []string{"10.0.0.1", "fd00::1"},
			IsLastPod:  false,
			Timestamp:  time.Now().Format(time.RFC3339),
		}

		// The IPv4 address has drained but IPv6 has not: the finalizer must stay.
		dt.CheckPendingPodDeletions(ctx)
		stillPod, err := kubeClient.CoreV1().Pods("default").Get(ctx, "test-pod", metav1.GetOptions{})
		assert.NoError(t, err)
		assert.True(t, hasPodFinalizer(stillPod), "finalizer must persist while the IPv6 address is still in NRP")
		assert.Len(t, dt.pendingPodDeletions, 1)

		// Drain the IPv6 address too; now the finalizer can be removed.
		delete(dt.NRPResources.Locations["192.168.1.1"].Addresses, "fd00::1")
		dt.CheckPendingPodDeletions(ctx)
		updatedPod, err := kubeClient.CoreV1().Pods("default").Get(ctx, "test-pod", metav1.GetOptions{})
		assert.NoError(t, err)
		assert.False(t, hasPodFinalizer(updatedPod), "finalizer must be removed once all addresses have drained")
		assert.Empty(t, dt.pendingPodDeletions)
	})
}

// TestGuardNonLastPodFinalizerRemovedOnlyAfterNRPDrain verifies the end-to-end contract for a
// non-last egress pod deletion: DeletePod enqueues a drain-gated PendingPodDeletion (it does not
// strip the finalizer inline), CheckPendingPodDeletions keeps the finalizer while the pod's
// address is still in NRP, and removes it only once the address has been drained. This guards the
// ordering that prevents the pod (and its IP) from being reclaimed while NRP still maps the
// address to the service's NAT Gateway.
func TestGuardNonLastPodFinalizerRemovedOnlyAfterNRPDrain(t *testing.T) {
	ctx := context.Background()
	uid := "egress-drain-gate"

	// Pod "a" is the non-last pod we delete; pod "b" keeps the service (NAT Gateway) alive.
	podA := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "a",
			Namespace:  "ns",
			Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
		},
	}
	kube := fake.NewSimpleClientset(podA)

	dt := newTestDiffTracker()
	dt.kubeClient = kube
	dt.NRPResources.NATGateways.Insert(uid)
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewOutboundServiceConfig(uid, nil),
		State:      StateCreated,
	}
	dt.AddPod(uid, "ns/a", "10.0.0.1", "10.244.0.1")
	dt.AddPod(uid, "ns/b", "10.0.0.1", "10.244.0.2")

	// NRP currently still maps both addresses (the location sync has not drained "a" yet).
	dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{
		Addresses: map[string]NRPAddress{
			"10.244.0.1": {Services: utilsets.NewString(uid)},
			"10.244.0.2": {Services: utilsets.NewString(uid)},
		},
	}

	// Delete the non-last pod "a".
	res := dt.DeletePod(uid, "10.0.0.1", []string{"10.244.0.1"}, "ns", "a", "")
	assert.False(t, res.IsLastPod, "deleting one of two pods is not the last-pod case")

	// It must be enqueued for drain-gated finalizer removal, not stripped inline.
	ppd, ok := dt.pendingPodDeletions["ns/a"]
	if assert.True(t, ok, "non-last pod must be enqueued for drain-gated finalizer removal") {
		assert.False(t, ppd.IsLastPod)
	}

	// While the address is still in NRP, the finalizer must remain.
	dt.CheckPendingPodDeletions(ctx)
	got, err := kube.CoreV1().Pods("ns").Get(ctx, "a", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.True(t, hasPodFinalizer(got), "finalizer must NOT be removed before NRP drains the address")
	assert.Len(t, dt.pendingPodDeletions, 1)

	// Simulate the LocationsUpdater draining "a" from NRP.
	loc := dt.NRPResources.Locations["10.0.0.1"]
	delete(loc.Addresses, "10.244.0.1")
	dt.NRPResources.Locations["10.0.0.1"] = loc

	// Now the finalizer is removed and the tracking entry is cleaned up.
	dt.CheckPendingPodDeletions(ctx)
	got, err = kube.CoreV1().Pods("ns").Get(ctx, "a", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.False(t, hasPodFinalizer(got), "finalizer must be removed after NRP drains the address")
	assert.Empty(t, dt.pendingPodDeletions)
}

// TestGuardNonLastPodFinalizerRetriesOnTransientUpdateError verifies that a transient
// (non-conflict) failure while removing a non-last pod's finalizer does not strand the pod in
// Terminating. removePodFinalizer only retries on 409 Conflict, so durability comes from the
// per-cycle retry in CheckPendingPodDeletions: the pod stays enqueued and the finalizer is
// removed on a later cycle once the API call succeeds.
func TestGuardNonLastPodFinalizerRetriesOnTransientUpdateError(t *testing.T) {
	ctx := context.Background()
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "p",
			Namespace:  "ns",
			Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
		},
	}
	kube := fake.NewSimpleClientset(pod)

	// Fail only the first finalizer-removing Update with a transient server error (HTTP 500),
	// which is NOT a 409 Conflict and so is not retried inside removePodFinalizer.
	failedOnce := false
	kube.PrependReactor("update", "pods", func(_ ktesting.Action) (bool, runtime.Object, error) {
		if !failedOnce {
			failedOnce = true
			return true, nil, apierrors.NewInternalError(fmt.Errorf("transient server error"))
		}
		return false, nil, nil
	})

	dt := &DiffTracker{
		kubeClient:          kube,
		pendingPodDeletions: make(map[string]*PendingPodDeletion),
		NRPResources: NRPState{
			Locations: make(map[string]NRPLocation), // address already drained from NRP
		},
	}
	dt.pendingPodDeletions["ns/p"] = &PendingPodDeletion{
		Namespace:  "ns",
		Name:       "p",
		ServiceUID: "egress-1",
		Addresses:  []string{"10.244.0.1"},
		IsLastPod:  false,
		Timestamp:  time.Now().Format(time.RFC3339),
	}

	// First cycle: the Update fails transiently. The finalizer must remain and the pod must stay
	// enqueued for retry rather than being stranded.
	dt.CheckPendingPodDeletions(ctx)
	got, err := kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.True(t, hasPodFinalizer(got), "finalizer must remain after a transient Update error")
	assert.Len(t, dt.pendingPodDeletions, 1, "pod must stay enqueued for retry after a transient error")

	// Second cycle: the Update now succeeds, so the finalizer is removed and tracking cleared.
	dt.CheckPendingPodDeletions(ctx)
	got, err = kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.False(t, hasPodFinalizer(got), "finalizer must be removed once the Update succeeds on retry")
	assert.Empty(t, dt.pendingPodDeletions)
}

// TestGuardAddPodFinalizerRetriesOnConflict verifies that AddPodFinalizer survives a transient
// 409 Conflict (which is expected because it runs during the pod's IP-assignment status burst)
// by getting a fresh copy and retrying, rather than giving up and registering the pod for egress
// with no finalizer.
func TestGuardAddPodFinalizerRetriesOnConflict(t *testing.T) {
	ctx := context.Background()
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p",
			Namespace: "ns",
		},
	}
	kube := fake.NewSimpleClientset(pod)

	// Fail the first Update with a 409 Conflict (stale resourceVersion), then allow it.
	failedOnce := false
	kube.PrependReactor("update", "pods", func(_ ktesting.Action) (bool, runtime.Object, error) {
		if !failedOnce {
			failedOnce = true
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "p", fmt.Errorf("stale resourceVersion"))
		}
		return false, nil, nil
	})

	dt := &DiffTracker{kubeClient: kube}

	// Pass the stale informer copy (no finalizer); the implementation must Get-fresh and retry.
	err := dt.AddPodFinalizer(ctx, pod.DeepCopy())
	assert.NoError(t, err, "AddPodFinalizer must succeed after a transient conflict")

	got, err := kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.True(t, hasPodFinalizer(got), "finalizer must be present after the conflict is retried")
}

// ================================================================================================
// isAddressInNRPLocked TESTS
// ================================================================================================

func TestOutboundAddressInAnyNRPLocationLocked(t *testing.T) {
	tests := []struct {
		name       string
		nrpState   NRPState
		serviceUID string
		location   string
		address    string
		expected   bool
	}{
		{
			name: "address exists with service",
			nrpState: NRPState{
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
			nrpState: NRPState{
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
			nrpState: NRPState{
				Locations: map[string]NRPLocation{},
			},
			serviceUID: "service-1",
			location:   "192.168.1.1",
			address:    "10.0.0.1",
			expected:   false,
		},
		{
			name: "address does not exist in location",
			nrpState: NRPState{
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
			nrpState: NRPState{
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

			result := dt.outboundAddressInAnyNRPLocationLocked(tt.serviceUID, tt.address)
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
		Addresses:  []string{"10.0.0.1"},
		IsLastPod:  true,
		Timestamp:  "2026-01-12T10:00:00Z",
	}

	assert.Equal(t, "default", pending.Namespace)
	assert.Equal(t, "test-pod", pending.Name)
	assert.Equal(t, "egress-service", pending.ServiceUID)
	assert.Equal(t, []string{"10.0.0.1"}, pending.Addresses)
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
		NRPResources: NRPState{
			Locations: make(map[string]NRPLocation),
		},
	}

	dt.pendingPodDeletions["default/test-pod"] = &PendingPodDeletion{
		Namespace:  "default",
		Name:       "test-pod",
		ServiceUID: "egress-1",
		Addresses:  []string{"10.0.0.1"},
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

// TestAddPodPreservesFinalizerOnEgressIdentityChange verifies that re-registering a live pod under a
// new egress identity drops the pending finalizer-removal record left by the previous identity, so
// CheckPendingPodDeletions does not strip the still-needed cleanup finalizer once the old identity
// drains from NRP.
func TestAddPodPreservesFinalizerOnEgressIdentityChange(t *testing.T) {
	const (
		oldEgress = "egress-a"
		newEgress = "egress-b"
		location  = "10.0.0.1"
		address   = "10.244.0.7"
	)
	pod := newFinalizerPod("foo", "uid-live", true)
	kube := fake.NewSimpleClientset(pod)

	dt := newTestDiffTracker()
	dt.kubeClient = kube

	dt.pendingPodDeletions["default/foo"] = &PendingPodDeletion{
		Namespace:  "default",
		Name:       "foo",
		UID:        "uid-live",
		ServiceUID: oldEgress,
		Addresses:  []string{address},
	}
	dt.NRPResources.NATGateways.Insert(newEgress)

	dt.AddPod(newEgress, "default/foo", location, address)
	if _, pending := dt.pendingPodDeletions["default/foo"]; pending {
		t.Fatalf("re-registering a live pod must drop its stale pending finalizer-removal record")
	}

	dt.CheckPendingPodDeletions(context.Background())
	got, err := kube.CoreV1().Pods("default").Get(context.Background(), "foo", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Contains(t, got.ObjectMeta.Finalizers, ServiceGatewayPodCleanupFinalizer,
		"a live pod under a new egress identity must retain its cleanup finalizer")
}
