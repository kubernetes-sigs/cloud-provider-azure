---
title: "Basic to Standard Load Balancer Migration"
linkTitle: "Basic to Standard Load Balancer Migration"
type: docs
description: >
    Support migration from Basic Load Balancer to Standard Load Balancer.
---

## Background

On September 30, 2025, Basic Load Balancer will be [retired](https://learn.microsoft.com/en-us/azure/load-balancer/skus). This design document describes the planned migration path from Basic Load Balancer to Standard Load Balancer.

## Design

> The following `User` stands for human users, or any cluster provisioning tools (e.g., AKS).

1. User must change the Load Balancer sku manually in the configuration file from `Basic` to `Standard`.

2. If the basic Load Balancer is not removed, it will be deleted during the migration.

3. All IPs of existing Load Balancer typed services will remain unchanged after migration, but the public IP sku will be changed to `Standard`.

4. All nodes will join the new Standard Load Balancer backend pool after migration.

5. User must manually configure the Standard Load Balancer for outbound traffic after migration.

6. User must manually restart the cloud-controller-manager to trigger the migration process.

## Workflow

> This proposed migration process may introduce downtime.

1. Introduce a new function that runs 1 time per pod restart to decouple the nodes and remove the basic Load Balancer if needed.

2. Check the service ingress IP address, if it is an internal service, create a corresponding frontend IP configuration with the IP address in `reconcileFrontendIPConfigs`.

3. If it is an external service, link the public IP ID to the frontend IP configuration, and update the public IP sku to `Standard` in `ensurePublicIPExists`.

4. Could keep other code path unchanged (to be validated).
