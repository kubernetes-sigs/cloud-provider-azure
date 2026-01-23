package difftracker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"k8s.io/klog/v2"
)

// ServiceUpdater processes service creation/deletion in parallel
type ServiceUpdater struct {
	diffTracker *DiffTracker
	onComplete  func(serviceUID string, success bool, err error)
	trigger     <-chan bool
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	semaphore   chan struct{}   // Limits concurrent operations to 10
	mu          sync.Mutex      // Protects activeOperations
	activeOps   map[string]bool // Tracks which services are being processed
}

// NewServiceUpdater creates a new ServiceUpdater instance
func NewServiceUpdater(ctx context.Context, diffTracker *DiffTracker, onComplete func(string, bool, error), triggerChan <-chan bool) *ServiceUpdater {
	if diffTracker == nil {
		panic("ServiceUpdater: diffTracker must not be nil")
	}
	if onComplete == nil {
		panic("ServiceUpdater: onComplete callback must not be nil")
	}
	if triggerChan == nil {
		panic("ServiceUpdater: triggerChan must not be nil")
	}
	if diffTracker.networkClientFactory == nil {
		panic("ServiceUpdater: diffTracker.networkClientFactory must not be nil")
	}
	childCtx, cancel := context.WithCancel(ctx)
	return &ServiceUpdater{
		diffTracker: diffTracker,
		onComplete:  onComplete,
		trigger:     triggerChan,
		ctx:         childCtx,
		cancel:      cancel,
		semaphore:   make(chan struct{}, 10), // Max 10 concurrent operations
		activeOps:   make(map[string]bool),
	}
}

// Run starts the ServiceUpdater main loop
func (s *ServiceUpdater) Run() {
	klog.Infof("ServiceUpdater: starting main loop")

	for {
		select {
		case <-s.ctx.Done():
			klog.Infof("ServiceUpdater: context canceled, shutting down")
			s.wg.Wait() // Wait for all goroutines to finish
			return
		case <-s.trigger:
			klog.V(4).Infof("ServiceUpdater: received trigger, processing batch")
			s.processBatch()
		}
	}
}

// Stop gracefully shuts down the ServiceUpdater
func (s *ServiceUpdater) Stop() {
	klog.Infof("ServiceUpdater: stopping")
	s.cancel()
	s.wg.Wait()
	klog.Infof("ServiceUpdater: stopped")
}

// processBatch scans pendingServiceOps and spawns goroutines for services that need processing
func (s *ServiceUpdater) processBatch() {
	// Collect work to do while holding lock, then spawn goroutines after releasing lock
	type workItem struct {
		serviceUID string
		config     ServiceConfig
		state      ResourceState
	}
	var workToDo []workItem

	s.diffTracker.mu.Lock()
	for serviceUID, opState := range s.diffTracker.pendingServiceOps {
		// Check if already being processed
		s.mu.Lock()
		if s.activeOps[serviceUID] {
			s.mu.Unlock()
			continue
		}
		s.activeOps[serviceUID] = true
		s.mu.Unlock()

		// Collect work based on state
		switch opState.State {
		case StateNotStarted:
			// Transition to CreationInProgress
			recordStateTransition(StateNotStarted, StateCreationInProgress)
			opState.State = StateCreationInProgress
			workToDo = append(workToDo, workItem{serviceUID, opState.Config, StateCreationInProgress})

		case StateCreationInProgress:
			// Already being processed by another goroutine, skip
			s.mu.Lock()
			delete(s.activeOps, serviceUID)
			s.mu.Unlock()
			klog.V(4).Infof("ServiceUpdater: service %s already in StateCreationInProgress, skipping", serviceUID)

		case StateCreated:
			// Service successfully created, nothing to do
			s.mu.Lock()
			delete(s.activeOps, serviceUID)
			s.mu.Unlock()
			klog.V(4).Infof("ServiceUpdater: service %s already created, skipping", serviceUID)

		case StateDeletionPending:
			// Services in StateDeletionPending are waiting for LocationsUpdater to clear their addresses.
			// They will be moved to pendingServiceDeletions map and checkPendingServiceDeletions() will transition
			// them to StateDeletionInProgress once locations are cleared. Skip processing here.
			s.mu.Lock()
			delete(s.activeOps, serviceUID)
			s.mu.Unlock()
			klog.V(4).Infof("ServiceUpdater: service %s in StateDeletionPending, waiting for locations to be cleared", serviceUID)

		case StateDeletionInProgress:
			workToDo = append(workToDo, workItem{serviceUID, opState.Config, StateDeletionInProgress})
		}
	}
	s.diffTracker.mu.Unlock()

	// Record batch size metric
	recordServiceUpdaterBatch(len(workToDo))

	if len(workToDo) > 0 {
		klog.Infof("ServiceUpdater: processBatch collected %d services to process", len(workToDo))
	}

	// Decrement in-flight trigger counter and check initialization completion
	// Run this asynchronously to avoid blocking goroutines waiting on completion callbacks
	defer func() {
		s.diffTracker.mu.Lock()
		shouldCheck := atomic.LoadInt32(&s.diffTracker.isInitializing) == 1
		s.diffTracker.mu.Unlock()

		if shouldCheck {
			atomic.AddInt32(&s.diffTracker.pendingUpdaterTriggers, -1)
			// Trigger check asynchronously to avoid holding the lock while goroutines complete
			go s.diffTracker.checkInitializationComplete()
		}
	}()

	// Spawn goroutines after releasing diffTracker lock
	for _, work := range workToDo {
		switch work.state {
		case StateCreationInProgress:
			s.wg.Add(1)
			go func(uid string, cfg ServiceConfig) {
				defer s.wg.Done()
				defer func() {
					s.mu.Lock()
					delete(s.activeOps, uid)
					s.mu.Unlock()
				}()

				// Acquire semaphore with context awareness
				select {
				case s.semaphore <- struct{}{}:
					updateServiceUpdaterConcurrentOps(len(s.semaphore))
					defer func() {
						<-s.semaphore
						updateServiceUpdaterConcurrentOps(len(s.semaphore))
					}()
				case <-s.ctx.Done():
					klog.V(4).Infof("ServiceUpdater: context cancelled before acquiring semaphore for service %s", uid)
					return
				}

				if cfg.IsInbound {
					s.createInboundService(uid, cfg.InboundConfig)
				} else {
					s.createOutboundService(uid, cfg.OutboundConfig)
				}
			}(work.serviceUID, work.config)
		case StateDeletionInProgress:
			s.wg.Add(1)
			go func(uid string, cfg ServiceConfig) {
				defer s.wg.Done()
				defer func() {
					s.mu.Lock()
					delete(s.activeOps, uid)
					s.mu.Unlock()
				}()

				// Acquire semaphore with context awareness
				select {
				case s.semaphore <- struct{}{}:
					updateServiceUpdaterConcurrentOps(len(s.semaphore))
					defer func() {
						<-s.semaphore
						updateServiceUpdaterConcurrentOps(len(s.semaphore))
					}()
				case <-s.ctx.Done():
					klog.V(4).Infof("ServiceUpdater: context cancelled before acquiring semaphore for service %s", uid)
					return
				}

				if cfg.IsInbound {
					s.deleteInboundService(uid)
				} else {
					s.deleteOutboundService(uid)
				}
			}(work.serviceUID, work.config)
		}
	}
}

// createInboundService creates LoadBalancer resources for inbound service
func (s *ServiceUpdater) createInboundService(serviceUID string, config *InboundConfig) {
	klog.Infof("ServiceUpdater: createInboundService started for %s", serviceUID)

	ctx := s.ctx

	// Step 0: Add finalizer to K8s service to prevent deletion until Azure resources are cleaned up
	svc, err := s.diffTracker.getServiceByUID(ctx, serviceUID)
	if err != nil {
		klog.Warningf("ServiceUpdater: failed to get service %s for finalizer: %v (continuing anyway)", serviceUID, err)
		// Continue - service may have been deleted or this is initialization cleanup
	} else {
		if err := s.diffTracker.addServiceGatewayFinalizer(ctx, svc); err != nil {
			klog.Errorf("ServiceUpdater: failed to add finalizer to service %s: %v", serviceUID, err)
			s.onComplete(serviceUID, false, fmt.Errorf("failed to add finalizer: %w", err))
			return
		}
		klog.V(3).Infof("ServiceUpdater: added finalizer to service %s", serviceUID)
	}

	// Step 1: Build resources using shared helper
	pipResource, lbResource, servicesDTO := buildInboundServiceResources(serviceUID, config, s.diffTracker.config)

	// Step 2: Create Public IP and capture the response to get the allocated IP address
	pipResponse, err := s.diffTracker.createOrUpdatePIPWithResponse(ctx, s.diffTracker.config.ResourceGroup, &pipResource)
	if err != nil {
		klog.Errorf("ServiceUpdater: failed to create Public IP for inbound service %s: %v", serviceUID, err)
		s.onComplete(serviceUID, false, fmt.Errorf("failed to create Public IP: %w", err))
		return
	}
	pipName := fmt.Sprintf("%s-pip", serviceUID)
	klog.V(3).Infof("ServiceUpdater: created Public IP %s for inbound service %s", pipName, serviceUID)

	// Extract IP address from PIP response
	var pipIPAddress string
	if pipResponse != nil && pipResponse.Properties != nil && pipResponse.Properties.IPAddress != nil {
		pipIPAddress = *pipResponse.Properties.IPAddress
		klog.V(3).Infof("ServiceUpdater: PIP %s has IP address %s", pipName, pipIPAddress)
	}

	// Step 3: Create LoadBalancer
	if err := s.diffTracker.createOrUpdateLB(ctx, lbResource); err != nil {
		klog.Errorf("ServiceUpdater: failed to create LoadBalancer for inbound service %s: %v", serviceUID, err)
		// Don't delete PIP here - retry will use existing PIP
		s.onComplete(serviceUID, false, fmt.Errorf("failed to create LoadBalancer: %w", err))
		return
	}
	lbRulesCount := 0
	if lbResource.Properties != nil && lbResource.Properties.LoadBalancingRules != nil {
		lbRulesCount = len(lbResource.Properties.LoadBalancingRules)
	}
	klog.V(3).Infof("ServiceUpdater: created LoadBalancer with %d rules for inbound service %s", lbRulesCount, serviceUID)

	// Step 4: Register service with ServiceGateway API
	if err := s.diffTracker.updateNRPSGWServices(ctx, s.diffTracker.config.ServiceGatewayResourceName, servicesDTO); err != nil {
		klog.Errorf("ServiceUpdater: failed to register inbound service %s with ServiceGateway: %v", serviceUID, err)
		// Don't delete resources - retry will reconcile
		s.onComplete(serviceUID, false, fmt.Errorf("failed to register with ServiceGateway: %w", err))
		return
	}
	klog.V(3).Infof("ServiceUpdater: registered inbound service %s with ServiceGateway", serviceUID)

	// Step 5: Update K8s Service status with the external IP
	// This is critical for ServiceGateway mode since EnsureLoadBalancer returns empty status immediately.
	// Without this, the Service.Status.LoadBalancer.Ingress would remain empty.
	if pipIPAddress != "" {
		if err := s.diffTracker.updateServiceLoadBalancerStatus(ctx, serviceUID, pipIPAddress); err != nil {
			klog.Warningf("ServiceUpdater: failed to update service %s status with IP %s: %v (non-fatal, Azure resources created)", serviceUID, pipIPAddress, err)
			// Non-fatal: Azure resources are created successfully, status update can be retried
		} else {
			klog.V(3).Infof("ServiceUpdater: updated service %s status with external IP %s", serviceUID, pipIPAddress)
		}
	} else {
		klog.Warningf("ServiceUpdater: PIP IP address not available for service %s, cannot update service status", serviceUID)
	}

	// Update NRPResources to reflect the sync
	s.diffTracker.UpdateNRPLoadBalancers(SyncServicesReturnType{
		Additions: newIgnoreCaseSetFromSlice([]string{serviceUID}),
		Removals:  nil,
	})

	// Step 6: Success callback
	s.onComplete(serviceUID, true, nil)
	klog.Infof("ServiceUpdater: createInboundService completed successfully for %s", serviceUID)
}

// createOutboundService creates NAT Gateway resources for outbound service
func (s *ServiceUpdater) createOutboundService(serviceUID string, config *OutboundConfig) {
	klog.Infof("ServiceUpdater: createOutboundService started for %s", serviceUID)

	ctx := s.ctx

	// Step 1: Build resources using shared helper
	pipResource, natGatewayResource, servicesDTO := buildOutboundServiceResources(serviceUID, config, s.diffTracker.config)

	// Step 2: Create Public IP
	if err := s.diffTracker.createOrUpdatePIP(ctx, s.diffTracker.config.ResourceGroup, &pipResource); err != nil {
		klog.Errorf("ServiceUpdater: failed to create Public IP for outbound service %s: %v", serviceUID, err)
		s.onComplete(serviceUID, false, fmt.Errorf("failed to create Public IP: %w", err))
		return
	}
	pipName := fmt.Sprintf("%s-pip", serviceUID)
	klog.V(3).Infof("ServiceUpdater: created Public IP %s for outbound service %s", pipName, serviceUID)

	// Step 3: Create NAT Gateway
	if err := s.diffTracker.createOrUpdateNatGateway(ctx, s.diffTracker.config.ResourceGroup, natGatewayResource); err != nil {
		klog.Errorf("ServiceUpdater: failed to create NAT Gateway for outbound service %s: %v", serviceUID, err)
		// Don't delete PIP here - retry will use existing PIP
		s.onComplete(serviceUID, false, fmt.Errorf("failed to create NAT Gateway: %w", err))
		return
	}
	klog.V(3).Infof("ServiceUpdater: created NAT Gateway for outbound service %s", serviceUID)

	// Step 4: Register service with ServiceGateway API
	if err := s.diffTracker.updateNRPSGWServices(ctx, s.diffTracker.config.ServiceGatewayResourceName, servicesDTO); err != nil {
		klog.Errorf("ServiceUpdater: failed to register outbound service %s with ServiceGateway: %v", serviceUID, err)
		// Don't delete resources - retry will reconcile
		s.onComplete(serviceUID, false, fmt.Errorf("failed to register with ServiceGateway: %w", err))
		return
	}
	klog.V(3).Infof("ServiceUpdater: registered outbound service %s with ServiceGateway", serviceUID)

	// Update NRPResources to reflect the sync
	s.diffTracker.UpdateNRPNATGateways(SyncServicesReturnType{
		Additions: newIgnoreCaseSetFromSlice([]string{serviceUID}),
		Removals:  nil,
	})

	// Step 4: Success callback
	s.onComplete(serviceUID, true, nil)
	klog.Infof("ServiceUpdater: createOutboundService completed successfully for %s", serviceUID)
}

// deleteInboundService deletes LoadBalancer resources
func (s *ServiceUpdater) deleteInboundService(serviceUID string) {
	klog.Infof("ServiceUpdater: deleteInboundService started for %s", serviceUID)

	ctx := s.ctx
	var lastErr error

	// Step 1: Remove backend pool references from ServiceGateway
	// This should be done before deleting the LoadBalancer to properly clean up references
	removeBackendPoolDTO := RemoveBackendPoolReferenceFromServicesDTO(
		SyncServicesReturnType{
			Additions: nil,
			Removals:  newIgnoreCaseSetFromSlice([]string{serviceUID}),
		},
		s.diffTracker.config.SubscriptionID,
		s.diffTracker.config.ResourceGroup,
	)

	if err := s.diffTracker.updateNRPSGWServices(ctx, s.diffTracker.config.ServiceGatewayResourceName, removeBackendPoolDTO); err != nil {
		klog.Warningf("ServiceUpdater: failed to remove backend pool reference for inbound service %s: %v", serviceUID, err)
		// Don't fail the deletion - continue with LoadBalancer deletion
	} else {
		klog.V(3).Infof("ServiceUpdater: removed backend pool reference for inbound service %s", serviceUID)
	}

	// Step 2: Delete LoadBalancer
	if err := s.diffTracker.deleteLB(ctx, serviceUID); err != nil {
		klog.Errorf("ServiceUpdater: failed to delete LoadBalancer for inbound service %s: %v", serviceUID, err)
		lastErr = fmt.Errorf("failed to delete LoadBalancer: %w", err)
		// Continue with PIP deletion
	} else {
		klog.V(3).Infof("ServiceUpdater: deleted LoadBalancer for inbound service %s", serviceUID)
	}

	// Step 3: Fully unregister service from ServiceGateway
	unregisterDTO := buildServiceGatewayRemovalDTO(serviceUID, true, s.diffTracker.config)

	if err := s.diffTracker.updateNRPSGWServices(ctx, s.diffTracker.config.ServiceGatewayResourceName, unregisterDTO); err != nil {
		// Treat 404 NotFound as success - the service is already gone from ServiceGateway
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound {
			klog.V(3).Infof("ServiceUpdater: inbound service %s already unregistered from ServiceGateway (404 NotFound)", serviceUID)
		} else {
			klog.Errorf("ServiceUpdater: failed to fully unregister inbound service %s from ServiceGateway: %v", serviceUID, err)
			lastErr = fmt.Errorf("failed to unregister from ServiceGateway: %w", err)
			// Continue with PIP deletion
		}
	} else {
		klog.V(3).Infof("ServiceUpdater: fully unregistered inbound service %s from ServiceGateway", serviceUID)
	}

	// Step 4: Delete Public IP
	_, pipName, _ := buildInboundResourceNames(serviceUID)
	if err := s.diffTracker.deletePublicIP(ctx, s.diffTracker.config.ResourceGroup, pipName); err != nil {
		klog.Errorf("ServiceUpdater: failed to delete Public IP %s for inbound service %s: %v", pipName, serviceUID, err)
		lastErr = fmt.Errorf("failed to delete Public IP: %w", err)
	} else {
		klog.V(3).Infof("ServiceUpdater: deleted Public IP %s for inbound service %s", pipName, serviceUID)
	}

	// Step 5: Update NRPResources and notify completion
	if lastErr != nil {
		klog.Warningf("ServiceUpdater: deleteInboundService completed with errors for %s: %v", serviceUID, lastErr)
		s.onComplete(serviceUID, false, lastErr)
	} else {
		klog.Infof("ServiceUpdater: deleteInboundService completed successfully for %s", serviceUID)
		// Update NRPResources to reflect the deletion
		s.diffTracker.UpdateNRPLoadBalancers(SyncServicesReturnType{
			Additions: nil,
			Removals:  newIgnoreCaseSetFromSlice([]string{serviceUID}),
		})

		// Step 6: Remove finalizer from K8s service to allow deletion
		svc, err := s.diffTracker.getServiceByUID(ctx, serviceUID)
		if err != nil {
			klog.V(3).Infof("ServiceUpdater: service %s not found for finalizer removal (may be already deleted): %v", serviceUID, err)
			// Service already gone - no finalizer to remove
		} else {
			if err := s.diffTracker.removeServiceGatewayFinalizer(ctx, svc); err != nil {
				klog.Warningf("ServiceUpdater: failed to remove finalizer from service %s: %v", serviceUID, err)
				// Don't fail the deletion - Azure resources are cleaned up
			} else {
				klog.V(3).Infof("ServiceUpdater: removed finalizer from service %s", serviceUID)
			}
		}

		s.onComplete(serviceUID, true, nil)
	}
}

// deleteOutboundService deletes NAT Gateway resources
func (s *ServiceUpdater) deleteOutboundService(serviceUID string) {
	klog.Infof("ServiceUpdater: deleteOutboundService started for %s", serviceUID)

	ctx := s.ctx
	var lastErr error

	// Step 1: Disassociate NAT Gateway from ServiceGateway
	if err := s.diffTracker.disassociateNatGatewayFromServiceGateway(ctx, s.diffTracker.config.ServiceGatewayResourceName, serviceUID); err != nil {
		klog.Warningf("ServiceUpdater: failed to disassociate NAT Gateway %s from ServiceGateway: %v", serviceUID, err)
		// Non-fatal - continue with deletion
	} else {
		klog.V(3).Infof("ServiceUpdater: disassociated NAT Gateway %s from ServiceGateway", serviceUID)
	}

	// Step 2: Unregister from ServiceGateway API
	servicesDTO := buildServiceGatewayRemovalDTO(serviceUID, false, s.diffTracker.config)

	if err := s.diffTracker.updateNRPSGWServices(ctx, s.diffTracker.config.ServiceGatewayResourceName, servicesDTO); err != nil {
		// Treat 404 NotFound as success - the service is already gone from ServiceGateway
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound {
			klog.V(3).Infof("ServiceUpdater: outbound service %s already unregistered from ServiceGateway (404 NotFound)", serviceUID)
		} else {
			klog.Errorf("ServiceUpdater: failed to unregister outbound service %s from ServiceGateway: %v", serviceUID, err)
			lastErr = fmt.Errorf("failed to unregister from ServiceGateway: %w", err)
			// Continue with deletion
		}
	} else {
		klog.V(3).Infof("ServiceUpdater: unregistered outbound service %s from ServiceGateway", serviceUID)
	}

	// Step 3: Delete NAT Gateway
	if err := s.diffTracker.deleteNatGateway(ctx, s.diffTracker.config.ResourceGroup, serviceUID); err != nil {
		klog.Errorf("ServiceUpdater: failed to delete NAT Gateway for outbound service %s: %v", serviceUID, err)
		lastErr = fmt.Errorf("failed to delete NAT Gateway: %w", err)
		// Continue with PIP deletion
	} else {
		klog.V(3).Infof("ServiceUpdater: deleted NAT Gateway for outbound service %s", serviceUID)
	}

	// Step 4: Delete Public IP
	_, pipName := buildOutboundResourceNames(serviceUID)
	if err := s.diffTracker.deletePublicIP(ctx, s.diffTracker.config.ResourceGroup, pipName); err != nil {
		klog.Errorf("ServiceUpdater: failed to delete Public IP %s for outbound service %s: %v", pipName, serviceUID, err)
		lastErr = fmt.Errorf("failed to delete Public IP: %w", err)
	} else {
		klog.V(3).Infof("ServiceUpdater: deleted Public IP %s for outbound service %s", pipName, serviceUID)
	}

	// Step 5: Update NRPResources and notify completion
	if lastErr != nil {
		klog.Warningf("ServiceUpdater: deleteOutboundService completed with errors for %s: %v", serviceUID, lastErr)
		s.onComplete(serviceUID, false, lastErr)
	} else {
		klog.Infof("ServiceUpdater: deleteOutboundService completed successfully for %s", serviceUID)
		// Update NRPResources to reflect the deletion
		s.diffTracker.UpdateNRPNATGateways(SyncServicesReturnType{
			Additions: nil,
			Removals:  newIgnoreCaseSetFromSlice([]string{serviceUID}),
		})

		// Step 6: Remove finalizers from last-pod entries now that NAT Gateway is deleted
		s.diffTracker.RemoveLastPodFinalizers(ctx, serviceUID)

		s.onComplete(serviceUID, true, nil)
	}
}
