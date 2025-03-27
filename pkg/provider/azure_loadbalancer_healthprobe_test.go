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

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/stretchr/testify/assert"

	"go.uber.org/mock/gomock"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

// Define a simple config struct for testing
type testConfig struct {
	UseSharedLBRuleHealthProbeMode *bool
}

// getTestProbes returns dualStack probes.
func getTestProbes(protocol, path string, interval, servicePort, probePort, numOfProbe *int32) map[bool][]*armnetwork.Probe {
	return map[bool][]*armnetwork.Probe{
		consts.IPVersionIPv4: {getTestProbe(protocol, path, interval, servicePort, probePort, numOfProbe, consts.IPVersionIPv4)},
		consts.IPVersionIPv6: {getTestProbe(protocol, path, interval, servicePort, probePort, numOfProbe, consts.IPVersionIPv6)},
	}
}

func getTestProbe(protocol, path string, interval, servicePort, probePort, numOfProbe *int32, isIPv6 bool) *armnetwork.Probe {
	suffix := ""
	if isIPv6 {
		suffix = "-" + consts.IPVersionIPv6String
	}
	expectedProbes := &armnetwork.Probe{
		Name: ptr.To(fmt.Sprintf("atest1-TCP-%d", *servicePort) + suffix),
		Properties: &armnetwork.ProbePropertiesFormat{
			Protocol:          to.Ptr(armnetwork.ProbeProtocol(protocol)),
			Port:              probePort,
			IntervalInSeconds: interval,
			ProbeThreshold:    numOfProbe,
		},
	}
	if (strings.EqualFold(protocol, "Http") || strings.EqualFold(protocol, "Https")) && len(strings.TrimSpace(path)) > 0 {
		expectedProbes.Properties.RequestPath = ptr.To(path)
	}
	return expectedProbes
}

// getDefaultTestProbes returns dualStack probes.
func getDefaultTestProbes(protocol, path string) map[bool][]*armnetwork.Probe {
	return getTestProbes(protocol, path, ptr.To(int32(5)), ptr.To(int32(80)), ptr.To(int32(10080)), ptr.To(int32(2)))
}

func TestFindProbe(t *testing.T) {
	tests := []struct {
		msg           string
		existingProbe []*armnetwork.Probe
		curProbe      *armnetwork.Probe
		expected      bool
	}{
		{
			msg:      "empty existing probes should return false",
			expected: false,
		},
		{
			msg: "probe names match while ports don't should return false",
			existingProbe: []*armnetwork.Probe{
				{
					Name: ptr.To("httpProbe"),
					Properties: &armnetwork.ProbePropertiesFormat{
						Port: ptr.To(int32(1)),
					},
				},
			},
			curProbe: &armnetwork.Probe{
				Name: ptr.To("httpProbe"),
				Properties: &armnetwork.ProbePropertiesFormat{
					Port: ptr.To(int32(2)),
				},
			},
			expected: false,
		},
		{
			msg: "probe ports match while names don't should return false",
			existingProbe: []*armnetwork.Probe{
				{
					Name: ptr.To("probe1"),
					Properties: &armnetwork.ProbePropertiesFormat{
						Port: ptr.To(int32(1)),
					},
				},
			},
			curProbe: &armnetwork.Probe{
				Name: ptr.To("probe2"),
				Properties: &armnetwork.ProbePropertiesFormat{
					Port: ptr.To(int32(1)),
				},
			},
			expected: false,
		},
		{
			msg: "probe protocol don't match should return false",
			existingProbe: []*armnetwork.Probe{
				{
					Name: ptr.To("probe1"),
					Properties: &armnetwork.ProbePropertiesFormat{
						Port:     ptr.To(int32(1)),
						Protocol: to.Ptr(armnetwork.ProbeProtocolHTTP),
					},
				},
			},
			curProbe: &armnetwork.Probe{
				Name: ptr.To("probe1"),
				Properties: &armnetwork.ProbePropertiesFormat{
					Port:     ptr.To(int32(1)),
					Protocol: to.Ptr(armnetwork.ProbeProtocolTCP),
				},
			},
			expected: false,
		},
		{
			msg: "probe path don't match should return false",
			existingProbe: []*armnetwork.Probe{
				{
					Name: ptr.To("probe1"),
					Properties: &armnetwork.ProbePropertiesFormat{
						Port:        ptr.To(int32(1)),
						RequestPath: ptr.To("/path1"),
					},
				},
			},
			curProbe: &armnetwork.Probe{
				Name: ptr.To("probe1"),
				Properties: &armnetwork.ProbePropertiesFormat{
					Port:        ptr.To(int32(1)),
					RequestPath: ptr.To("/path2"),
				},
			},
			expected: false,
		},
		{
			msg: "probe interval don't match should return false",
			existingProbe: []*armnetwork.Probe{
				{
					Name: ptr.To("probe1"),
					Properties: &armnetwork.ProbePropertiesFormat{
						Port:              ptr.To(int32(1)),
						RequestPath:       ptr.To("/path"),
						IntervalInSeconds: ptr.To(int32(5)),
					},
				},
			},
			curProbe: &armnetwork.Probe{
				Name: ptr.To("probe1"),
				Properties: &armnetwork.ProbePropertiesFormat{
					Port:              ptr.To(int32(1)),
					RequestPath:       ptr.To("/path"),
					IntervalInSeconds: ptr.To(int32(10)),
				},
			},
			expected: false,
		},
		{
			msg: "probe match should return true",
			existingProbe: []*armnetwork.Probe{
				{
					Name: ptr.To("matchName"),
					Properties: &armnetwork.ProbePropertiesFormat{
						Port: ptr.To(int32(1)),
					},
				},
			},
			curProbe: &armnetwork.Probe{
				Name: ptr.To("matchName"),
				Properties: &armnetwork.ProbePropertiesFormat{
					Port: ptr.To(int32(1)),
				},
			},
			expected: true,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.msg, func(t *testing.T) {
			findResult := findProbe(test.existingProbe, test.curProbe)
			assert.Equal(t, test.expected, findResult)
		})
	}
}

func TestShouldKeepSharedProbe(t *testing.T) {
	testCases := []struct {
		desc             string
		service          *v1.Service
		lb               armnetwork.LoadBalancer
		wantLB           bool
		expected         bool
		expectedErr      error
		expectedProbeMod bool // indicates if we expect the probe to be modified/removed
		azConfig         testConfig
	}{
		{
			desc:     "When the lb.Properties.Probes is nil",
			service:  &v1.Service{},
			lb:       armnetwork.LoadBalancer{},
			expected: false,
		},
		{
			desc:    "When the lb.Properties.Probes is not nil but does not contain a probe with the name consts.SharedProbeName",
			service: &v1.Service{},
			lb: armnetwork.LoadBalancer{
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					Probes: []*armnetwork.Probe{
						{
							Name: ptr.To("notSharedProbe"),
						},
					},
				},
			},
			expected: false,
		},
		{
			desc:    "When the lb.Properties.Probes contains a probe with the name consts.SharedProbeName, but none of the LoadBalancingRules in the probe matches the service",
			service: &v1.Service{},
			lb: armnetwork.LoadBalancer{
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					Probes: []*armnetwork.Probe{
						{
							Name: ptr.To(consts.SharedProbeName),
							Properties: &armnetwork.ProbePropertiesFormat{
								LoadBalancingRules: []*armnetwork.SubResource{},
							},
						},
					},
				},
			},
			expected: false,
		},
		{
			desc: "When the lb.Properties.Probes contains a probe with the name consts.SharedProbeName, and at least one of the LoadBalancingRules in the probe does not match the service",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					UID: types.UID("uid"),
				},
			},
			lb: armnetwork.LoadBalancer{
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					Probes: []*armnetwork.Probe{
						{
							Name: ptr.To(consts.SharedProbeName),
							ID:   ptr.To("id"),
							Properties: &armnetwork.ProbePropertiesFormat{
								LoadBalancingRules: []*armnetwork.SubResource{
									{
										ID: ptr.To("other"),
									},
									{
										ID: ptr.To("auid"),
									},
								},
							},
						},
					},
				},
			},
			expected: true,
		},
		{
			desc: "When wantLB is true and the shared probe mode is not turned on",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					UID: types.UID("uid"),
				},
			},
			lb: armnetwork.LoadBalancer{
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					Probes: []*armnetwork.Probe{
						{
							Name: ptr.To(consts.SharedProbeName),
							ID:   ptr.To("id"),
							Properties: &armnetwork.ProbePropertiesFormat{
								LoadBalancingRules: []*armnetwork.SubResource{
									{
										ID: ptr.To("other"),
									},
									{
										ID: ptr.To("auid"),
									},
								},
							},
						},
					},
				},
			},
			wantLB: true,
		},
		{
			desc: "When the lb.Properties.Probes contains a probe with the name consts.SharedProbeName, and all of the LoadBalancingRules in the probe match the service",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					UID: types.UID("uid"),
				},
			},
			lb: armnetwork.LoadBalancer{
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					Probes: []*armnetwork.Probe{
						{
							Name: ptr.To(consts.SharedProbeName),
							ID:   ptr.To("id"),
							Properties: &armnetwork.ProbePropertiesFormat{
								LoadBalancingRules: []*armnetwork.SubResource{
									{
										ID: ptr.To("auid"),
									},
								},
							},
						},
					},
				},
			},
			expected: false,
		},
		{
			desc:     "Edge cases such as when the service or LoadBalancer is nil",
			service:  nil,
			lb:       armnetwork.LoadBalancer{},
			expected: false,
		},
		{
			desc:    "Case: Invalid LoadBalancingRule ID format causing getLastSegment to return an error",
			service: &v1.Service{},
			lb: armnetwork.LoadBalancer{
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					Probes: []*armnetwork.Probe{
						{
							Name: ptr.To(consts.SharedProbeName),
							ID:   ptr.To("id"),
							Properties: &armnetwork.ProbePropertiesFormat{
								LoadBalancingRules: []*armnetwork.SubResource{
									{
										ID: ptr.To(""),
									},
								},
							},
						},
					},
				},
			},
			expected:    false,
			expectedErr: fmt.Errorf("resource name was missing from identifier"),
		},
		{
			desc: "When service switches from Cluster to Local with exactly one rule referencing the shared probe",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					UID: types.UID("uid"),
				},
				Spec: v1.ServiceSpec{
					ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
				},
			},
			lb: armnetwork.LoadBalancer{
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					Probes: []*armnetwork.Probe{
						{
							Name: ptr.To(consts.SharedProbeName),
							ID:   ptr.To("id"),
							Properties: &armnetwork.ProbePropertiesFormat{
								LoadBalancingRules: []*armnetwork.SubResource{
									{
										ID: ptr.To("auid"),
									},
								},
							},
						},
					},
				},
			},
			expected:         false,
			expectedProbeMod: true,
		},
		{
			desc: "When service is Local but not owner of the rule - should not remove probe",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					UID: types.UID("uid"),
				},
				Spec: v1.ServiceSpec{
					ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
				},
			},
			lb: armnetwork.LoadBalancer{
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					Probes: []*armnetwork.Probe{
						{
							Name: ptr.To(consts.SharedProbeName),
							ID:   ptr.To("id"),
							Properties: &armnetwork.ProbePropertiesFormat{
								LoadBalancingRules: []*armnetwork.SubResource{
									{
										ID: ptr.To("otherService"),
									},
								},
							},
						},
					},
				},
			},
			expected:         false,
			expectedProbeMod: false,
		},
		{
			desc: "When service is Cluster with a single rule - should not remove probe",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					UID: types.UID("uid"),
				},
				Spec: v1.ServiceSpec{
					ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster,
				},
			},
			lb: armnetwork.LoadBalancer{
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					Probes: []*armnetwork.Probe{
						{
							Name: ptr.To(consts.SharedProbeName),
							ID:   ptr.To("id"),
							Properties: &armnetwork.ProbePropertiesFormat{
								LoadBalancingRules: []*armnetwork.SubResource{
									{
										ID: ptr.To("auid"),
									},
								},
							},
						},
					},
				},
			},
			expected:         false,
			expectedProbeMod: false,
		},
		{
			desc: "Error case: When service is Local with a single rule that causes parse error",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					UID: types.UID("uid"),
				},
				Spec: v1.ServiceSpec{
					ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
				},
			},
			lb: armnetwork.LoadBalancer{
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					Probes: []*armnetwork.Probe{
						{
							Name: ptr.To(consts.SharedProbeName),
							ID:   ptr.To("id"),
							Properties: &armnetwork.ProbePropertiesFormat{
								LoadBalancingRules: []*armnetwork.SubResource{
									{
										ID: ptr.To(""),
									},
								},
							},
						},
					},
				},
			},
			expected:         false,
			expectedProbeMod: false,
			expectedErr:      fmt.Errorf("resource name was missing from identifier"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			az := GetTestCloud(ctrl)

			// Set up test config if needed
			if tc.azConfig.UseSharedLBRuleHealthProbeMode != nil {
				// Set the cluster service load balancer health probe mode
				if *tc.azConfig.UseSharedLBRuleHealthProbeMode {
					az.ClusterServiceLoadBalancerHealthProbeMode = consts.ClusterServiceLoadBalancerHealthProbeModeShared
				} else {
					az.ClusterServiceLoadBalancerHealthProbeMode = consts.ClusterServiceLoadBalancerHealthProbeModeServiceNodePort
				}
			}

			var expectedProbes []*armnetwork.Probe

			// Make a copy of the original probe for checking modifications
			originalProbes := []*armnetwork.Probe{}
			if tc.lb.Properties != nil && tc.lb.Properties.Probes != nil {
				originalProbes = append(originalProbes, tc.lb.Properties.Probes...)
			}

			result, err := az.keepSharedProbe(tc.service, tc.lb, expectedProbes, tc.wantLB)
			assert.Equal(t, tc.expectedErr, err)
			if tc.expected {
				assert.Equal(t, 1, len(result))
			}

			// Check if the probe was modified/removed as expected
			if tc.expectedProbeMod {
				// Check if the original probe was actually modified/removed
				if tc.lb.Properties != nil && tc.lb.Properties.Probes != nil {
					assert.NotEqual(t, len(originalProbes), len(tc.lb.Properties.Probes),
						"Expected probe to be modified/removed but it was not")
				}
			} else if len(originalProbes) > 0 && tc.lb.Properties != nil && tc.lb.Properties.Probes != nil {
				// If not expecting modification, probe count should remain the same
				assert.Equal(t, len(originalProbes), len(tc.lb.Properties.Probes),
					"Probe was unexpectedly modified/removed")
			}
		})
	}
}
