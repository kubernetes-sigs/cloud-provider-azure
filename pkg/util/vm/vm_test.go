/*
Copyright 2024 The Kubernetes Authors.

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

package vm

import (
	"testing"

	"k8s.io/utils/pointer"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-03-01/compute"
	"github.com/stretchr/testify/assert"
)

func TestGetVMPowerState(t *testing.T) {
	type testCase struct {
		name       string
		vmStatuses *[]compute.InstanceViewStatus
		expected   string
	}

	tests := []testCase{
		{
			name: "should return power state when there is power state status",
			vmStatuses: &[]compute.InstanceViewStatus{
				{Code: pointer.String("foo")},
				{Code: pointer.String("PowerState/Running")},
			},
			expected: "Running",
		},
		{
			name: "should return unknown when there is no power state status",
			vmStatuses: &[]compute.InstanceViewStatus{
				{Code: pointer.String("foo")},
			},
			expected: "unknown",
		},
		{
			name:       "should return unknown when vmStatuses is nil",
			vmStatuses: nil,
			expected:   "unknown",
		},
		{
			name:       "should return unknown when vmStatuses is empty",
			vmStatuses: &[]compute.InstanceViewStatus{},
			expected:   "unknown",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.expected, GetVMPowerState("vm", test.vmStatuses))
		})
	}
}

func TestIsNotActiveVMState(t *testing.T) {
	type testCase struct {
		name              string
		provisioningState string
		powerState        string
		expected          bool
	}

	tests := []testCase{
		{
			name:              "should return true when provisioning state is deleting",
			provisioningState: "deleting",
			powerState:        "running",
			expected:          true,
		},
		{
			name:              "should return true when provisioning state is unknown",
			provisioningState: "unknown",
			powerState:        "running",
			expected:          true,
		},
		{
			name:              "should return true when power state is unknown",
			provisioningState: "running",
			powerState:        "unknown",
			expected:          true,
		},
		{
			name:              "should return true when power state is stopped",
			provisioningState: "running",
			powerState:        "stopped",
			expected:          true,
		},
		{
			name:              "should return true when power state is stopping",
			provisioningState: "running",
			powerState:        "stopping",
			expected:          true,
		},
		{
			name:              "should return true when power state is deallocated",
			provisioningState: "running",
			powerState:        "deallocated",
			expected:          true,
		},
		{
			name:              "should return true when power state is deallocating",
			provisioningState: "running",
			powerState:        "deallocating",
			expected:          true,
		},
		{
			name:              "should return false when provisioning state is running and power state is running",
			provisioningState: "running",
			powerState:        "running",
			expected:          false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.expected, IsNotActiveVMState(test.provisioningState, test.powerState))
		})
	}
}
