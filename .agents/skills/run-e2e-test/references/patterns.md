# E2E Test Pattern → CLI Command Reference

This document maps Go e2e test patterns from `tests/e2e/` to equivalent
`kubectl` and `az` CLI commands.

## Resource Group Discovery
<!-- keywords: resource group, RG, providerID -->

Tests derive the cluster resource group from a node's `spec.providerID`.

```bash
# Extract RG from first node's providerID
RG=$(kubectl get nodes -o jsonpath='{.items[0].spec.providerID}' \
  | sed -n 's|.*[Rr]esource[Gg]roups/\([^/]*\).*|\1|p')

# Extract subscription ID
SUB=$(kubectl get nodes -o jsonpath='{.items[0].spec.providerID}' \
  | sed -n 's|.*[Ss]ubscriptions/\([^/]*\).*|\1|p')
```

## Cluster Location
<!-- keywords: location -->

```bash
LOCATION=$(az group show -n "$RG" --query location -o tsv)
```

## Namespace Operations
<!-- keywords: namespace -->

### Create test namespace
```bash
NS="e2e-test-$(head -c 5 /dev/urandom | xxd -p)"
kubectl create namespace "$NS"
```

### Delete namespace
```bash
kubectl delete namespace "$NS" --wait=true --timeout=5m
```

## Deployment Operations
<!-- keywords: deployment, agnhost, replica -->

### Create agnhost server deployment
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${DEPLOYMENT_NAME}
  namespace: ${NS}
spec:
  replicas: 5
  selector:
    matchLabels:
      app: ${LABEL}
  template:
    metadata:
      labels:
        app: ${LABEL}
    spec:
      nodeSelector:
        kubernetes.io/os: linux
      containers:
      - name: agnhost
        image: registry.k8s.io/e2e-test-images/agnhost:2.36
        args:
        - netexec
        - --http-port=80
        ports:
        - containerPort: 80
          name: http
```

### Wait for pods ready
```bash
kubectl wait --for=condition=ready pod -l "app=${LABEL}" \
  -n "$NS" --timeout=5m
```

## Service Operations
<!-- keywords: service, LoadBalancer, annotation, port -->

### Create LoadBalancer service
```yaml
apiVersion: v1
kind: Service
metadata:
  name: ${SVC_NAME}
  namespace: ${NS}
  annotations:
    ${ANNOTATION_KEY}: "${ANNOTATION_VALUE}"
  labels:
    app: ${LABEL}
spec:
  type: LoadBalancer
  ipFamilyPolicy: PreferDualStack
  selector:
    app: ${LABEL}
  ports:
  - name: http
    port: 80
    targetPort: 80
    protocol: TCP
```

### Common annotations
| Annotation | Example Value | Purpose |
|---|---|---|
| `service.beta.kubernetes.io/azure-load-balancer-internal` | `"true"` | Internal LB |
| `service.beta.kubernetes.io/azure-load-balancer-resource-group` | `"my-rg"` | PIP resource group |
| `service.beta.kubernetes.io/azure-load-balancer-ipv4` | `"1.2.3.4"` | Specify LB IPv4 |
| `service.beta.kubernetes.io/azure-load-balancer-ipv6` | `"fd00::1"` | Specify LB IPv6 |
| `service.beta.kubernetes.io/azure-dns-label-name` | `"my-dns"` | DNS label on PIP |
| `service.beta.kubernetes.io/azure-load-balancer-tcp-idle-timeout` | `"5"` | Idle timeout (min) |
| `service.beta.kubernetes.io/azure-load-balancer-disable-tcp-reset` | `"true"` | Disable TCP reset |
| `service.beta.kubernetes.io/azure-load-balancer-enable-high-availability-ports` | `"true"` | HA ports |
| `service.beta.kubernetes.io/azure-load-balancer-health-probe-protocol` | `"Http"` | Probe protocol |
| `service.beta.kubernetes.io/azure-load-balancer-health-probe-request-path` | `"/healthz"` | Probe path |
| `service.beta.kubernetes.io/azure-load-balancer-health-probe-interval` | `"10"` | Probe interval (s) |
| `service.beta.kubernetes.io/azure-load-balancer-health-probe-num-of-probe` | `"3"` | Probe threshold |
| `service.beta.kubernetes.io/azure-pip-name` | `"my-pip"` | Specify PIP name |
| `service.beta.kubernetes.io/azure-pip-prefix-id` | `/subscriptions/.../publicIPPrefixes/pfx` | PIP prefix |
| `service.beta.kubernetes.io/azure-load-balancer-mixed-protocols` | `"true"` | Mixed TCP/UDP |
| `service.beta.kubernetes.io/azure-shared-securityrule` | `"true"` | Shared NSG rule |
| `service.beta.kubernetes.io/azure-pls-create` | `"true"` | Create Private Link Service |
| `service.beta.kubernetes.io/azure-pls-name` | `"my-pls"` | PLS name |

### Wait for service external IP
```bash
# Poll every 10s for up to 5 minutes (Standard LB) or 10 minutes (Basic LB)
for i in $(seq 1 30); do
  IP=$(kubectl get svc "$SVC_NAME" -n "$NS" \
    -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
  if [ -n "$IP" ]; then
    echo "Service IP: $IP"
    break
  fi
  echo "Waiting for external IP... ($((i*10))s)"
  sleep 10
done
```

### Update service (with conflict retry)
```bash
# Get current, edit, apply — kubectl handles conflict retry
kubectl get svc "$SVC_NAME" -n "$NS" -o yaml > /tmp/svc.yaml
# Edit /tmp/svc.yaml as needed
kubectl apply -f /tmp/svc.yaml
```

### Delete service
```bash
kubectl delete svc "$SVC_NAME" -n "$NS" --wait=true --timeout=5m
```

## Public IP Operations
<!-- keywords: PIP, public IP, IP prefix -->

### Create PIP
```bash
az network public-ip create \
  --name "$PIP_NAME" \
  --resource-group "$RG" \
  --location "$LOCATION" \
  --sku Standard \
  --allocation-method Static \
  --version IPv4 \
  --tags foo=bar
```

### Wait for PIP to get an IP
```bash
for i in $(seq 1 30); do
  ADDR=$(az network public-ip show -n "$PIP_NAME" -g "$RG" \
    --query ipAddress -o tsv)
  if [ -n "$ADDR" ] && [ "$ADDR" != "null" ]; then
    echo "PIP address: $ADDR"
    break
  fi
  sleep 10
done
```

### Get PIP by address
```bash
az network public-ip list -g "$RG" \
  --query "[?ipAddress=='$TARGET_IP']" -o json
```

### Delete PIP
```bash
az network public-ip delete -n "$PIP_NAME" -g "$RG"
```

### Create PIP prefix
```bash
az network public-ip prefix create \
  --name "$PREFIX_NAME" \
  --resource-group "$RG" \
  --location "$LOCATION" \
  --sku Standard \
  --length 28 \
  --version IPv4
```

### Delete PIP prefix
```bash
az network public-ip prefix delete -n "$PREFIX_NAME" -g "$RG"
```

## Load Balancer Verification
<!-- keywords: load balancer, LB, rule, probe, backend pool, backend -->

### List LBs
```bash
az network lb list -g "$RG" -o json
```

### Get LB details
```bash
az network lb show -g "$RG" -n "$LB_NAME" -o json
```

### Find LB by service public IP
```bash
# List LBs, find the one whose frontend IP config references the PIP
az network lb list -g "$RG" \
  --query "[].{name:name, frontendIPs:properties.frontendIPConfigurations[].properties.publicIPAddress.id}" \
  -o json
```

### Check LB rules
```bash
az network lb rule list -g "$RG" --lb-name "$LB_NAME" -o json
```

### Check LB probes
```bash
az network lb probe list -g "$RG" --lb-name "$LB_NAME" -o json
```

### Check backend pool
```bash
az network lb address-pool list -g "$RG" --lb-name "$LB_NAME" -o json
```

## NSG Verification
<!-- keywords: NSG, security group, security rule -->

### List NSGs
```bash
az network nsg list -g "$RG" -o json
```

### Check NSG rules
```bash
az network nsg rule list -g "$RG" --nsg-name "$NSG_NAME" -o json
```

## VNet / Subnet Operations
<!-- keywords: VNet, subnet, IP availability -->

### Get cluster VNet
```bash
az network vnet list -g "$RG" -o json
```

### Check IP availability
```bash
az network vnet check-ip-address -g "$RG" -n "$VNET_NAME" \
  --ip-address "$IP_ADDR"
```

### Create subnet
```bash
az network vnet subnet create \
  --name "$SUBNET_NAME" \
  --vnet-name "$VNET_NAME" \
  --resource-group "$RG" \
  --address-prefix "$CIDR"
```

### Delete subnet
```bash
az network vnet subnet delete \
  --name "$SUBNET_NAME" \
  --vnet-name "$VNET_NAME" \
  --resource-group "$RG"
```

## Private Link Service
<!-- keywords: PLS, private link -->

### Get PLS
```bash
az network private-link-service show -g "$RG" -n "$PLS_NAME" -o json
```

### List PLS
```bash
az network private-link-service list -g "$RG" -o json
```

## Connectivity Testing
<!-- keywords: connectivity, nc, exec-agnhost, TCP, UDP -->

### Create host-exec pod (for nc connectivity checks)
```yaml
apiVersion: v1
kind: Pod
metadata:
  name: exec-agnhost
  namespace: ${NS}
spec:
  hostNetwork: true
  terminationGracePeriodSeconds: 0
  nodeSelector:
    kubernetes.io/os: linux
  tolerations:
  - key: node-role.kubernetes.io/control-plane
    operator: Exists
    effect: NoSchedule
  - key: node-role.kubernetes.io/master
    operator: Exists
    effect: NoSchedule
  containers:
  - name: agnhost
    image: registry.k8s.io/e2e-test-images/agnhost:2.36
    args: ["pause"]
    imagePullPolicy: IfNotPresent
```

### TCP connectivity check
```bash
# Poll every 10s for up to 10 minutes
for i in $(seq 1 60); do
  RESULT=$(kubectl exec exec-agnhost -n "$NS" -- \
    nc -vz -w 4 "$SVC_IP" "$SVC_PORT" 2>&1) || true
  if echo "$RESULT" | grep -q "succeeded"; then
    echo "PASS: TCP connectivity to $SVC_IP:$SVC_PORT"
    break
  fi
  echo "Waiting for connectivity... ($((i*10))s)"
  sleep 10
done
```

### UDP connectivity check
```bash
kubectl exec exec-agnhost -n "$NS" -- \
  nc -vz -w 4 -u "$SVC_IP" "$SVC_PORT"
```

## ETag / No-op Verification
<!-- keywords: ETag, no-op, etag -->

### Get resource ETags
```bash
LB_ETAG=$(az network lb show -g "$RG" -n "$LB_NAME" --query etag -o tsv)
NSG_ETAG=$(az network nsg show -g "$RG" -n "$NSG_NAME" --query etag -o tsv)
PIP_ETAG=$(az network public-ip show -g "$RG" -n "$PIP_NAME" --query etag -o tsv)
```

### Verify no-op (ETags unchanged after annotation update)
```bash
# After updating a dummy annotation and re-waiting:
NEW_LB_ETAG=$(az network lb show -g "$RG" -n "$LB_NAME" --query etag -o tsv)
[ "$LB_ETAG" = "$NEW_LB_ETAG" ] && echo "PASS: LB etag unchanged" || echo "FAIL: LB was reconciled"
```

## VMSS Operations
<!-- keywords: VMSS, scale set -->

### List VMSS
```bash
az vmss list -g "$RG" -o json
```

### List VMSS VMs
```bash
az vmss list-instances -g "$RG" -n "$VMSS_NAME" -o json
```

## Skip Conditions
<!-- keywords: skip, skip condition -->

Tests check these conditions and skip if not met:

| Condition Check | CLI Equivalent |
|---|---|
| Standard LB only | Check `AZURE_LOADBALANCER_SKU` env or cluster config |
| AKS cluster | Check `E2E_ON_AKS_CLUSTER` env |
| CAPZ cluster | Check `TEST_CCM` env |
| Multi-SLB | Check `TEST_MULTI_SLB` env |
| VMSS cluster | `kubectl get nodes -o json` → check providerID contains "virtualMachineScaleSets" |
