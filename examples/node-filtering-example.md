# Node Filtering in Cloud Controller Manager

This document explains how to configure the Azure Cloud Controller Manager to only manage specific nodes based on label selectors.

## Overview

The node filtering feature allows the Azure Cloud Controller Manager (CCM) to selectively manage nodes using Kubernetes label selector expressions. This enables scenarios where some nodes should be managed by CCM while others are handled by different systems or during maintenance.

## Configuration

### Command-Line Flag

```bash
--node-filter-requirements="<kubernetes-label-selector>"
```

The flag accepts standard Kubernetes label selector syntax. For complete syntax reference, see the [official Kubernetes documentation](https://pkg.go.dev/k8s.io/apimachinery/pkg/labels#Parser).

### Examples

#### Basic Examples
```bash
# Only manage nodes with specific label
--node-filter-requirements="environment=production"

# Exclude nodes in maintenance
--node-filter-requirements="!maintenance"

# Multiple conditions (AND)
--node-filter-requirements="environment=production,tier=frontend"
```

#### Advanced Examples
```bash
# Set-based matching
--node-filter-requirements="environment in (production,staging)"

# Complex combinations
--node-filter-requirements="app=web,environment in (prod,staging),!maintenance"
```

## How It Works

When `--node-filter-requirements` is specified:

1. **CCM creates filtered informers** that only watch nodes matching the selector
2. **All CCM controllers** operate only on the filtered subset of nodes:
   - Cloud Node Controller - Only initializes filtered nodes
   - Cloud Node Lifecycle Controller - Only monitors/deletes filtered nodes  
   - Service Controller - Only includes filtered nodes in load balancer backends
   - Route Controller - Only manages routes for filtered nodes
   - Node IPAM Controller - Only allocates IP ranges for filtered nodes
3. **Cloud Node Manager continues to run** on all nodes as daemonset and manages them individually

## Deployment Example

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cloud-controller-manager
  namespace: kube-system
spec:
  template:
    spec:
      containers:
      - name: cloud-controller-manager
        image: mcr.microsoft.com/oss/kubernetes/azure-cloud-controller-manager:latest
        command:
        - cloud-controller-manager
        - --node-filter-requirements=environment=production,!maintenance
        - --cloud-provider=azure
        - --cloud-config=/etc/kubernetes/azure.json
```

## Testing Your Configuration

Before applying the filter, test your selector syntax:

```bash
# Verify which nodes match your selector
kubectl get nodes -l "environment=production,!maintenance"

# Check node labels
kubectl get nodes --show-labels
```

## Use Cases

- **Mixed environments**: Some nodes managed by CCM, others by external systems
- **Maintenance windows**: Temporarily exclude nodes from CCM management  
- **Gradual migration**: Incrementally move nodes to CCM management
- **Testing**: Isolate test nodes from production CCM operations

## Important Notes

- **Label nodes appropriately** before enabling filtering
- **Cloud Node Manager** should continue running on all nodes for full functionality
- **Test selectors** with `kubectl get nodes -l` before deployment
- **Empty selector** (default) means CCM manages all nodes

For complete label selector syntax and capabilities, refer to the [Kubernetes label selector documentation](https://pkg.go.dev/k8s.io/apimachinery/pkg/labels#Parser).