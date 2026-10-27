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
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	cloudproviderapi "k8s.io/cloud-provider/api"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/internal/testutil/imdstest"
	"sigs.k8s.io/cloud-provider-azure/pkg/nodemanager"
	azureprovider "sigs.k8s.io/cloud-provider-azure/pkg/provider"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
)

const (
	composedNodeName   = "vm1"
	composedProviderID = "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0"
	composedOldPrivate = "10.0.0.4"
	composedNewPrivate = "10.0.0.5"
	composedPublicA    = "20.0.0.1"
	composedPublicB    = "20.0.0.9"
	composedEmptyLB    = `{"loadbalancer":{"publicIpAddresses":[]}}`
)

var (
	composedHostname    = v1.NodeAddress{Type: v1.NodeHostName, Address: composedNodeName}
	composedOldInternal = v1.NodeAddress{Type: v1.NodeInternalIP, Address: composedOldPrivate}
	composedNewInternal = v1.NodeAddress{Type: v1.NodeInternalIP, Address: composedNewPrivate}
	composedExternalA   = v1.NodeAddress{Type: v1.NodeExternalIP, Address: composedPublicA}
	composedExternalB   = v1.NodeAddress{Type: v1.NodeExternalIP, Address: composedPublicB}
)

func composedInstanceBody(privateIP string) string {
	return fmt.Sprintf(`{"compute":{"name":%q,"vmSize":"Standard_D2s_v3"},`+
		`"network":{"interface":[{"ipv4":{"ipAddress":[{"privateIpAddress":%q,"publicIpAddress":""}]}}]}}`,
		composedNodeName, privateIP)
}

func composedLBBody(publicIP, privateIP string) string {
	return fmt.Sprintf(`{"loadbalancer":{"publicIpAddresses":[{"frontendIpAddress":%q,"privateIpAddress":%q}]}}`, publicIP, privateIP)
}

// composedStage scripts the IMDS responses for one UpdateNodeStatus call and
// the expected outcome.
type composedStage struct {
	desc      string
	privateIP string
	// instanceStatus, when non-zero, is returned for instance requests.
	instanceStatus int
	// lbStatus, when non-zero, is returned for loadbalancer requests instead of lbBody.
	lbStatus int
	lbBody   string
	// providedIP, when set, is written to the provided node IP annotation before the stage.
	providedIP *string
	// reuseMetadata keeps the metadata service, and thus its cached entry, from the
	// previous stage. Otherwise a new service is created so that the stage performs
	// exactly one real cache fill without waiting for the TTL to expire.
	reuseMetadata bool

	expectedAddresses []v1.NodeAddress
	expectedPatches   int
	expectedInstance  int32
	expectedLB        int32
}

// imdsHandlers returns the scripted instance and load balancer metadata handlers
// for the stage. The handlers capture a copy of the stage.
func (stage composedStage) imdsHandlers() (instance, lb http.HandlerFunc) {
	instance = func(w http.ResponseWriter, _ *http.Request) {
		if stage.instanceStatus != 0 {
			w.WriteHeader(stage.instanceStatus)
			return
		}
		_, _ = fmt.Fprint(w, composedInstanceBody(stage.privateIP))
	}
	lb = func(w http.ResponseWriter, _ *http.Request) {
		if stage.lbStatus != 0 {
			w.WriteHeader(stage.lbStatus)
			return
		}
		_, _ = fmt.Fprint(w, stage.lbBody)
	}
	return instance, lb
}

// composedWarmStage publishes ExternalIP A through healthy metadata.
var composedWarmStage = composedStage{
	desc:              "healthy loadbalancer metadata should publish the ExternalIP",
	privateIP:         composedOldPrivate,
	lbBody:            composedLBBody(composedPublicA, composedOldPrivate),
	expectedAddresses: []v1.NodeAddress{composedHostname, composedOldInternal, composedExternalA},
	expectedPatches:   1,
	expectedInstance:  1,
	expectedLB:        1,
}

// composedHarness wires a scripted IMDS server through the real metadata service,
// Cloud and IMDSNodeProvider into a real CloudNodeController backed by a fake client.
// The node informer is not started; its indexer is synced from the client explicitly.
type composedHarness struct {
	t          *testing.T
	imds       *imdstest.Server
	client     *fake.Clientset
	indexer    cache.Indexer
	cloud      *azureprovider.Cloud
	controller *nodemanager.CloudNodeController
}

func newComposedHarness(t *testing.T, recorded []v1.NodeAddress) *composedHarness {
	h := &composedHarness{t: t}
	h.imds = imdstest.NewServer(t, nil, nil, false)

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: composedNodeName},
		Spec:       v1.NodeSpec{ProviderID: composedProviderID},
		Status:     v1.NodeStatus{Addresses: recorded},
	}
	h.client = fake.NewSimpleClientset(node)
	nodeInformer := informers.NewSharedInformerFactory(h.client, 0).Core().V1().Nodes()
	h.indexer = nodeInformer.Informer().GetIndexer()
	h.cloud = &azureprovider.Cloud{
		Config: config.Config{UseInstanceMetadata: true},
	}
	h.controller = nodemanager.NewCloudNodeController(composedNodeName, nodeInformer, h.client,
		&IMDSNodeProvider{azure: h.cloud}, time.Minute, false, false)
	h.syncInformer()
	return h
}

func (h *composedHarness) getNode() *v1.Node {
	h.t.Helper()
	node, err := h.client.CoreV1().Nodes().Get(context.Background(), composedNodeName, metav1.GetOptions{})
	if err != nil {
		h.t.Fatalf("failed to get node: %v", err)
	}
	return node
}

func (h *composedHarness) syncInformer() {
	h.t.Helper()
	if err := h.indexer.Update(h.getNode()); err != nil {
		h.t.Fatalf("failed to sync node informer: %v", err)
	}
}

func (h *composedHarness) statusPatches() int {
	count := 0
	for _, action := range h.client.Actions() {
		if action.GetVerb() == "patch" && action.GetResource().Resource == "nodes" && action.GetSubresource() == "status" {
			count++
		}
	}
	return count
}

func (h *composedHarness) run(stage composedStage) {
	t := h.t
	t.Helper()

	if stage.providedIP != nil {
		node := h.getNode()
		node.Annotations = map[string]string{cloudproviderapi.AnnotationAlphaProvidedIPAddr: *stage.providedIP}
		if _, err := h.client.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("failed to annotate node: %v", err)
		}
		h.syncInformer()
	}
	if !stage.reuseMetadata {
		ims, err := azureprovider.NewInstanceMetadataService(h.imds.URL)
		if err != nil {
			t.Fatalf("failed to create instance metadata service: %v", err)
		}
		h.cloud.Metadata = ims
	}
	instance, lb := stage.imdsHandlers()
	h.imds.SetInstanceHandler(instance)
	h.imds.SetLoadBalancerHandler(lb)
	h.imds.ResetCalls()
	h.client.ClearActions()
	before := h.getNode()

	h.controller.UpdateNodeStatus(context.Background())
	h.syncInformer()

	after := h.getNode()
	assert.Equal(t, stage.expectedAddresses, after.Status.Addresses, "%s: node addresses", stage.desc)
	assert.Equal(t, stage.expectedPatches, h.statusPatches(), "%s: node status patches", stage.desc)
	assert.Equal(t, stage.expectedInstance, h.imds.InstanceCalls(), "%s: instance metadata requests", stage.desc)
	assert.Equal(t, stage.expectedLB, h.imds.LBCalls(), "%s: loadbalancer metadata requests", stage.desc)
	assert.Equal(t, before.Annotations, after.Annotations, "%s: node annotations", stage.desc)
	assert.Equal(t, before.Spec, after.Spec, "%s: node spec", stage.desc)
}

func TestCloudNodeControllerUpdateNodeStatusIMDSSequence(t *testing.T) {
	h := newComposedHarness(t, []v1.NodeAddress{composedHostname, composedOldInternal})
	for _, stage := range []composedStage{
		composedWarmStage,
		{
			desc:              "loadbalancer 503 with an unchanged private IP should keep the ExternalIP",
			privateIP:         composedOldPrivate,
			lbStatus:          http.StatusServiceUnavailable,
			expectedAddresses: []v1.NodeAddress{composedHostname, composedOldInternal, composedExternalA},
			expectedInstance:  1,
			expectedLB:        1,
		},
		{
			desc:              "cached loadbalancer 503 should keep the ExternalIP without IMDS requests or patches",
			reuseMetadata:     true,
			expectedAddresses: []v1.NodeAddress{composedHostname, composedOldInternal, composedExternalA},
		},
		{
			desc:              "loadbalancer 503 with a changed private IP should update it and keep the ExternalIP",
			privateIP:         composedNewPrivate,
			lbStatus:          http.StatusServiceUnavailable,
			expectedAddresses: []v1.NodeAddress{composedHostname, composedNewInternal, composedExternalA},
			expectedPatches:   1,
			expectedInstance:  1,
			expectedLB:        1,
		},
		{
			desc:              "recovered loadbalancer metadata should replace the ExternalIP",
			privateIP:         composedNewPrivate,
			lbBody:            composedLBBody(composedPublicB, composedNewPrivate),
			expectedAddresses: []v1.NodeAddress{composedHostname, composedNewInternal, composedExternalB},
			expectedPatches:   1,
			expectedInstance:  1,
			expectedLB:        1,
		},
		{
			desc:              "healthy empty loadbalancer metadata should remove the ExternalIP",
			privateIP:         composedNewPrivate,
			lbBody:            composedEmptyLB,
			expectedAddresses: []v1.NodeAddress{composedHostname, composedNewInternal},
			expectedPatches:   1,
			expectedInstance:  1,
			expectedLB:        1,
		},
	} {
		h.run(stage)
	}
}

func TestCloudNodeControllerUpdateNodeStatusIMDSEdgeCases(t *testing.T) {
	for _, tc := range []composedStage{
		{
			desc:              "benign loadbalancer 404 should remove the ExternalIP",
			privateIP:         composedOldPrivate,
			lbStatus:          http.StatusNotFound,
			expectedAddresses: []v1.NodeAddress{composedHostname, composedOldInternal},
			expectedPatches:   1,
			expectedInstance:  1,
			expectedLB:        1,
		},
		{
			desc:              "loadbalancer 410 should keep the ExternalIP",
			privateIP:         composedOldPrivate,
			lbStatus:          http.StatusGone,
			expectedAddresses: []v1.NodeAddress{composedHostname, composedOldInternal, composedExternalA},
			expectedInstance:  1,
			expectedLB:        1,
		},
		{
			desc:              "loadbalancer 503 with a valid provided private IP should update it and keep the ExternalIP",
			privateIP:         composedNewPrivate,
			lbStatus:          http.StatusServiceUnavailable,
			providedIP:        ptr.To(composedNewPrivate),
			expectedAddresses: []v1.NodeAddress{composedHostname, composedNewInternal, composedExternalA},
			expectedPatches:   1,
			expectedInstance:  1,
			expectedLB:        1,
		},
		{
			desc:              "loadbalancer 503 with a stale provided private IP should not patch the node",
			privateIP:         composedNewPrivate,
			lbStatus:          http.StatusServiceUnavailable,
			providedIP:        ptr.To(composedOldPrivate),
			expectedAddresses: []v1.NodeAddress{composedHostname, composedOldInternal, composedExternalA},
			expectedInstance:  1,
			expectedLB:        1,
		},
		{
			desc:              "instance metadata failure should not patch the node or request loadbalancer metadata",
			instanceStatus:    http.StatusServiceUnavailable,
			expectedAddresses: []v1.NodeAddress{composedHostname, composedOldInternal, composedExternalA},
			expectedInstance:  1,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			h := newComposedHarness(t, []v1.NodeAddress{composedHostname, composedOldInternal})
			h.run(composedWarmStage)
			h.run(tc)
		})
	}
}
