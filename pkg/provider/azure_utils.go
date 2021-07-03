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

package provider

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2021-02-01/network"
	"github.com/Azure/go-autorest/autorest/to"

	"k8s.io/klog/v2"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

var strToExtendedLocationType = map[string]network.ExtendedLocationTypes{
	"edgezone": network.ExtendedLocationTypesEdgeZone,
}

// lockMap used to lock on entries
type lockMap struct {
	sync.Mutex
	mutexMap map[string]*sync.Mutex
}

// NewLockMap returns a new lock map
func newLockMap() *lockMap {
	return &lockMap{
		mutexMap: make(map[string]*sync.Mutex),
	}
}

// LockEntry acquires a lock associated with the specific entry
func (lm *lockMap) LockEntry(entry string) {
	lm.Lock()
	// check if entry does not exists, then add entry
	if _, exists := lm.mutexMap[entry]; !exists {
		lm.addEntry(entry)
	}

	lm.Unlock()
	lm.lockEntry(entry)
}

// UnlockEntry release the lock associated with the specific entry
func (lm *lockMap) UnlockEntry(entry string) {
	lm.Lock()
	defer lm.Unlock()

	if _, exists := lm.mutexMap[entry]; !exists {
		return
	}
	lm.unlockEntry(entry)
}

func (lm *lockMap) addEntry(entry string) {
	lm.mutexMap[entry] = &sync.Mutex{}
}

func (lm *lockMap) lockEntry(entry string) {
	lm.mutexMap[entry].Lock()
}

func (lm *lockMap) unlockEntry(entry string) {
	lm.mutexMap[entry].Unlock()
}

func getContextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

func convertMapToMapPointer(origin map[string]string) map[string]*string {
	newly := make(map[string]*string)
	for k, v := range origin {
		value := v
		newly[k] = &value
	}
	return newly
}

func parseTags(tags string) map[string]*string {
	kvs := strings.Split(tags, consts.TagsDelimiter)
	formatted := make(map[string]*string)
	for _, kv := range kvs {
		res := strings.Split(kv, consts.TagKeyValueDelimiter)
		if len(res) != 2 {
			klog.Warningf("parseTags: error when parsing key-value pair %s, would ignore this one", kv)
			continue
		}
		k, v := strings.TrimSpace(res[0]), strings.TrimSpace(res[1])
		if k == "" || v == "" {
			klog.Warningf("parseTags: error when parsing key-value pair %s-%s, would ignore this one", k, v)
			continue
		}
		formatted[k] = to.StringPtr(v)
	}
	return formatted
}

func findKeyInMapCaseInsensitive(targetMap map[string]*string, key string) (bool, string) {
	for k := range targetMap {
		if strings.EqualFold(k, key) {
			return true, k
		}
	}

	return false, ""
}

func (az *Cloud) reconcileTags(currentTagsOnResource, newTags map[string]*string) (reconciledTags map[string]*string, changed bool) {
	var systemTags []string
	systemTagsMap := make(map[string]*string)

	if az.SystemTags != "" {
		systemTags = strings.Split(az.SystemTags, consts.TagsDelimiter)
		for i := 0; i < len(systemTags); i++ {
			systemTags[i] = strings.TrimSpace(systemTags[i])
		}

		for _, systemTag := range systemTags {
			systemTagsMap[systemTag] = to.StringPtr("")
		}
	}

	// if the systemTags is not set, just add/update new currentTagsOnResource and not delete old currentTagsOnResource
	for k, v := range newTags {
		found, key := findKeyInMapCaseInsensitive(currentTagsOnResource, k)

		if !found {
			currentTagsOnResource[k] = v
			changed = true
		} else if !strings.EqualFold(to.String(v), to.String(currentTagsOnResource[key])) {
			currentTagsOnResource[key] = v
			changed = true
		}
	}

	// if the systemTags is set, delete the old currentTagsOnResource
	if len(systemTagsMap) > 0 {
		for k := range currentTagsOnResource {
			if _, ok := newTags[k]; !ok {
				if found, _ := findKeyInMapCaseInsensitive(systemTagsMap, k); !found {
					delete(currentTagsOnResource, k)
					changed = true
				}
			}
		}
	}

	return currentTagsOnResource, changed
}

func (az *Cloud) getVMSetNamesSharingPrimarySLB() sets.String {
	vmSetNames := make([]string, 0)
	if az.NodePoolsWithoutDedicatedSLB != "" {
		vmSetNames = strings.Split(az.Config.NodePoolsWithoutDedicatedSLB, consts.VMSetNamesSharingPrimarySLBDelimiter)
		for i := 0; i < len(vmSetNames); i++ {
			vmSetNames[i] = strings.ToLower(strings.TrimSpace(vmSetNames[i]))
		}
	}

	return sets.NewString(vmSetNames...)
}

func getExtendedLocationTypeFromString(extendedLocationType string) network.ExtendedLocationTypes {
	extendedLocationType = strings.ToLower(extendedLocationType)
	if val, ok := strToExtendedLocationType[extendedLocationType]; ok {
		return val
	}
	return network.ExtendedLocationTypesEdgeZone
}

func getServiceAdditionalPublicIPs(service *v1.Service) ([]string, error) {
	if service == nil {
		return nil, nil
	}

	result := []string{}
	if val, ok := service.Annotations[consts.ServiceAnnotationAdditionalPublicIPs]; ok {
		pips := strings.Split(strings.TrimSpace(val), ",")
		for _, pip := range pips {
			ip := strings.TrimSpace(pip)
			if ip == "" {
				continue // skip empty string
			}

			if net.ParseIP(ip) == nil {
				return nil, fmt.Errorf("%s is not a valid IP address", ip)
			}

			result = append(result, ip)
		}
	}

	return result, nil
}
