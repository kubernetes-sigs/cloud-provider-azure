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

package config

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"

	azclients "sigs.k8s.io/cloud-provider-azure/pkg/azureclients"
)

var (
	testAzureConfig = `{
		"cloudProviderRatelimit": true,
		"cloudProviderRateLimitBucket": 1,
		"cloudProviderRateLimitBucketWrite": 1,
		"cloudProviderRateLimitQPS": 1,
		"cloudProviderRateLimitQPSWrite": 1,
		"virtualMachineScaleSetRateLimit": {
			"cloudProviderRatelimit": true,
			"cloudProviderRateLimitBucket": 2,
			"CloudProviderRateLimitBucketWrite": 2,
			"cloudProviderRateLimitQPS": 0,
			"CloudProviderRateLimitQPSWrite": 0
		},
		"loadBalancerRateLimit": {
			"cloudProviderRatelimit": false
		}
	}`

	testDefaultRateLimitConfig = azclients.RateLimitConfig{
		CloudProviderRateLimit:            true,
		CloudProviderRateLimitBucket:      1,
		CloudProviderRateLimitBucketWrite: 1,
		CloudProviderRateLimitQPS:         1,
		CloudProviderRateLimitQPSWrite:    1,
	}

	testAttachDetachDiskDefaultRateLimitConfig = azclients.RateLimitConfig{
		CloudProviderRateLimit:            true,
		CloudProviderRateLimitQPS:         0,
		CloudProviderRateLimitBucket:      0,
		CloudProviderRateLimitQPSWrite:    DefaultAtachDetachDiskQPS,
		CloudProviderRateLimitBucketWrite: DefaultAtachDetachDiskBucket,
	}
)

func TestParseConfig(t *testing.T) {
	expected := &CloudProviderRateLimitConfig{
		RateLimitConfig: testDefaultRateLimitConfig,
		VirtualMachineScaleSetRateLimit: &azclients.RateLimitConfig{
			CloudProviderRateLimit:            true,
			CloudProviderRateLimitBucket:      2,
			CloudProviderRateLimitBucketWrite: 2,
		},
		LoadBalancerRateLimit: &azclients.RateLimitConfig{
			CloudProviderRateLimit: false,
		},
	}

	buffer := bytes.NewBufferString(testAzureConfig)
	config := &CloudProviderRateLimitConfig{}
	err := json.Unmarshal(buffer.Bytes(), config)
	assert.NoError(t, err)
	assert.Equal(t, expected, config)
}

func TestInitializeCloudProviderRateLimitConfig(t *testing.T) {
	buffer := bytes.NewBufferString(testAzureConfig)
	config := &CloudProviderRateLimitConfig{}
	err := json.Unmarshal(buffer.Bytes(), config)
	assert.NoError(t, err)

	InitializeCloudProviderRateLimitConfig(config)
	assert.Equal(t, config.LoadBalancerRateLimit, &azclients.RateLimitConfig{
		CloudProviderRateLimit: false,
	})
	assert.Equal(t, config.VirtualMachineScaleSetRateLimit, &azclients.RateLimitConfig{
		CloudProviderRateLimit:            true,
		CloudProviderRateLimitBucket:      2,
		CloudProviderRateLimitBucketWrite: 2,
		CloudProviderRateLimitQPS:         1,
		CloudProviderRateLimitQPSWrite:    1,
	})
	assert.Equal(t, config.VirtualMachineSizeRateLimit, &testDefaultRateLimitConfig)
	assert.Equal(t, config.VirtualMachineRateLimit, &testDefaultRateLimitConfig)
	assert.Equal(t, config.RouteRateLimit, &testDefaultRateLimitConfig)
	assert.Equal(t, config.SubnetsRateLimit, &testDefaultRateLimitConfig)
	assert.Equal(t, config.InterfaceRateLimit, &testDefaultRateLimitConfig)
	assert.Equal(t, config.RouteTableRateLimit, &testDefaultRateLimitConfig)
	assert.Equal(t, config.SecurityGroupRateLimit, &testDefaultRateLimitConfig)
	assert.Equal(t, config.StorageAccountRateLimit, &testDefaultRateLimitConfig)
	assert.Equal(t, config.DiskRateLimit, &testDefaultRateLimitConfig)
	assert.Equal(t, config.SnapshotRateLimit, &testDefaultRateLimitConfig)
	assert.Equal(t, config.AttachDetachDiskRateLimit, &testAttachDetachDiskDefaultRateLimitConfig)
}
