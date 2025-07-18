# Node Filtering in Cloud Controller Manager

This document explains how to configure the Azure Cloud Controller Manager to only manage specific nodes while allowing the Cloud Node Manager to continue managing all nodes.

## Overview

The node filtering functionality provides two independent features:

1. **Node Exclude Labels**: Always skip managing nodes with specific labels (works regardless of other settings)
2. **Selective Node Management**: When enabled, only manage nodes that match a specific label selector

These features work together to provide flexible node management control.

## Configuration

### Command-Line Arguments

The node filtering feature provides two independent command-line arguments:

```bash
# Feature 1: Skip nodes with specific labels (always active when set)
--node-exclude-labels="kubernetes.azure.com/managed=true"

# Feature 2: Only manage nodes with specific labels (requires --enable-node-filtering)
--enable-node-filtering
--node-label-selector="kubernetes.azure.com/managed=true"
```

### How the Features Work

1. **`--node-exclude-labels`**: 
   - Always active when set (no need for `--enable-node-filtering`)
   - Exclude managing nodes with these labels (comma-separated)
   - Example: `--node-exclude-labels="kubernetes.azure.com/managed=true"`

2. **`--enable-node-filtering` + `--node-label-selector`**:
   - Only active when `--enable-node-filtering` is set to true
   - Only manage nodes with these labels
   - Example: `--node-label-selector="kubernetes.azure.com/managed=true"` ### Example Use Cases

#### Use Case 1: Exclude nodes with specific labels
```bash
# Skip managing nodes with 'kubernetes.azure.com/managed=true' label
cloud-controller-manager \
  --node-exclude-labels="kubernetes.azure.com/managed=true"
```

#### Use Case 2: Only manage specific nodes
```bash
# Only manage nodes with 'kubernetes.azure.com/managed=true' label
cloud-controller-manager \
  --enable-node-filtering \
  --node-label-selector="kubernetes.azure.com/managed=true"
```

#### Use Case 3: Combine both features
```bash
# Only manage nodes with 'environment=production' label, but exclude maintenance nodes
cloud-controller-manager \
  --enable-node-filtering \
  --node-label-selector="environment=production" \
  --node-exclude-labels="maintenance=true"
```

#### Use Case 4: Multiple exclude labels
```bash
# Skip nodes with any of these labels
cloud-controller-manager \
  --node-exclude-labels="maintenance=true,external.io/managed=true,node.kubernetes.io/exclude-from-external-load-balancers=true"
```

## How It Works

The node filtering system provides two independent features:

### Feature 1: Node Exclude Labels (`--node-exclude-labels`)
- **Always active** when the flag is set (no need for `--enable-node-filtering`)
- Exclude managing nodes with these labels (comma-separated)
- Works by adding exclusion requirements to the label selector
- Example: `--node-exclude-labels="kubernetes.azure.com/managed=true"` will skip nodes with this exact label and value

### Feature 2: Selective Node Management (`--enable-node-filtering` + `--node-label-selector`)
- **Only active** when `--enable-node-filtering` is set to true
- Only manage nodes with these labels
- Uses standard Kubernetes label selector syntax
- Example: `--node-label-selector="kubernetes.azure.com/managed=true"` will only manage nodes with this exact label and value

### Combined Behavior
When both features are used together:
1. First, the system applies the label selector (if `--enable-node-filtering` is true)
2. Then, it excludes nodes with any of the exclude labels
3. The result is nodes that match the selector AND don't have any exclude labels

### Technical Implementation
- Both features create filtered informers for `CloudNodeController` and `CloudNodeLifecycleController`
- The filtering happens at the informer level, so controllers never see filtered-out nodes
- The Cloud Node Manager continues to manage all nodes regardless of CCM filtering

## Technical Details

### Label Selector Syntax
The `--node-label-selector` flag accepts standard Kubernetes label selector syntax:
- `key=value` - Exact match
- `key!=value` - Not equal
- `key in (value1,value2)` - In set
- `key notin (value1,value2)` - Not in set
- `key` - Key exists
- `!key` - Key does not exist

### Exclude Labels Behavior
The `--node-exclude-labels` flag accepts a comma-separated list of labels in `key=value` format:
- Nodes are excluded if they have ANY of the specified labels with matching values
- Example: `--node-exclude-labels="kubernetes.azure.com/managed=true,maintenance=true"`
  - Excludes nodes with `kubernetes.azure.com/managed: "true"` 
  - Excludes nodes with `maintenance: "true"`
  - Does NOT exclude nodes with `kubernetes.azure.com/managed: "false"`

## Default Behavior

- **Without any flags**: The Cloud Controller Manager manages all nodes as before
- **With only `--node-exclude-labels`**: The CCM manages all nodes EXCEPT those with the specified labels
- **With only `--enable-node-filtering` and `--node-label-selector`**: The CCM only manages nodes that match the selector
- **With both features**: The CCM only manages nodes that match the selector AND don't have any exclude labels

## Troubleshooting

### Verify Filtering is Active
Check the CCM logs for messages like:
```
"Node filtering enabled for CloudNodeController (EnableNodeFiltering: true, NodeExcludeLabels: kubernetes.azure.com/managed=true)"
"Node filtering enabled: only managing nodes with selector: kubernetes.azure.com/managed=true"
"Node exclude labels: skipping nodes with labels: kubernetes.azure.com/managed=true"
"Creating filtered node informer with label selector: kubernetes.azure.com/managed=true"
```

### Debug Node Selection
Use kubectl to verify which nodes match your criteria:

```bash
# Test your label selector
kubectl get nodes -l "kubernetes.azure.com/managed=true"

# Test exclude labels (nodes that would be excluded)
kubectl get nodes -l "kubernetes.azure.com/managed=true" -o name

# Test nodes that would be managed (if using selective filtering)
kubectl get nodes -l "kubernetes.azure.com/managed=true"
```

## Migration Guide

If you're migrating from environment variable configuration to command-line arguments, use the following mapping:
- Environment variable approach → Command-line argument approach
- `export ENABLE_NODE_FILTERING=true` → `--enable-node-filtering`
- `export NODE_LABEL_SELECTOR=key=value` → `--node-label-selector=key=value`
- `export NODE_EXCLUDE_LABELS=key1=value1,key2=value2` → `--node-exclude-labels=key1=value1,key2=value2`

## Deployment Examples

### Helm Chart Values

```yaml
cloudControllerManager:
  enabled: true
  # ... other configurations ...
  args:
    - --enable-node-filtering
    - --node-label-selector=kubernetes.azure.com/managed=true
    - --node-exclude-labels=maintenance=true

cloudNodeManager:
  enabled: true
  # ... continues to manage all nodes ...
```

### Kubernetes Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cloud-controller-manager
spec:
  template:
    spec:
      containers:
      - name: cloud-controller-manager
        args:
        - --enable-node-filtering
        - --node-label-selector=kubernetes.azure.com/managed=true
        - --node-exclude-labels=maintenance=true
        # ... other args and configuration ...
```

## Verification

Check the logs to verify filtering is working:

```bash
kubectl logs -n kube-system deployment/cloud-controller-manager | grep "Node filtering"
```

You should see logs like:
```
I0717 12:00:00.000000       1 core.go:XXX] Node filtering enabled for CloudNodeController (EnableNodeFiltering: true, NodeExcludeLabels: maintenance=true)
I0717 12:00:00.000000       1 core.go:XXX] Node filtering enabled: only managing nodes with selector: kubernetes.azure.com/managed=true
I0717 12:00:00.000000       1 core.go:XXX] Node exclude labels: excluding nodes with labels: maintenance=true
```

## Benefits

1. **Selective Management**: CCM only manages specific nodes you designate
2. **Flexible Exclusion**: Skip managing nodes based on specific label conditions
3. **Hybrid Scenarios**: Mix CCM-managed and externally-managed nodes in the same cluster
4. **Gradual Migration**: Gradually migrate nodes from external management to CCM
5. **Conflict Avoidance**: Prevent CCM from interfering with nodes managed by other systems
6. **Resource Efficiency**: Reduced resource usage as CCM only processes relevant nodes

## Important Notes

- Node filtering only affects the Cloud Controller Manager, not the Cloud Node Manager
- The Cloud Node Manager continues to run on all nodes and manage them individually
- Ensure nodes are properly labeled before enabling filtering
- Test thoroughly in non-production environments first
- The exclude labels feature works independently of the selective filtering feature
