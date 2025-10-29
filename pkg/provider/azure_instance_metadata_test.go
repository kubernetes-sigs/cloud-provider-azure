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
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
)

// TestFillNetInterfacePublicIPs tests if IPv6 IPs from imds load balancer are
// properly handled.
func TestFillNetInterfacePublicIPs(t *testing.T) {
	testcases := []struct {
		desc                 string
		publicIPs            []PublicIPMetadata
		netInterface         *NetworkInterface
		expectedNetInterface *NetworkInterface
	}{
		{
			desc: "IPv6/DualStack",
			publicIPs: []PublicIPMetadata{
				{
					FrontendIPAddress: "20.0.0.0",
					PrivateIPAddress:  "10.244.0.0",
				},
				{
					FrontendIPAddress: "[2001::1]",
					PrivateIPAddress:  "[fd00::1]",
				},
			},
			netInterface: &NetworkInterface{
				IPV4: NetworkData{
					IPAddress: []IPAddress{
						{
							PrivateIP: "10.244.0.0",
						},
					},
				},
				IPV6: NetworkData{
					IPAddress: []IPAddress{
						{
							PrivateIP: "fd00::1",
						},
					},
				},
			},
			expectedNetInterface: &NetworkInterface{
				IPV4: NetworkData{
					IPAddress: []IPAddress{
						{
							PrivateIP: "10.244.0.0",
							PublicIP:  "20.0.0.0",
						},
					},
				},
				IPV6: NetworkData{
					IPAddress: []IPAddress{
						{
							PrivateIP: "fd00::1",
							PublicIP:  "2001::1",
						},
					},
				},
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			fillNetInterfacePublicIPs(tc.publicIPs, tc.netInterface)
			assert.Equal(t, tc.expectedNetInterface, tc.netInterface)
		})
	}
}

func TestGetPlatformSubFaultDomain(t *testing.T) {
	for _, testCase := range []struct {
		description string
		nilCompute  bool
		expectedErr error
	}{
		{
			description: "GetPlatformSubFaultDomain should parse the correct platformSubFaultDomain",
		},
		{
			description: "GetPlatformSubFaultDomain should report an error if the compute is nil",
			nilCompute:  true,
			expectedErr: errors.New("failure of getting compute information from instance metadata"),
		},
	} {
		t.Run(testCase.description, func(t *testing.T) {
			cloud := &Cloud{
				Config: config.Config{
					Location:            "eastus",
					UseInstanceMetadata: true,
				},
			}

			getter := func(context.Context, string) (interface{}, error) {
				if testCase.nilCompute {
					return &InstanceMetadata{}, nil
				}
				return &InstanceMetadata{
					Compute: &ComputeMetadata{
						PlatformSubFaultDomain: "2",
					},
				}, nil
			}
			cache, err := azcache.NewTimedCache(time.Minute, getter, false)
			assert.NoError(t, err)
			cloud.Metadata = &InstanceMetadataService{
				imsCache: cache,
			}

			fd, err := cloud.GetPlatformSubFaultDomain(context.TODO())
			if testCase.expectedErr != nil {
				assert.Equal(t, testCase.expectedErr, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, "2", fd)
			}
		})
	}
}

func TestGetInterconnectGroupID(t *testing.T) {
	testCases := []struct {
		name                string
		useInstanceMetadata bool
		respString          string
		expectedID          string
		expectedErr         bool
	}{
		{
			name:                "InterconnectGroup tag present",
			useInstanceMetadata: true,
			respString:          `{"compute":{"tagsList":[{"name":"Platform_Interconnect_Group","value":"group-123"},{"name":"Other_Tag","value":"other"}]}}`,
			expectedID:          "group-123",
			expectedErr:         false,
		},
		{
			name:                "InterconnectGroup tag absent",
			useInstanceMetadata: true,
			respString:          `{"compute":{"tagsList":[{"name":"Other_Tag","value":"other"}]}}`,
			expectedID:          "",
			expectedErr:         false,
		},
		{
			name:                "Empty tagsList",
			useInstanceMetadata: true,
			respString:          `{"compute":{"tagsList":[]}}`,
			expectedID:          "",
			expectedErr:         false,
		},
		{
			name:                "UseInstanceMetadata false",
			useInstanceMetadata: false,
			respString:          `{"compute":{"tagsList":[{"name":"Platform_Interconnect_Group","value":"group-123"}]}}`,
			expectedID:          "",
			expectedErr:         false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cloud := &Cloud{
				Config: config.Config{
					Location:            "eastus",
					UseInstanceMetadata: tc.useInstanceMetadata,
				},
			}

			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Errorf("Test [%s] unexpected error: %v", tc.name, err)
			}

			mux := http.NewServeMux()
			mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, tc.respString)
			}))
			go func() {
				_ = http.Serve(listener, mux)
			}()
			defer listener.Close()

			cloud.Metadata, err = NewInstanceMetadataService("http://" + listener.Addr().String() + "/")
			if err != nil {
				t.Errorf("Test [%s] unexpected error: %v", tc.name, err)
			}

			id, err := cloud.GetInterconnectGroupID(context.TODO())

			if tc.expectedErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expectedID, id)
			}
		})
	}
}
