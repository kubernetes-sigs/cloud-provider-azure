package difftracker

import (
	"context"
	"math/rand"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"

	"sigs.k8s.io/cloud-provider-azure/pkg/metrics"
)

// Bounded backoff bounds for retrying a failed NRP location sync.
const (
	locationsRetryBaseDelay = 1 * time.Second
	locationsRetryMaxDelay  = 30 * time.Second
)

// defaultNRPOperationTimeout bounds a single NRP/Azure operation attempt (a location sync in the
// LocationsUpdater, or a service create/update/delete in the ServiceUpdater). Normal operations
// complete in seconds; the timeout exists only so a hung or pathologically slow ARM call fails into
// the existing bounded-retry/park logic instead of pinning the single LocationsUpdater worker or
// permanently holding a ServiceUpdater semaphore slot. It is generous to avoid aborting a legitimate
// long-running LRO.
const defaultNRPOperationTimeout = 5 * time.Minute

// nrpOperationTimeout holds the operation timeout in nanoseconds. Stored atomically so it can be
// overridden without racing the updater goroutines that read it via getNRPOperationTimeout.
var nrpOperationTimeout atomic.Int64

func init() { nrpOperationTimeout.Store(int64(defaultNRPOperationTimeout)) }

func getNRPOperationTimeout() time.Duration { return time.Duration(nrpOperationTimeout.Load()) }

// LocationsUpdater syncs location and address changes to NRP Service Gateway
type LocationsUpdater struct {
	diffTracker *DiffTracker
	ctx         context.Context
	cancel      context.CancelFunc

	// failureCount is the number of consecutive failed NRP syncs, used to compute the
	// retry backoff. Accessed only from the single Run goroutine (process), so no lock.
	failureCount int

	logger logr.Logger
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
		logger:      diffTracker.logger.WithName("LocationsUpdater"),
	}
}

// Run is the main loop that processes location update requests
func (lu *LocationsUpdater) Run() {
	lu.logger.V(2).Info("Started LocationsUpdater")

	for {
		select {
		case <-lu.ctx.Done():
			lu.logger.V(2).Info("Context cancelled, stopping LocationsUpdater")
			return

		case <-lu.diffTracker.locationsUpdaterTrigger:
			lu.logger.V(4).Info("Triggered LocationsUpdater")
			// Bound each attempt so a hung/slow NRP call cannot pin the single worker and starve all
			// other services' location/finalizer syncs; a timeout fails into the deferred backoffAndRetry.
			attemptCtx, cancel := context.WithTimeout(lu.ctx, getNRPOperationTimeout())
			lu.process(attemptCtx)
			cancel()
		}
	}
}

// Stop gracefully shuts down the LocationsUpdater
func (lu *LocationsUpdater) Stop() {
	lu.logger.V(2).Info("Stopping LocationsUpdater")
	lu.cancel()
	lu.logger.V(2).Info("Stopped LocationsUpdater")
}

// process computes location/address diff and syncs to NRP
func (lu *LocationsUpdater) process(ctx context.Context) {
	mc := metrics.NewMetricContext("locations", "LocationsUpdater.process",
		lu.diffTracker.config.ResourceGroup, lu.diffTracker.config.SubscriptionID, "sync")
	isOperationSucceeded := false
	// terminalSyncErr is set when NRP rejects the batch with a deterministic error that retrying the
	// identical payload cannot fix; it suppresses the self-rescheduling backoff below.
	terminalSyncErr := false
	var numLocations, numAddresses int

	defer func() {
		mc.ObserveOperationWithResult(isOperationSucceeded,
			"num_locations", numLocations,
			"num_addresses", numAddresses)

		// On failure, schedule a bounded-backoff retry so a transient NRP/ARM error does
		// not leave the computed diff unsynced until some unrelated future trigger. This
		// runs BEFORE the in-flight trigger counter is decremented below, so initialization
		// stays blocked (WaitForInitialSync) until a sync actually succeeds. On success,
		// reset the backoff. The retry wait is cancellable via the updater context.
		//
		// A terminal (deterministic) failure is the exception (see the terminalSyncErr branch in
		// process): do not self-reschedule. Init accounting still proceeds so it cannot block init.
		switch {
		case isOperationSucceeded:
			lu.failureCount = 0
		case terminalSyncErr:
			lu.failureCount = 0
		default:
			lu.backoffAndRetry()
		}

		// Decrement in-flight trigger counter and check initialization completion
		lu.diffTracker.mu.Lock()
		shouldCheck := atomic.LoadInt32(&lu.diffTracker.isInitializing) == 1
		lu.diffTracker.mu.Unlock()

		if shouldCheck {
			atomic.AddInt32(&lu.diffTracker.pendingUpdaterTriggers, -1)
			lu.diffTracker.checkInitializationComplete()
		}
	}()

	// The locations_total / addresses_total gauges are documented and alerted on as live totals,
	// so refresh them from the current NRP-tracked counts on every cycle — including the no-diff
	// early return and error returns. numLocations/numAddresses (the per-sync diff sizes) are kept
	// only for the log line and the operation-metric dimensions.
	defer func() {
		locations, addresses := lu.diffTracker.countTrackedLocationsAndAddresses()
		updateLocationsAndAddressesMetric(locations, addresses)
	}()

	startTime := time.Now()

	// Get locations and addresses diff from DiffTracker
	locationData := lu.diffTracker.GetSyncLocationsAddresses()

	if len(locationData.Locations) == 0 {
		lu.logger.V(4).Info("No location changes to sync")
		// Even with no location diff, recovered pending service/pod deletions must
		// still be processed so their finalizers are not left pending.
		lu.diffTracker.CheckPendingServiceDeletions()
		readyRemovalPending := lu.diffTracker.CheckPendingPodDeletions(ctx)
		if lu.initPodFinalizersStillPending() || readyRemovalPending {
			// Retry instead of reporting success: during init, init completion requires
			// pendingPodDeletions==0; post-init, a ready non-last finalizer removal that failed
			// transiently must be retried via backoffAndRetry rather than waiting for the next
			// unrelated trigger (which on a quiet cluster could strand the pod Terminating).
			lu.logger.V(4).Info("Pod finalizer removal incomplete, retrying")
			return
		}
		isOperationSucceeded = true
		return
	}

	// Calculate metrics dimensions
	numLocations = len(locationData.Locations)
	for _, loc := range locationData.Locations {
		numAddresses += len(loc.Addresses)
	}

	// Convert to DTO format for NRP API
	locationsDTO := MapLocationDataToDTO(locationData)

	// Call NRP Service Gateway API to update locations/addresses
	err := lu.diffTracker.updateNRPSGWAddressLocations(ctx, lu.diffTracker.config.ServiceGatewayResourceName, locationsDTO)
	if err != nil {
		if httpStatus, errCode := extractAzureErrorInfo(err); isTerminalLocationSyncStatus(httpStatus) {
			// Deterministic NRP rejection (e.g. 400): the identical batch can never be accepted, so
			// retrying would spin the single worker forever and starve every other service's
			// location/finalizer sync. Abandon this batch; the next real change recomputes the diff
			// and re-attempts. Surfaced via metric + error log so operators see NRP state diverging.
			terminalSyncErr = true
			recordLocationSyncTerminalError()
			lu.logger.Error(err, "Terminal error syncing locations to NRP; not retrying until the next change",
				"httpStatus", httpStatus, "errorCode", errCode)
			return
		}
		lu.logger.V(4).Info("Could not sync locations to NRP", "err", err, "attempt", lu.failureCount+1)
		// Leave isOperationSucceeded=false so the deferred backoffAndRetry re-triggers a
		// sync; the diff is recomputed fresh on the next pass, so no state is lost.
		return
	}

	duration := time.Since(startTime)
	lu.logger.V(2).Info("Synced locations to NRP", "locations", numLocations, "addresses", numAddresses, "duration", duration)

	// Update NRPResources to reflect the sync
	lu.diffTracker.UpdateLocationsAddresses(locationData)

	// Check pending deletions after location sync
	// Services waiting for their locations to clear can now be deleted
	lu.diffTracker.CheckPendingServiceDeletions()

	// Check pending pod deletions after location sync. The address removals were just synced
	// to NRP and reflected in NRPResources above, so any non-last pod whose address has now
	// left NRP gets its finalizer removed here. Last-pod entries are skipped (their finalizers
	// are removed after NAT Gateway deletion by RemoveLastPodFinalizers).
	readyRemovalPending := lu.diffTracker.CheckPendingPodDeletions(ctx)
	if lu.initPodFinalizersStillPending() || readyRemovalPending {
		// Retry instead of reporting success: during init this keeps WaitForInitialSync blocked
		// until finalizers clear; post-init it reschedules a backoff retry for a ready non-last
		// removal that failed transiently, so a quiet cluster does not strand the pod Terminating.
		lu.logger.V(4).Info("Pod finalizer removal incomplete after sync, retrying")
		return
	}

	isOperationSucceeded = true
}

// initPodFinalizersStillPending reports whether init is in progress and recovered pod
// deletions remain. Init completion requires pendingPodDeletions==0, and
// CheckPendingPodDeletions swallows transient errors, so process() uses this to keep
// retrying during init. Returns false post-init (steady-state behavior unchanged).
func (lu *LocationsUpdater) initPodFinalizersStillPending() bool {
	dt := lu.diffTracker
	if atomic.LoadInt32(&dt.isInitializing) != 1 {
		return false
	}
	dt.mu.Lock()
	defer dt.mu.Unlock()
	return len(dt.pendingPodDeletions) > 0
}

// computeRetryBackoff returns a bounded, jittered backoff delay for the given 1-based attempt
// number, using the shared retry schedule (base delay doubling, capped, +~20% jitter). It is used
// by both the LocationsUpdater sync retry and the ServiceUpdater operation retry so the two stay
// in lockstep.
func computeRetryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := locationsRetryBaseDelay << min(attempt-1, 5)
	if delay <= 0 || delay > locationsRetryMaxDelay {
		delay = locationsRetryMaxDelay
	}
	// Add up to ~20% jitter to avoid synchronized retries across controllers.
	delay += time.Duration(rand.Int63n(int64(delay)/5 + 1))
	return delay
}

// isTerminalLocationSyncStatus reports whether an NRP location-sync HTTP status is a deterministic
// client error that retrying the identical payload cannot fix (a malformed or unprocessable batch).
// Throttling (429), conflict (409), not-found (404) and 5xx are transient and remain retryable.
func isTerminalLocationSyncStatus(httpStatus int) bool {
	return httpStatus == http.StatusBadRequest || httpStatus == http.StatusUnprocessableEntity
}

// backoffAndRetry waits a bounded, jittered delay and then re-triggers the LocationsUpdater
// so a failed NRP/ARM sync is retried instead of stalling until an unrelated future trigger.
// It must be called from process() BEFORE the in-flight trigger counter is decremented, so
// initialization stays blocked until a sync actually succeeds. The wait is cancellable via the
// updater context (shutdown), and post-initialization a buffered trigger shortcuts it.
func (lu *LocationsUpdater) backoffAndRetry() {
	lu.failureCount++
	delay := computeRetryBackoff(lu.failureCount)

	lu.logger.V(4).Info("Scheduled NRP location sync retry", "delay", delay, "attempt", lu.failureCount)

	// Post-initialization, a trigger buffered by a fresh cluster change shortcuts the wait so it is
	// not delayed up to locationsRetryMaxDelay behind this retry; the re-trigger below coalesces with
	// the consumed token into a single process pass. During initialization wake stays nil so the wait
	// runs in full: consuming the token there would unbalance the in-flight trigger accounting that
	// WaitForInitialSync depends on.
	var wake <-chan bool
	if atomic.LoadInt32(&lu.diffTracker.isInitializing) == 0 {
		wake = lu.diffTracker.locationsUpdaterTrigger
	}
	select {
	case <-lu.ctx.Done():
		return
	case <-wake:
	case <-time.After(delay):
	}
	lu.diffTracker.triggerLocationsUpdater()
}
