/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ipam

import (
	"fmt"
	"net"
	"sync"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	informers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
	netutils "k8s.io/utils/net"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/nodeipam/ipam/cidrset"
	providerazure "sigs.k8s.io/cloud-provider-azure/pkg/provider"
	nodeutil "sigs.k8s.io/cloud-provider-azure/pkg/util/controller/node"
	utilnode "sigs.k8s.io/cloud-provider-azure/pkg/util/node"
	utiltaints "sigs.k8s.io/cloud-provider-azure/pkg/util/taints"
)

// cloudCIDRAllocator allocates node CIDRs according to the node subnet mask size
// tagged on each VMSS/VMAS.
type cloudCIDRAllocator struct {
	client clientset.Interface
	cloud  *providerazure.Cloud

	// nodeLister is able to list/get nodes and is populated by the shared informer passed to
	// NewCloudCIDRAllocator.
	nodeLister corelisters.NodeLister
	// nodesSynced returns true if the node shared informer has been synced at least once.
	nodesSynced cache.InformerSynced

	// Channel that is used to pass updating Nodes to the background.
	// This increases the throughput of CIDR assignment by parallelization
	// and not blocking on long operations (which shouldn't be done from
	// event handlers anyway).
	nodeUpdateChannel chan nodeReservedCIDRs
	recorder          record.EventRecorder

	// Keep a set of nodes that are correctly being processed to avoid races in CIDR allocation
	lock              sync.Mutex
	nodesInProcessing map[string]struct{}

	// nodeName -> nodeSubnetMaskSizes for ipv4 and/or ipv6
	nodeNameSubnetMaskSizesMap map[string][]int
	maxSubnetMaskSizes         []int
	cidrSets                   []*cidrset.CidrSet
	clusterCIDRs               []*net.IPNet

	nodeNamePodCIDRsMap map[string][]string
}

var _ CIDRAllocator = (*cloudCIDRAllocator)(nil)

// NewCloudCIDRAllocator creates a new cloud CIDR allocator.
func NewCloudCIDRAllocator(
	client clientset.Interface,
	cloud cloudprovider.Interface,
	nodeInformer informers.NodeInformer,
	allocatorParams CIDRAllocatorParams,
	nodeList *v1.NodeList,
) (CIDRAllocator, error) {
	if client == nil {
		klog.Fatalf("kubeClient is nil when starting NodeController")
	}

	eventBroadcaster := record.NewBroadcaster()
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "cidrAllocator"})
	eventBroadcaster.StartStructuredLogging(0)
	klog.V(0).Infof("Sending events to api server.")
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: client.CoreV1().Events("")})

	az, ok := cloud.(*providerazure.Cloud)
	if !ok {
		err := fmt.Errorf("cloudCIDRAllocator does not support %v provider", cloud.ProviderName())
		return nil, err
	}

	ca := &cloudCIDRAllocator{
		client:                     client,
		cloud:                      az,
		nodeLister:                 nodeInformer.Lister(),
		nodesSynced:                nodeInformer.Informer().HasSynced,
		nodeUpdateChannel:          make(chan nodeReservedCIDRs, cidrUpdateQueueSize),
		recorder:                   recorder,
		nodesInProcessing:          map[string]struct{}{},
		nodeNameSubnetMaskSizesMap: make(map[string][]int),
		maxSubnetMaskSizes:         make([]int, len(allocatorParams.ClusterCIDRs)),
		clusterCIDRs:               allocatorParams.ClusterCIDRs,
		nodeNamePodCIDRsMap:        make(map[string][]string),
	}

	// update the node subnet mask size
	if nodeList != nil {
		for _, node := range nodeList.Items {
			if node.Spec.ProviderID == "" {
				klog.Warningf("NewCloudCIDRAllocator: failed when trying to read the node mask size on node %s: no provider ID", node.Name)
				continue
			}
			err := ca.updateNodeSubnetMaskSizes(node.Name, node.Spec.ProviderID)
			if err != nil {
				return nil, err
			}
		}
	}

	// find the max node subnet mask size and use them to create cidr sets
	ca.updateMaxSubnetMaskSizes()

	cidrSets := make([]*cidrset.CidrSet, len(allocatorParams.ClusterCIDRs))
	for i, clusterCIDR := range allocatorParams.ClusterCIDRs {
		clusterCIDR := clusterCIDR
		cidrSet, err := cidrset.NewCIDRSet(clusterCIDR, ca.maxSubnetMaskSizes[i])
		if err != nil {
			return nil, err
		}
		cidrSets[i] = cidrSet
	}
	ca.cidrSets = cidrSets

	if allocatorParams.ServiceCIDR != nil {
		filterOutServiceRange(ca.clusterCIDRs, ca.cidrSets, allocatorParams.ServiceCIDR)
	} else {
		klog.V(0).Info("No Service CIDR provided. Skipping filtering out service addresses.")
	}

	if allocatorParams.SecondaryServiceCIDR != nil {
		filterOutServiceRange(ca.clusterCIDRs, ca.cidrSets, allocatorParams.SecondaryServiceCIDR)
	} else {
		klog.V(0).Info("No Secondary Service CIDR provided. Skipping filtering out secondary service addresses.")
	}

	// mark the CIDRs on the existing nodes as used
	if nodeList != nil {
		for _, node := range nodeList.Items {
			node := node
			if len(node.Spec.PodCIDRs) == 0 {
				klog.V(4).Infof("Node %v has no CIDR, ignoring", node.Name)
				continue
			}
			klog.V(4).Infof("Node %v has CIDR %s, occupying it in CIDR map", node.Name, node.Spec.PodCIDR)
			if err := ca.occupyCIDRs(&node); err != nil {
				// This will happen if:
				// 1. We find garbage in the podCIDRs field. Retrying is useless.
				// 2. CIDR out of range: This means a node CIDR has changed.
				// This error will keep crashing cloud controller manager.
				return nil, err
			}
		}
	}

	_, _ = nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: nodeutil.CreateAddNodeHandler(ca.AllocateOrOccupyCIDR),
		UpdateFunc: nodeutil.CreateUpdateNodeHandler(func(_, newNode *v1.Node) error {
			if newNode.Spec.PodCIDR == "" {
				return ca.AllocateOrOccupyCIDR(newNode)
			}
			// Even if PodCIDR is assigned, but NetworkUnavailable condition is
			// set to true, we need to process the node to set the condition.
			networkUnavailableTaint := &v1.Taint{Key: v1.TaintNodeNetworkUnavailable, Effect: v1.TaintEffectNoSchedule}
			_, cond := nodeutil.GetNodeCondition(&newNode.Status, v1.NodeNetworkUnavailable)
			if cond == nil || cond.Status != v1.ConditionFalse || utiltaints.TaintExists(newNode.Spec.Taints, networkUnavailableTaint) {
				return ca.AllocateOrOccupyCIDR(newNode)
			}
			return nil
		}),
		DeleteFunc: nodeutil.CreateDeleteNodeHandler(ca.ReleaseCIDR),
	})

	klog.V(0).Infof("Using cloud CIDR allocator (provider: %v)", cloud.ProviderName())
	return ca, nil
}

// updateMaxSubnetMaskSizes finds the max subnet size on all existing nodes
func (ca *cloudCIDRAllocator) updateMaxSubnetMaskSizes() {
	ca.lock.Lock()
	defer ca.lock.Unlock()

	maxNodeSubnetMaskSizes := make([]int, len(ca.clusterCIDRs))
	for i := 0; i < len(ca.clusterCIDRs); i++ {
		maxNodeSubnetMaskSizes[i] = 0
	}

	// find the max mask sizes on all existing nodes
	for _, sizes := range ca.nodeNameSubnetMaskSizesMap {
		for i := 0; i < len(ca.clusterCIDRs); i++ {
			if sizes[i] > maxNodeSubnetMaskSizes[i] {
				maxNodeSubnetMaskSizes[i] = sizes[i]
			}
		}
	}

	// update them to the cloud allocator
	ca.maxSubnetMaskSizes = maxNodeSubnetMaskSizes
}

// updateNodeSubnetMaskSizes gets the node's VMSS/VMAS, reads the mask size tag on it and updates them into the map
func (ca *cloudCIDRAllocator) updateNodeSubnetMaskSizes(nodeName, providerID string) error {
	ca.lock.Lock()
	defer ca.lock.Unlock()

	if providerID == "" {
		klog.Warningf("updateNodeSubnetMaskSizes(%s): empty providerID", providerID)
	}

	ipv4Mask, ipv6Mask, err := ca.cloud.VMSet.GetNodeCIDRMasksByProviderID(providerID)
	if err != nil {
		klog.Warningf("updateNodeSubnetMaskSizes(%s): cannot get node subnet mask size by providerID: %v", providerID, err)
	}

	if ipv4Mask == 0 {
		ipv4Mask = consts.DefaultNodeMaskCIDRIPv4
	}
	if ipv6Mask == 0 {
		ipv6Mask = consts.DefaultNodeMaskCIDRIPv6
	}

	maskSizes := make([]int, 0)
	for _, clusterCIDR := range ca.clusterCIDRs {
		clusterMaskSize, _ := clusterCIDR.Mask.Size()
		if netutils.IsIPv6CIDR(clusterCIDR) {
			if ipv6Mask < clusterMaskSize {
				return fmt.Errorf("updateNodeSubnetMaskSizes: invalid ipv6 mask size %d of node %s because it is out of the range of the cluster CIDR with the mask size %d", ipv6Mask, nodeName, clusterMaskSize)
			}
			maskSizes = append(maskSizes, ipv6Mask)
		} else {
			if ipv4Mask < clusterMaskSize {
				return fmt.Errorf("updateNodeSubnetMaskSizes: invalid ipv4 mask size %d of node %s because it is out of the range of the cluster CIDR with the mask size %d", ipv4Mask, nodeName, clusterMaskSize)
			}
			maskSizes = append(maskSizes, ipv4Mask)
		}
	}

	ca.nodeNameSubnetMaskSizesMap[nodeName] = maskSizes
	return nil
}

func (ca *cloudCIDRAllocator) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()

	klog.Infof("Starting cloud CIDR allocator")
	defer klog.Infof("Shutting down cloud CIDR allocator")

	if !cache.WaitForNamedCacheSync("cidrallocator", stopCh, ca.nodesSynced) {
		return
	}

	for i := 0; i < cidrUpdateWorkers; i++ {
		go ca.worker(stopCh)
	}

	<-stopCh
}

func (ca *cloudCIDRAllocator) worker(stopChan <-chan struct{}) {
	for {
		select {
		case workItem, ok := <-ca.nodeUpdateChannel:
			if !ok {
				klog.Warning("Channel nodeUpdateChannel was unexpectedly closed")
				return
			}
			if err := ca.updateCIDRsAllocation(workItem); err != nil {
				// Requeue the failed node for update again.
				ca.nodeUpdateChannel <- workItem
			}
		case <-stopChan:
			return
		}
	}
}

func (ca *cloudCIDRAllocator) insertNodeToProcessing(nodeName string) bool {
	ca.lock.Lock()
	defer ca.lock.Unlock()
	if _, found := ca.nodesInProcessing[nodeName]; found {
		return false
	}
	ca.nodesInProcessing[nodeName] = struct{}{}
	return true
}

func (ca *cloudCIDRAllocator) removeNodeFromProcessing(nodeName string) {
	ca.lock.Lock()
	defer ca.lock.Unlock()
	delete(ca.nodesInProcessing, nodeName)
}

// marks node.PodCIDRs[...] as used in allocator's tracked cidrSet
func (ca *cloudCIDRAllocator) occupyCIDRs(node *v1.Node) error {
	defer ca.removeNodeFromProcessing(node.Name)
	if len(node.Spec.PodCIDRs) == 0 {
		return nil
	}
	podCIDRs := make([]string, len(ca.clusterCIDRs))
	for i, cidr := range node.Spec.PodCIDRs {
		_, podCIDR, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("failed to parse node %s, CIDR %s", node.Name, node.Spec.PodCIDR)
		}
		// If node has a pre allocate cidr that does not exist in our cidrs.
		// This will happen if cluster went from dualstack(multi cidrs) to non-dualstack
		// then we have now way of locking it
		if i >= len(ca.cidrSets) {
			return fmt.Errorf("node:%s has an allocated cidr: %v at index:%v that does not exist in cluster cidrs configuration", node.Name, cidr, i)
		}

		if err := ca.cidrSets[i].Occupy(podCIDR); err != nil {
			return fmt.Errorf("failed to mark cidr[%v] at i [%v] as occupied for node %s: %w", podCIDR, i, node.Name, err)
		}

		podCIDRs[i] = cidr
	}
	ca.lock.Lock()
	ca.nodeNamePodCIDRsMap[node.Name] = podCIDRs
	ca.lock.Unlock()

	return nil
}

// WARNING: If you're adding any return calls or defer any more work from this
// function you have to make sure to update nodesInProcessing properly with the
// disposition of the node when the work is done.
func (ca *cloudCIDRAllocator) AllocateOrOccupyCIDR(node *v1.Node) error {
	if node == nil || node.Spec.ProviderID == "" {
		return nil
	}
	if !ca.insertNodeToProcessing(node.Name) {
		klog.V(2).InfoS("Node is already in a process of CIDR assignment", "node", klog.KObj(node))
		return nil
	}

	err := ca.updateNodeSubnetMaskSizes(node.Name, node.Spec.ProviderID)
	if err != nil {
		klog.Errorf("AllocateOrOccupyCIDR(%s): failed to update node subnet mask sizes: %v", node.Name, err)
		return err
	}
	ca.updateMaxSubnetMaskSizes()

	// Keep the mask size in each cidr set the max one when new node added in.
	// The mask size would not change unless the new node is from a new VMSS/VMAS
	// and the mask value tagging on it is different from the existing ones.
	for i, cidrSet := range ca.cidrSets {
		cidrSet := cidrSet
		err := cidrSet.UpdateSubnetMaskSize(ca.maxSubnetMaskSizes[i], ca.nodeNamePodCIDRsMap)
		if err != nil {
			ca.removeNodeFromProcessing(node.Name)
			return err
		}
		ca.cidrSets[i] = cidrSet
	}

	if len(node.Spec.PodCIDRs) > 0 {
		return ca.occupyCIDRs(node)
	}

	allocated := nodeReservedCIDRs{
		nodeName:       node.Name,
		allocatedCIDRs: make([]*net.IPNet, len(ca.cidrSets)),
	}

	for i := range ca.cidrSets {
		podCIDR, err := ca.cidrSets[i].AllocateNextWithNodeMaskSize(ca.nodeNameSubnetMaskSizesMap[node.Name][i])
		if err != nil {
			ca.removeNodeFromProcessing(node.Name)
			nodeutil.RecordNodeStatusChange(ca.recorder, node, "CIDRNotAvailable")
			return fmt.Errorf("failed to allocate cidr from cluster cidr at idx:%v: %w", i, err)
		}
		allocated.allocatedCIDRs[i] = podCIDR
	}

	klog.V(4).Infof("Putting node %s into the work queue", node.Name)
	ca.nodeUpdateChannel <- allocated
	return nil
}

// updateCIDRsAllocation assigns CIDR to Node and sends an update to the API server.
func (ca *cloudCIDRAllocator) updateCIDRsAllocation(data nodeReservedCIDRs) error {
	var err error
	var node *v1.Node
	defer ca.removeNodeFromProcessing(data.nodeName)
	cidrsString := cidrsAsString(data.allocatedCIDRs)
	node, err = ca.nodeLister.Get(data.nodeName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil // node no longer available, skip processing
		}
		klog.Errorf("Failed while getting node %v for updating Node.Spec.PodCIDR: %s", data.nodeName, err)
		return err
	}

	// if cidr list matches the proposed.
	// then we possibly updated this node
	// and just failed to ack the success.
	if len(node.Spec.PodCIDRs) == len(data.allocatedCIDRs) {
		match := true
		for idx, cidr := range cidrsString {
			if node.Spec.PodCIDRs[idx] != cidr {
				match = false
				break
			}
		}
		if match {
			klog.V(4).Infof("Node %v already has allocated CIDR %v. It matches the proposed one.", node.Name, data.allocatedCIDRs)
			return nil
		}
	}

	// node has cidrs, release the reserved
	if len(node.Spec.PodCIDRs) != 0 {
		klog.Errorf("Node %v already has a CIDR allocated %v. Releasing the new one.", node.Name, node.Spec.PodCIDRs)
		for idx, cidr := range data.allocatedCIDRs {
			if releaseErr := ca.cidrSets[idx].Release(cidr); releaseErr != nil {
				klog.Errorf("Error when releasing CIDR idx:%v value: %v err:%v", idx, cidr, releaseErr)
			}
		}
		return nil
	}

	// If we reached here, it means that the node has no CIDR currently assigned. So we set it.
	for i := 0; i < cidrUpdateRetries; i++ {
		if err = utilnode.PatchNodeCIDRs(ca.client, types.NodeName(node.Name), cidrsString); err == nil {
			return nil
		}
	}
	// failed release back to the pool
	klog.Errorf("Failed to update node %v PodCIDR to %v after multiple attempts: %v", node.Name, cidrsString, err)
	nodeutil.RecordNodeStatusChange(ca.recorder, node, "CIDRAssignmentFailed")
	// We accept the fact that we may leak CIDRs here. This is safer than releasing
	// them in case when we don't know if request went through.
	// NodeController restart will return all falsely allocated CIDRs to the pool.
	if !apierrors.IsServerTimeout(err) {
		klog.Errorf("CIDR assignment for node %v failed: %v. Releasing allocated CIDR", node.Name, err)
		for idx, cidr := range data.allocatedCIDRs {
			if releaseErr := ca.cidrSets[idx].Release(cidr); releaseErr != nil {
				klog.Errorf("Error releasing allocated CIDR for node %v: %v", node.Name, releaseErr)
			}
		}
	}

	err = utilnode.SetNodeCondition(ca.client, types.NodeName(node.Name), v1.NodeCondition{
		Type:               v1.NodeNetworkUnavailable,
		Status:             v1.ConditionFalse,
		Reason:             "RouteCreated",
		Message:            "NodeController create implicit route",
		LastTransitionTime: metav1.Now(),
	})
	if err != nil {
		klog.Errorf("Error setting route status for node %v: %v", node.Name, err)
	}

	return err
}

func (ca *cloudCIDRAllocator) ReleaseCIDR(node *v1.Node) error {
	if node == nil || len(node.Spec.PodCIDRs) == 0 {
		return nil
	}

	for i, cidr := range node.Spec.PodCIDRs {
		_, podCIDR, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("failed to parse CIDR %s on Node %v: %w", cidr, node.Name, err)
		}

		if i >= len(ca.cidrSets) {
			return fmt.Errorf("node:%s has an allocated cidr: %v at index:%v that does not exist in cluster cidrs configuration", node.Name, cidr, i)
		}

		klog.V(4).Infof("release CIDR %s for node:%v", cidr, node.Name)
		if err = ca.cidrSets[i].Release(podCIDR); err != nil {
			return fmt.Errorf("error when releasing CIDR %v: %w", cidr, err)
		}

	}
	delete(ca.nodeNamePodCIDRsMap, node.Name)

	return nil
}
