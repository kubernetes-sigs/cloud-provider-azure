/*
Copyright 2021 The Kubernetes Authors.

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

package nodeipam

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	netutils "k8s.io/utils/net"

	"sigs.k8s.io/cloud-provider-azure/pkg/nodeipam/ipam"
	providerazure "sigs.k8s.io/cloud-provider-azure/pkg/provider"
	"sigs.k8s.io/cloud-provider-azure/pkg/util/controller/testutil"
)

func newTestNodeIpamController(clusterCIDRs []*net.IPNet, serviceCIDR *net.IPNet, secondaryServiceCIDR *net.IPNet, nodeCIDRMaskSizes []int, allocatorType ipam.CIDRAllocatorType) (*Controller, error) {
	clientSet := fake.NewSimpleClientset()
	fakeNodeHandler := &testutil.FakeNodeHandler{
		Existing: []*v1.Node{
			{ObjectMeta: metav1.ObjectMeta{Name: "node0"}},
		},
		Clientset: fake.NewSimpleClientset(),
	}
	fakeClient := &fake.Clientset{}
	fakeInformerFactory := informers.NewSharedInformerFactory(fakeClient, time.Duration(0))
	fakeNodeInformer := fakeInformerFactory.Core().V1().Nodes()

	for _, node := range fakeNodeHandler.Existing {
		_ = fakeNodeInformer.Informer().GetStore().Add(node)
	}

	fakeAZ := &providerazure.Cloud{}
	return NewNodeIpamController(
		fakeNodeInformer, fakeAZ, clientSet,
		clusterCIDRs, serviceCIDR, secondaryServiceCIDR, nodeCIDRMaskSizes, allocatorType,
	)
}

// TestNewNodeIpamControllerWithCIDRMasks tests if the controller can be
// created with combinations of network CIDRs and masks.
func TestNewNodeIpamControllerWithCIDRMasks(t *testing.T) {
	emptyServiceCIDR := ""
	for _, tc := range []struct {
		desc                 string
		clusterCIDR          string
		serviceCIDR          string
		secondaryServiceCIDR string
		maskSize             []int
		allocatorType        ipam.CIDRAllocatorType
		wantFatal            bool
	}{
		{"valid_range_allocator", "10.0.0.0/21", "10.1.0.0/21", emptyServiceCIDR, []int{24}, ipam.RangeAllocatorType, false},

		{"valid_range_allocator_dualstack", "10.0.0.0/21,2000::/10", "10.1.0.0/21", emptyServiceCIDR, []int{24, 98}, ipam.RangeAllocatorType, false},
		{"valid_range_allocator_dualstack_dualstackservice", "10.0.0.0/21,2000::/10", "10.1.0.0/21", "3000::/10", []int{24, 98}, ipam.RangeAllocatorType, false},

		{"valid_cloud_allocator", "10.0.0.0/21", "10.1.0.0/21", emptyServiceCIDR, []int{24}, ipam.CloudAllocatorType, false},
		{"valid_skip_cluster_CIDR_validation_for_cloud_allocator", "invalid", "10.1.0.0/21", emptyServiceCIDR, []int{24}, ipam.CloudAllocatorType, false},
		{"valid_CIDR_smaller_than_mask_cloud_allocator", "10.0.0.0/26", "10.1.0.0/21", emptyServiceCIDR, []int{24}, ipam.CloudAllocatorType, false},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			clusterCidrs, _ := netutils.ParseCIDRs(strings.Split(tc.clusterCIDR, ","))
			_, serviceCIDRIpNet, _ := net.ParseCIDR(tc.serviceCIDR)
			_, secondaryServiceCIDRIpNet, _ := net.ParseCIDR(tc.secondaryServiceCIDR)

			if os.Getenv("EXIT_ON_FATAL") == "1" {
				// This is the subprocess which runs the actual code.
				_, _ = newTestNodeIpamController(clusterCidrs, serviceCIDRIpNet, secondaryServiceCIDRIpNet, tc.maskSize, tc.allocatorType)
				return
			}
			// This is the host process that monitors the exit code of the subprocess.
			arg0 := os.Args[0]
			arg1 := "-test.run=TestNewNodeIpamControllerWithCIDRMasks/" + tc.desc
			cmd := exec.Command(arg0, arg1)
			cmd.Env = append(os.Environ(), "EXIT_ON_FATAL=1")
			err := cmd.Run()
			var gotFatal bool
			if err != nil {
				var exitErr *exec.ExitError
				ok := errors.As(err, &exitErr)
				if !ok {
					t.Fatalf("Failed to run subprocess: %v", err)
				}
				gotFatal = !exitErr.Success()
			}
			if gotFatal != tc.wantFatal {
				t.Errorf("newTestNodeIpamController(%v, %v, %v, %v) : gotFatal = %t ; wantFatal = %t", clusterCidrs, serviceCIDRIpNet, tc.maskSize, tc.allocatorType, gotFatal, tc.wantFatal)
			}
		})
	}
}
