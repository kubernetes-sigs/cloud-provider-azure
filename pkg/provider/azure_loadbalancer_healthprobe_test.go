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

package provider

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/stretchr/testify/assert"
	"k8s.io/utils/pointer"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

// getTestProbes returns dualStack probes.
func getTestProbes(protocol, path string, interval, servicePort, probePort, numOfProbe *int32) map[bool][]network.Probe {
	return map[bool][]network.Probe{
		consts.IPVersionIPv4: {getTestProbe(protocol, path, interval, servicePort, probePort, numOfProbe, consts.IPVersionIPv4)},
		consts.IPVersionIPv6: {getTestProbe(protocol, path, interval, servicePort, probePort, numOfProbe, consts.IPVersionIPv6)},
	}
}

func getTestProbe(protocol, path string, interval, servicePort, probePort, numOfProbe *int32, isIPv6 bool) network.Probe {
	suffix := ""
	if isIPv6 {
		suffix = "-" + consts.IPVersionIPv6String
	}
	expectedProbes := network.Probe{
		Name: pointer.String(fmt.Sprintf("atest1-TCP-%d", *servicePort) + suffix),
		ProbePropertiesFormat: &network.ProbePropertiesFormat{
			Protocol:          network.ProbeProtocol(protocol),
			Port:              probePort,
			IntervalInSeconds: interval,
			ProbeThreshold:    numOfProbe,
		},
	}
	if (strings.EqualFold(protocol, "Http") || strings.EqualFold(protocol, "Https")) && len(strings.TrimSpace(path)) > 0 {
		expectedProbes.RequestPath = pointer.String(path)
	}
	return expectedProbes
}

// getDefaultTestProbes returns dualStack probes.
func getDefaultTestProbes(protocol, path string) map[bool][]network.Probe {
	return getTestProbes(protocol, path, pointer.Int32(5), pointer.Int32(80), pointer.Int32(10080), pointer.Int32(2))
}

func TestFindProbe(t *testing.T) {
	tests := []struct {
		msg           string
		existingProbe []network.Probe
		curProbe      network.Probe
		expected      bool
	}{
		{
			msg:      "empty existing probes should return false",
			expected: false,
		},
		{
			msg: "probe names match while ports don't should return false",
			existingProbe: []network.Probe{
				{
					Name: pointer.String("httpProbe"),
					ProbePropertiesFormat: &network.ProbePropertiesFormat{
						Port: pointer.Int32(1),
					},
				},
			},
			curProbe: network.Probe{
				Name: pointer.String("httpProbe"),
				ProbePropertiesFormat: &network.ProbePropertiesFormat{
					Port: pointer.Int32(2),
				},
			},
			expected: false,
		},
		{
			msg: "probe ports match while names don't should return false",
			existingProbe: []network.Probe{
				{
					Name: pointer.String("probe1"),
					ProbePropertiesFormat: &network.ProbePropertiesFormat{
						Port: pointer.Int32(1),
					},
				},
			},
			curProbe: network.Probe{
				Name: pointer.String("probe2"),
				ProbePropertiesFormat: &network.ProbePropertiesFormat{
					Port: pointer.Int32(1),
				},
			},
			expected: false,
		},
		{
			msg: "probe protocol don't match should return false",
			existingProbe: []network.Probe{
				{
					Name: pointer.String("probe1"),
					ProbePropertiesFormat: &network.ProbePropertiesFormat{
						Port:     pointer.Int32(1),
						Protocol: network.ProbeProtocolHTTP,
					},
				},
			},
			curProbe: network.Probe{
				Name: pointer.String("probe1"),
				ProbePropertiesFormat: &network.ProbePropertiesFormat{
					Port:     pointer.Int32(1),
					Protocol: network.ProbeProtocolTCP,
				},
			},
			expected: false,
		},
		{
			msg: "probe path don't match should return false",
			existingProbe: []network.Probe{
				{
					Name: pointer.String("probe1"),
					ProbePropertiesFormat: &network.ProbePropertiesFormat{
						Port:        pointer.Int32(1),
						RequestPath: pointer.String("/path1"),
					},
				},
			},
			curProbe: network.Probe{
				Name: pointer.String("probe1"),
				ProbePropertiesFormat: &network.ProbePropertiesFormat{
					Port:        pointer.Int32(1),
					RequestPath: pointer.String("/path2"),
				},
			},
			expected: false,
		},
		{
			msg: "probe interval don't match should return false",
			existingProbe: []network.Probe{
				{
					Name: pointer.String("probe1"),
					ProbePropertiesFormat: &network.ProbePropertiesFormat{
						Port:              pointer.Int32(1),
						RequestPath:       pointer.String("/path"),
						IntervalInSeconds: pointer.Int32(5),
					},
				},
			},
			curProbe: network.Probe{
				Name: pointer.String("probe1"),
				ProbePropertiesFormat: &network.ProbePropertiesFormat{
					Port:              pointer.Int32(1),
					RequestPath:       pointer.String("/path"),
					IntervalInSeconds: pointer.Int32(10),
				},
			},
			expected: false,
		},
		{
			msg: "probe match should return true",
			existingProbe: []network.Probe{
				{
					Name: pointer.String("matchName"),
					ProbePropertiesFormat: &network.ProbePropertiesFormat{
						Port: pointer.Int32(1),
					},
				},
			},
			curProbe: network.Probe{
				Name: pointer.String("matchName"),
				ProbePropertiesFormat: &network.ProbePropertiesFormat{
					Port: pointer.Int32(1),
				},
			},
			expected: true,
		},
	}

	for _, test := range tests {
		t.Run(test.msg, func(t *testing.T) {
			findResult := findProbe(test.existingProbe, test.curProbe)
			assert.Equal(t, test.expected, findResult)
		})
	}
}
