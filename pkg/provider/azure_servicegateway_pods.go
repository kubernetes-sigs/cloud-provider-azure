package provider

import (
	"crypto/rand"
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
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/difftracker"
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

func (az *Cloud) podInformerAddPod(pod *v1.Pod) {
	// klog.Infof("CLB-ENECHITOAIA-Pod %s/%s has been added\n", pod.Namespace, pod.Name)
	if pod.Labels == nil || pod.Labels[consts.PodLabelServiceEgressGateway] == "" {
		klog.Errorf("podInformerAddPod: Pod %s/%s has no labels or staticGatewayConfiguration label. Cannot process add event.",
			pod.Namespace, pod.Name)
		return
	}
	// logObject(pod)
	if pod.Status.HostIP == "" || pod.Status.PodIP == "" {
		klog.Errorf("podInformerAddPod: Pod %s/%s has no HostIP or PodIP. Cannot process add event.",
			pod.Namespace, pod.Name)
		return
	}
	// klog.Infof("podInformerAddPod: Pod %s/%s has Location: %s, PodIP: %s\n", pod.Namespace, pod.Name, pod.Status.HostIP, pod.Status.PodIP)
	staticGatewayConfigurationName := strings.ToLower(pod.Labels[consts.PodLabelServiceEgressGateway])
	klog.Infof("podInformerAddPod: Pod %s/%s has static gateway configuration: %s", pod.Namespace, pod.Name, staticGatewayConfigurationName)
	_, ok := az.diffTracker.LocalServiceNameToNRPServiceMap.Load(staticGatewayConfigurationName)
	if ok {
		klog.Infof("podInformerAddPod: Pod %s/%s has static gateway configuration annotation. Found in localServiceNameToNRPServiceMap.",
			pod.Namespace, pod.Name)
		az.diffTracker.UpdateK8sPod(
			difftracker.UpdatePodInputType{
				PodOperation:           difftracker.ADD,
				PublicOutboundIdentity: staticGatewayConfigurationName,
				Location:               pod.Status.HostIP,
				Address:                pod.Status.PodIP,
			},
		)
		az.TriggerLocationAndNRPServiceBatchUpdate()
	} else {
		klog.Infof("podInformerAddPod: Pod %s/%s has static gateway configuration annotation. Not found in localServiceNameToNRPServiceMap.",
			pod.Namespace, pod.Name)
		key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(pod)
		if err != nil {
			klog.Errorf("Failed to get key for pod %s/%s: %v", pod.Namespace, pod.Name, err)
			return
		}
		az.diffTracker.PodEgressQueue.Add(difftracker.PodCrudEvent{
			AddPodEvent: difftracker.AddPodEvent{
				Key: key,
			},
			EventType: "Add",
		})
	}
}

func (az *Cloud) podInformerRemovePod(pod *v1.Pod) {
	// klog.Infof("CLB-ENECHITOAIA-Pod %s/%s has been deleted\n", pod.Namespace, pod.Name)
	if pod.Labels == nil || pod.Labels[consts.PodLabelServiceEgressGateway] == "" {
		klog.Errorf("Pod %s/%s has no labels. Cannot process delete event.",
			pod.Namespace, pod.Name)
		return
	}

	staticGatewayConfigurationName := strings.ToLower(pod.Labels[consts.PodLabelServiceEgressGateway])
	counter, ok := az.diffTracker.LocalServiceNameToNRPServiceMap.Load(staticGatewayConfigurationName)
	// klog.Infof("CLB-ENECHITOAIA-Pod %s/%s has static gateway configuration: %s", pod.Namespace, pod.Name, staticGatewayConfigurationName)
	klog.Infof("CLB-ENECHITOAIA-Pod %s/%s has Location: %s, PodIP: %s\n", pod.Namespace, pod.Name, pod.Status.HostIP, pod.Status.PodIP)
	// logObject(pod)
	logSyncStringIntMap("CLB_ENECHITOAOIA: LocalServiceNameToNRPServiceMap", &az.diffTracker.LocalServiceNameToNRPServiceMap)
	if ok {
		if counter.(int) > 1 {
			az.diffTracker.UpdateK8sPod(
				difftracker.UpdatePodInputType{
					PodOperation:           difftracker.REMOVE,
					PublicOutboundIdentity: staticGatewayConfigurationName,
					Location:               pod.Status.HostIP,
					Address:                pod.Status.PodIP,
				},
			)
			az.TriggerLocationAndNRPServiceBatchUpdate()
		} else {
			az.diffTracker.PodEgressQueue.Add(difftracker.PodCrudEvent{
				DeletePodEvent: difftracker.DeletePodEvent{
					Location: pod.Status.HostIP,
					Address:  pod.Status.PodIP,
					Service:  staticGatewayConfigurationName,
				},
				EventType: "Delete",
			})
		}
	} else {
		klog.Errorf("podInformerRemovePod: Pod %s/%s has static gateway configuration %s, but it is not found in localServiceNameToNRPServiceMap. Cannot decrement its counter.",
			pod.Namespace, pod.Name, staticGatewayConfigurationName)
	}
}

func (az *Cloud) setUpPodInformerForEgress() {
	// klog.Infof("CLB-ENECHITOAIA-setUpPodInformerForEgress: setting up pod informer for egress with label selector: %s\n", consts.PodLabelServiceEgressGateway)
	podInformerFactory := informers.NewSharedInformerFactoryWithOptions(
		az.KubeClient,
		ResyncPeriod(1*time.Second)(),
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = consts.PodLabelServiceEgressGateway
		}),
	)
	podInformer := podInformerFactory.Core().V1().Pods().Informer()
	// klog.Infof("podInformer: %v\n", podInformer)
	_, err := podInformer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				pod := obj.(*v1.Pod)
				az.podInformerAddPod(pod)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				oldPod := oldObj.(*v1.Pod)
				newPod := newObj.(*v1.Pod)
				// klog.Infof("CLB-ENECHITOAIA-Pod %s/%s has been updated\n", newPod.Namespace, newPod.Name)
				// logObject(oldObj)
				// logObject(newObj)

				var (
					prevEgressGatewayName = ""
					currEgressGatewayName = ""
				)
				if oldPod.Labels != nil {
					prevEgressGatewayName = strings.ToLower(oldPod.Labels[consts.PodLabelServiceEgressGateway])
				}
				if newPod.Labels != nil {
					currEgressGatewayName = strings.ToLower(newPod.Labels[consts.PodLabelServiceEgressGateway])
				}

				podJustCompletedNodeIPAndPodIPInitialization := (oldPod.Status.HostIP == "" || oldPod.Status.PodIP == "") && (newPod.Status.HostIP != "" && newPod.Status.PodIP != "")
				if currEgressGatewayName != "" && podJustCompletedNodeIPAndPodIPInitialization {
					// klog.Infof("CLB-ENECHITOAIA-Pod %s/%s has just completed NodeIP and PodIP initialization. Treating as an Add event.",
					// 	newPod.Namespace, newPod.Name)
					az.podInformerAddPod(newPod)
					return
				}

				if prevEgressGatewayName == currEgressGatewayName {
					klog.Infof("setUpPodInformerForEgress: Pod %s/%s has no change in static gateway configuration. No action needed.",
						newPod.Namespace, newPod.Name)
					return
				}

				// klog.Infof("CLB-ENECHITOAIA-Pod %s/%s has changed static gateway configuration from %s to %s",
				// 	newPod.Namespace, newPod.Name, prevEgressGatewayName, currEgressGatewayName)
				if prevEgressGatewayName != "" {
					az.podInformerRemovePod(oldPod)
				}

				if currEgressGatewayName != "" {
					az.podInformerAddPod(newPod)
				}
			},
			DeleteFunc: func(obj interface{}) {
				pod := obj.(*v1.Pod)
				az.podInformerRemovePod(pod)
			},
		})
	if err != nil {
		klog.Errorf("setUpPodInformerForEgress: failed to add event handlers to pod informer: %v\n", err)
		return
	}
	// else {
	// 	klog.Infof("CLB-ENECHITOAIA-setUpPodInformerForEgress: successfully added event handlers to pod informer\n")
	// }

	az.podLister = podInformerFactory.Core().V1().Pods().Lister()
	// klog.Infof("az.podLister: %v\n", az.podLister)
	// klog.Infof("CLB-ENECHITOAIA-setUpPodInformerForEgress: starting pod informer factory\n")
	podInformerFactory.Start(wait.NeverStop)
	podInformerFactory.WaitForCacheSync(wait.NeverStop)
	// klog.Infof("CLB-ENECHITOAIA-setUpPodInformerForEgress: end of setUpPodInformerForEgress\n")
}
