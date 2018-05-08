#!/bin/bash

set -e

# az login --service-principal \
#   -t $K8S_AZURE_TENANTID \
#   -u $K8S_AZURE_SPID \
#   -p $K8S_AZURE_SPSEC \
#   > /dev/null

k8s-azure $@
