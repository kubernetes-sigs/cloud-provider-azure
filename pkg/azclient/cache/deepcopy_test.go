/*
Copyright 2022 The Kubernetes Authors.

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

package cache

import (
	"sync"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-08-01/compute"
	"github.com/stretchr/testify/assert"

	"k8s.io/utils/ptr"
)

type fakeStruct struct {
	Name string
}

type fakeIntf interface {
	Get() string
}

func (f fakeStruct) Get() string {
	return f.Name
}

// TestCopyBasic tests object with pointer, struct, map, slice, interface.
func TestCopyBasic(t *testing.T) {
	zones := []string{"zone0", "zone1"}
	var vmOriginal *armcompute.VirtualMachine = &armcompute.VirtualMachine{
		Properties: &armcompute.VirtualMachineProperties{
			ProvisioningState: ptr.To("Failed"),
		},
		Name:  ptr.To("vmOriginal"),
		Zones: to.SliceOfPtrs(zones...),
		Tags: map[string]*string{
			"tag0": ptr.To("tagVal0"),
		},
	}
	vmCopied := Copy(vmOriginal).(*armcompute.VirtualMachine)

	psOriginal := vmOriginal.Properties.ProvisioningState
	psCopied := vmCopied.Properties.ProvisioningState
	assert.Equal(t, psOriginal, psCopied)
	assert.Equal(t, vmOriginal.Name, vmCopied.Name)
	assert.Equal(t, vmOriginal.Zones, vmCopied.Zones)
	assert.Equal(t, vmOriginal.Tags, vmCopied.Tags)

	var fakeOriginal fakeIntf = fakeStruct{Name: "fakeOriginal"}
	fakeCopied := Copy(fakeOriginal).(fakeIntf)
	assert.Equal(t, fakeOriginal.Get(), fakeCopied.Get())
}

// TestCopyVMInSyncMap tests object like compute.VirtualMachine in a sync.Map.
func TestCopyVMInSyncMap(t *testing.T) {
	var vmOriginal *compute.VirtualMachine = &compute.VirtualMachine{
		VirtualMachineProperties: &compute.VirtualMachineProperties{
			ProvisioningState: ptr.To("Failed"),
		},
		Name: ptr.To("vmOriginal"),
	}
	vmCacheOriginal := &sync.Map{}
	vmCacheOriginal.Store("vmOriginal", vmOriginal)
	vmCacheCopied := Copy(vmCacheOriginal).(*sync.Map)

	psOriginal := vmOriginal.VirtualMachineProperties.ProvisioningState
	vCopied, ok := vmCacheCopied.Load("vmOriginal")
	assert.True(t, ok)
	vmCopied := vCopied.(*compute.VirtualMachine)
	psCopied := vmCopied.VirtualMachineProperties.ProvisioningState
	assert.Equal(t, psOriginal, psCopied)
	assert.Equal(t, vmOriginal.Name, vmCopied.Name)
}

type vmssEntry struct {
	*compute.VirtualMachineScaleSet
	Name *string
}

// TestCopyVMSSEntryInSyncMap tests object like vmssEntry in sync.Map.
func TestCopyVMSSEntryInSyncMap(t *testing.T) {
	vmssEntryOriginal := &vmssEntry{
		Name: ptr.To("vmssEntryName"),
		VirtualMachineScaleSet: &compute.VirtualMachineScaleSet{
			Name: ptr.To("vmssOriginal"),
		},
	}
	vmssCacheOriginal := &sync.Map{}
	vmssCacheOriginal.Store("vmssEntry", vmssEntryOriginal)
	vmssCacheCopied := Copy(vmssCacheOriginal)

	vCopied, ok := vmssCacheCopied.(*sync.Map).Load("vmssEntry")
	assert.True(t, ok)
	vmssEntryCopied := vCopied.(*vmssEntry)
	entryNameOriginal := vmssEntryOriginal.Name
	entryNameCopied := vmssEntryCopied.Name
	assert.Equal(t, entryNameOriginal, entryNameCopied)
	vmNameOriginal := vmssEntryOriginal.VirtualMachineScaleSet.Name
	vmNameCopied := vmssEntryCopied.VirtualMachineScaleSet.Name
	assert.Equal(t, vmNameOriginal, vmNameCopied)
}
