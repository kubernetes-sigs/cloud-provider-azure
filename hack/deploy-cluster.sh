#!/bin/bash
# Copyright 2019 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
set -e

RESOURCE_GROUP_NAME=${RESOURCE_GROUP_NAME:-""}
LOCATION=${LOCATION:-""}

#check for variables is initialized or not

if [ "$RESOURCE_GROUP_NAME" = "" ]
then
echo "RESOURCE GROUP NAME must be specified"
exit 1
fi
if [ "$LOCATION" = "" ]
then
echo "LOCATION must be specified"
exit 1
fi

#getting the subscription id

INPUT=$(az group create --name $RESOURCE_GROUP_NAME --location $LOCATION)
CUT_ID=$(echo $INPUT| cut -d"/" -f 3)
SUBSCRIPTION_ID=${SUBSCRIPTION_ID:-"$CUT_ID"}

#getting client_id

INPUT1=$(az ad sp create-for-rbac --role="Contributor" --scopes="/subscriptions/$SUBSCRIPTION_ID/resourceGroups/$resrc")
SUB1=$(echo $INPUT1| cut -d":" -f 2)
SUB2=$(echo $SUB1| cut -d"," -f 1)
CLIENT_ID=${CLIENT_ID:-"$SUB2"}

#getting client_secret

SUB3=$(echo $INPUT1| cut -d":" -f 2)
SUB4=$(echo $SUB3| cut -d"," -f 1)
CLIENT_SECRET=${CLIENT_SECRET:-"$SUB4"}

#getting tenant_id

SUB5=$(echo $INPUT1| cut -d":" -f 7-)
SUB6=$(echo $SUB5| cut -d"}" -f 1)
TENANT_ID=${TENANT_ID:-"$SUB6"}


echo "Type the filename of the generated api-model to be used in .json format"
read FILENAME
echo "Client_id is $CLIENT_ID"
echo "Client_secret is $CLIENT_SECRET"
echo "Please edit dnsPrefix,keydata,clientId and secret"
gedit ../tests/k8s-azure/manifest/$FILENAME

check=$(aks-engine version)
if [ "$check" != "" ]
then
echo "Ok! aks-engine installed.Let go ahead"
  aks-engine deploy --subscription-id $SUBSCRIPTION_ID \
  --auth-method cli \
  --dns-prefix \
  --resource-group $RESOURCE_GROUP_NAME \
  --location $LOCATION \
  --api-model ../tests/k8s-azure/manifest/$FILENAME \
  --set servicePrincipalProfile.clientId="$CLIENT_ID" \
  --set servicePrincipalProfile.secret="$CLIENT_SECRET"
 #deployed cluster successfully
else
echo "aks-engine not installed.Please refer to link https://github.com/Azure/aks-engine/blob/master/docs/tutorials/quickstart.md"
fi
