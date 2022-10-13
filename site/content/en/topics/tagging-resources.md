---
title: "Tagging resources managed by Cloud Provider Azure"
linkTitle: "Tagging resources managed by Cloud Provider Azure"
weight: 7
type: docs
---

> This feature is supported since v1.20.0.

We could use tags to organize your Azure resources and management hierarchy. Cloud Provider Azure supports tagging managed resource through configuration file or service annotation.

Specifically, the shared resources (load balancer, route table and security group) could be tagged by setting `tags` in `azure.json`:

```json
{
  "tags": "a=b,c=d"
}
```

the controller manager would parse this configuration and tag the shared resources once restarted.

The non-shared resource (public IP) could be tagged by setting `tags` in `azure.json` or service annotation `service.beta.kubernetes.io/azure-pip-tags`. The format of the two is similar and the tags in the annotation would be considered first when there are conflicts between the configuration file and the annotation.

> The annotation `service.beta.kubernetes.io/azure-pip-tags` only works for managed public IPs. For BYO public IPs, the cloud provider would not apply any tags to them.

When the configuration, file or annotation, is updated, the old ones would be updated if there are conflicts. For example, after updating `{"tags": "a=b,c=d"}` to `{"tags": "a=c,e=f"}`, the new tags would be `a=c,c=d,e=f`.

## Integrating with system tags

> This feature is supported since v1.21.0.

Normally the controller manager don't delete the existing tags even if they are not included in the new version of azure configuration files, because the controller manager doesn't know which tags should be deleted and which should not (e.g., tags managed by cloud provider itself). We can leverage the config `systemTags` in the cloud configuration file to control what tags can be deleted. Here are the examples:

| Tags | SystemTags | existing tags on resources | new tags on resources |
| ----- | ------------ | ----- | ----- |
| "a=b,c=d" | "" | {} | {"a": "b", "c": "d"} |
| "a=b,c=d" | "" | {"a": "x", "c": "y"} | {"a": "b", "c": "d"} |
| "a=b,c=d" | "" | {"e": "f"} | {"a": "b", "c": "d", "e": "f"} /* won't delete `e` because the SystemTags is empty */ |
| "c=d" | "a" | {"a": "b"} | {"a": "b", "c": "d"} /* won't delete `a` because it's in the SystemTags */ |
| "c=d" | "x" | {"a": "b"} | {"c": "d"} /* will delete `a` because it's not in Tags or SystemTags */ |

> Please consider migrating existing "tags" to "tagsMap", the support of "tags" configuration would be removed in a future release.

## Including special characters in tags

> This feature is supported since v1.23.0.

Normally we don't support special characters such as `=` or `,` in key-value pairs. These characters will be treated as separator and will not be included in the key/value literal. To solve this problem, `tagsMap` is introduced since v1.23.0, in which a JSON-style tag is acceptable.

```json
{
  "tags": "a=b,c=d",
  "tagsMap": {"e": "f", "g=h": "i,j"}
}
```

`tags` and `tagsMap` will be merged, and similarly, they are case-insensitive.
