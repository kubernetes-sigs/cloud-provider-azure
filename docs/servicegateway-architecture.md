# ServiceGateway Architecture (ServiceGatewayEnabled=true)

This document describes the architecture and control flow when the Cloud Provider Azure operates with `ServiceGatewayEnabled=true`. This mode replaces the synchronous `EnsureLoadBalancer()` pattern with an **asynchronous, non-blocking architecture** for managing Kubernetes LoadBalancer services and NAT Gateways for pod egress.

## Overview

### Key Components

| Component | File | Description |
|-----------|------|-------------|
| **DiffTracker** | `pkg/provider/difftracker/difftracker.go` | Core state manager holding K8s and NRP state representations |
| **Engine** | `pkg/provider/difftracker/engine.go` | API layer with state machine for service lifecycle (`AddService`, `UpdateEndpoints`, `DeleteService`, `AddPod`, `DeletePod`) |
| **ServiceUpdater** | `pkg/provider/difftracker/service_updater.go` | Background goroutine that creates/deletes Azure resources (PIP, LB, NAT Gateway) |
| **LocationsUpdater** | `pkg/provider/difftracker/locations_updater.go` | Background goroutine that syncs pod IP locations to NRP via `UpdateAddressLocations` |
| **Repository** | `pkg/provider/difftracker/repository.go` | Azure SDK operations for ServiceGateway API (`UpdateServices`, `UpdateAddressLocations`) |

### State Machine

Services are tracked through a 5-state lifecycle:

```
┌──────────────────┐     ┌─────────────────────────┐     ┌───────────────┐
│  StateNotStarted │ ──▶ │ StateCreationInProgress │ ──▶ │  StateCreated │
└──────────────────┘     └─────────────────────────┘     └───────────────┘
                                                                 │
                                                                 ▼
                         ┌─────────────────────────┐     ┌───────────────────────┐
                         │ StateDeletionInProgress │ ◀── │  StateDeletionPending │
                         └─────────────────────────┘     └───────────────────────┘
```

### Buffering Mechanism

- **`pendingEndpoints[serviceUID]`**: Buffers EndpointSlice updates while service is being created
- **`pendingPods[serviceUID]`**: Buffers pod additions while NAT Gateway is being created
- **`pendingDeletions[serviceUID]`**: Tracks services awaiting location clearance before deletion

---

## Chapter 1: Inbound (Services, EndpointSlices)

### High-Level Flow

1. **Service Creation Trigger**
   - Kubernetes Service of type `LoadBalancer` is created
   - Service Informer detects the event and calls `EnsureLoadBalancer()`
   - When `ServiceGatewayEnabled=true`, `EnsureLoadBalancer()` returns **immediately** (non-blocking)

2. **Engine State Management**
   - `Engine.AddService()` creates entry in `pendingServiceOps[serviceUID]` with `State=StateNotStarted`
   - Triggers `ServiceUpdater` via channel

3. **Asynchronous Resource Creation (ServiceUpdater)**
   - Transitions state to `StateCreationInProgress`
   - Creates Public IP (PIP) via Azure SDK
   - Creates Load Balancer via `CreateOrUpdateLB()`
   - Registers service with ServiceGateway via `UpdateServices()`
   - Calls `OnServiceCreationComplete(serviceUID, success=true)`

4. **Endpoint Handling During Creation**
   - EndpointSlice Informer calls `Engine.UpdateEndpoints()`
   - If `State=StateCreationInProgress`: endpoints are **buffered** in `pendingEndpoints[serviceUID]`
   - If `State=StateCreated`: endpoints are applied immediately via `updateK8sEndpointsLocked()`

5. **Buffer Promotion**
   - On successful creation, `promotePendingEndpointsLocked()` merges all buffered endpoints
   - Triggers `LocationsUpdater` to sync pod IPs to NRP

6. **Location Synchronization (LocationsUpdater)**
   - Computes diff between `K8sResources.Nodes` and `NRPResources.Locations`
   - Calls `UpdateAddressLocations()` to sync pod IPs to ServiceGateway

7. **Service Deletion**
   - `Engine.DeleteService()` transitions to `StateDeletionPending`
   - Adds entry to `pendingDeletions[serviceUID]`
   - `LocationsUpdater` clears locations, then `CheckPendingDeletions()` triggers deletion
   - `ServiceUpdater` deletes LB, unregisters from ServiceGateway, deletes PIP

### Sequence Diagram

Copy and paste the following into [sequencediagram.org](https://sequencediagram.org):

```
title Inbound Service Lifecycle (LoadBalancer)

participant "K8s Service\nInformer" as K8sSvc
participant "K8s EndpointSlice\nInformer" as K8sEP
participant "EnsureLoadBalancer\n(azure_loadbalancer.go)" as EnsureLB
participant "Engine" as Engine
participant "ServiceUpdater\n(goroutine)" as SvcUpdater
participant "LocationsUpdater\n(goroutine)" as LocUpdater
participant "Azure APIs\n(PIP, LB, SGW)" as Azure

==Phase 1: Service Creation Request==

K8sSvc->EnsureLB: Service created (type: LoadBalancer)
EnsureLB->Engine: AddService(serviceUID)
note right of Engine: State = NotStarted\npendingServiceOps[uid] created
Engine->SvcUpdater: trigger (async)
Engine-->EnsureLB: return immediately
EnsureLB-->K8sSvc: empty status (non-blocking)

==Phase 2: Endpoints Arrive (during creation)==

K8sEP->Engine: UpdateEndpoints(serviceUID, podIPs)
note right of Engine: State still NotStarted/InProgress\nBuffer in pendingEndpoints[uid]
Engine-->K8sEP: buffered

==Phase 3: Async Resource Creation==

SvcUpdater->SvcUpdater: receive trigger
note right of SvcUpdater: State → CreationInProgress
SvcUpdater->Azure: Create Public IP
Azure-->SvcUpdater: PIP ready
SvcUpdater->Azure: Create Load Balancer
Azure-->SvcUpdater: LB ready
SvcUpdater->Azure: Register with ServiceGateway
Azure-->SvcUpdater: registered
SvcUpdater->Engine: OnServiceCreationComplete(success)

==Phase 4: Buffer Promotion==

note right of Engine: State → Created
Engine->Engine: promotePendingEndpoints()
note right of Engine: Merge buffered endpoints\nUpdate K8sResources
Engine->LocUpdater: trigger location sync

==Phase 5: Location Sync to NRP==

LocUpdater->LocUpdater: receive trigger
LocUpdater->Engine: get location diff (K8s vs NRP)
LocUpdater->Azure: UpdateAddressLocations(podIPs)
Azure-->LocUpdater: synced
note right of LocUpdater: Pod IPs now routable

==Phase 6: Runtime Endpoint Updates==

K8sEP->Engine: UpdateEndpoints(serviceUID, newPodIPs)
note right of Engine: State = Created\nApply immediately
Engine->LocUpdater: trigger
LocUpdater->Azure: UpdateAddressLocations(delta)

==Phase 7: Service Deletion==

K8sSvc->EnsureLB: Service deleted
EnsureLB->Engine: DeleteService(serviceUID)
note right of Engine: State → DeletionPending\nAdd to pendingDeletions

alt Locations exist in NRP
    note right of Engine: Wait for locations to clear
    LocUpdater->Azure: UpdateAddressLocations(remove all)
    LocUpdater->Engine: CheckPendingDeletions()
    note right of Engine: No locations remaining\nState → DeletionInProgress
    Engine->SvcUpdater: trigger deletion
end

SvcUpdater->Azure: Unregister from ServiceGateway
SvcUpdater->Azure: Delete Load Balancer
Azure-->SvcUpdater: deleted
SvcUpdater->Azure: Delete Public IP
Azure-->SvcUpdater: deleted
SvcUpdater->Engine: OnServiceCreationComplete(success)
note right of Engine: Cleanup all state\nRemove from pendingServiceOps
```

---

## Chapter 2: Outbound (Pods with Egress Label)

### High-Level Flow

1. **Pod Detection**
   - Pods labeled with `kubernetes.azure.com/service-egress-gateway=<egress-name>` are detected
   - Pod Informer (filtered by label selector) triggers `podInformerAddPod()`

2. **First Pod for Egress Gateway**
   - `Engine.AddPod()` checks if NAT Gateway exists in `NRPResources.NATGateways`
   - If **not exists**: creates `pendingServiceOps[egressName]` with `State=StateNotStarted`
   - Buffers pod in `pendingPods[egressName]`
   - Triggers `ServiceUpdater` to create NAT Gateway

3. **NAT Gateway Creation (ServiceUpdater)**
   - Creates Public IP (PIP)
   - Creates NAT Gateway via `CreateOrUpdateNatGateway()`
   - Registers with ServiceGateway via `UpdateServices()`
   - Calls `OnServiceCreationComplete()`

4. **Buffer Promotion**
   - `promotePendingPodsLocked()` iterates through buffered pods
   - Calls `updateK8sPodLocked()` for each pod
   - Updates `LocalServiceNameToNRPServiceMap[egressName]` counter

5. **Subsequent Pods**
   - If NAT Gateway already exists (`State=StateCreated` or in NRP)
   - Pod is added **immediately** via `updateK8sPodLocked()`
   - Counter in `LocalServiceNameToNRPServiceMap` is incremented

6. **Pod Deletion**
   - `Engine.DeletePod()` decrements counter
   - If counter reaches 0 (last pod): marks service for deletion via `StateDeletionPending`
   - `LocationsUpdater` clears locations, then NAT Gateway is deleted

### Sequence Diagram

Copy and paste the following into [sequencediagram.org](https://sequencediagram.org):

```
title Outbound Pod Lifecycle (NAT Gateway for Egress)

participant "K8s Pod Informer\n(egress label filter)" as K8sPod
participant "Pod Handler\n(azure_servicegateway_pods.go)" as PodHandler
participant "Engine" as Engine
participant "ServiceUpdater\n(goroutine)" as SvcUpdater
participant "LocationsUpdater\n(goroutine)" as LocUpdater
participant "Azure APIs\n(PIP, NAT, SGW)" as Azure

==Phase 1: First Pod - Triggers NAT Gateway Creation==

K8sPod->PodHandler: Pod created with label\nkubernetes.azure.com/service-egress-gateway=my-egress
PodHandler->PodHandler: Extract egress name, podIP, nodeIP
PodHandler->Engine: AddPod("my-egress", podKey, nodeIP, podIP)

note right of Engine: NAT Gateway doesn't exist\nState = NotStarted\nBuffer pod in pendingPods[uid]
Engine->SvcUpdater: trigger (async)

==Phase 2: Async NAT Gateway Creation==

SvcUpdater->SvcUpdater: receive trigger
note right of SvcUpdater: State → CreationInProgress
SvcUpdater->Azure: Create Public IP
Azure-->SvcUpdater: PIP ready
SvcUpdater->Azure: Create NAT Gateway (SKU: StandardV2)
Azure-->SvcUpdater: NAT ready
SvcUpdater->Azure: Register with ServiceGateway
Azure-->SvcUpdater: registered
SvcUpdater->Engine: OnServiceCreationComplete(success)

==Phase 3: Buffer Promotion==

note right of Engine: State → Created
Engine->Engine: promotePendingPods()
note right of Engine: Add pod to K8sResources\nCounter["my-egress"] = 1
Engine->LocUpdater: trigger location sync

==Phase 4: Location Sync==

LocUpdater->Azure: UpdateAddressLocations(nodeIP, podIP→"my-egress")
Azure-->LocUpdater: synced
note right of LocUpdater: Pod egress now routable via NAT

==Phase 5: Subsequent Pods (NAT exists)==

K8sPod->PodHandler: Another pod with same egress label
PodHandler->Engine: AddPod("my-egress", pod-2, nodeIP2, podIP2)
note right of Engine: State = Created\nAdd immediately\nCounter["my-egress"] = 2
Engine->LocUpdater: trigger
LocUpdater->Azure: UpdateAddressLocations(nodeIP2, podIP2)

==Phase 6: Pod Deletion (not last)==

K8sPod->PodHandler: pod-2 deleted
PodHandler->Engine: DeletePod("my-egress", nodeIP2, podIP2)
note right of Engine: Counter = 2, not last pod\nRemove from K8sResources\nCounter["my-egress"] = 1
Engine->LocUpdater: trigger
LocUpdater->Azure: UpdateAddressLocations(remove podIP2)

==Phase 7: Last Pod Deletion - NAT Gateway Cleanup==

K8sPod->PodHandler: pod-1 deleted (last pod)
PodHandler->Engine: DeletePod("my-egress", nodeIP, podIP)
note right of Engine: Counter = 1, this is last pod!\nState → DeletionPending\nAdd to pendingDeletions
Engine->LocUpdater: trigger

LocUpdater->Azure: UpdateAddressLocations(remove podIP)
LocUpdater->Engine: CheckPendingDeletions()
note right of Engine: No locations remaining\nState → DeletionInProgress
Engine->SvcUpdater: trigger deletion

SvcUpdater->Azure: Unregister from ServiceGateway
SvcUpdater->Azure: Delete NAT Gateway
Azure-->SvcUpdater: deleted
SvcUpdater->Azure: Delete Public IP
Azure-->SvcUpdater: deleted
SvcUpdater->Engine: OnServiceCreationComplete(success)
note right of Engine: Cleanup all state

==Phase 8: Pod Label Change (optional)==

K8sPod->PodHandler: Pod label changed: my-egress → other-egress
PodHandler->Engine: DeletePod("my-egress", nodeIP, podIP)
PodHandler->Engine: AddPod("other-egress", podKey, nodeIP, podIP)
note right of Engine: Handled as delete + add
```

---

## Chapter 3: Initialization

### High-Level Flow

1. **CCM Startup**
   - Cloud Controller Manager starts
   - `InitializeCloudFromConfig()` is called with Azure config

2. **ServiceGateway Setup**
   - Check if ServiceGateway exists via `existsServiceGateway()`
   - If not exists: create via `createServiceGateway()`
   - Attach ServiceGateway to subnet via `attachServiceGatewayToSubnet()`

3. **DiffTracker Initialization**
   - `InitializeFromCluster()` is called
   - Builds K8s state from API server (Services, EndpointSlices, Egress pods)
   - Builds NRP state from Azure (existing LBs, NAT Gateways, locations)

4. **State Reconciliation**
   - Compute sync operations: services to create, services to delete, locations to update
   - Initialize DiffTracker with computed state
   - Start `ServiceUpdater` and `LocationsUpdater` goroutines

5. **Async Reconciliation**
   - `reconcileServices()` triggers creation of missing services and deletion of orphans
   - Track in-flight operations via `pendingUpdaterTriggers` counter

6. **Wait for Completion**
   - `WaitForInitialSync()` blocks until all async operations complete
   - Signals via `initCompletionChecker` channel when done
   - Cleanup orphaned PIPs after sync

7. **Default Outbound Service**
   - `ensureDefaultOutboundServiceExists()` creates default NAT Gateway if needed

### Sequence Diagram

Copy and paste the following into [sequencediagram.org](https://sequencediagram.org):

```
title CCM Initialization (ServiceGatewayEnabled=true)

participant "CCM Main" as CCM
participant "Cloud Provider\n(azure.go)" as Azure
participant "Initialization\n(initialization.go)" as Init
participant "K8s API Server" as K8sAPI
participant "Azure APIs\n(SGW, LB, NAT, PIP)" as AzureAPI
participant "DiffTracker\n+ Engine" as DT
participant "ServiceUpdater\n(goroutine)" as SvcUpdater
participant "LocationsUpdater\n(goroutine)" as LocUpdater

==Phase 1: CCM Startup==

CCM->Azure: InitializeCloudFromConfig(config)
Azure->Azure: Parse config, check ServiceGatewayEnabled=true

==Phase 2: ServiceGateway Setup==

Azure->AzureAPI: existsServiceGateway(resourceName)

alt ServiceGateway doesn't exist
    AzureAPI-->Azure: 404 Not Found
    Azure->AzureAPI: createServiceGateway(resourceName)
    AzureAPI-->Azure: created
else Already exists
    AzureAPI-->Azure: 200 OK
end

Azure->AzureAPI: attachServiceGatewayToSubnet()
AzureAPI-->Azure: subnet updated

==Phase 3: Build K8s State==

Azure->Init: InitializeFromCluster(ctx, config, kubeClient)

Init->K8sAPI: List Services (type=LoadBalancer)
K8sAPI-->Init: services list
note right of Init: Extract serviceUIDs\nBuild serviceUIDToService map

Init->K8sAPI: List EndpointSlices
K8sAPI-->Init: endpointslices list
note right of Init: Map podIP→nodeIP per service

Init->K8sAPI: List Pods (egress label filter)
K8sAPI-->Init: egress pods list
note right of Init: Group by egress gateway name\nBuild pod counters

Init->Init: Construct K8s_State{Services, Egresses, Nodes}

==Phase 4: Build NRP State==

Init->AzureAPI: GetServiceGateway(resourceName)
AzureAPI-->Init: SGW with registered services

Init->AzureAPI: List LoadBalancers
AzureAPI-->Init: existing LBs
note right of Init: Filter LBs owned by this SGW

Init->AzureAPI: List NATGateways
AzureAPI-->Init: existing NATs
note right of Init: Filter NATs owned by this SGW

Init->AzureAPI: GetAddressLocations()
AzureAPI-->Init: current NRP locations

Init->Init: Construct NRP_State{LoadBalancers, NATGateways, Locations}

==Phase 5: Initialize DiffTracker==

Init->DT: Create DiffTracker instance
note right of DT: K8sResources = k8s\nNRPResources = nrp\nisInitializing = true\ninitCompletionChecker = channel

Init->DT: GetSyncOperations()
note right of DT: Compare K8s vs NRP\nservicesToCreate = K8s - NRP\nservicesToDelete = NRP - K8s\nlocationChanges = diff

DT-->Init: SyncOperations{creates, deletes, locationChanges}

==Phase 6: Start Updater Goroutines==

Init->SvcUpdater: Start goroutine (listen on channel)
Init->LocUpdater: Start goroutine (listen on channel)

==Phase 7: Reconcile Services==

loop For each service to create
    Init->DT: AddService(serviceUID, config)
    note right of DT: State = NotStarted\npendingUpdaterTriggers++
    DT->SvcUpdater: trigger
end

loop For each service to delete
    Init->DT: DeleteService(serviceUID)
    note right of DT: State = DeletionPending\nAdd to pendingDeletions
end

==Phase 8: Async Operations Execute==

par ServiceUpdater creates missing services
    SvcUpdater->AzureAPI: Create PIP, LB/NAT, Register
    AzureAPI-->SvcUpdater: created
    SvcUpdater->DT: OnServiceCreationComplete(success)
    note right of DT: State → Created\npendingUpdaterTriggers--\nCheck if init complete
and LocationsUpdater syncs locations
    LocUpdater->AzureAPI: UpdateAddressLocations(diff)
    AzureAPI-->LocUpdater: synced
    LocUpdater->DT: CheckPendingDeletions()
    note right of DT: pendingUpdaterTriggers--\nCheck if init complete
end

==Phase 9: Wait for Completion==

Init->DT: WaitForInitialSync(ctx)
note right of DT: Block on initCompletionChecker channel

DT->DT: checkInitializationComplete()
note right of DT: pendingOps == 0?\npendingUpdaterTriggers == 0?

alt All operations complete
    DT->DT: isInitializing = false
    DT->DT: close(initCompletionChecker)
    DT-->Init: return nil
end

==Phase 10: Cleanup & Finalize==

Init->AzureAPI: cleanupOrphanedPIPs()
note right of Init: Delete PIPs without LB/NAT

Init-->Azure: Return initialized DiffTracker

Azure->DT: ensureDefaultOutboundServiceExists()
note right of DT: Create default NAT if needed

Azure->Azure: az.diffTracker = diffTracker
Azure-->CCM: Cloud initialized

==Runtime==

CCM->CCM: Start informers (Service, EndpointSlice, Pod)
note right of CCM: All events now handled by Engine
```

---

## Azure Resource Relationships

```
┌─────────────────────────────────────────────────────────────────────┐
│                     SERVICE GATEWAY (NRP)                            │
│                                                                      │
│  ┌─────────────────┐    ┌─────────────────┐                        │
│  │    Services     │    │ AddressLocations│                        │
│  ├─────────────────┤    ├─────────────────┤                        │
│  │ - Inbound (LB)  │    │ nodeIP1:        │                        │
│  │   svc-uid-1     │    │   podIP1 → svc1 │                        │
│  │   svc-uid-2     │    │   podIP2 → svc1 │                        │
│  │ - Outbound (NAT)│    │   podIP3 → egr1 │                        │
│  │   my-egress     │    │ nodeIP2:        │                        │
│  │   default-natgw │    │   podIP4 → svc2 │                        │
│  └─────────────────┘    └─────────────────┘                        │
└─────────────────────────────────────────────────────────────────────┘
        │                         │
        │ References              │ References
        ▼                         ▼
┌─────────────────┐    ┌─────────────────┐    ┌─────────────────┐
│  LoadBalancers  │    │  NAT Gateways   │    │   Public IPs    │
├─────────────────┤    ├─────────────────┤    ├─────────────────┤
│ Name: svc-uid-1 │    │ Name: my-egress │    │ svc-uid-1-pip   │
│ SKU: Service    │    │ SKU: StandardV2 │    │ my-egress-pip   │
│ BackendPool     │    │ ServiceGateway  │    │ default-pip     │
└─────────────────┘    └─────────────────┘    └─────────────────┘
```

---

## Key Characteristics

| Aspect | Description |
|--------|-------------|
| **Non-blocking** | `EnsureLoadBalancer()` returns immediately; resource creation is async |
| **Parallel Processing** | ServiceUpdater processes up to 10 operations concurrently |
| **Event Buffering** | EndpointSlice/Pod events buffered until service reaches `StateCreated` |
| **Ordered Deletion** | Locations must be cleared from NRP before service deletion |
| **Pod Egress** | Pods with label `kubernetes.azure.com/service-egress-gateway` trigger NAT Gateway creation |
| **Counter-based Lifecycle** | NAT Gateways deleted when pod counter reaches 0 |
| **Initialization Sync** | `WaitForInitialSync()` blocks until all reconciliation complete |
