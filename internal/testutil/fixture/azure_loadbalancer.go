/*
Copyright 2023 The Kubernetes Authors.

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

package fixture

import (
	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"

	"k8s.io/utils/ptr"
)

func (f *AzureFixture) LoadBalancer() *AzureLoadBalancerFixture {
	return &AzureLoadBalancerFixture{
		lb: &network.LoadBalancer{
			Name:                         ptr.To("lb"),
			LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
				// TODO
			},
		},
	}
}

type AzureLoadBalancerFixture struct {
	lb *network.LoadBalancer
}

func (f *AzureLoadBalancerFixture) Build() network.LoadBalancer {
	return *f.lb
}

func (f *AzureLoadBalancerFixture) IPv4Addresses() []string {
	return []string{
		"10.0.0.1",
		"10.0.0.2",
	}
}

func (f *AzureLoadBalancerFixture) IPv6Addresses() []string {
	return []string{
		"2001:db8:ac10:fe01::",
		"2001:db8:ac10:fe01::1",
	}
}

func (f *AzureLoadBalancerFixture) Addresses() []string {
	return append(f.IPv4Addresses(), f.IPv6Addresses()...)
}

func (f *AzureLoadBalancerFixture) BackendPoolIPv4Addresses() []string {
	return []string{
		"192.168.10.1",
		"192.168.10.2",
		"192.168.10.3",
	}
}

func (f *AzureLoadBalancerFixture) BackendPoolIPv6Addresses() []string {
	return []string{
		"2001:db8:85a3::8a2e:370:7331",
		"2001:db8:85a3::8a2e:370:7332",
		"2001:db8:85a3::8a2e:370:7333",
	}
}

func (f *AzureLoadBalancerFixture) BackendPoolAddresses() []string {
	return append(f.BackendPoolIPv4Addresses(), f.BackendPoolIPv6Addresses()...)
}

func (f *AzureLoadBalancerFixture) AdditionalIPv4Addresses() []string {
	return []string{
		"172.55.0.1",
		"172.55.0.2",
	}
}

func (f *AzureLoadBalancerFixture) AdditionalIPv6Addresses() []string {
	return []string{
		"1001:db4::1",
		"1001:db4::2",
	}
}

func (f *AzureLoadBalancerFixture) AdditionalAddresses() []string {
	return append(f.AdditionalIPv4Addresses(), f.AdditionalIPv6Addresses()...)
}
