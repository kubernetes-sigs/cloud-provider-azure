package difftracker

import (
	"context"
	"fmt"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"k8s.io/klog/v2"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
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
		isInbound  bool
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
			workToDo = append(workToDo, workItem{serviceUID, opState.IsInbound, StateCreationInProgress})

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
			// They will be moved to pendingDeletions map and checkPendingDeletions() will transition
			// them to StateDeletionInProgress once locations are cleared. Skip processing here.
			s.mu.Lock()
			delete(s.activeOps, serviceUID)
			s.mu.Unlock()
			klog.V(4).Infof("ServiceUpdater: service %s in StateDeletionPending, waiting for locations to be cleared", serviceUID)

		case StateDeletionInProgress:
			workToDo = append(workToDo, workItem{serviceUID, opState.IsInbound, StateDeletionInProgress})
		}
	}
	s.diffTracker.mu.Unlock()

	// Record batch size metric
	recordServiceUpdaterBatch(len(workToDo))

	// Spawn goroutines after releasing diffTracker lock
	for _, work := range workToDo {
		switch work.state {
		case StateCreationInProgress:
			s.wg.Add(1)
			go func(uid string, isInbound bool) {
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

				if isInbound {
					s.createInboundService(uid)
				} else {
					s.createOutboundService(uid)
				}
			}(work.serviceUID, work.isInbound)
		case StateDeletionInProgress:
			s.wg.Add(1)
			go func(uid string, isInbound bool) {
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

				if isInbound {
					s.deleteInboundService(uid)
				} else {
					s.deleteOutboundService(uid)
				}
			}(work.serviceUID, work.isInbound)
		}
	}
}

// createInboundService creates LoadBalancer resources for inbound service
func (s *ServiceUpdater) createInboundService(serviceUID string) {
	klog.Infof("ServiceUpdater: createInboundService started for %s", serviceUID)

	ctx := s.ctx

	// Step 1: Create Public IP
	pipName := fmt.Sprintf("%s-pip", serviceUID)
	pipResource := armnetwork.PublicIPAddress{
		Name: to.Ptr(pipName),
		ID: to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPAddresses/%s",
			s.diffTracker.config.SubscriptionID, s.diffTracker.config.ResourceGroup, pipName)),
		SKU: &armnetwork.PublicIPAddressSKU{
			Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandardV2),
		},
		Location: to.Ptr(s.diffTracker.config.Location),
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
		},
	}

	if err := s.diffTracker.createOrUpdatePIP(ctx, s.diffTracker.config.ResourceGroup, &pipResource); err != nil {
		klog.Errorf("ServiceUpdater: failed to create Public IP for inbound service %s: %v", serviceUID, err)
		s.onComplete(serviceUID, false, fmt.Errorf("failed to create Public IP: %w", err))
		return
	}
	klog.V(3).Infof("ServiceUpdater: created Public IP %s for inbound service %s", pipName, serviceUID)

	// Step 2: Get service object
	svc, err := s.diffTracker.getServiceByUID(ctx, serviceUID)
	if err != nil {
		klog.Errorf("ServiceUpdater: failed to get service for inbound service %s: %v", serviceUID, err)
		s.onComplete(serviceUID, false, fmt.Errorf("failed to get service: %w", err))
		return
	}
	if svc == nil {
		klog.Errorf("ServiceUpdater: service not found for inbound service %s", serviceUID)
		s.onComplete(serviceUID, false, fmt.Errorf("service not found"))
		return
	}

	// Step 3: Create LoadBalancer resource structure
	lbResource := armnetwork.LoadBalancer{
		Name:     to.Ptr(serviceUID),
		Location: to.Ptr(s.diffTracker.config.Location),
		SKU: &armnetwork.LoadBalancerSKU{
			Name: to.Ptr(armnetwork.LoadBalancerSKUNameService),
		},
		Properties: &armnetwork.LoadBalancerPropertiesFormat{
			FrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{
				{
					Name: to.Ptr("frontend"),
					Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &armnetwork.PublicIPAddress{
							ID: to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPAddresses/%s",
								s.diffTracker.config.SubscriptionID, s.diffTracker.config.ResourceGroup, pipName)),
						},
					},
				},
			},
		},
	}

	// Step 4: Create LoadBalancer via CreateOrUpdateLB
	if err := s.diffTracker.createOrUpdateLB(ctx, svc, lbResource); err != nil {
		klog.Errorf("ServiceUpdater: failed to create LoadBalancer for inbound service %s: %v", serviceUID, err)
		// Don't delete PIP here - retry will use existing PIP
		s.onComplete(serviceUID, false, fmt.Errorf("failed to create LoadBalancer: %w", err))
		return
	}
	klog.V(3).Infof("ServiceUpdater: created LoadBalancer for inbound service %s", serviceUID)

	// Step 5: Register service with ServiceGateway API
	servicesDTO := MapLoadBalancerAndNATGatewayUpdatesToServicesDataDTO(
		SyncServicesReturnType{
			Additions: newIgnoreCaseSetFromSlice([]string{serviceUID}),
			Removals:  nil,
		},
		SyncServicesReturnType{
			Additions: nil,
			Removals:  nil,
		},
		s.diffTracker.config.SubscriptionID,
		s.diffTracker.config.ResourceGroup,
	)

	if err := s.diffTracker.updateNRPSGWServices(ctx, s.diffTracker.config.ServiceGatewayResourceName, servicesDTO); err != nil {
		klog.Errorf("ServiceUpdater: failed to register inbound service %s with ServiceGateway: %v", serviceUID, err)
		// Don't delete resources - retry will reconcile
		s.onComplete(serviceUID, false, fmt.Errorf("failed to register with ServiceGateway: %w", err))
		return
	}
	klog.V(3).Infof("ServiceUpdater: registered inbound service %s with ServiceGateway", serviceUID)

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
func (s *ServiceUpdater) createOutboundService(serviceUID string) {
	klog.Infof("ServiceUpdater: createOutboundService started for %s", serviceUID)

	ctx := s.ctx

	// Step 1: Create Public IP
	pipName := fmt.Sprintf("%s-pip", serviceUID)
	pipResource := armnetwork.PublicIPAddress{
		Name: to.Ptr(pipName),
		ID: to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPAddresses/%s",
			s.diffTracker.config.SubscriptionID, s.diffTracker.config.ResourceGroup, pipName)),
		SKU: &armnetwork.PublicIPAddressSKU{
			Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandardV2),
		},
		Location: to.Ptr(s.diffTracker.config.Location),
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
		},
	}

	if err := s.diffTracker.createOrUpdatePIP(ctx, s.diffTracker.config.ResourceGroup, &pipResource); err != nil {
		klog.Errorf("ServiceUpdater: failed to create Public IP for outbound service %s: %v", serviceUID, err)
		s.onComplete(serviceUID, false, fmt.Errorf("failed to create Public IP: %w", err))
		return
	}
	klog.V(3).Infof("ServiceUpdater: created Public IP %s for outbound service %s", pipName, serviceUID)

	// Step 2: Create NAT Gateway
	natGatewayResource := armnetwork.NatGateway{
		Name: to.Ptr(serviceUID),
		ID: to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/natGateways/%s",
			s.diffTracker.config.SubscriptionID, s.diffTracker.config.ResourceGroup, serviceUID)),
		SKU: &armnetwork.NatGatewaySKU{
			Name: to.Ptr(armnetwork.NatGatewaySKUNameStandardV2),
		},
		Location: to.Ptr(s.diffTracker.config.Location),
		Properties: &armnetwork.NatGatewayPropertiesFormat{
			ServiceGateway: &armnetwork.ServiceGateway{
				ID: to.Ptr(s.diffTracker.config.ServiceGatewayID),
			},
			PublicIPAddresses: []*armnetwork.SubResource{
				{
					ID: to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPAddresses/%s",
						s.diffTracker.config.SubscriptionID, s.diffTracker.config.ResourceGroup, pipName)),
				},
			},
		},
	}

	if err := s.diffTracker.createOrUpdateNatGateway(ctx, s.diffTracker.config.ResourceGroup, natGatewayResource); err != nil {
		klog.Errorf("ServiceUpdater: failed to create NAT Gateway for outbound service %s: %v", serviceUID, err)
		// Don't delete PIP here - retry will use existing PIP
		s.onComplete(serviceUID, false, fmt.Errorf("failed to create NAT Gateway: %w", err))
		return
	}
	klog.V(3).Infof("ServiceUpdater: created NAT Gateway for outbound service %s", serviceUID)

	// Step 3: Register service with ServiceGateway API
	servicesDTO := MapLoadBalancerAndNATGatewayUpdatesToServicesDataDTO(
		SyncServicesReturnType{
			Additions: nil,
			Removals:  nil,
		},
		SyncServicesReturnType{
			Additions: newIgnoreCaseSetFromSlice([]string{serviceUID}),
			Removals:  nil,
		},
		s.diffTracker.config.SubscriptionID,
		s.diffTracker.config.ResourceGroup,
	)

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
	unregisterDTO := MapLoadBalancerAndNATGatewayUpdatesToServicesDataDTO(
		SyncServicesReturnType{
			Additions: nil,
			Removals:  newIgnoreCaseSetFromSlice([]string{serviceUID}),
		},
		SyncServicesReturnType{
			Additions: nil,
			Removals:  nil,
		},
		s.diffTracker.config.SubscriptionID,
		s.diffTracker.config.ResourceGroup,
	)

	if err := s.diffTracker.updateNRPSGWServices(ctx, s.diffTracker.config.ServiceGatewayResourceName, unregisterDTO); err != nil {
		klog.Errorf("ServiceUpdater: failed to fully unregister inbound service %s from ServiceGateway: %v", serviceUID, err)
		lastErr = fmt.Errorf("failed to unregister from ServiceGateway: %w", err)
		// Continue with PIP deletion
	} else {
		klog.V(3).Infof("ServiceUpdater: fully unregistered inbound service %s from ServiceGateway", serviceUID)
	}

	// Step 4: Delete Public IP
	pipName := fmt.Sprintf("%s-pip", serviceUID)
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
		s.onComplete(serviceUID, true, nil)
	}
}

// deleteOutboundService deletes NAT Gateway resources
func (s *ServiceUpdater) deleteOutboundService(serviceUID string) {
	klog.Infof("ServiceUpdater: deleteOutboundService started for %s", serviceUID)

	ctx := s.ctx
	var lastErr error

	// Step 1: Unregister from ServiceGateway API
	servicesDTO := MapLoadBalancerAndNATGatewayUpdatesToServicesDataDTO(
		SyncServicesReturnType{
			Additions: nil,
			Removals:  nil,
		},
		SyncServicesReturnType{
			Additions: nil,
			Removals:  newIgnoreCaseSetFromSlice([]string{serviceUID}),
		},
		s.diffTracker.config.SubscriptionID,
		s.diffTracker.config.ResourceGroup,
	)

	if err := s.diffTracker.updateNRPSGWServices(ctx, s.diffTracker.config.ServiceGatewayResourceName, servicesDTO); err != nil {
		klog.Errorf("ServiceUpdater: failed to unregister outbound service %s from ServiceGateway: %v", serviceUID, err)
		lastErr = fmt.Errorf("failed to unregister from ServiceGateway: %w", err)
		// Continue with deletion
	} else {
		klog.V(3).Infof("ServiceUpdater: unregistered outbound service %s from ServiceGateway", serviceUID)
	}

	// Step 2: Delete NAT Gateway
	if err := s.diffTracker.deleteNatGateway(ctx, s.diffTracker.config.ResourceGroup, serviceUID); err != nil {
		klog.Errorf("ServiceUpdater: failed to delete NAT Gateway for outbound service %s: %v", serviceUID, err)
		lastErr = fmt.Errorf("failed to delete NAT Gateway: %w", err)
		// Continue with PIP deletion
	} else {
		klog.V(3).Infof("ServiceUpdater: deleted NAT Gateway for outbound service %s", serviceUID)
	}

	// Step 3: Delete Public IP
	pipName := fmt.Sprintf("%s-pip", serviceUID)
	if err := s.diffTracker.deletePublicIP(ctx, s.diffTracker.config.ResourceGroup, pipName); err != nil {
		klog.Errorf("ServiceUpdater: failed to delete Public IP %s for outbound service %s: %v", pipName, serviceUID, err)
		lastErr = fmt.Errorf("failed to delete Public IP: %w", err)
	} else {
		klog.V(3).Infof("ServiceUpdater: deleted Public IP %s for outbound service %s", pipName, serviceUID)
	}

	// Step 4: Update NRPResources and notify completion
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
		s.onComplete(serviceUID, true, nil)
	}
}

// newIgnoreCaseSetFromSlice creates an IgnoreCaseSet from a slice of strings
func newIgnoreCaseSetFromSlice(items []string) *utilsets.IgnoreCaseSet {
	set := utilsets.NewString()
	for _, item := range items {
		set.Insert(item)
	}
	return set
}
