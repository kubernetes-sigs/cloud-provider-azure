package provider

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

// ResyncPeriod returns a function that generates a randomized resync duration
// to prevent controllers from syncing in lock-step and overloading the API server.
func ResyncPeriod(base time.Duration) func() time.Duration {
	return func() time.Duration {
		n, _ := rand.Int(rand.Reader, big.NewInt(1000))
		factor := float64(n.Int64())/1000.0 + 1.0
		return time.Duration(float64(base.Nanoseconds()) * factor)
	}
}

// setUpPodInformerForEgress creates an informer for Pods with egress labels.
// It uses label selectors to filter pods efficiently at the API server level,
// reducing memory and CPU overhead by only watching relevant pods.
func (az *Cloud) setUpPodInformerForEgress() {
	klog.V(2).Infof("setUpPodInformerForEgress: Setting up pod informer with label selector: %s", consts.PodLabelServiceEgressGateway)

	// Create a separate informer factory with label selector to filter pods at the API server
	podInformerFactory := informers.NewSharedInformerFactoryWithOptions(
		az.KubeClient,
		ResyncPeriod(12*time.Hour)(),
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			// Only watch pods with the egress gateway label
			options.LabelSelector = consts.PodLabelServiceEgressGateway
		}),
	)

	podInformer := podInformerFactory.Core().V1().Pods().Informer()
	_, err := podInformer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				pod := obj.(*v1.Pod)
				az.podInformerAddPod(pod)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				oldPod := oldObj.(*v1.Pod)
				newPod := newObj.(*v1.Pod)

				// Extract egress gateway names from labels
				var prevEgressGatewayName, currEgressGatewayName string
				if oldPod.Labels != nil {
					prevEgressGatewayName = strings.ToLower(oldPod.Labels[consts.PodLabelServiceEgressGateway])
				}
				if newPod.Labels != nil {
					currEgressGatewayName = strings.ToLower(newPod.Labels[consts.PodLabelServiceEgressGateway])
				}

				// Handle pod that just completed IP initialization
				podJustCompletedIPInitialization := (oldPod.Status.HostIP == "" || oldPod.Status.PodIP == "") &&
					(newPod.Status.HostIP != "" && newPod.Status.PodIP != "")
				if currEgressGatewayName != "" && podJustCompletedIPInitialization {
					klog.V(2).Infof("setUpPodInformerForEgress: Pod %s/%s completed IP initialization, treating as add event",
						newPod.Namespace, newPod.Name)
					az.podInformerAddPod(newPod)
					return
				}

				// Handle egress label change
				if prevEgressGatewayName != currEgressGatewayName {
					klog.V(2).Infof("setUpPodInformerForEgress: Pod %s/%s changed egress gateway from %s to %s",
						newPod.Namespace, newPod.Name, prevEgressGatewayName, currEgressGatewayName)
					if prevEgressGatewayName != "" {
						az.podInformerRemovePod(oldPod)
					}
					if currEgressGatewayName != "" {
						az.podInformerAddPod(newPod)
					}
					return
				}

				// Handle IP change (pod moved to different node or got new IP)
				if currEgressGatewayName != "" && (oldPod.Status.HostIP != newPod.Status.HostIP || oldPod.Status.PodIP != newPod.Status.PodIP) {
					klog.V(2).Infof("setUpPodInformerForEgress: Pod %s/%s IPs changed (HostIP: %s→%s, PodIP: %s→%s)",
						newPod.Namespace, newPod.Name, oldPod.Status.HostIP, newPod.Status.HostIP, oldPod.Status.PodIP, newPod.Status.PodIP)
					if oldPod.Status.HostIP != "" && oldPod.Status.PodIP != "" {
						az.podInformerRemovePod(oldPod)
					}
					if newPod.Status.HostIP != "" && newPod.Status.PodIP != "" {
						az.podInformerAddPod(newPod)
					}
					return
				}

				klog.V(4).Infof("setUpPodInformerForEgress: Pod %s/%s update has no relevant changes, skipping",
					newPod.Namespace, newPod.Name)
			},
			DeleteFunc: func(obj interface{}) {
				pod := obj.(*v1.Pod)
				az.podInformerRemovePod(pod)
			},
		})
	if err != nil {
		klog.Errorf("setUpPodInformerForEgress: Failed to add event handlers to pod informer: %v", err)
		return
	}

	// Start the informer factory
	klog.V(2).Infof("setUpPodInformerForEgress: Starting pod informer factory")
	podInformerFactory.Start(wait.NeverStop)
	podInformerFactory.WaitForCacheSync(wait.NeverStop)
	klog.V(2).Infof("setUpPodInformerForEgress: Pod informer successfully initialized and synced")
}

// podInformerAddPod handles pod addition events for egress.
// It validates the pod has the required egress label and IPs, then calls Engine.AddPod().
// The Engine handles all states:
// - Service doesn't exist → Engine creates NAT Gateway and buffers pod
// - Service being created → Engine buffers pod
// - Service ready → Engine adds pod immediately
func (az *Cloud) podInformerAddPod(pod *v1.Pod) {
	// Validate pod has egress label (should always be true due to label selector, but check anyway)
	if pod.Labels == nil || pod.Labels[consts.PodLabelServiceEgressGateway] == "" {
		klog.V(4).Infof("podInformerAddPod: Pod %s/%s has no egress label, skipping", pod.Namespace, pod.Name)
		return
	}

	// Validate pod has required IPs
	if pod.Status.HostIP == "" || pod.Status.PodIP == "" {
		klog.V(4).Infof("podInformerAddPod: Pod %s/%s has egress label but no HostIP or PodIP yet, skipping",
			pod.Namespace, pod.Name)
		return
	}

	egressName := strings.ToLower(pod.Labels[consts.PodLabelServiceEgressGateway])
	podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)

	klog.V(2).Infof("podInformerAddPod: Pod %s added with egress %s (HostIP=%s, PodIP=%s)",
		podKey, egressName, pod.Status.HostIP, pod.Status.PodIP)

	// Call Engine.AddPod - it handles all states and service creation
	az.diffTracker.AddPod(egressName, podKey, pod.Status.HostIP, pod.Status.PodIP)
}

// podInformerRemovePod handles pod deletion events for egress.
// It calls Engine.DeletePod() which handles last-pod deletion logic:
// - Not last pod → Engine removes pod, decrements counter, triggers LocationsUpdater
// - Last pod → Engine removes pod, marks service for deletion, triggers DeletionChecker
func (az *Cloud) podInformerRemovePod(pod *v1.Pod) {
	// Validate pod has egress label
	if pod.Labels == nil || pod.Labels[consts.PodLabelServiceEgressGateway] == "" {
		klog.V(4).Infof("podInformerRemovePod: Pod %s/%s has no egress label, skipping", pod.Namespace, pod.Name)
		return
	}

	// Need IPs to identify which location/address to remove
	if pod.Status.HostIP == "" || pod.Status.PodIP == "" {
		klog.Warningf("podInformerRemovePod: Pod %s/%s has egress label but no IPs, cannot remove",
			pod.Namespace, pod.Name)
		return
	}

	egressName := strings.ToLower(pod.Labels[consts.PodLabelServiceEgressGateway])
	podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)

	klog.V(2).Infof("podInformerRemovePod: Pod %s removed from egress %s (HostIP=%s, PodIP=%s)",
		podKey, egressName, pod.Status.HostIP, pod.Status.PodIP)

	// Call Engine.DeletePod - it handles all cases including last-pod deletion
	az.diffTracker.DeletePod(egressName, pod.Status.HostIP, pod.Status.PodIP)
}
