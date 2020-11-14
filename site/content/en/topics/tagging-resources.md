---
title: "Tagging resources managed by Cloud Provider Azure"
linkTitle: "Tagging resources managed by Cloud Provider Azure"
weight: 7
type: docs
---

We could use tags to organize your Azure resources and management hierarchy. Cloud Provider Azure supports tagging managed resource through configuration file or service annotation.

Specifically, the shared resources (load balancer, route table and security group) could be tagged by setting `tags` in `azure.json`:

```json
{
"tags": "a=b,c=d",
}
```

the controller manager would parse this configuration and tag the shared resources once restarted.

The non-shared resource (public IP) could be tagged by setting `tags` in `azure.json` or service annotation `service.beta.kubernetes.io/azure-pip-tags`. The format of the two is similiar and the tags in the annotation would be considered first when there are conflicts between the configuration file and the annotation.

When the configuration, file or annotation, is updated, the old ones would be updated if there are conflicts. For example, after updating `{"tags": "a=b,c=d"}` to `{"tags": "a=c,e=f"}`, the new tags would be `a=c,c=d,e=f`.