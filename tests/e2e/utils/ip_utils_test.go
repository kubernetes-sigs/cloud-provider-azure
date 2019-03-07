/*
Copyright 2019 The Kubernetes Authors.

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

package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCIDRString2intArray(t *testing.T) {
	cidr := "10.240.0.0/16"
	intArray, prefix, err := cidrString2intArray(cidr)
	assert.Empty(t, err)
	assert.Equal(t, prefix, 16)
	intArraySuppose := []int{
		0, 0, 0, 0, 1, 0, 1, 0,
		1, 1, 1, 1, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0,
	}
	for i := range intArray {
		assert.Equal(t, intArray[i], intArraySuppose[i])
	}
}

func TestPrefixIntArray2String(t *testing.T) {
	prefix := 16
	intArray := []int{
		0, 0, 0, 0, 1, 0, 1, 0,
		1, 1, 1, 1, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0,
	}
	cidrIP := prefixIntArray2String(intArray, prefix)
	cidrSuppose := "10.240.0.0/16"
	assert.Equal(t, cidrIP, cidrSuppose)
}

func TestValidateIPInCIDR(t *testing.T) {
	cidr := "10.24.0.0/16"
	ip1 := "10.24.0.100"
	ip2 := "20.24.0.0"
	flag1, _ := ValidateIPInCIDR(ip1, cidr)
	assert.Equal(t, flag1, true)
	flag2, _ := ValidateIPInCIDR(ip2, cidr)
	assert.Equal(t, flag2, false)
}

func TestGetNextSubnet(t *testing.T) {
	vNetCIDR := "10.24.0.0/16"
	existSubnets := []string{
		"10.24.0.0/24",
		"10.24.1.0/24",
	}
	cidrResult := "10.24.2.0/24"
	cidr, err := getNextSubnet(vNetCIDR, existSubnets)
	assert.Empty(t, err)
	assert.Equal(t, cidrResult, cidr)
}
