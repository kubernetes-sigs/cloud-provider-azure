/*
Copyright 2026 The Kubernetes Authors.

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

package node

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	azureprovider "sigs.k8s.io/cloud-provider-azure/pkg/provider"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
	utilnode "sigs.k8s.io/cloud-provider-azure/pkg/util/node"
)

const (
	testNodeName         = "vm1"
	testResourceID       = "/subscriptions/sub/resourcegroups/rg/providers/Microsoft.Compute/virtualMachines/vm1"
	testInstanceMetadata = `{"compute":{"name":"vm1","subscriptionId":"sub","resourceGroupName":"rg",` +
		`"resourceId":"/subscriptions/sub/resourcegroups/rg/providers/Microsoft.Compute/virtualMachines/vm1","vmSize":"Standard_D2s_v3"},` +
		`"network":{"interface":[{"ipv4":{"ipAddress":[{"privateIpAddress":"10.0.0.4"}]}}]}}`
	testLoadBalancerMetadata = `{"loadbalancer":{"publicIpAddresses":[{"frontendIpAddress":"20.0.0.1","privateIpAddress":"10.0.0.4"}]}}`
)

type testIMDSServer struct {
	server        *httptest.Server
	instanceCalls atomic.Int32
	lbCalls       atomic.Int32
}

func newTestIMDSServer(t *testing.T, lbStatus int) *testIMDSServer {
	s := &testIMDSServer{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case consts.ImdsInstanceURI:
			s.instanceCalls.Add(1)
			_, _ = fmt.Fprint(w, testInstanceMetadata)
		case consts.ImdsLoadBalancerURI:
			s.lbCalls.Add(1)
			if lbStatus != http.StatusOK {
				w.WriteHeader(lbStatus)
				return
			}
			_, _ = fmt.Fprint(w, testLoadBalancerMetadata)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.server.Close)
	return s
}

func newTestCloud(t *testing.T, imdsServer string) *azureprovider.Cloud {
	ims, err := azureprovider.NewInstanceMetadataService(imdsServer)
	if err != nil {
		t.Fatalf("failed to create instance metadata service: %v", err)
	}
	return &azureprovider.Cloud{
		Config:   config.Config{UseInstanceMetadata: true},
		Metadata: ims,
	}
}

type nodeAddressesProvider interface {
	NodeAddresses(ctx context.Context, name types.NodeName) ([]v1.NodeAddress, error)
	InstanceID(ctx context.Context, name types.NodeName) (string, error)
}

func TestNodeProvidersNodeAddresses(t *testing.T) {
	hostname := v1.NodeAddress{Type: v1.NodeHostName, Address: testNodeName}
	internalIP := v1.NodeAddress{Type: v1.NodeInternalIP, Address: "10.0.0.4"}
	externalIP := v1.NodeAddress{Type: v1.NodeExternalIP, Address: "20.0.0.1"}

	providers := []struct {
		name string
		new  func(cloud *azureprovider.Cloud) nodeAddressesProvider
	}{
		{
			name: "IMDSNodeProvider",
			new:  func(cloud *azureprovider.Cloud) nodeAddressesProvider { return &IMDSNodeProvider{azure: cloud} },
		},
		{
			name: "ARMNodeProvider",
			new:  func(cloud *azureprovider.Cloud) nodeAddressesProvider { return &ARMNodeProvider{azure: cloud} },
		},
	}

	testCases := []struct {
		desc              string
		lbStatus          int
		expectedAddresses []v1.NodeAddress
		expectedErr       string
	}{
		{
			desc:              "healthy loadbalancer metadata should return ExternalIP without error",
			lbStatus:          http.StatusOK,
			expectedAddresses: []v1.NodeAddress{hostname, internalIP, externalIP},
		},
		{
			desc:              "transient loadbalancer metadata failure should forward partial addresses with the marked error",
			lbStatus:          http.StatusServiceUnavailable,
			expectedAddresses: []v1.NodeAddress{hostname, internalIP},
			expectedErr:       "failed to get loadbalancer metadata: load balancer metadata unavailable: failure of getting loadbalancer metadata, status code 503",
		},
		{
			desc:              "benign loadbalancer metadata failure should keep legacy behavior",
			lbStatus:          http.StatusNotFound,
			expectedAddresses: []v1.NodeAddress{hostname, internalIP},
		},
	}

	for _, p := range providers {
		for _, tc := range testCases {
			t.Run(p.name+"/"+tc.desc, func(t *testing.T) {
				imds := newTestIMDSServer(t, tc.lbStatus)
				provider := p.new(newTestCloud(t, imds.server.URL))
				ctx := context.Background()

				addresses, err := provider.NodeAddresses(ctx, testNodeName)
				firstErr := err
				assert.Equal(t, tc.expectedAddresses, addresses)
				if tc.expectedErr != "" {
					assert.EqualError(t, err, tc.expectedErr)
					assert.True(t, errors.Is(err, utilnode.ErrLoadBalancerMetadataUnavailable))
				} else {
					assert.NoError(t, err)
				}

				// Identity from the same cached metadata is unaffected.
				instanceID, err := provider.InstanceID(ctx, testNodeName)
				assert.NoError(t, err)
				assert.Equal(t, "azure://"+testResourceID, instanceID)

				// Repeated reads are served from cache and return the same cached error.
				addressesAgain, errAgain := provider.NodeAddresses(ctx, testNodeName)
				assert.Equal(t, tc.expectedAddresses, addressesAgain)
				if tc.expectedErr != "" {
					assert.Same(t, firstErr, errAgain, "cached error should be returned unchanged")
				} else {
					assert.NoError(t, errAgain)
				}
				assert.Equal(t, int32(1), imds.instanceCalls.Load())
				assert.Equal(t, int32(1), imds.lbCalls.Load())
			})
		}
	}
}
