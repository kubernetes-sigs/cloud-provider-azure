package difftracker

import (
	"context"
	"sync/atomic"
	"time"

	"k8s.io/klog/v2"
	"sigs.k8s.io/cloud-provider-azure/pkg/metrics"
)

// LocationsUpdater syncs location and address changes to NRP Service Gateway
type LocationsUpdater struct {
	diffTracker *DiffTracker
	ctx         context.Context
	cancel      context.CancelFunc
}

// NewLocationsUpdater creates a new LocationsUpdater
func NewLocationsUpdater(ctx context.Context, diffTracker *DiffTracker) *LocationsUpdater {
	if diffTracker == nil {
		panic("LocationsUpdater: diffTracker must not be nil")
	}
	if diffTracker.networkClientFactory == nil {
		panic("LocationsUpdater: diffTracker.networkClientFactory must not be nil")
	}
	childCtx, cancel := context.WithCancel(ctx)
	return &LocationsUpdater{
		diffTracker: diffTracker,
		ctx:         childCtx,
		cancel:      cancel,
	}
}

// Run is the main loop that processes location update requests
func (lu *LocationsUpdater) Run() {
	klog.Infof("LocationsUpdater: Starting")

	for {
		select {
		case <-lu.ctx.Done():
			klog.Infof("LocationsUpdater: Context cancelled, stopping")
			return

		case <-lu.diffTracker.locationsUpdaterTrigger:
			klog.V(4).Infof("LocationsUpdater: Triggered by channel")
			lu.process(lu.ctx)
		}
	}
}

// Stop gracefully shuts down the LocationsUpdater
func (lu *LocationsUpdater) Stop() {
	klog.Infof("LocationsUpdater: stopping")
	lu.cancel()
	klog.Infof("LocationsUpdater: stopped")
}

// process computes location/address diff and syncs to NRP
func (lu *LocationsUpdater) process(ctx context.Context) {
	mc := metrics.NewMetricContext("locations", "LocationsUpdater.process",
		lu.diffTracker.config.ResourceGroup, lu.diffTracker.config.SubscriptionID, "sync")
	isOperationSucceeded := false
	var numLocations, numAddresses int

	defer func() {
		mc.ObserveOperationWithResult(isOperationSucceeded,
			"num_locations", numLocations,
			"num_addresses", numAddresses)

		// Decrement in-flight trigger counter and check initialization completion
		lu.diffTracker.mu.Lock()
		shouldCheck := atomic.LoadInt32(&lu.diffTracker.isInitializing) == 1
		lu.diffTracker.mu.Unlock()

		if shouldCheck {
			atomic.AddInt32(&lu.diffTracker.pendingUpdaterTriggers, -1)
			lu.diffTracker.checkInitializationComplete()
		}
	}()

	startTime := time.Now()
	klog.V(2).Infof("LocationsUpdater: Starting location sync")

	// Get locations and addresses diff from DiffTracker
	locationData := lu.diffTracker.GetSyncLocationsAddresses()

	if len(locationData.Locations) == 0 {
		klog.V(4).Infof("LocationsUpdater: No changes to sync")
		isOperationSucceeded = true
		return
	}

	// Calculate metrics dimensions
	numLocations = len(locationData.Locations)
	for _, loc := range locationData.Locations {
		numAddresses += len(loc.Addresses)
	}

	klog.V(2).Infof("LocationsUpdater: Syncing %d locations with %d total addresses", numLocations, numAddresses)

	// Convert to DTO format for NRP API
	locationsDTO := MapLocationDataToDTO(locationData)

	// Call NRP Service Gateway API to update locations/addresses
	err := lu.diffTracker.updateNRPSGWAddressLocations(ctx, lu.diffTracker.config.ServiceGatewayResourceName, locationsDTO)
	if err != nil {
		klog.Errorf("LocationsUpdater: Failed to update locations in NRP: %v", err)
		recordLocationsUpdate(startTime, numLocations, numAddresses, err)
		// Return without updating state - will retry on next trigger when new changes occur
		return
	}

	duration := time.Since(startTime)
	klog.V(2).Infof("LocationsUpdater: Successfully synced locations to NRP in %v", duration)

	// Record metrics
	recordLocationsUpdate(startTime, numLocations, numAddresses, nil)

	// Update NRPResources to reflect the sync
	lu.diffTracker.UpdateLocationsAddresses(locationData)

	// Check pending deletions after location sync
	// Services waiting for their locations to clear can now be deleted
	lu.diffTracker.CheckPendingDeletions()

	isOperationSucceeded = true
}
