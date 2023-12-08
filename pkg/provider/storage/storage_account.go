/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package storage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/klog/v2"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/accountclient"
)

// GetStorageAccesskey gets the storage account access key
// getLatestAccountKey: get the latest account key per CreationTime if true, otherwise get the first account key
func GetStorageAccesskey(ctx context.Context, saClient accountclient.Interface, account, resourceGroup string, getLatestAccountKey bool) (string, error) {
	if saClient == nil {
		return "", fmt.Errorf("StorageAccountClient is nil")
	}

	result, err := saClient.ListKeys(ctx, resourceGroup, account)
	if err != nil {
		return "", err
	}
	if len(result) == 0 {
		return "", fmt.Errorf("empty keys")
	}

	var key string
	var creationTime time.Time

	for _, k := range result {
		if k.Value != nil && *k.Value != "" {
			v := *k.Value
			if ind := strings.LastIndex(v, " "); ind >= 0 {
				v = v[(ind + 1):]
			}
			if !getLatestAccountKey {
				// get first key
				return v, nil
			}
			// get account key with latest CreationTime
			if key == "" {
				key = v
				if k.CreationTime != nil {
					creationTime = *k.CreationTime
				}
				klog.V(2).Infof("got storage account key with creation time: %v", creationTime)
			} else {
				if k.CreationTime != nil && creationTime.Before(*k.CreationTime) {
					key = v
					creationTime = *k.CreationTime
					klog.V(2).Infof("got storage account key with latest creation time: %v", creationTime)
				}
			}
		}
	}

	if key == "" {
		return "", fmt.Errorf("no valid keys")
	}
	return key, nil
}
