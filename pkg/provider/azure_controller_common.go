/*
Copyright 2020 The Kubernetes Authors.

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

package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-03-01/compute"
	"github.com/Azure/go-autorest/autorest/azure"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"k8s.io/apimachinery/pkg/types"
	kwait "k8s.io/apimachinery/pkg/util/wait"
	cloudprovider "k8s.io/cloud-provider"
	volerr "k8s.io/cloud-provider/volume/errors"
	"k8s.io/klog/v2"

	"sigs.k8s.io/cloud-provider-azure/pkg/batch"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/metrics"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

type CloudContextKey string

const (
	LunChannelContextKey CloudContextKey = "cloud-provier-azure/lun-chan"
	// Disk Caching is not supported for disks 4 TiB and larger
	// https://docs.microsoft.com/en-us/azure/virtual-machines/premium-storage-performance#disk-caching
	diskCachingLimit = 4096 // GiB

	maxLUN                 = 64 // max number of LUNs per VM
	errStatusCode400       = "statuscode=400"
	errInvalidParameter    = `code="invalidparameter"`
	errTargetInstanceIds   = `target="instanceids"`
	sourceSnapshot         = "snapshot"
	sourceVolume           = "volume"
	attachDiskMapKeySuffix = "attachdiskmap"
	detachDiskMapKeySuffix = "detachdiskmap"

	updateVMRetryDuration = time.Duration(1) * time.Second
	updateVMRetryFactor   = 3.0
	updateVMRetrySteps    = 5

	// WriteAcceleratorEnabled support for Azure Write Accelerator on Azure Disks
	// https://docs.microsoft.com/azure/virtual-machines/windows/how-to-enable-write-accelerator
	WriteAcceleratorEnabled = "writeacceleratorenabled"

	// see https://docs.microsoft.com/en-us/rest/api/compute/disks/createorupdate#create-a-managed-disk-by-copying-a-snapshot.
	diskSnapshotPath = "/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/snapshots/%s"

	// see https://docs.microsoft.com/en-us/rest/api/compute/disks/createorupdate#create-a-managed-disk-from-an-existing-managed-disk-in-the-same-or-different-subscription.
	managedDiskPath = "/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/disks/%s"
)

var defaultBackOff = kwait.Backoff{
	Steps:    20,
	Duration: 2 * time.Second,
	Factor:   1.5,
	Jitter:   0.0,
}

var updateVMBackoff = kwait.Backoff{
	Duration: updateVMRetryDuration,
	Factor:   updateVMRetryFactor,
	Steps:    updateVMRetrySteps,
}

var (
	managedDiskPathRE  = regexp.MustCompile(`.*/subscriptions/(?:.*)/resourceGroups/(?:.*)/providers/Microsoft.Compute/disks/(.+)`)
	diskSnapshotPathRE = regexp.MustCompile(`.*/subscriptions/(?:.*)/resourceGroups/(?:.*)/providers/Microsoft.Compute/snapshots/(.+)`)
	errorCodeRE        = regexp.MustCompile(`Code="(.*?)".*`)
)

type controllerCommon struct {
	diskStateMap        sync.Map // <diskURI, attaching/detaching state>
	lockMap             *lockMap
	cloud               *Cloud
	attachDiskProcessor *batch.Processor
	detachDiskProcessor *batch.Processor

	// DisableUpdateCache whether disable update cache in disk attach/detach
	DisableUpdateCache bool
}

// AttachDiskOptions attach disk options
type AttachDiskOptions struct {
	cachingMode             compute.CachingTypes
	diskName                string
	diskEncryptionSetID     string
	writeAcceleratorEnabled bool
	lun                     int32
	lunCh                   chan (int32) // channel for early return of lun value
}

func (a *AttachDiskOptions) String() string {
	return fmt.Sprintf("AttachDiskOptions{diskName: %q, lun: %d}", a.diskName, a.lun)
}

// ExtendedLocation contains additional info about the location of resources.
type ExtendedLocation struct {
	// Name - The name of the extended location.
	Name string `json:"name,omitempty"`
	// Type - The type of the extended location.
	Type string `json:"type,omitempty"`
}

// getNodeVMSet gets the VMSet interface based on config.VMType and the real virtual machine type.
func (c *controllerCommon) getNodeVMSet(nodeName types.NodeName, crt azcache.AzureCacheReadType) (VMSet, error) {
	// 1. vmType is standard or vmssflex, return cloud.VMSet directly.
	// 1.1 all the nodes in the cluster are avset nodes.
	// 1.2 all the nodes in the cluster are vmssflex nodes.
	if c.cloud.VMType == consts.VMTypeStandard || c.cloud.VMType == consts.VMTypeVmssFlex {
		return c.cloud.VMSet, nil
	}

	// 2. vmType is Virtual Machine Scale Set (vmss), convert vmSet to ScaleSet.
	// 2.1 all the nodes in the cluster are vmss uniform nodes.
	// 2.2 mix node: the nodes in the cluster can be any of avset nodes, vmss uniform nodes and vmssflex nodes.
	ss, ok := c.cloud.VMSet.(*ScaleSet)
	if !ok {
		return nil, fmt.Errorf("error of converting vmSet (%q) to ScaleSet with vmType %q", c.cloud.VMSet, c.cloud.VMType)
	}

	vmManagementType, err := ss.getVMManagementTypeByNodeName(mapNodeNameToVMName(nodeName), crt)
	if err != nil {
		return nil, fmt.Errorf("getNodeVMSet: failed to check the node %s management type: %w", mapNodeNameToVMName(nodeName), err)
	}
	// 3. If the node is managed by availability set, then return ss.availabilitySet.
	if vmManagementType == ManagedByAvSet {
		// vm is managed by availability set.
		return ss.availabilitySet, nil
	}
	if vmManagementType == ManagedByVmssFlex {
		// 4. If the node is managed by vmss flex, then return ss.flexScaleSet.
		// vm is managed by vmss flex.
		return ss.flexScaleSet, nil
	}

	// 5. Node is managed by vmss
	return ss, nil

}

// AttachDisk attaches a disk to vm
// parameter async indicates whether allow multiple batch disk attach on one node in parallel
// return (lun, error)
func (c *controllerCommon) AttachDisk(ctx context.Context, async bool, diskName, diskURI string, nodeName types.NodeName,
	cachingMode compute.CachingTypes, disk *compute.Disk) (int32, error) {
	// lun channel is used to return assigned lun values preemptively

	diskEncryptionSetID := ""
	writeAcceleratorEnabled := false
	var waitForBatch bool
	var lunCh chan int32
	defer func() {
		if !waitForBatch && lunCh != nil {
			close(lunCh)
		}
	}()

	// there is possibility that disk is nil when GetDisk is throttled
	// don't check disk state when GetDisk is throttled
	if disk != nil {
		if disk.ManagedBy != nil && (disk.MaxShares == nil || *disk.MaxShares <= 1) {
			vmset, err := c.getNodeVMSet(nodeName, azcache.CacheReadTypeUnsafe)
			if err != nil {
				return -1, err
			}
			attachedNode, err := vmset.GetNodeNameByProviderID(*disk.ManagedBy)
			if err != nil {
				return -1, err
			}
			if strings.EqualFold(string(nodeName), string(attachedNode)) {
				klog.Warningf("volume %s is actually attached to current node %s, invalidate vm cache and return error", diskURI, nodeName)
				// update VM(invalidate vm cache)
				if errUpdate := c.UpdateVM(ctx, nodeName); errUpdate != nil {
					return -1, errUpdate
				}
				lun, _, err := c.GetDiskLun(diskName, diskURI, nodeName)
				return lun, err
			}

			attachErr := fmt.Sprintf(
				"disk(%s) already attached to node(%s), could not be attached to node(%s)",
				diskURI, *disk.ManagedBy, nodeName)
			klog.V(2).Infof("found dangling volume %s attached to node %s, could not be attached to node(%s)", diskURI, attachedNode, nodeName)
			return -1, volerr.NewDanglingError(attachErr, attachedNode, "")
		}

		if disk.DiskProperties != nil {
			if disk.DiskProperties.DiskSizeGB != nil && *disk.DiskProperties.DiskSizeGB >= diskCachingLimit && cachingMode != compute.CachingTypesNone {
				// Disk Caching is not supported for disks 4 TiB and larger
				// https://docs.microsoft.com/en-us/azure/virtual-machines/premium-storage-performance#disk-caching
				cachingMode = compute.CachingTypesNone
				klog.Warningf("size of disk(%s) is %dGB which is bigger than limit(%dGB), set cacheMode as None",
					diskURI, *disk.DiskProperties.DiskSizeGB, diskCachingLimit)
			}

			if disk.DiskProperties.Encryption != nil &&
				disk.DiskProperties.Encryption.DiskEncryptionSetID != nil {
				diskEncryptionSetID = *disk.DiskProperties.Encryption.DiskEncryptionSetID
			}

			if disk.DiskProperties.DiskState != compute.Unattached && (disk.MaxShares == nil || *disk.MaxShares <= 1) {
				return -1, fmt.Errorf("state of disk(%s) is %s, not in expected %s state", diskURI, disk.DiskProperties.DiskState, compute.Unattached)
			}
		}

		if v, ok := disk.Tags[WriteAcceleratorEnabled]; ok {
			if v != nil && strings.EqualFold(*v, "true") {
				writeAcceleratorEnabled = true
			}
		}
	}

	if val := ctx.Value(LunChannelContextKey); val != nil {
		lunCh = val.(chan int32)
	}

	options := &AttachDiskOptions{
		lun:                     -1,
		lunCh:                   lunCh,
		diskName:                diskName,
		cachingMode:             cachingMode,
		diskEncryptionSetID:     diskEncryptionSetID,
		writeAcceleratorEnabled: writeAcceleratorEnabled,
	}

	diskToAttach := &AttachDiskParams{
		diskURI: diskURI,
		options: options,
	}

	resourceGroup, err := c.cloud.GetNodeResourceGroup(string(nodeName))
	if err != nil {
		resourceGroup = c.cloud.ResourceGroup
	}

	batchKey := metrics.KeyFromAttributes(c.cloud.SubscriptionID, strings.ToLower(resourceGroup), strings.ToLower(string(nodeName)))
	waitForBatch = true
	r, err := c.attachDiskProcessor.Do(ctx, batchKey, diskToAttach)
	if err == nil {
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case result := <-r.(chan (attachDiskResult)):
			if err = result.err; err == nil {
				return result.lun, nil
			}
		}
	}

	klog.Errorf("azureDisk - attach disk(%s, %s) failed, err: %v", diskName, diskURI, err)
	return -1, err
}

type AttachDiskParams struct {
	diskURI string
	options *AttachDiskOptions
}

func (a *AttachDiskParams) CleanUp() {
	if a.options != nil && a.options.lunCh != nil {
		close(a.options.lunCh)
	}
}

type attachDiskResult struct {
	lun int32
	err error
}

func (c *controllerCommon) attachDiskBatchToNode(ctx context.Context, subscriptionID, resourceGroup string, nodeName types.NodeName, disksToAttach []*AttachDiskParams) ([]chan (attachDiskResult), error) {
	diskMap := make(map[string]*AttachDiskOptions, len(disksToAttach))
	lunChans := make([]chan (attachDiskResult), len(disksToAttach))

	for i, disk := range disksToAttach {
		lunChans[i] = make(chan (attachDiskResult), 1)

		diskMap[disk.diskURI] = disk.options

		diskURI := strings.ToLower(disk.diskURI)
		c.diskStateMap.Store(diskURI, "attaching")
		defer c.diskStateMap.Delete(diskURI)
	}

	err := c.cloud.SetDiskLun(nodeName, diskMap)
	if err != nil {
		return nil, err
	}

	vmset, err := c.getNodeVMSet(nodeName, azcache.CacheReadTypeUnsafe)
	if err != nil {
		return nil, err
	}

	c.lockMap.LockEntry(string(nodeName))
	defer c.lockMap.UnlockEntry(string(nodeName))

	defer func() {
		// invalidate the cache if there is error in disk attach
		if err != nil {
			_ = vmset.DeleteCacheForNode(string(nodeName))
		}
	}()

	future, err := vmset.AttachDisk(ctx, nodeName, diskMap)
	if future == nil {
		err = status.Errorf(codes.Internal, "nil future was returned: %v", err)
		return nil, err
	}
	// err will be handled by waitForUpdateResult below

	attachFn := func() {
		klog.V(2).Infof("azuredisk - trying to attach disks to node %s, diskMap len:%d, %s", nodeName, len(diskMap), diskMap)

		resultCtx := ctx
		err = c.waitForUpdateResult(resultCtx, vmset, nodeName, future, err)

		for i, disk := range disksToAttach {
			lunChans[i] <- attachDiskResult{lun: diskMap[disk.diskURI].lun, err: err}
		}
	}

	attachFn()
	return lunChans, nil
}

// waitForUpdateResult handles asynchronous VM update operations and retries with backoff if OperationPreempted error is observed
func (c *controllerCommon) waitForUpdateResult(ctx context.Context, vmset VMSet, nodeName types.NodeName, future *azure.Future, updateErr error) (err error) {
	err = updateErr
	if err == nil {
		err = vmset.WaitForUpdateResult(ctx, future, nodeName, "attach_disk")
	}

	if vmUpdateRequired(future, err) {
		if derr := kwait.ExponentialBackoffWithContext(ctx, updateVMBackoff, func() (bool, error) {
			klog.Errorf("Retry VM Update on node (%s) due to error (%v)", nodeName, err)
			future, err = vmset.UpdateVMAsync(ctx, nodeName)
			if err == nil {
				err = vmset.WaitForUpdateResult(ctx, future, nodeName, "attach_disk")
			}
			return !vmUpdateRequired(future, err), nil
		}); derr != nil {
			err = derr
			return
		}
	}

	if err != nil && VMConfigAccepted(future) {
		err = retry.NewPartialUpdateError(err.Error())
	}
	return
}

// DetachDisk detaches a disk from VM
func (c *controllerCommon) DetachDisk(ctx context.Context, diskName, diskURI string, nodeName types.NodeName) error {
	if _, err := c.cloud.InstanceID(ctx, nodeName); err != nil {
		if errors.Is(err, cloudprovider.InstanceNotFound) {
			// if host doesn't exist, no need to detach
			klog.Warningf("azureDisk - failed to get azure instance id(%s), DetachDisk(%s) will assume disk is already detached",
				nodeName, diskURI)
			return nil
		}
		klog.Warningf("failed to get azure instance id (%v)", err)
		return fmt.Errorf("failed to get azure instance id for node %q: %w", nodeName, err)
	}

	resourceGroup, err := c.cloud.GetNodeResourceGroup(string(nodeName))
	if err != nil {
		resourceGroup = c.cloud.ResourceGroup
	}

	diskToDetach := &detachDiskParams{
		diskName: diskName,
		diskURI:  diskURI,
	}

	batchKey := metrics.KeyFromAttributes(c.cloud.SubscriptionID, strings.ToLower(resourceGroup), strings.ToLower(string(nodeName)))
	if _, err := c.detachDiskProcessor.Do(ctx, batchKey, diskToDetach); err != nil {
		klog.Errorf("azureDisk - detach disk(%s, %s) failed, err: %v", diskName, diskURI, err)
		return err
	}

	klog.V(2).Infof("azureDisk - detach disk(%s, %s) succeeded", diskName, diskURI)
	return nil
}

type detachDiskParams struct {
	diskName string
	diskURI  string
}

func (c *controllerCommon) detachDiskBatchFromNode(ctx context.Context, subscriptionID, resourceGroup string, nodeName types.NodeName, disksToDetach []*detachDiskParams) error {
	diskMap := make(map[string]string, len(disksToDetach))
	for _, disk := range disksToDetach {
		diskMap[disk.diskURI] = disk.diskName

		diskURI := strings.ToLower(disk.diskURI)
		c.diskStateMap.Store(diskURI, "detaching")
		defer c.diskStateMap.Delete(diskURI)
	}

	vmset, err := c.getNodeVMSet(nodeName, azcache.CacheReadTypeUnsafe)
	if err != nil {
		return err
	}

	c.lockMap.LockEntry(string(nodeName))
	defer c.lockMap.UnlockEntry(string(nodeName))

	klog.V(2).Infof("azuredisk - trying to detach disks from node %s, diskMap len:%d, %s", nodeName, len(diskMap), diskMap)

	err = vmset.DetachDisk(ctx, nodeName, diskMap)
	if err != nil {
		if isInstanceNotFoundError(err) {
			// if host doesn't exist, no need to detach
			klog.Warningf("azureDisk - got InstanceNotFoundError(%v), assuming disks are already detached: %v", err, diskMap)
			return nil
		}
		return err
	}

	klog.V(2).Infof("azuredisk - successfully detached disks from node %s, diskMap len:%d, %s", nodeName, len(diskMap), diskMap)

	return nil
}

// UpdateVM updates a vm
func (c *controllerCommon) UpdateVM(ctx context.Context, nodeName types.NodeName) error {
	vmset, err := c.getNodeVMSet(nodeName, azcache.CacheReadTypeUnsafe)
	if err != nil {
		return err
	}
	node := strings.ToLower(string(nodeName))
	c.lockMap.LockEntry(node)
	defer c.lockMap.UnlockEntry(node)

	defer func() {
		_ = vmset.DeleteCacheForNode(string(nodeName))
	}()

	klog.V(2).Infof("azureDisk - update: vm(%s)", nodeName)
	return vmset.UpdateVM(ctx, nodeName)
}

// GetNodeDataDisks invokes vmSet interfaces to get data disks for the node.
func (c *controllerCommon) GetNodeDataDisks(nodeName types.NodeName, crt azcache.AzureCacheReadType) ([]compute.DataDisk, *string, error) {
	vmset, err := c.getNodeVMSet(nodeName, crt)
	if err != nil {
		return nil, nil, err
	}

	return vmset.GetDataDisks(nodeName, crt)
}

// GetDiskLun finds the lun on the host that the vhd is attached to, given a vhd's diskName and diskURI.
func (c *controllerCommon) GetDiskLun(diskName, diskURI string, nodeName types.NodeName) (int32, *string, error) {
	// GetNodeDataDisks need to fetch the cached data/fresh data if cache expired here
	// to ensure we get LUN based on latest entry.
	disks, provisioningState, err := c.GetNodeDataDisks(nodeName, azcache.CacheReadTypeDefault)
	if err != nil {
		klog.Errorf("error of getting data disks for node %s: %v", nodeName, err)
		return -1, provisioningState, err
	}

	for _, disk := range disks {
		if disk.Lun != nil && (disk.Name != nil && diskName != "" && strings.EqualFold(*disk.Name, diskName)) ||
			(disk.Vhd != nil && disk.Vhd.URI != nil && diskURI != "" && strings.EqualFold(*disk.Vhd.URI, diskURI)) ||
			(disk.ManagedDisk != nil && strings.EqualFold(*disk.ManagedDisk.ID, diskURI)) {
			if disk.ToBeDetached != nil && *disk.ToBeDetached {
				klog.Warningf("azureDisk - find disk(ToBeDetached): lun %d name %s uri %s", *disk.Lun, diskName, diskURI)
			} else {
				// found the disk
				klog.V(2).Infof("azureDisk - find disk: lun %d name %s uri %s", *disk.Lun, diskName, diskURI)
				return *disk.Lun, provisioningState, nil
			}
		}
	}
	return -1, provisioningState, fmt.Errorf("%s for disk %s", consts.CannotFindDiskLUN, diskName)
}

// SetDiskLun find unused luns and allocate lun for every disk in disksPendingAttach map.
// Return err if not enough luns are found.
func (c *controllerCommon) SetDiskLun(nodeName types.NodeName, disksPendingAttach map[string]*AttachDiskOptions) error {
	disks, _, err := c.GetNodeDataDisks(nodeName, azcache.CacheReadTypeDefault)
	if err != nil {
		klog.Errorf("error of getting data disks for node %s: %v", nodeName, err)
		return err
	}

	allLuns := make([]bool, maxLUN)
	uriToLun := make(map[string]int32, len(disks))
	for _, disk := range disks {
		if disk.Lun != nil && *disk.Lun >= 0 && *disk.Lun < maxLUN {
			allLuns[*disk.Lun] = true
			if disk.ManagedDisk != nil {
				uriToLun[*disk.ManagedDisk.ID] = *disk.Lun
			}
		}
	}
	if len(disksPendingAttach) == 0 {
		// attach disk request is empty, return directly
		return nil
	}

	// allocate lun for every disk in disksPendingAttach
	var availableDiskLuns []int32
	freeLunsCount := 0
	for lun, inUse := range allLuns {
		if !inUse {
			availableDiskLuns = append(availableDiskLuns, int32(lun))
			freeLunsCount++
			// found enough luns for to assign to all pending disks
			if freeLunsCount >= len(disksPendingAttach) {
				break
			}
		}
	}

	if len(availableDiskLuns) < len(disksPendingAttach) {
		return fmt.Errorf("could not find enough disk luns(current: %d) for disksPendingAttach(%v, len=%d)",
			len(availableDiskLuns), disksPendingAttach, len(disksPendingAttach))
	}

	count := 0
	for uri, opt := range disksPendingAttach {
		if opt == nil {
			return fmt.Errorf("unexpected nil pointer in disksPendingAttach(%v)", disksPendingAttach)
		}
		// disk already exists and has assigned lun
		lun, exists := uriToLun[uri]
		if exists {
			opt.lun = lun
		}
		opt.lun = availableDiskLuns[count]
		if opt.lunCh != nil {
			// if lun channel is provided, feed the channel the determined lun value
			opt.lunCh <- opt.lun
		}
		count++
	}

	if count <= 0 {
		return fmt.Errorf("could not find lun of, disksPendingAttach(%v)", disksPendingAttach)
	}
	return nil
}

// DisksAreAttached checks if a list of volumes are attached to the node with the specified NodeName.
func (c *controllerCommon) DisksAreAttached(diskNames []string, nodeName types.NodeName) (map[string]bool, error) {
	attached := make(map[string]bool)
	for _, diskName := range diskNames {
		attached[diskName] = false
	}

	// doing stalled read for GetNodeDataDisks to ensure we don't call ARM
	// for every reconcile call. The cache is invalidated after Attach/Detach
	// disk. So the new entry will be fetched and cached the first time reconcile
	// loop runs after the Attach/Disk OP which will reflect the latest model.
	disks, _, err := c.GetNodeDataDisks(nodeName, azcache.CacheReadTypeUnsafe)
	if err != nil {
		if errors.Is(err, cloudprovider.InstanceNotFound) {
			// if host doesn't exist, no need to detach.
			klog.Warningf("azureDisk - Cannot find node %s, DisksAreAttached will assume disks %v are not attached to it.",
				nodeName, diskNames)
			return attached, nil
		}

		return attached, err
	}

	for _, disk := range disks {
		for _, diskName := range diskNames {
			if disk.Name != nil && diskName != "" && strings.EqualFold(*disk.Name, diskName) {
				attached[diskName] = true
			}
		}
	}

	return attached, nil
}

func filterDetachingDisks(unfilteredDisks []compute.DataDisk) []compute.DataDisk {
	filteredDisks := []compute.DataDisk{}
	for _, disk := range unfilteredDisks {
		if disk.ToBeDetached != nil && *disk.ToBeDetached {
			if disk.Name != nil {
				klog.V(2).Infof("Filtering disk: %s with ToBeDetached flag set.", *disk.Name)
			}
		} else {
			filteredDisks = append(filteredDisks, disk)
		}
	}
	return filteredDisks
}

func (c *controllerCommon) filterNonExistingDisks(ctx context.Context, unfilteredDisks []compute.DataDisk) []compute.DataDisk {
	filteredDisks := []compute.DataDisk{}
	for _, disk := range unfilteredDisks {
		filter := false
		if disk.ManagedDisk != nil && disk.ManagedDisk.ID != nil {
			diskURI := *disk.ManagedDisk.ID
			exist, err := c.cloud.checkDiskExists(ctx, diskURI)
			if err != nil {
				klog.Errorf("checkDiskExists(%s) failed with error: %v", diskURI, err)
			} else {
				// only filter disk when checkDiskExists returns <false, nil>
				filter = !exist
				if filter {
					klog.Errorf("disk(%s) does not exist, removed from data disk list", diskURI)
				}
			}
		}

		if !filter {
			filteredDisks = append(filteredDisks, disk)
		}
	}
	return filteredDisks
}

func (c *controllerCommon) checkDiskExists(ctx context.Context, diskURI string) (bool, error) {
	diskName := path.Base(diskURI)
	resourceGroup, subsID, err := getInfoFromDiskURI(diskURI)
	if err != nil {
		return false, err
	}

	if _, rerr := c.cloud.DisksClient.Get(ctx, subsID, resourceGroup, diskName); rerr != nil {
		if rerr.HTTPStatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, rerr.Error()
	}

	return true, nil
}

func vmUpdateRequired(future *azure.Future, err error) bool {
	errCode := getAzureErrorCode(err)
	return VMConfigAccepted(future) && errCode == consts.OperationPreemptedErrorCode
}

func getValidCreationData(subscriptionID, resourceGroup, sourceResourceID, sourceType string) (compute.CreationData, error) {
	if sourceResourceID == "" {
		return compute.CreationData{
			CreateOption: compute.Empty,
		}, nil
	}

	switch sourceType {
	case sourceSnapshot:
		if match := diskSnapshotPathRE.FindString(sourceResourceID); match == "" {
			sourceResourceID = fmt.Sprintf(diskSnapshotPath, subscriptionID, resourceGroup, sourceResourceID)
		}

	case sourceVolume:
		if match := managedDiskPathRE.FindString(sourceResourceID); match == "" {
			sourceResourceID = fmt.Sprintf(managedDiskPath, subscriptionID, resourceGroup, sourceResourceID)
		}
	default:
		return compute.CreationData{
			CreateOption: compute.Empty,
		}, nil
	}

	splits := strings.Split(sourceResourceID, "/")
	if len(splits) > 9 {
		if sourceType == sourceSnapshot {
			return compute.CreationData{}, fmt.Errorf("sourceResourceID(%s) is invalid, correct format: %s", sourceResourceID, diskSnapshotPathRE)
		}
		return compute.CreationData{}, fmt.Errorf("sourceResourceID(%s) is invalid, correct format: %s", sourceResourceID, managedDiskPathRE)
	}
	return compute.CreationData{
		CreateOption:     compute.Copy,
		SourceResourceID: &sourceResourceID,
	}, nil
}

func isInstanceNotFoundError(err error) bool {
	errMsg := strings.ToLower(err.Error())
	if strings.Contains(errMsg, strings.ToLower(consts.VmssVMNotActiveErrorMessage)) {
		return true
	}
	return strings.Contains(errMsg, errStatusCode400) && strings.Contains(errMsg, errInvalidParameter) && strings.Contains(errMsg, errTargetInstanceIds)
}

// getAzureErrorCode uses regex to parse out the error code encapsulated in the error string.
func getAzureErrorCode(err error) string {
	if err == nil {
		return ""
	}
	matches := errorCodeRE.FindStringSubmatch(err.Error())
	if matches == nil {
		return ""
	}
	return matches[1]
}

// configAccepted returns true if storage profile change had been committed (i.e. HTTP status code == 2xx) and returns false otherwise.
func VMConfigAccepted(future *azure.Future) bool {
	// if status code indicates success, the storage profile change was committed
	return future != nil && future.Response() != nil && future.Response().StatusCode/100 == 2
}
