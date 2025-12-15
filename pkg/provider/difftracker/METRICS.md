# CLB DiffTracker Engine Metrics

This document describes the observability metrics exposed by the CLB DiffTracker Engine for production monitoring and debugging.

## Overview

The DiffTracker Engine exposes comprehensive Prometheus-style metrics via the Kubernetes component-base metrics framework. All metrics are prefixed with `difftracker_engine_` and registered in the legacy Prometheus registry for automatic scraping.

## Metric Types

### 1. Engine Lifecycle Metrics

#### `difftracker_engine_operation_duration_seconds`
- **Type**: HistogramVec
- **Labels**: `operation`, `result`
- **Description**: Duration of Engine API operations (AddService, DeleteService, UpdateEndpoints, OnServiceCreationComplete, CheckPendingDeletions)
- **Buckets**: 0.001s to 5.0s
- **Usage**: Track API latency and identify slow operations

**Example Query**:
```promql
# 99th percentile latency for AddService operations
histogram_quantile(0.99, rate(difftracker_engine_operation_duration_seconds_bucket{operation="AddService"}[5m]))

# Error rate for all operations
sum(rate(difftracker_engine_operation_duration_seconds_count{result="error"}[5m])) by (operation)
```

---

### 2. Service State Tracking Metrics

#### `difftracker_engine_pending_service_operations`
- **Type**: GaugeVec
- **Labels**: `state`, `service_type`
- **Description**: Number of service operations by state (not_started, creation_in_progress, created, deletion_pending, deletion_in_progress)
- **States**:
  - `not_started`: Services queued for creation
  - `creation_in_progress`: Services currently being created
  - `created`: Services successfully created
  - `deletion_pending`: Services waiting for location cleanup before deletion
  - `deletion_in_progress`: Services currently being deleted
- **Service Types**: `inbound`, `outbound`

**Example Query**:
```promql
# Services stuck in creation for extended periods
difftracker_engine_pending_service_operations{state="creation_in_progress"}

# Total pending operations
sum(difftracker_engine_pending_service_operations)
```

#### `difftracker_engine_buffered_updates`
- **Type**: GaugeVec
- **Labels**: `update_type`
- **Description**: Number of buffered updates waiting for service creation to complete
- **Update Types**: `endpoints`, `pods`

**Example Query**:
```promql
# Buffered endpoint updates
difftracker_engine_buffered_updates{update_type="endpoints"}
```

#### `difftracker_engine_tracked_services_total`
- **Type**: GaugeVec
- **Labels**: `state`
- **Description**: Total number of services tracked in various states
- **States**: `k8s_services`, `nrp_loadbalancers`, `nrp_natgateways`

**Example Query**:
```promql
# Total K8s services tracked
difftracker_engine_tracked_services_total{state="k8s_services"}
```

---

### 3. Service Creation/Deletion Metrics

#### `difftracker_engine_service_operations_total`
- **Type**: CounterVec
- **Labels**: `operation`, `service_type`, `result`
- **Description**: Total count of service creation/deletion operations
- **Operations**: `create`, `delete`
- **Service Types**: `inbound`, `outbound`
- **Results**: `success`, `error`

**Example Query**:
```promql
# Creation success rate
rate(difftracker_engine_service_operations_total{operation="create", result="success"}[5m]) / 
rate(difftracker_engine_service_operations_total{operation="create"}[5m])

# Total deletions per minute
rate(difftracker_engine_service_operations_total{operation="delete"}[1m]) * 60
```

#### `difftracker_engine_service_operation_duration_seconds`
- **Type**: HistogramVec
- **Labels**: `operation`, `service_type`
- **Description**: Duration of service creation/deletion operations
- **Buckets**: 0.1s to 120s
- **Usage**: Track NRP API call latency for creates/deletes

**Example Query**:
```promql
# Median deletion latency
histogram_quantile(0.5, rate(difftracker_engine_service_operation_duration_seconds_bucket{operation="delete"}[5m]))
```

#### `difftracker_engine_service_operation_retries`
- **Type**: HistogramVec
- **Labels**: `operation`, `service_type`
- **Description**: Number of retries for failed service operations
- **Buckets**: 0 to 20
- **Usage**: Detect operations requiring multiple retries (potential NRP issues)

**Example Query**:
```promql
# Services requiring >3 retries
histogram_quantile(0.95, rate(difftracker_engine_service_operation_retries_bucket{operation="create"}[5m]))
```

---

### 4. Deletion Workflow Metrics

#### `difftracker_engine_pending_deletions`
- **Type**: Gauge
- **Description**: Number of services pending deletion waiting for location cleanup

**Example Query**:
```promql
# Alert if deletions are stuck
difftracker_engine_pending_deletions > 10
```

#### `difftracker_engine_deletion_check_duration_seconds`
- **Type**: Histogram
- **Description**: Duration of CheckPendingDeletions runs
- **Buckets**: 0.001s to 0.5s
- **Usage**: Ensure deletion checker runs efficiently

**Example Query**:
```promql
# 99th percentile deletion check latency
histogram_quantile(0.99, rate(difftracker_engine_deletion_check_duration_seconds_bucket[5m]))
```

#### `difftracker_engine_services_blocked_by_locations`
- **Type**: Gauge
- **Description**: Number of services blocked from deletion due to active locations in NRP

**Example Query**:
```promql
# Alert if many services are blocked
difftracker_engine_services_blocked_by_locations > 5
```

---

### 5. ServiceUpdater Metrics

#### `difftracker_engine_service_updater_concurrent_operations`
- **Type**: Gauge
- **Description**: Current number of concurrent operations in ServiceUpdater (max 10 due to semaphore)

**Example Query**:
```promql
# Semaphore saturation
difftracker_engine_service_updater_concurrent_operations / 10
```

#### `difftracker_engine_service_updater_batch_size`
- **Type**: Histogram
- **Description**: Number of services processed per ServiceUpdater batch
- **Buckets**: 0 to 100
- **Usage**: Understand batch processing patterns

**Example Query**:
```promql
# Average batch size
rate(difftracker_engine_service_updater_batch_size_sum[5m]) / 
rate(difftracker_engine_service_updater_batch_size_count[5m])
```

---

### 6. LocationsUpdater Metrics

#### `difftracker_engine_locations_update_duration_seconds`
- **Type**: HistogramVec
- **Labels**: `result`
- **Description**: Duration of LocationsUpdater sync operations to NRP Service Gateway
- **Buckets**: 0.01s to 5.0s
- **Results**: `success`, `error`

**Example Query**:
```promql
# Location update success rate
rate(difftracker_engine_locations_update_duration_seconds_count{result="success"}[5m]) / 
rate(difftracker_engine_locations_update_duration_seconds_count[5m])
```

#### `difftracker_engine_locations_total`
- **Type**: Gauge
- **Description**: Total number of locations tracked in NRP Service Gateway

**Example Query**:
```promql
# Alert if location count is abnormally high
difftracker_engine_locations_total > 1000
```

#### `difftracker_engine_addresses_total`
- **Type**: Gauge
- **Description**: Total number of addresses tracked across all locations

**Example Query**:
```promql
# Address churn rate
rate(difftracker_engine_addresses_total[5m])
```

---

### 7. State Transition Metrics

#### `difftracker_engine_state_transitions_total`
- **Type**: CounterVec
- **Labels**: `from_state`, `to_state`
- **Description**: Total number of service state transitions
- **Transitions**:
  - `not_started` → `creation_in_progress`
  - `creation_in_progress` → `created`
  - `creation_in_progress` → `not_started` (retry)
  - `deletion_pending` → `deletion_in_progress`

**Example Query**:
```promql
# Creation retry rate
rate(difftracker_engine_state_transitions_total{from_state="creation_in_progress", to_state="not_started"}[5m])

# Total state transitions per minute
sum(rate(difftracker_engine_state_transitions_total[1m])) * 60
```

---

## Dashboard Recommendations

### Key Performance Indicators (KPIs)

1. **Operation Latency**: 99th percentile of `difftracker_engine_operation_duration_seconds`
2. **Error Rate**: Percentage of operations with `result="error"`
3. **Pending Operations**: `difftracker_engine_pending_service_operations` by state
4. **Retry Rate**: Percentage of operations requiring retries
5. **Blocked Deletions**: `difftracker_engine_services_blocked_by_locations`

### Suggested Grafana Panels

**Panel 1: Engine Operation Latency (Heatmap)**
```promql
sum(rate(difftracker_engine_operation_duration_seconds_bucket[5m])) by (le, operation)
```

**Panel 2: Service State Distribution (Stacked Area)**
```promql
difftracker_engine_pending_service_operations
```

**Panel 3: Operation Success Rate (Gauge)**
```promql
sum(rate(difftracker_engine_service_operations_total{result="success"}[5m])) / 
sum(rate(difftracker_engine_service_operations_total[5m]))
```

**Panel 4: ServiceUpdater Concurrency (Time Series)**
```promql
difftracker_engine_service_updater_concurrent_operations
```

**Panel 5: Location Sync Health (Single Stat)**
```promql
rate(difftracker_engine_locations_update_duration_seconds_count{result="success"}[5m]) / 
rate(difftracker_engine_locations_update_duration_seconds_count[5m])
```

---

## Alerting Rules

### Critical Alerts

**High Error Rate**
```yaml
- alert: DiffTrackerHighErrorRate
  expr: |
    rate(difftracker_engine_service_operations_total{result="error"}[5m]) / 
    rate(difftracker_engine_service_operations_total[5m]) > 0.1
  for: 5m
  annotations:
    summary: "DiffTracker error rate exceeds 10%"
```

**Services Stuck in Creation**
```yaml
- alert: DiffTrackerServicesStuck
  expr: difftracker_engine_pending_service_operations{state="creation_in_progress"} > 50
  for: 10m
  annotations:
    summary: "More than 50 services stuck in creation for 10+ minutes"
```

**Deletions Blocked by Locations**
```yaml
- alert: DiffTrackerDeletionsBlocked
  expr: difftracker_engine_services_blocked_by_locations > 10
  for: 15m
  annotations:
    summary: "More than 10 services blocked from deletion due to locations"
```

### Warning Alerts

**High Retry Rate**
```yaml
- alert: DiffTrackerHighRetryRate
  expr: |
    histogram_quantile(0.9, rate(difftracker_engine_service_operation_retries_bucket[5m])) > 3
  for: 10m
  annotations:
    summary: "90% of operations require >3 retries"
```

**Semaphore Saturation**
```yaml
- alert: DiffTrackerSemaphoreSaturated
  expr: difftracker_engine_service_updater_concurrent_operations >= 10
  for: 5m
  annotations:
    summary: "ServiceUpdater semaphore saturated (10/10 slots in use)"
```

---

## Debugging Scenarios

### Scenario 1: Slow Service Creation
**Symptoms**: High latency reported by users

**Queries**:
```promql
# Check operation duration
histogram_quantile(0.99, rate(difftracker_engine_operation_duration_seconds_bucket{operation="AddService"}[5m]))

# Check if NRP API is slow
histogram_quantile(0.99, rate(difftracker_engine_service_operation_duration_seconds_bucket{operation="create"}[5m]))

# Check if services are queuing
difftracker_engine_pending_service_operations{state="not_started"}
```

### Scenario 2: Deletions Not Completing
**Symptoms**: Services remain in LoadBalancer state after deletion

**Queries**:
```promql
# Check pending deletions
difftracker_engine_pending_deletions

# Check blocked deletions
difftracker_engine_services_blocked_by_locations

# Check location count
difftracker_engine_locations_total
```

### Scenario 3: High Retry Rate
**Symptoms**: Operations failing intermittently

**Queries**:
```promql
# Check retry histogram
rate(difftracker_engine_service_operation_retries_bucket[5m])

# Check error rate
rate(difftracker_engine_service_operations_total{result="error"}[5m])

# Check state transition failures
rate(difftracker_engine_state_transitions_total{from_state="creation_in_progress", to_state="not_started"}[5m])
```

---

## Metric Collection

Metrics are automatically registered with the Kubernetes legacy Prometheus registry during package initialization. They are scraped via the standard `/metrics` endpoint on the cloud-controller-manager.

**Scrape Configuration**:
```yaml
scrape_configs:
  - job_name: 'cloud-controller-manager'
    kubernetes_sd_configs:
      - role: pod
        namespaces:
          names:
            - kube-system
    relabel_configs:
      - source_labels: [__meta_kubernetes_pod_label_component]
        regex: cloud-controller-manager
        action: keep
```

---

## Performance Impact

All metrics use efficient collection methods:
- **Histograms**: Pre-allocated buckets with O(1) observation
- **Gauges**: Direct value updates with no aggregation overhead
- **Counters**: Atomic increment operations

**Expected overhead**: <1% CPU, <10MB memory per 10,000 operations/minute.

---

## Maintenance

### Adding New Metrics

1. Define metric variable in `metrics.go` `var()` block
2. Register in `registerMetrics()` function
3. Create helper function (e.g., `recordXXXOperation()`)
4. Add metric calls at instrumentation points
5. Update this document with metric description and queries

### Deprecating Metrics

Follow Kubernetes deprecation policy:
1. Mark metric as deprecated in help text
2. Wait 2 releases before removal
3. Provide migration path in release notes
