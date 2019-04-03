#!/bin/bash
# Copyright 2018 The Kubernetes Authors.
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
echo "Put name of resource to be created"
read resrc
echo "Enter location"
read location
az group create --name $resrc --location $location
echo "Paste your subscription id here"
read id2
echo "Type the filename of the generated api-model to be used in .json format"
read filename
echo "Please edit dnsPrefix,keydata,clientId and secret"
gedit ../tests/k8s-azure/manifest/$filename
az ad sp create-for-rbac --role="Contributor" --scopes="/subscriptions/$id2/resourceGroups/$resrc"
echo "Paste the  name"
read cliedId
echo "Paste the password here"
read clientscr
echo "Do you have aks-engine installed?[Yes]/[No]"
read e
if [ $e = 'Yes' ]
then
echo "Let's go ahead"
  aks-engine deploy --subscription-id $id2 \
  --auth-method cli \
  --dns-prefix $resrc \
  --resource-group $resrc \
  --location $location \
  --api-model ../tests/k8s-azure/manifest/$filename \
  --set servicePrincipalProfile.clientId="$clientId" \
  --set servicePrincipalProfile.secret="$clientscr"
 #deployed cluster successfully
else
echo "Please refer to link https://github.com/Azure/aks-engine/blob/master/docs/tutorials/quickstart.md"
fi
