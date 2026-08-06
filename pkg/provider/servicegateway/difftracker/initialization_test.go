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

package difftracker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/component-base/metrics/testutil"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/loadbalancerclient/mock_loadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/natgatewayclient/mock_natgatewayclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/publicipaddressclient/mock_publicipaddressclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/servicegatewayclient/mock_servicegatewayclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func TestNewIgnoreCaseSetFromSlice_Duplicates(t *testing.T) {
	items := []string{"service1", "SERVICE1", "service1", "service2"}
	set := newIgnoreCaseSetFromSlice(items)

	// Should deduplicate case-insensitively
	assert.Equal(t, 2, set.Len())
	assert.True(t, set.Has("service1"))
	assert.True(t, set.Has("service2"))
}

func TestExtractInboundConfigFromService_MixedTargetPorts(t *testing.T) {
	service := createTestService("mixed-service", []servicePort{
		{name: "http", port: 80, targetPort: intstr.FromInt(8080), protocol: "TCP"},
		{name: "https", port: 443, targetPort: intstr.IntOrString{}, protocol: "TCP"},       // Unset
		{name: "dns", port: 53, targetPort: intstr.FromString("dns-port"), protocol: "UDP"}, // Named
	})

	config := ExtractInboundConfigFromService(service)

	assert.NotNil(t, config)
	assert.Len(t, config.FrontendPorts, 3)
	assert.Len(t, config.BackendPorts, 3)

	// HTTP: should use TargetPort
	assert.Equal(t, int32(80), config.FrontendPorts[0].Port)
	assert.Equal(t, int32(8080), config.BackendPorts[0].Port)

	// HTTPS: should fall back to Port
	assert.Equal(t, int32(443), config.FrontendPorts[1].Port)
	assert.Equal(t, int32(443), config.BackendPorts[1].Port)

	// DNS: named port should fall back to Port
	assert.Equal(t, int32(53), config.FrontendPorts[2].Port)
	assert.Equal(t, int32(53), config.BackendPorts[2].Port)
	assert.Equal(t, "UDP", config.FrontendPorts[2].Protocol)
}

func TestBuildInboundServiceResources_MismatchedPortCounts(t *testing.T) {
	// Frontend has more ports than backend (edge case)
	config := &InboundConfig{
		FrontendPorts: []PortMapping{
			{Port: 80, Protocol: "TCP"},
			{Port: 443, Protocol: "TCP"},
			{Port: 8080, Protocol: "TCP"},
		},
		BackendPorts: []PortMapping{
			{Port: 8000, Protocol: "TCP"},
			{Port: 8443, Protocol: "TCP"},
		},
	}

	dtConfig := Config{
		SubscriptionID:             "test-sub",
		ResourceGroup:              "test-rg",
		Location:                   "eastus",
		ServiceGatewayResourceName: "test-sgw",
	}

	_, lb, _, err := buildInboundServiceResources("test-service", config, dtConfig)
	assert.NoError(t, err)

	// Should create 3 rules
	assert.Len(t, lb.Properties.LoadBalancingRules, 3)

	// First two should use backend ports
	assert.Equal(t, int32(80), *lb.Properties.LoadBalancingRules[0].Properties.FrontendPort)
	assert.Equal(t, int32(8000), *lb.Properties.LoadBalancingRules[0].Properties.BackendPort)

	assert.Equal(t, int32(443), *lb.Properties.LoadBalancingRules[1].Properties.FrontendPort)
	assert.Equal(t, int32(8443), *lb.Properties.LoadBalancingRules[1].Properties.BackendPort)

	// Third should fall back to frontend port
	assert.Equal(t, int32(8080), *lb.Properties.LoadBalancingRules[2].Properties.FrontendPort)
	assert.Equal(t, int32(8080), *lb.Properties.LoadBalancingRules[2].Properties.BackendPort)
}

func TestBuildInboundServiceResources_EmptyConfig(t *testing.T) {
	config := &InboundConfig{
		FrontendPorts: []PortMapping{},
		BackendPorts:  []PortMapping{},
	}

	dtConfig := Config{
		SubscriptionID:             "test-sub",
		ResourceGroup:              "test-rg",
		Location:                   "eastus",
		ServiceGatewayResourceName: "test-sgw",
	}

	_, lb, _, err := buildInboundServiceResources("test-service", config, dtConfig)
	assert.NoError(t, err)

	// Should create LB with no rules (empty config is valid)
	assert.Empty(t, lb.Properties.LoadBalancingRules)
	assert.Len(t, lb.Properties.BackendAddressPools, 1)
}

func TestBuildInboundServiceResources_LongServiceUID(t *testing.T) {
	// Test with very long service UID
	longUID := "very-long-service-uid-that-exceeds-normal-length-abcdef123456789012345678901234567890"

	config := &InboundConfig{
		FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}},
		BackendPorts:  []PortMapping{{Port: 8080, Protocol: "TCP"}},
	}

	dtConfig := Config{
		SubscriptionID:             "test-sub",
		ResourceGroup:              "test-rg",
		Location:                   "eastus",
		ServiceGatewayResourceName: "test-sgw",
	}

	pip, lb, _, err := buildInboundServiceResources(longUID, config, dtConfig)
	assert.NoError(t, err)

	// Should handle long UIDs without truncation
	assert.Equal(t, longUID, *lb.Name)
	assert.Equal(t, longUID+"-pip", *pip.Name)
	assert.Equal(t, longUID, *lb.Properties.BackendAddressPools[0].Name)
}

func TestBuildOutboundServiceResources_NilConfig(t *testing.T) {
	// OutboundConfig is currently not used but test nil handling
	dtConfig := Config{
		SubscriptionID:             "test-sub",
		ResourceGroup:              "test-rg",
		Location:                   "westus",
		ServiceGatewayResourceName: "test-sgw",
	}

	pips, natGw, servicesDTO := buildOutboundServiceResources("egress-123", nil, dtConfig)
	pip := pips[0]

	// Should create resources even with nil config
	assert.NotNil(t, pip)
	assert.NotNil(t, natGw)
	assert.NotNil(t, servicesDTO)
	assert.Equal(t, "egress-123-pip", *pip.Name)
	assert.Equal(t, "egress-123", *natGw.Name)
}

func TestBuildInboundServiceResources_MultipleConfigs(t *testing.T) {
	// Test that multiple invocations produce independent resources
	config1 := &InboundConfig{
		FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}},
		BackendPorts:  []PortMapping{{Port: 8080, Protocol: "TCP"}},
	}

	config2 := &InboundConfig{
		FrontendPorts: []PortMapping{{Port: 443, Protocol: "TCP"}},
		BackendPorts:  []PortMapping{{Port: 8443, Protocol: "TCP"}},
	}

	dtConfig := Config{
		SubscriptionID:             "test-sub",
		ResourceGroup:              "test-rg",
		Location:                   "eastus",
		ServiceGatewayResourceName: "test-sgw",
	}

	pip1, lb1, _, err := buildInboundServiceResources("service-1", config1, dtConfig)
	assert.NoError(t, err)
	pip2, lb2, _, err := buildInboundServiceResources("service-2", config2, dtConfig)
	assert.NoError(t, err)

	// Should produce different resources
	assert.NotEqual(t, *pip1.Name, *pip2.Name)
	assert.NotEqual(t, *lb1.Name, *lb2.Name)

	// Each should have correct config
	assert.Equal(t, int32(80), *lb1.Properties.LoadBalancingRules[0].Properties.FrontendPort)
	assert.Equal(t, int32(443), *lb2.Properties.LoadBalancingRules[0].Properties.FrontendPort)
}

// TestNewIgnoreCaseSetFromSlice_IsOrderIndependent pins that membership does not depend on the
// order of the input slice. The previous name claimed the constructor "preserves order", which it
// neither does nor is asserted here - the set is backed by a map and has no defined iteration order.
func TestNewIgnoreCaseSetFromSlice_IsOrderIndependent(t *testing.T) {
	// Order must not matter for set membership.
	items1 := []string{"a", "b", "c"}
	items2 := []string{"c", "b", "a"}

	set1 := newIgnoreCaseSetFromSlice(items1)
	set2 := newIgnoreCaseSetFromSlice(items2)

	// Both should contain same elements
	assert.Equal(t, set1.Len(), set2.Len())
	for _, item := range items1 {
		assert.True(t, set1.Has(item))
		assert.True(t, set2.Has(item))
	}
}

// Helper types and functions for tests

type servicePort struct {
	name       string
	port       int32
	targetPort intstr.IntOrString
	protocol   string
}

func createTestService(name string, ports []servicePort) *v1.Service {
	v1Ports := make([]v1.ServicePort, len(ports))
	for i, p := range ports {
		protocol := v1.ProtocolTCP
		if p.protocol == "UDP" {
			protocol = v1.ProtocolUDP
		}
		v1Ports[i] = v1.ServicePort{
			Name:       p.name,
			Port:       p.port,
			TargetPort: p.targetPort,
			Protocol:   protocol,
		}
	}

	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: v1.ServiceSpec{
			Ports: v1Ports,
		},
	}
}

// newK8sStateForSeeders returns an empty K8sState suitable for the processK8s* seeders.
func newK8sStateForSeeders(trackedServices ...string) K8sState {
	return K8sState{
		Services: utilsets.NewString(trackedServices...),
		Egresses: utilsets.NewString(),
		Nodes:    make(map[string]Node),
	}
}

// newServiceOwnedEndpointSlice builds an EndpointSlice owned by the given Service UID;
// extractServiceUIDFromEndpointSlice resolves ownership via the Service OwnerReference.
func newServiceOwnedEndpointSlice(name, namespace, svcUID string, addrType discoveryv1.AddressType, endpoints []discoveryv1.Endpoint) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "Service", Name: name, UID: types.UID(svcUID)},
			},
		},
		AddressType: addrType,
		Endpoints:   endpoints,
	}
}

// newEgressPod builds an egress-labeled pod for the processK8sEgresses seeder. hostIP is the
// node's IP that the runtime egress path uses as the location key (pod.Status.HostIP).
func newEgressPod(name, namespace, egressVal, nodeName, podIP, hostIP string, phase v1.PodPhase) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{consts.PodLabelServiceEgressGateway: egressVal},
		},
		Spec:   v1.PodSpec{NodeName: nodeName},
		Status: v1.PodStatus{Phase: phase, PodIP: podIP, HostIP: hostIP},
	}
}

// podIPTracked reports whether any node in K8s state tracks a pod at the given IP.
func podIPTracked(k8s *K8sState, podIP string) bool {
	for _, node := range k8s.Nodes {
		if _, ok := node.Pods[podIP]; ok {
			return true
		}
	}
	return false
}

// TestProcessK8sEndpoints_SkipsNotReadyEndpoints verifies the cold-start seeder excludes
// EndpointSlice endpoints whose Conditions.Ready==false (a nil Ready is treated as ready),
// matching the runtime informer filter in azure_local_services.go. Without this, a CCM restart
// would import not-ready pod IPs as LoadBalancer backends that the runtime diff can never remove.
func TestProcessK8sEndpoints_SkipsNotReadyEndpoints(t *testing.T) {
	const (
		svcUID     = "svc-unready"
		nodeName   = "node-1"
		nodeIP     = "10.0.0.5"
		readyIP    = "10.244.0.10"
		notReadyIP = "10.244.0.11"
	)

	k8s := newK8sStateForSeeders(svcUID)
	nodeNameToIPMap := map[string][]string{nodeName: {nodeIP}}

	eps := newServiceOwnedEndpointSlice("eps-1", "default", svcUID, discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{
			Addresses:  []string{readyIP},
			NodeName:   ptr.To(nodeName),
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
		},
		{
			Addresses:  []string{notReadyIP},
			NodeName:   ptr.To(nodeName),
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(false)},
		},
	})
	kube := fake.NewSimpleClientset(eps)

	_, err := processK8sEndpoints(context.Background(), kube, &k8s, nodeNameToIPMap)
	assert.NoError(t, err)

	assert.True(t, podIPTracked(&k8s, readyIP),
		"a ready endpoint must be imported into K8s state at init")
	assert.False(t, podIPTracked(&k8s, notReadyIP),
		"an endpoint with Conditions.Ready==false must not be imported into K8s state at init")
}

// TestInitOutboundRefCount_NotNegativeOnServiceUIDEgressLabelCollision verifies the outbound
// ref-counter is seeded solely from real egress pods (in New()) and never goes negative when an
// inbound LoadBalancer service UID happens to equal a pod egress label. A negative seed would
// trip DeletePod's `counter <= 0` guard and strand the pod (and its NAT gateway).
func TestInitOutboundRefCount_NotNegativeOnServiceUIDEgressLabelCollision(t *testing.T) {
	const (
		collideID = "collide-id" // serves as BOTH the inbound svc UID and the egress label
		nodeName  = "node-1"
		nodeIP    = "10.0.0.21"
		podIP     = "10.244.3.3"
	)

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "svc-collide",
			Namespace: "default",
			UID:       types.UID(collideID),
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	pod := newEgressPod("pod-collide", "default", collideID, nodeName, podIP, nodeIP, v1.PodRunning)
	kube := fake.NewSimpleClientset(svc, pod)

	k8s := newK8sStateForSeeders()

	// Build the cold-start K8s state: inbound LB service + colliding egress pod.
	_, _, err := processK8sServices(context.Background(), kube, &k8s)
	assert.NoError(t, err)
	_, err = processK8sEgresses(context.Background(), kube, &k8s)
	assert.NoError(t, err)

	// New() seeds outboundIdentityPodRefCount from the egress pods now present in k8s state.
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	nrp := NRPState{
		LoadBalancers: utilsets.NewString(),
		NATGateways:   utilsets.NewString(),
		Locations:     map[string]NRPLocation{},
	}
	cfg := Config{
		SubscriptionID:             "sub",
		ResourceGroup:              "rg",
		Location:                   "loc",
		ServiceGatewayResourceName: "sgw",
		VNetName:                   "vnet",
	}
	dt, err := New(logr.Discard(), k8s, nrp, cfg, mock_azclient.NewMockClientFactory(ctrl), kube)
	if !assert.NoError(t, err) {
		t.FailNow()
	}

	v, ok := dt.outboundIdentityPodRefCount.Load(strings.ToLower(collideID))
	assert.True(t, ok, "the colliding egress identity must be seeded")
	assert.GreaterOrEqual(t, v.(int), 0,
		"an inbound-UID/egress-label collision must not seed a negative outbound ref-count (got %v)", v)
	assert.Equal(t, 1, v.(int),
		"the outbound ref-count must equal the egress pod count (1)")

	// A correct (non-negative) counter must allow DeletePod to complete.
	res := dt.DeletePod(collideID, nodeIP, []string{podIP}, "default", "pod-collide", "")
	assert.True(t, res.IsLastPod,
		"with exactly one live egress pod the ref-count must be 1 (last pod)")
	assert.False(t, podIPTracked(&dt.K8sResources, podIP),
		"DeletePod must remove the egress pod")
}

// TestRecoverStuckFinalizers_KeepsFinalizerWhenAzureResourceExists verifies that a service whose
// PIP/LB was created in Azure but never registered with ServiceGateway (a crash between LB-create and
// SGW-register) keeps its cleanup finalizer during recovery. NRPResources does not list it, but the
// Azure LB enumeration does, so the finalizer is preserved as the anchor and the orphan cleanup
// deletes the resource and removes the finalizer in the correct order.
func TestRecoverStuckFinalizers_KeepsFinalizerWhenAzureResourceExists(t *testing.T) {
	uid := "uid-crashwindow"
	delTime := metav1.Now()
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc-crash", Namespace: "default", UID: types.UID(uid),
			DeletionTimestamp: &delTime,
			Finalizers:        []string{ServiceGatewayServiceCleanupFinalizer},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	kube := fake.NewSimpleClientset(svc)
	dt := newTestDiffTracker()
	dt.kubeClient = kube
	// NRPResources does NOT have the UID (registration never completed) - the crash window.
	services := &v1.ServiceList{Items: []v1.Service{*svc}}

	// A real Azure LB exists for the UID even though it is absent from NRPResources.
	recoverStuckFinalizers(context.Background(), dt, services, nil, nil, utilsets.NewString(uid), utilsets.NewString(), nil)

	got, err := kube.CoreV1().Services("default").Get(context.Background(), "svc-crash", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.True(t, hasServiceGatewayFinalizer(got),
		"finalizer must be kept while a real Azure resource exists so cleanup can remove it after deleting the resource")
}

// TestRecoverStuckFinalizers_KeepsFinalizerWhenOnlyPIPExists covers the PIP-only crash window: the
// Public IP was created but the LB was not, so the UID is absent from both NRPResources and the LB
// enumeration, yet the {uid}-pip Public IP exists in Azure. The finalizer must still be preserved.
func TestRecoverStuckFinalizers_KeepsFinalizerWhenOnlyPIPExists(t *testing.T) {
	uid := "uid-pip-only"
	delTime := metav1.Now()
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc-pip", Namespace: "default", UID: types.UID(uid),
			DeletionTimestamp: &delTime,
			Finalizers:        []string{ServiceGatewayServiceCleanupFinalizer},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	kube := fake.NewSimpleClientset(svc)
	dt := newTestDiffTracker()
	dt.kubeClient = kube
	services := &v1.ServiceList{Items: []v1.Service{*svc}}

	// The PIP exists in Azure but its static address has not been allocated yet (nil IPAddress) - the
	// crash-after-PIP-create window. Existence must still be recognized (by name, via
	// pipNamesInAzureFromList) so the Service finalizer is preserved for orphan cleanup.
	azurePIPs := []*armnetwork.PublicIPAddress{{Name: ptr.To(uid + "-pip")}}
	recoverStuckFinalizers(context.Background(), dt, services, nil, nil, utilsets.NewString(), utilsets.NewString(), pipNamesInAzureFromList(azurePIPs))

	got, err := kube.CoreV1().Services("default").Get(context.Background(), "svc-pip", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.True(t, hasServiceGatewayFinalizer(got),
		"finalizer must be kept while a real Azure Public IP exists for the service")
}

// TestPIPNamesInAzureFromList_IncludesAddressLessPIP verifies the PIP existence oracle counts a PIP
// whose static address has not been allocated yet (nil IPAddress, or nil Properties). pipNameToIP
// omits these, so using it for existence would leak an address-less PIP and prematurely strip its
// Service finalizer (the crash-after-PIP-create window).
func TestPIPNamesInAzureFromList_IncludesAddressLessPIP(t *testing.T) {
	pips := []*armnetwork.PublicIPAddress{
		{Name: ptr.To("alloc-pip"), Properties: &armnetwork.PublicIPAddressPropertiesFormat{IPAddress: ptr.To("1.2.3.4")}},
		{Name: ptr.To("pending-pip"), Properties: &armnetwork.PublicIPAddressPropertiesFormat{}}, // address not allocated yet
		{Name: ptr.To("nilprops-pip")},                              // no Properties at all
		{Properties: &armnetwork.PublicIPAddressPropertiesFormat{}}, // no Name -> skipped
	}

	names := pipNamesInAzureFromList(pips)

	assert.True(t, names.Has("alloc-pip"), "an allocated PIP must be counted as existing")
	assert.True(t, names.Has("pending-pip"), "an address-less PIP must be counted as existing")
	assert.True(t, names.Has("nilprops-pip"), "a PIP with nil Properties must be counted as existing")
	assert.Equal(t, 3, names.Len(), "a nameless PIP must be skipped")
}

// TestRecoverStuckFinalizers_RemovesFinalizerWhenNoAzureResource verifies the complementary case: a
// stuck finalizer with no Azure resource anywhere (not in ServiceGateway, not in the Azure LB/NAT/PIP
// enumeration) is removed directly, since there is nothing to clean up.
func TestRecoverStuckFinalizers_RemovesFinalizerWhenNoAzureResource(t *testing.T) {
	uid := "uid-noresource"
	delTime := metav1.Now()
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc-clean", Namespace: "default", UID: types.UID(uid),
			DeletionTimestamp: &delTime,
			Finalizers:        []string{ServiceGatewayServiceCleanupFinalizer},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	kube := fake.NewSimpleClientset(svc)
	dt := newTestDiffTracker()
	dt.kubeClient = kube
	services := &v1.ServiceList{Items: []v1.Service{*svc}}

	recoverStuckFinalizers(context.Background(), dt, services, nil, nil, utilsets.NewString(), utilsets.NewString(), nil)

	got, err := kube.CoreV1().Services("default").Get(context.Background(), "svc-clean", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.False(t, hasServiceGatewayFinalizer(got),
		"finalizer with no Azure resource must be removed since there is nothing to clean up")
}

// TestRecoverStuckFinalizers_SeedsAllDualStackAddresses verifies that a Terminating dual-stack egress
// pod recovered at cold start is seeded with EVERY IP family, so its drain-gated finalizer is held
// until both families leave NRP (a single-address seed would release the finalizer while the secondary
// family is still mapped).
func TestRecoverStuckFinalizers_SeedsAllDualStackAddresses(t *testing.T) {
	delTime := metav1.Now()
	const v4, v6, hostIP = "10.244.9.1", "fd00::9", "10.0.0.60"
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "ds-terminating",
			Namespace:         "default",
			UID:               types.UID("uid-ds-term"),
			DeletionTimestamp: &delTime,
			Finalizers:        []string{ServiceGatewayPodCleanupFinalizer},
			Labels:            map[string]string{consts.PodLabelServiceEgressGateway: "corp-egress"},
		},
		Status: v1.PodStatus{HostIP: hostIP, PodIP: v4, PodIPs: []v1.PodIP{{IP: v4}, {IP: v6}}},
	}
	kube := fake.NewSimpleClientset(pod)
	dt := newTestDiffTracker()
	dt.kubeClient = kube

	egressPods := &v1.PodList{Items: []v1.Pod{*pod}}
	recoverStuckFinalizers(context.Background(), dt, nil, egressPods, nil, utilsets.NewString(), utilsets.NewString(), nil)

	entry := dt.pendingPodDeletions["default/ds-terminating"]
	if assert.NotNil(t, entry, "a Terminating dual-stack egress pod must be seeded for drain-gated finalizer removal") {
		assert.ElementsMatch(t, []string{v4, v6}, entry.Addresses,
			"recovery must seed every IP family so the finalizer waits for both to drain")
		assert.Equal(t, "corp-egress", entry.ServiceUID)
	}
}

func TestRecoverStuckFinalizers_NoIPPodUsesServiceDrainVerification(t *testing.T) {
	delTime := metav1.Now()
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "no-ip-terminating",
			Namespace:         "default",
			UID:               types.UID("uid-no-ip"),
			DeletionTimestamp: &delTime,
			Finalizers:        []string{ServiceGatewayPodCleanupFinalizer},
			Labels:            map[string]string{consts.PodLabelServiceEgressGateway: "corp-egress"},
		},
		Status: v1.PodStatus{Phase: v1.PodFailed},
	}
	kube := fake.NewSimpleClientset(pod)
	dt := newTestDiffTracker()
	dt.kubeClient = kube
	dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{
		Addresses: map[string]NRPAddress{
			"10.244.9.1": {Services: utilsets.NewString("corp-egress")},
		},
	}

	egressPods := &v1.PodList{Items: []v1.Pod{*pod}}
	recoverStuckFinalizers(context.Background(), dt, nil, egressPods, nil, utilsets.NewString(), utilsets.NewString(), nil)

	got, err := kube.CoreV1().Pods("default").Get(context.Background(), "no-ip-terminating", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Contains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer,
		"restart recovery must not release a no-IP pod while NRP still has an unbacked service address")
	entry := dt.pendingPodDeletions["default/no-ip-terminating"]
	if assert.NotNil(t, entry) {
		assert.True(t, entry.VerifyServiceDrain)
		assert.Empty(t, entry.Addresses)
		assert.Equal(t, "corp-egress", entry.ServiceUID)
	}
}

// TestCheckInitializationComplete_ParkedOpDoesNotBlockCompletion verifies that
// checkInitializationCompleteLocked does NOT count a transient-failure-parked op (RetriesExhausted=true,
// CreationFailedTerminal=false) as pending. Such an op self-heals in the background (retryGate cooldown
// re-arm), so it must not hold initial sync open: the real caller waits on a no-timeout context, and a
// single un-provisionable service would otherwise stall the whole cloud-provider init.
func TestCheckInitializationComplete_ParkedOpDoesNotBlockCompletion(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-init-parked"

	// Set up the initialization state the engine uses during cold start.
	atomic.StoreInt32(&dt.isInitializing, 1)
	dt.initCompletionChecker = make(chan struct{})

	// Retry-exhausted parked op: retryGate skips it and it is not terminal, but it must not block init.
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:             uid,
		Config:                 NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:                  StateCreationInProgress,
		RetryCount:             maxServiceRetries,
		RetriesExhausted:       true,
		CreationFailedTerminal: false,
		NextRetryAt:            time.Now().Add(time.Hour),
	}

	dt.checkInitializationComplete()
	select {
	case <-dt.initCompletionChecker:
	default:
		t.Fatal("init must complete when the only pending op is retry-exhausted parked")
	}
	assert.Equal(t, int32(0), atomic.LoadInt32(&dt.isInitializing), "isInitializing must be cleared on completion")

	// WaitForInitialSync therefore returns successfully rather than draining its context.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	assert.NoError(t, dt.WaitForInitialSync(ctx), "WaitForInitialSync must complete despite the parked op")
}

// TestShouldTriggerInitialLocationSync checks that an already-tracked NRP service forces the initial
// location sync even when additions are present and may all terminal-park (so their
// OnServiceCreationComplete callbacks never fire).
func TestShouldTriggerInitialLocationSync(t *testing.T) {
	// Additions present (onlyExisting=false), no deletions or recovered items, but a service already
	// tracked in NRP must still force the sync.
	assert.True(t, shouldTriggerInitialLocationSync(false, false, false, true),
		"an existing NRP service must trigger the initial sync even when every addition may park")

	// No signal at all: nothing to sync.
	assert.False(t, shouldTriggerInitialLocationSync(false, false, false, false),
		"no deletions, additions, recovered items, or existing NRP services means no initial sync")

	// The pre-existing sufficient conditions each still trigger on their own.
	assert.True(t, shouldTriggerInitialLocationSync(true, false, false, false), "deletions must trigger")
	assert.True(t, shouldTriggerInitialLocationSync(false, true, false, false), "no additions (only existing services) must trigger")
	assert.True(t, shouldTriggerInitialLocationSync(false, false, true, false), "recovered finalizer items must trigger")
}

// TestCheckInitializationComplete_InProgressOpBlocksCompletion verifies the other direction: an op that is
// genuinely still in flight (not parked, not created) keeps initial sync open.
func TestCheckInitializationComplete_InProgressOpBlocksCompletion(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-init-inflight"

	atomic.StoreInt32(&dt.isInitializing, 1)
	dt.initCompletionChecker = make(chan struct{})

	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:      StateCreationInProgress,
	}

	dt.checkInitializationComplete()
	select {
	case <-dt.initCompletionChecker:
		t.Fatal("init must not complete while a genuinely in-flight op remains pending")
	default:
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&dt.isInitializing), "isInitializing must stay set while work is pending")
}

// TestEgressRefCount_Robust verifies the outbound (NAT Gateway) ref-count lifecycle:
//
//   - Two AddPod calls for the same egress identity and pod (an informer Add followed by an Update
//     that re-delivers the same egress label) are idempotent: the ref-count stays at 1.
//   - Removing the last pod marks the service for deletion exactly once (StateDeletionPending) and
//     drops the ref-count entry to 0.
//   - A subsequent stale DeletePod is a no-op and the ref-count never goes negative.
func TestEgressRefCount_Robust(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "egress-refcount"
	const node = "10.0.0.1"
	const podIP = "10.244.0.9"

	// Outbound service already exists in NRP (the AddPod no-tracking-yet path).
	dt.NRPResources.NATGateways.Insert(uid)

	// First AddPod registers the pod and bumps the counter to 1.
	dt.AddPod(uid, "ns/pod-a", node, podIP)
	v, ok := dt.outboundIdentityPodRefCount.Load(strings.ToLower(uid))
	if assert.True(t, ok, "first AddPod must register the egress identity in the ref-counter") {
		assert.Equal(t, 1, v.(int), "first AddPod must set the ref-count to 1")
	}

	// Second AddPod for the SAME identity + SAME pod must be idempotent: counter stays at 1.
	dt.AddPod(uid, "ns/pod-a", node, podIP)
	v, _ = dt.outboundIdentityPodRefCount.Load(strings.ToLower(uid))
	assert.Equal(t, 1, v.(int),
		"duplicate AddPod for the same egress identity + pod must be idempotent (ref-count stays at 1)")

	// Removing the last pod must report IsLastPod=true and drop the counter entry to 0
	// (the sync.Map key is removed on counter==1 by decrementOutboundRefCount).
	res := dt.DeletePod(uid, node, []string{podIP}, "ns", "pod-a", "")
	assert.True(t, res.IsLastPod, "removing the only pod must report IsLastPod=true exactly once")
	_, stillExists := dt.outboundIdentityPodRefCount.Load(strings.ToLower(uid))
	assert.False(t, stillExists,
		"last-pod removal must delete the ref-count key (counter went 1 → 0)")

	// The teardown path must run EXACTLY ONCE — the service is marked for deletion.
	op := dt.pendingServiceOps[uid]
	if assert.NotNil(t, op, "last-pod removal must synthesize a deletion-tracking entry") {
		assert.Equal(t, StateDeletionPending, op.State,
			"last-pod removal must mark the service StateDeletionPending exactly once")
	}
	_, queued := dt.pendingServiceDeletions[uid]
	assert.True(t, queued, "last-pod removal must enqueue PendingServiceDeletion exactly once")

	// A stale duplicate DeletePod must be a no-op (the pod is no longer in live state) and
	// must not drive the counter negative. We can also call AddPod again as the same pod
	// (synthesizing a same-name replacement) and re-delete it without a negative counter.
	dup := dt.DeletePod(uid, node, []string{podIP}, "ns", "pod-a", "")
	assert.False(t, dup.IsLastPod, "stale duplicate DeletePod must be a no-op (IsLastPod=false)")
	if v, ok := dt.outboundIdentityPodRefCount.Load(strings.ToLower(uid)); ok {
		if cnt, _ := v.(int); cnt < 0 {
			t.Fatalf("egress ref-count went negative after duplicate delete: %d", cnt)
		}
	}
}

// TestProcessK8sEgresses_SkipsTerminatedPhases verifies the cold-start seeder imports only egress
// pods in Running/Pending phase (matching podInformerAddPod): a Succeeded/Failed pod keeps its
// PodIP/HostIP until GC, so without the filter a restart programs a stale NRP address.
func TestProcessK8sEgresses_SkipsTerminatedPhases(t *testing.T) {
	const (
		egressVal   = "corp-egress"
		nodeName    = "node-1"
		hostIP      = "10.0.0.30"
		runningIP   = "10.244.5.1"
		pendingIP   = "10.244.5.2"
		succeededIP = "10.244.5.3"
		failedIP    = "10.244.5.4"
	)

	k8s := newK8sStateForSeeders()
	kube := fake.NewSimpleClientset(
		newEgressPod("pod-running", "default", egressVal, nodeName, runningIP, hostIP, v1.PodRunning),
		newEgressPod("pod-pending", "default", egressVal, nodeName, pendingIP, hostIP, v1.PodPending),
		newEgressPod("pod-succeeded", "default", egressVal, nodeName, succeededIP, hostIP, v1.PodSucceeded),
		newEgressPod("pod-failed", "default", egressVal, nodeName, failedIP, hostIP, v1.PodFailed),
	)

	_, err := processK8sEgresses(context.Background(), kube, &k8s)
	assert.NoError(t, err)

	assert.True(t, podIPTracked(&k8s, runningIP), "a Running egress pod must be imported")
	assert.True(t, podIPTracked(&k8s, pendingIP), "a Pending egress pod must be imported")
	assert.False(t, podIPTracked(&k8s, succeededIP), "a Succeeded egress pod must not be imported")
	assert.False(t, podIPTracked(&k8s, failedIP), "a Failed egress pod must not be imported")
}

// TestProcessK8sEgresses_SkipsMalformedIPs verifies the cold-start seeder rejects egress pods with a
// malformed HostIP/PodIP (matching podInformerAddPod), which would otherwise make NRP reject the batch.
func TestProcessK8sEgresses_SkipsMalformedIPs(t *testing.T) {
	const (
		egressVal = "corp-egress"
		nodeName  = "node-1"
		goodHost  = "10.0.0.40"
		goodPodIP = "10.244.6.1"
	)

	k8s := newK8sStateForSeeders()
	kube := fake.NewSimpleClientset(
		newEgressPod("pod-good", "default", egressVal, nodeName, goodPodIP, goodHost, v1.PodRunning),
		newEgressPod("pod-bad-podip", "default", egressVal, nodeName, "not-an-ip", goodHost, v1.PodRunning),
		newEgressPod("pod-bad-hostip", "default", egressVal, nodeName, "10.244.6.3", "bad-host", v1.PodRunning),
	)

	_, err := processK8sEgresses(context.Background(), kube, &k8s)
	assert.NoError(t, err)

	assert.True(t, podIPTracked(&k8s, goodPodIP), "an egress pod with valid IPs must be imported")
	assert.False(t, podIPTracked(&k8s, "10.244.6.3"), "an egress pod with a malformed HostIP must be skipped")
	assert.False(t, podIPTracked(&k8s, "not-an-ip"), "an egress pod with a malformed PodIP must be skipped")
}

// TestProcessK8sEgresses_ImportsAllDualStackAddresses verifies the cold-start seeder registers every
// IP family of a dual-stack egress pod (Status.PodIPs), so the secondary family's egress is restored
// after a CCM restart instead of being silently dropped.
func TestProcessK8sEgresses_ImportsAllDualStackAddresses(t *testing.T) {
	const (
		egressVal = "corp-egress"
		nodeName  = "node-1"
		hostIP    = "10.0.0.50"
		v6Host    = "fd00::50"
		v4        = "10.244.7.1"
		v6        = "fd00::7"
	)

	pod := newEgressPod("pod-dualstack", "default", egressVal, nodeName, v4, hostIP, v1.PodRunning)
	pod.Status.PodIPs = []v1.PodIP{{IP: v4}, {IP: v6}}
	pod.Status.HostIPs = []v1.HostIP{{IP: hostIP}, {IP: v6Host}}

	k8s := newK8sStateForSeeders()
	kube := fake.NewSimpleClientset(pod)

	_, err := processK8sEgresses(context.Background(), kube, &k8s)
	assert.NoError(t, err)

	assert.True(t, podIPTracked(&k8s, v4), "the primary (IPv4) egress address must be imported")
	assert.True(t, podIPTracked(&k8s, v6), "the secondary (IPv6) egress address must be imported")
	// The IPv6 address must be filed under the IPv6 node location, never under the IPv4 HostIP.
	assert.NotContains(t, k8s.Nodes[hostIP].Pods, v6, "the IPv6 address must not be filed under the IPv4 node location")
	assert.Contains(t, k8s.Nodes[v6Host].Pods, v6, "the IPv6 address must be filed under the IPv6 node location")
}

// TestPodEgressAddresses verifies the egress address extractor prefers Status.PodIPs (all IP
// families) and falls back to the single Status.PodIP, skipping empty entries.
func TestPodEgressAddresses(t *testing.T) {
	tests := []struct {
		name string
		pod  *v1.Pod
		want []string
	}{
		{
			name: "dual-stack uses every PodIPs entry",
			pod:  &v1.Pod{Status: v1.PodStatus{PodIP: "10.0.0.1", PodIPs: []v1.PodIP{{IP: "10.0.0.1"}, {IP: "fd00::1"}}}},
			want: []string{"10.0.0.1", "fd00::1"},
		},
		{
			name: "single-stack uses the sole PodIPs entry",
			pod:  &v1.Pod{Status: v1.PodStatus{PodIP: "10.0.0.1", PodIPs: []v1.PodIP{{IP: "10.0.0.1"}}}},
			want: []string{"10.0.0.1"},
		},
		{
			name: "falls back to PodIP when PodIPs is empty",
			pod:  &v1.Pod{Status: v1.PodStatus{PodIP: "10.0.0.1"}},
			want: []string{"10.0.0.1"},
		},
		{
			name: "no addresses when both are empty",
			pod:  &v1.Pod{Status: v1.PodStatus{}},
			want: nil,
		},
		{
			name: "canonicalizes IPv6 representation (uppercase/expanded to lowercase/compressed)",
			pod:  &v1.Pod{Status: v1.PodStatus{PodIPs: []v1.PodIP{{IP: "10.0.0.1"}, {IP: "FD00:0:0:0:0:0:0:1"}}}},
			want: []string{"10.0.0.1", "fd00::1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, PodEgressAddresses(tt.pod))
		})
	}
}

// TestPodNodeLocationsByFamily_CanonicalizesRepresentation verifies node locations are keyed by the
// canonical IP form, so NRP's uppercase/expanded IPv6 and the pod's lowercase/compressed form are a
// single key (avoiding a duplicate location across a CCM restart).
func TestPodNodeLocationsByFamily_CanonicalizesRepresentation(t *testing.T) {
	pod := &v1.Pod{Status: v1.PodStatus{
		HostIP:  "10.0.0.1",
		HostIPs: []v1.HostIP{{IP: "10.0.0.1"}, {IP: "FD61:4620:4C96:C887:0:0:0:4"}},
	}}
	got := PodNodeLocationsByFamily(pod)
	assert.Equal(t, "10.0.0.1", got[false])
	assert.Equal(t, "fd61:4620:4c96:c887::4", got[true], "the IPv6 node location must be canonical")
}

// TestProcessK8sEndpoints_SkipsMalformedAddresses verifies the cold-start seeder rejects malformed
// EndpointSlice addresses, which would otherwise poison the AddressLocations payload and block sync.
func TestProcessK8sEndpoints_SkipsMalformedAddresses(t *testing.T) {
	const (
		svcUID = "svc-malformed-eps"
		node   = "node-1"
		nodeIP = "10.0.0.41"
		goodIP = "10.244.7.1"
		badIP  = "not-an-ip"
	)

	k8s := newK8sStateForSeeders(svcUID)
	nodeNameToIPMap := map[string][]string{node: {nodeIP}}

	eps := newServiceOwnedEndpointSlice("eps-1", "default", svcUID, discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{
			Addresses:  []string{goodIP, badIP},
			NodeName:   ptr.To(node),
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
		},
	})
	kube := fake.NewSimpleClientset(eps)

	_, err := processK8sEndpoints(context.Background(), kube, &k8s, nodeNameToIPMap)
	assert.NoError(t, err)

	assert.True(t, podIPTracked(&k8s, goodIP), "a valid endpoint address must be imported")
	assert.False(t, podIPTracked(&k8s, badIP), "a malformed endpoint address must be skipped")
}

// TestReconcileServices_AppliesRuntimeAdmissionOnStartup pins that the startup path applies the
// same admission gate as the runtime path.
//
// Startup's only structural criterion is Spec.Type == LoadBalancer, so without an explicit gate a
// Service that ReconcileInboundService rejects - notably one requesting an internal load balancer -
// is admitted after a CCM restart and provisioned with Scope="Public". A Service claimed by another
// LoadBalancerClass, which the upstream controller never hands to a cloud provider, would likewise
// be provisioned by both controllers.
func TestReconcileServices_AppliesRuntimeAdmissionOnStartup(t *testing.T) {
	const (
		internalUID = "11111111-1111-1111-1111-111111111111"
		classedUID  = "22222222-2222-2222-2222-222222222222"
		okUID       = "33333333-3333-3333-3333-333333333333"
	)

	ports := []v1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(8080), Protocol: v1.ProtocolTCP}}
	foreignClass := "example.com/other-controller"

	serviceUIDToService := map[string]*v1.Service{
		internalUID: {
			ObjectMeta: metav1.ObjectMeta{
				Name: "internal", Namespace: "ns", UID: types.UID(internalUID),
				Annotations: map[string]string{consts.ServiceAnnotationLoadBalancerInternal: "True"},
			},
			Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer, Ports: ports},
		},
		classedUID: {
			ObjectMeta: metav1.ObjectMeta{Name: "classed", Namespace: "ns", UID: types.UID(classedUID)},
			Spec: v1.ServiceSpec{
				Type: v1.ServiceTypeLoadBalancer, Ports: ports, LoadBalancerClass: &foreignClass,
			},
		},
		okUID: {
			ObjectMeta: metav1.ObjectMeta{Name: "ok", Namespace: "ns", UID: types.UID(okUID)},
			Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer, Ports: ports},
		},
	}

	dt := newTestDiffTracker()
	syncOps := &SyncDiffTrackerReturnType{
		LoadBalancerUpdates: SyncServicesReturnType{
			Additions: newIgnoreCaseSetFromSlice([]string{internalUID, classedUID, okUID}),
		},
		NATGatewayUpdates: SyncServicesReturnType{Additions: utilsets.NewString()},
	}

	dt.reconcileServices(syncOps, serviceUIDToService)

	dt.mu.Lock()
	defer dt.mu.Unlock()
	assert.NotContains(t, dt.pendingServiceOps, internalUID,
		"an internal-LB Service rejected at runtime must not be provisioned as public on restart")
	assert.NotContains(t, dt.pendingServiceOps, classedUID,
		"a Service owned by another LoadBalancerClass must not be claimed on restart")
	assert.Contains(t, dt.pendingServiceOps, okUID,
		"a supported Service must still be provisioned on restart")
}

// TestRecoverServiceExternalIPs_SnapshotsNRPStateUnderLock pins that the NRP LoadBalancer set is
// read under dt.mu.
//
// recoverServiceExternalIPs runs after WaitForInitialSync, so the ServiceUpdater and
// LocationsUpdater goroutines are already live and mutate NRPResources.LoadBalancers under dt.mu.
// IgnoreCaseSet wraps a map, so an unsynchronised read there is not a tolerable data race but a
// fatal "concurrent map read and map write" that recover() cannot catch, killing the CCM during
// startup recovery. Run with -race.
func TestRecoverServiceExternalIPs_SnapshotsNRPStateUnderLock(t *testing.T) {
	dt := newTestDiffTracker()
	dt.config = testConfig()
	dt.kubeClient = fake.NewSimpleClientset()

	serviceUIDToService := make(map[string]*v1.Service)
	for i := 0; i < 50; i++ {
		uid := fmt.Sprintf("svc-%d", i)
		serviceUIDToService[uid] = &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: uid, Namespace: "ns", UID: types.UID(uid)},
			Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
		}
		dt.NRPResources.LoadBalancers.Insert(uid)
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			// Exactly what the ServiceUpdater does on every create/delete completion.
			dt.UpdateNRPLoadBalancers(SyncServicesReturnType{
				Additions: newIgnoreCaseSetFromSlice([]string{fmt.Sprintf("churn-%d", i)}),
				Removals:  newIgnoreCaseSetFromSlice([]string{fmt.Sprintf("churn-%d", i-1)}),
			})
		}
	}()

	for i := 0; i < 20; i++ {
		recoverServiceExternalIPs(context.Background(), dt, serviceUIDToService, map[string]string{})
	}

	close(stop)
	<-done

	dt.mu.Lock()
	stillTracked := dt.NRPResources.LoadBalancers.Has("svc-0")
	dt.mu.Unlock()
	assert.True(t, stillTracked, "recovery must only read the NRP LoadBalancer set, never mutate it")
}

// TestCleanupOrphanedPublicIPs_KeepsPIPForServiceStillDesiredInKubernetes pins that orphan
// classification consults Kubernetes desired state, not only NRP.
//
// A Public IP with no NRP registration is the crash-mid-create state: the address was allocated
// before the LoadBalancer and ServiceGateway registration completed. The restart's re-create can
// park (terminal spec failure, or retries exhausted), and parked operations do not block
// initialization, so this cleanup runs while the Service is still being reconciled. Treating that
// as an orphan destroys an in-use address and forces a new one on the next successful create.
func TestCleanupOrphanedPublicIPs_KeepsPIPForServiceStillDesiredInKubernetes(t *testing.T) {
	const (
		desiredUID = "11111111-1111-1111-1111-111111111111"
		egressUID  = "team-egress"
		orphanUID  = "22222222-2222-2222-2222-222222222222"
	)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockPIP := mock_publicipaddressclient.NewMockInterface(ctrl)
	mockFactory.EXPECT().GetPublicIPAddressClient().Return(mockPIP).AnyTimes()

	// The sweep deletes in parallel through a worker pool, so the recorder must be locked.
	var deletedMu sync.Mutex
	var deleted []string
	mockPIP.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, name string) error {
			deletedMu.Lock()
			defer deletedMu.Unlock()
			deleted = append(deleted, name)
			return nil
		}).AnyTimes()

	dt := newTestDiffTracker()
	dt.config = testConfig()
	dt.networkClientFactory = mockFactory
	// Both are desired in Kubernetes but neither has reached NRP yet.
	dt.K8sResources.Services.Insert(desiredUID)
	dt.K8sResources.Egresses.Insert(egressUID)

	detached := func(name string) *armnetwork.PublicIPAddress {
		return &armnetwork.PublicIPAddress{
			Name:       ptr.To(name),
			Properties: &armnetwork.PublicIPAddressPropertiesFormat{},
		}
	}

	assert.NoError(t, dt.cleanupOrphanedPublicIPs(context.Background(), []*armnetwork.PublicIPAddress{
		detached(PublicIPName(desiredUID)),
		detached(PublicIPName(egressUID)),
		detached(PublicIPName(orphanUID)),
	}))

	assert.NotContains(t, deleted, PublicIPName(desiredUID),
		"the Public IP of a Service Kubernetes still wants must not be deleted as an orphan")
	assert.NotContains(t, deleted, PublicIPName(egressUID),
		"the Public IP of an egress identity Kubernetes still wants must not be deleted as an orphan")
	assert.Contains(t, deleted, PublicIPName(orphanUID),
		"a Public IP desired by neither Kubernetes nor NRP must still be cleaned up")
}

// TestCleanupOrphanedPublicIPs_SweepsEveryUnusedManagedAddress pins what the sweeper may and may
// not delete. The resource group is the cluster's managed node resource group, so every "*-pip" in
// it belongs to this controller and an unused one is a leak, whether it is named from a Service UUID
// or from an egress pod label. What still has to survive is an address that is attached, reserved
// for the default gateway, or wanted by a Kubernetes object.
func TestCleanupOrphanedPublicIPs_SweepsEveryUnusedManagedAddress(t *testing.T) {
	run := func(t *testing.T, pips []*armnetwork.PublicIPAddress) []string {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		mockFactory := mock_azclient.NewMockClientFactory(ctrl)
		mockPIP := mock_publicipaddressclient.NewMockInterface(ctrl)
		mockFactory.EXPECT().GetPublicIPAddressClient().Return(mockPIP).AnyTimes()

		// The sweep deletes in parallel through a worker pool, so the recorder must be locked.
		var deletedMu sync.Mutex
		var deleted []string
		mockPIP.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, _, name string) error {
				deletedMu.Lock()
				defer deletedMu.Unlock()
				deleted = append(deleted, name)
				return nil
			}).AnyTimes()

		dt := newTestDiffTracker()
		dt.config = testConfig()
		dt.networkClientFactory = mockFactory
		assert.NoError(t, dt.cleanupOrphanedPublicIPs(context.Background(), pips))
		return deleted
	}

	detached := func(name string) *armnetwork.PublicIPAddress {
		return &armnetwork.PublicIPAddress{
			Name:       ptr.To(name),
			Properties: &armnetwork.PublicIPAddressPropertiesFormat{},
		}
	}

	const managedOrphan = "22222222-2222-2222-2222-222222222222-pip"

	deleted := run(t, []*armnetwork.PublicIPAddress{
		detached("team-egress-pip"),
		detached("team-egress-pip-v6"),
		detached(managedOrphan),
		detached(PublicIPName(DefaultOutboundNATGatewayName)),
		detached("not-one-of-ours"),
	})

	assert.Contains(t, deleted, managedOrphan,
		"a managed Service address desired by neither Kubernetes nor NRP must be cleaned up")
	assert.Contains(t, deleted, "team-egress-pip",
		"an unused egress address is a leak: its NAT Gateway is already gone, so nothing else deletes it")
	assert.Contains(t, deleted, "team-egress-pip-v6",
		"the IPv6 half of an egress identity leaks the same way as the IPv4 half")
	assert.NotContains(t, deleted, PublicIPName(DefaultOutboundNATGatewayName),
		"the cluster's default egress address is RP-owned and must never be deleted")
	assert.NotContains(t, deleted, "not-one-of-ours",
		"a name that does not follow the controller's convention must be left alone")
}

// TestRecoverStuckFinalizers_CountsRemovalSeparatelyFromScheduling pins that startup recovery
// reports what it actually did.
//
// finalizers_recovered_total previously incremented on paths that left the finalizer in place -- a
// Service handed to the diff, a pod enqueued for drain -- while the one path that genuinely removed
// a pod finalizer incremented nothing. An operator watching it for recovery progress was therefore
// misled in both directions. Scheduled work now has its own counter, and a removal that fails is
// counted so the resulting Terminating object is detectable.
func TestRecoverStuckFinalizers_CountsRemovalSeparatelyFromScheduling(t *testing.T) {
	RegisterMetrics()

	read := func() (recovered, scheduled, svcFailed float64) {
		r, err := testutil.GetCounterMetricValue(finalizersRecoveredTotal)
		assert.NoError(t, err)
		s, err := testutil.GetCounterMetricValue(finalizersRecoveryScheduledTotal)
		assert.NoError(t, err)
		f, err := testutil.GetCounterMetricValue(serviceFinalizerRemoveFailedTotal)
		assert.NoError(t, err)
		return r, s, f
	}

	deleting := metav1.NewTime(time.Now())
	svcScheduled := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc-with-azure", Namespace: "ns", UID: types.UID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
			DeletionTimestamp: &deleting, Finalizers: []string{ServiceGatewayServiceCleanupFinalizer},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	dt := newProviderDiffTracker(t, ctrl, fake.NewSimpleClientset(svcScheduled))

	t.Run("a Service left to the diff counts as scheduled, not recovered", func(t *testing.T) {
		r0, s0, _ := read()
		// Its LoadBalancer still exists in Azure, so recovery must defer to the diff.
		recoverStuckFinalizers(context.Background(), dt,
			&v1.ServiceList{Items: []v1.Service{*svcScheduled}}, nil, nil,
			utilsets.NewString(strings.ToLower(string(svcScheduled.UID))), utilsets.NewString(), utilsets.NewString())
		r1, s1, _ := read()
		assert.Equal(t, float64(0), r1-r0, "the finalizer is still on the Service; it was not recovered")
		assert.Equal(t, float64(1), s1-s0, "handing it to the diff must be counted as scheduled work")
	})

	t.Run("a failed direct removal is counted", func(t *testing.T) {
		kube := fake.NewSimpleClientset(svcScheduled)
		kube.PrependReactor("update", "services", func(_ k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewInternalError(fmt.Errorf("transient apiserver failure"))
		})
		failing := newProviderDiffTracker(t, ctrl, kube)

		r0, _, f0 := read()
		// No Azure resource anywhere, so recovery takes the direct-removal path -- which fails.
		recoverStuckFinalizers(context.Background(), failing,
			&v1.ServiceList{Items: []v1.Service{*svcScheduled}}, nil, nil,
			utilsets.NewString(), utilsets.NewString(), utilsets.NewString())
		r1, _, f1 := read()
		assert.Equal(t, float64(1), f1-f0, "a forgotten Service finalizer removal must be counted")
		assert.Equal(t, float64(0), r1-r0, "a failed removal must not be reported as a recovery")
	})
}

// TestRecoverStuckFinalizers_RecoversServiceRetypedAwayFromLoadBalancer pins that recovery keys on
// our finalizer rather than spec.type. A LoadBalancer switched to ClusterIP and then deleted still
// needs its finalizer stripped; gating on the type skips it on every restart, so the namespace stays
// Terminating. The LoadBalancer case is the control.
func TestRecoverStuckFinalizers_RecoversServiceRetypedAwayFromLoadBalancer(t *testing.T) {
	delTime := metav1.Now()
	newStuck := func(name, uid string, svcType v1.ServiceType) *v1.Service {
		return &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "default", UID: types.UID(uid),
				DeletionTimestamp: &delTime,
				Finalizers:        []string{ServiceGatewayServiceCleanupFinalizer},
			},
			Spec: v1.ServiceSpec{Type: svcType},
		}
	}
	retyped := newStuck("was-lb", "uid-retyped", v1.ServiceTypeClusterIP)
	stillLB := newStuck("still-lb", "uid-lb", v1.ServiceTypeLoadBalancer)

	kube := fake.NewSimpleClientset(retyped, stillLB)
	dt := newTestDiffTracker()
	dt.kubeClient = kube
	services := &v1.ServiceList{Items: []v1.Service{*retyped, *stillLB}}

	// No Azure resource exists for either, so recovery should strip both finalizers directly.
	recoverStuckFinalizers(context.Background(), dt, services, nil, nil, utilsets.NewString(), utilsets.NewString(), nil)

	got, err := kube.CoreV1().Services("default").Get(context.Background(), "was-lb", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.False(t, hasServiceGatewayFinalizer(got),
		"a Terminating Service retyped away from LoadBalancer must still have its finalizer recovered, or its namespace never deletes")

	gotLB, err := kube.CoreV1().Services("default").Get(context.Background(), "still-lb", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.False(t, hasServiceGatewayFinalizer(gotLB), "control: a LoadBalancer Service recovers as before")
}

// TestProcessK8sServices_ExcludesServicesNotOwnedByServiceGateway pins that ownership is decided
// where desired state is built. A Service claimed by another LoadBalancerClass, or one admission
// rejects, is neither an addition nor a removal once its LoadBalancer already exists, so admitting
// it here would keep it synced forever. The ordinary Service is the control.
func TestProcessK8sServices_ExcludesServicesNotOwnedByServiceGateway(t *testing.T) {
	newLB := func(name, uid string, mutate func(*v1.Service)) *v1.Service {
		svc := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(uid)},
			Spec: v1.ServiceSpec{
				Type:  v1.ServiceTypeLoadBalancer,
				Ports: []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP}},
			},
		}
		if mutate != nil {
			mutate(svc)
		}
		return svc
	}

	owned := newLB("owned", "uid-owned", nil)
	foreignClass := newLB("foreign", "uid-foreign", func(s *v1.Service) {
		s.Spec.LoadBalancerClass = ptr.To("example.com/other-controller")
	})
	internal := newLB("internal", "uid-internal", func(s *v1.Service) {
		s.Annotations = map[string]string{consts.ServiceAnnotationLoadBalancerInternal: "true"}
	})

	kube := fake.NewSimpleClientset(owned, foreignClass, internal)
	k8s := &K8sState{Services: utilsets.NewString(), Egresses: utilsets.NewString(), Nodes: map[string]Node{}}

	_, serviceUIDToService, err := processK8sServices(context.Background(), kube, k8s)
	assert.NoError(t, err)

	assert.True(t, k8s.Services.Has("uid-owned"), "control: a Service ServiceGateway owns must stay desired")
	assert.Contains(t, serviceUIDToService, "uid-owned")

	assert.False(t, k8s.Services.Has("uid-foreign"),
		"a Service claimed by another LoadBalancerClass must not be desired, or ServiceGateway keeps syncing it forever")

	// An admission rejection is a mutated spec on a Service that may already be serving. Excluding
	// it would make the diff read it as removed and destroy its live LoadBalancer and Public IP on
	// the next restart, which is not what the runtime path does with the same Service.
	assert.True(t, k8s.Services.Has("uid-internal"),
		"a Service inbound admission rejects must stay desired so its live Azure resources are not deleted")
	assert.Contains(t, serviceUIDToService, "uid-internal",
		"reconcileServices needs the Service object to re-apply admission and decline to provision it")
}

// TestProcessK8sServices_ForeignServiceBecomesRemoval pins that an existing LoadBalancer for a
// Service that is no longer ours is relinquished by the diff rather than kept in sync.
func TestProcessK8sServices_ForeignServiceBecomesRemoval(t *testing.T) {
	foreign := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: "default", UID: types.UID("uid-foreign")},
		Spec: v1.ServiceSpec{
			Type:              v1.ServiceTypeLoadBalancer,
			LoadBalancerClass: ptr.To("example.com/other-controller"),
			Ports:             []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP}},
		},
	}
	kube := fake.NewSimpleClientset(foreign)

	dt := newTestDiffTracker()
	// A LoadBalancer from before the class was set.
	dt.NRPResources.LoadBalancers.Insert("uid-foreign")

	_, _, err := processK8sServices(context.Background(), kube, &dt.K8sResources)
	assert.NoError(t, err)

	sync := dt.GetSyncLoadBalancerServices()
	assert.True(t, sync.Removals.Has("uid-foreign"),
		"a ServiceGateway LoadBalancer for a Service now owned by another controller must be relinquished")
	assert.False(t, sync.Additions.Has("uid-foreign"), "it must not be re-provisioned either")
}

// TestFetchServiceGatewayServices_MatchesServiceTypeCaseInsensitively pins how NRP service types are
// parsed. A service dropped here is absent from NRPState while still present in Azure, which is what
// scheduleOrphanedResourceDeletions treats as an orphan, so an unrecognized type costs the live
// resource. InboundOutbound counts as both, casing must not matter, and a genuinely unknown type
// fails initialization rather than proceeding on a partial view.
func TestFetchServiceGatewayServices_MatchesServiceTypeCaseInsensitively(t *testing.T) {
	svc := func(name, serviceType string) *armnetwork.ServiceGatewayService {
		return &armnetwork.ServiceGatewayService{
			Name:       ptr.To(name),
			Properties: &armnetwork.ServiceGatewayServicePropertiesFormat{ServiceType: ptr.To(armnetwork.ServiceType(serviceType))},
		}
	}

	cases := []struct {
		name            string
		services        []*armnetwork.ServiceGatewayService
		wantLB, wantNAT []string
		wantErr         bool
	}{
		{"canonical", []*armnetwork.ServiceGatewayService{svc("lb", "Inbound"), svc("nat", "Outbound")},
			[]string{"lb"}, []string{"nat"}, false},
		{"lowercase", []*armnetwork.ServiceGatewayService{svc("lb", "inbound"), svc("nat", "outbound")},
			[]string{"lb"}, []string{"nat"}, false},
		{"inbound outbound counts as both", []*armnetwork.ServiceGatewayService{svc("both", "InboundOutbound")},
			[]string{"both"}, []string{"both"}, false},
		{"unknown type fails initialization", []*armnetwork.ServiceGatewayService{svc("weird", "Sideways")},
			nil, nil, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockFactory := mock_azclient.NewMockClientFactory(ctrl)
			mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
			mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
			mockSGW.EXPECT().GetServices(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.services, nil).AnyTimes()

			nrp := &NRPState{LoadBalancers: utilsets.NewString(), NATGateways: utilsets.NewString()}
			err := fetchServiceGatewayServices(context.Background(), testConfig(), mockFactory, nrp)

			if tc.wantErr {
				assert.Error(t, err, "an unrecognized service type must not yield a silently partial NRP view")
				return
			}
			assert.NoError(t, err)
			for _, name := range tc.wantLB {
				assert.True(t, nrp.LoadBalancers.Has(name), "expected %q tracked as a LoadBalancer", name)
			}
			for _, name := range tc.wantNAT {
				assert.True(t, nrp.NATGateways.Has(name), "expected %q tracked as a NAT Gateway", name)
			}
		})
	}
}

// TestRecoverServiceExternalIPs_RetriesTransientPatchFailure pins that the one pass which recovers a
// Service's External IP retries a transient apiserver error instead of swallowing it. Nothing
// re-drives the write afterwards, so a single swallowed failure leaves the Service with an empty
// ingress until the next CCM restart even though its Azure resources exist.
func TestRecoverServiceExternalIPs_RetriesTransientPatchFailure(t *testing.T) {
	const uid = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: types.UID(uid)},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}

	kube := fake.NewSimpleClientset(svc)
	var patches int32
	kube.PrependReactor("patch", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		// Fail the first attempt only; the retry must then succeed.
		if atomic.AddInt32(&patches, 1) == 1 {
			return true, nil, errors.New("apiserver unavailable")
		}
		return false, nil, nil
	})

	dt := newTestDiffTracker()
	dt.kubeClient = kube
	dt.NRPResources.LoadBalancers.Insert(uid)

	recoverServiceExternalIPs(context.Background(), dt,
		map[string]*v1.Service{uid: svc},
		map[string]string{strings.ToLower(PublicIPName(uid)): "20.30.40.50"})

	assert.Greater(t, atomic.LoadInt32(&patches), int32(1),
		"a transient failure must be retried, not swallowed")
	got, err := kube.CoreV1().Services("default").Get(context.Background(), "web", metav1.GetOptions{})
	assert.NoError(t, err)
	if assert.Len(t, got.Status.LoadBalancer.Ingress, 1, "the External IP must be recovered") {
		assert.Equal(t, "20.30.40.50", got.Status.LoadBalancer.Ingress[0].IP)
	}
}

// TestScheduleOrphanedResourceDeletions_SkipsNonUUIDLoadBalancers pins the guard that keeps startup
// from deleting LoadBalancers this controller never created. Managed LoadBalancers are named after
// the Service UID, so anything else in the resource group belongs to someone else - the cluster's
// own "kubernetes" LoadBalancers among them.
func TestScheduleOrphanedResourceDeletions_SkipsNonUUIDLoadBalancers(t *testing.T) {
	const orphanUUID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	dt := newTestDiffTracker()

	azureLBs := utilsets.NewString("kubernetes", "kubernetes-internal", "customer-lb", orphanUUID)
	scheduleOrphanedResourceDeletions(dt, azureLBs, utilsets.NewString(), utilsets.NewString())

	dt.mu.Lock()
	defer dt.mu.Unlock()
	for _, name := range []string{"kubernetes", "kubernetes-internal", "customer-lb"} {
		_, scheduled := dt.pendingServiceOps[name]
		assert.False(t, scheduled, "%q is not a managed LoadBalancer and must never be scheduled for deletion", name)
	}
	_, scheduled := dt.pendingServiceOps[orphanUUID]
	assert.True(t, scheduled, "control: a genuine UUID-named orphan must still be collected")
}

// TestParseLocationAddresses_ReadsAddressesAndServices pins that the startup NRP snapshot is
// actually parsed. Every other test stubs the locations call out, so a parser that silently returned
// nothing would leave the diff engine comparing against an empty NRP view.
func TestParseLocationAddresses_ReadsAddressesAndServices(t *testing.T) {
	location := armnetwork.ServiceGatewayAddressLocation{
		AddressLocation: ptr.To("10.0.0.1"),
		Addresses: []*armnetwork.ServiceGatewayAddress{
			{Address: ptr.To("10.244.0.5"), Services: []*string{ptr.To("svc-a"), ptr.To("svc-b")}},
			{Address: ptr.To("10.244.0.6")},
			{Address: nil},
		},
	}

	got := parseLocationAddresses(location)

	assert.Len(t, got, 2, "addresses with no value must be skipped, the rest parsed")
	if assert.Contains(t, got, "10.244.0.5") {
		assert.True(t, got["10.244.0.5"].Services.Has("svc-a"))
		assert.True(t, got["10.244.0.5"].Services.Has("svc-b"))
	}
	if assert.Contains(t, got, "10.244.0.6") {
		assert.Zero(t, got["10.244.0.6"].Services.Len(), "an address with no services parses to an empty set")
	}
}

// TestOutboundIPFamiliesLocked_FollowsClusterAddressFamilies pins that egress families are decided
// from the cluster's Nodes. There is no outbound update path, so this decision is made once per NAT
// Gateway and cannot be corrected later; deriving it from one identity's pods would be wrong
// because a dual-stack cluster can run single-stack pods.
func TestOutboundIPFamiliesLocked_FollowsClusterAddressFamilies(t *testing.T) {
	node := func(name string, ips ...string) *v1.Node {
		addrs := make([]v1.NodeAddress, 0, len(ips))
		for _, ip := range ips {
			addrs = append(addrs, v1.NodeAddress{Type: v1.NodeInternalIP, Address: ip})
		}
		return &v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status:     v1.NodeStatus{Addresses: addrs},
		}
	}
	families := func(t *testing.T, nodes ...*v1.Node) []string {
		t.Helper()
		dt := newTestDiffTracker()
		if len(nodes) > 0 {
			setTestNodeLister(t, dt, nodes...)
		}
		dt.mu.Lock()
		defer dt.mu.Unlock()
		return dt.outboundIPFamiliesLocked()
	}

	assert.Equal(t, []string{"IPv4", "IPv6"},
		families(t, node("dual", "10.0.0.1", "fd00::1")),
		"a dual-stack node means IPv6 pods can exist, so the gateway needs an IPv6 public path")

	assert.Equal(t, []string{"IPv4"},
		families(t, node("v4", "10.0.0.1")),
		"an IPv4-only cluster must not be charged for an unused IPv6 address")

	assert.Equal(t, []string{"IPv4", "IPv6"},
		families(t, node("v4", "10.0.0.1"), node("dual", "10.0.0.2", "fd00::2")),
		"a mixed node pool still requires an IPv6 path for the pods that get one")

	assert.Equal(t, []string{"IPv4"}, families(t),
		"with no Node lister the safe answer is today's IPv4-only behaviour")
}

// TestClusterProvidesIPv6Locked_CachesPositiveResult pins that the Node walk happens at most once
// per process. It runs under dt.mu on the outbound create path, so repeating an O(nodes) scan for
// every new egress identity would put cluster-sized work inside the engine's global lock.
func TestClusterProvidesIPv6Locked_CachesPositiveResult(t *testing.T) {
	dualNode := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "dual"},
		Status: v1.NodeStatus{Addresses: []v1.NodeAddress{
			{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
			{Type: v1.NodeInternalIP, Address: "fd00::1"},
		}},
	}

	dt := newTestDiffTracker()
	setTestNodeLister(t, dt, dualNode)

	dt.mu.Lock()
	first := dt.clusterProvidesIPv6Locked()
	dt.mu.Unlock()
	assert.True(t, first)

	// Drop the lister entirely. Without the cache the answer would flip to false.
	dt.SetNodeLister(nil)
	dt.mu.Lock()
	second := dt.clusterProvidesIPv6Locked()
	dt.mu.Unlock()
	assert.True(t, second, "an observed IPv6 cluster must not need a second Node walk")

	// CONTROL: a tracker that never saw IPv6 still answers false, so the cache is not a blanket true.
	fresh := newTestDiffTracker()
	fresh.mu.Lock()
	defer fresh.mu.Unlock()
	assert.False(t, fresh.clusterProvidesIPv6Locked())
}

// TestReconcileServices_OutboundAdditionsCarryClusterIPFamilies pins that the startup create path
// decides egress address families exactly as the runtime pod path does. Outbound has no update
// path, so a NAT Gateway created during reconciliation without them stays single-stack for its
// whole life and no restart repairs it.
//
// The startup case deliberately runs with NO Node lister, because that is production: the lister is
// published only after InitializeFromCluster returns. The seed from the Node list initialization
// already fetched is what has to carry the decision, and the unseeded case is the control that
// proves it.
func TestReconcileServices_OutboundAdditionsCarryClusterIPFamilies(t *testing.T) {
	dualNode := func() *v1.Node {
		return &v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "dual"},
			Status: v1.NodeStatus{Addresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: v1.NodeInternalIP, Address: "fd00::1"},
			}},
		}
	}
	dualNodeIPs := map[string][]string{"dual": {"10.0.0.1", "fd00::1"}}

	const uid = "team-egress"

	familiesOf := func(t *testing.T, dt *DiffTracker) (families []string, natHasV6 bool, pipCount int) {
		t.Helper()
		dt.mu.Lock()
		op := dt.pendingServiceOps[uid]
		dt.mu.Unlock()
		if op == nil {
			t.Fatalf("no pending service operation was created")
		}
		if op.Config.OutboundConfig != nil {
			families = op.Config.OutboundConfig.IPFamilies
		}
		pips, nat, _ := buildOutboundServiceResources(uid, op.Config.OutboundConfig, dt.config)
		return families, len(nat.Properties.PublicIPAddressesV6) > 0, len(pips)
	}

	startupReconcile := func(dt *DiffTracker) {
		dt.reconcileServices(&SyncDiffTrackerReturnType{
			LoadBalancerUpdates: SyncServicesReturnType{
				Additions: utilsets.NewString(), Removals: utilsets.NewString(),
			},
			NATGatewayUpdates: SyncServicesReturnType{
				Additions: utilsets.NewString(uid), Removals: utilsets.NewString(),
			},
		}, map[string]*v1.Service{})
	}

	// Reference: the runtime pod path, which does have a Node lister.
	runtimeTracker := newTestDiffTracker()
	setTestNodeLister(t, runtimeTracker, dualNode())
	runtimeTracker.addPod(uid, "ns/pod-1", "pod-uid-1", "loc-1", "10.244.0.5")
	runtimeFamilies, runtimeHasV6, runtimePIPs := familiesOf(t, runtimeTracker)

	assert.Equal(t, []string{"IPv4", "IPv6"}, runtimeFamilies)
	assert.True(t, runtimeHasV6)
	assert.Equal(t, 2, runtimePIPs)

	// Startup, as production runs it: no Node lister, families carried by the seed.
	seeded := newTestDiffTracker()
	seeded.seedClusterAddressFamilies(dualNodeIPs)
	startupReconcile(seeded)
	seededFamilies, seededHasV6, seededPIPs := familiesOf(t, seeded)

	assert.Equal(t, runtimeFamilies, seededFamilies,
		"startup reconciliation must derive the same families as the pod path")
	assert.Equal(t, runtimeHasV6, seededHasV6,
		"a NAT Gateway created at startup must get the same IPv6 public path")
	assert.Equal(t, runtimePIPs, seededPIPs,
		"a NAT Gateway created at startup must get the same Public IPs")

	// CONTROL: without the seed the same call sees no Nodes at all and falls back to IPv4-only, so
	// the assertions above are carried by the seed and not by an unrelated default.
	unseeded := newTestDiffTracker()
	startupReconcile(unseeded)
	unseededFamilies, unseededHasV6, unseededPIPs := familiesOf(t, unseeded)

	assert.Equal(t, []string{"IPv4"}, unseededFamilies)
	assert.False(t, unseededHasV6)
	assert.Equal(t, 1, unseededPIPs)
}

// TestSeedClusterAddressFamilies_OnlyIPv6NodeAddressesCount pins the seed's parsing: an IPv4-only
// cluster must not be charged for an IPv6 address, and an IPv4-mapped IPv6 form is not IPv6.
func TestSeedClusterAddressFamilies_OnlyIPv6NodeAddressesCount(t *testing.T) {
	seeded := func(nodeIPs map[string][]string) bool {
		dt := newTestDiffTracker()
		dt.seedClusterAddressFamilies(nodeIPs)
		return dt.clusterHasIPv6.Load()
	}

	assert.True(t, seeded(map[string][]string{"a": {"10.0.0.1"}, "b": {"10.0.0.2", "fd00::2"}}),
		"one dual-stack node in a mixed pool still means IPv6 pods can exist")
	assert.False(t, seeded(map[string][]string{"a": {"10.0.0.1"}, "b": {"10.0.0.2"}}),
		"an IPv4-only cluster must not be charged for an unused IPv6 address")
	assert.False(t, seeded(map[string][]string{"a": {"::ffff:10.0.0.1"}}),
		"an IPv4-mapped address is an IPv4 address")
	assert.False(t, seeded(map[string][]string{"a": {"not-an-ip"}}))
	assert.False(t, seeded(nil))
}

// TestInitializeFromCluster_SeedsClusterAddressFamiliesFromNodes pins the production wiring: the
// Node lister is published only after InitializeFromCluster returns, so initialization must take
// the cluster's address families from the Node list it fetches itself. Without this the startup
// create path provisions IPv4-only NAT Gateways on a dual-stack cluster, and outbound has no update
// path to correct them.
func TestInitializeFromCluster_SeedsClusterAddressFamiliesFromNodes(t *testing.T) {
	initWithNodes := func(t *testing.T, nodes ...*v1.Node) *DiffTracker {
		t.Helper()
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		mockFactory := mock_azclient.NewMockClientFactory(ctrl)
		mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
		mockLB := mock_loadbalancerclient.NewMockInterface(ctrl)
		mockNAT := mock_natgatewayclient.NewMockInterface(ctrl)
		mockPIP := mock_publicipaddressclient.NewMockInterface(ctrl)

		mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
		mockFactory.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()
		mockFactory.EXPECT().GetNatGatewayClient().Return(mockNAT).AnyTimes()
		mockFactory.EXPECT().GetPublicIPAddressClient().Return(mockPIP).AnyTimes()

		mockSGW.EXPECT().GetServices(gomock.Any(), "rg", "sgw").Return(nil, nil).AnyTimes()
		mockSGW.EXPECT().GetAddressLocations(gomock.Any(), "rg", "sgw").Return(nil, nil).AnyTimes()
		mockSGW.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		mockSGW.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		mockLB.EXPECT().List(gomock.Any(), "rg").Return(nil, nil).AnyTimes()
		mockNAT.EXPECT().List(gomock.Any(), "rg").Return(nil, nil).AnyTimes()
		mockPIP.EXPECT().List(gomock.Any(), "rg").Return(nil, nil).AnyTimes()

		objects := make([]runtime.Object, 0, len(nodes))
		for _, node := range nodes {
			objects = append(objects, node)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		dt, err := InitializeFromCluster(ctx, testConfig(), mockFactory, fake.NewSimpleClientset(objects...))
		assert.NoError(t, err)
		if dt == nil {
			t.Fatal("InitializeFromCluster returned no tracker")
		}
		return dt
	}

	node := func(name string, ips ...string) *v1.Node {
		addrs := make([]v1.NodeAddress, 0, len(ips))
		for _, ip := range ips {
			addrs = append(addrs, v1.NodeAddress{Type: v1.NodeInternalIP, Address: ip})
		}
		return &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: v1.NodeStatus{Addresses: addrs}}
	}

	dual := initWithNodes(t, node("dual", "10.0.0.1", "fd00::1"))
	assert.True(t, dual.clusterHasIPv6.Load(),
		"a dual-stack cluster must be observed during init, while no Node lister exists yet")
	dual.mu.Lock()
	dualFamilies := dual.outboundIPFamiliesLocked()
	dual.mu.Unlock()
	assert.Equal(t, []string{"IPv4", "IPv6"}, dualFamilies)

	// CONTROL: an IPv4-only cluster reaches the opposite conclusion through the same code path, so
	// the assertion above is not satisfied by a default.
	v4Only := initWithNodes(t, node("v4", "10.0.0.1"))
	assert.False(t, v4Only.clusterHasIPv6.Load(),
		"an IPv4-only cluster must not be charged for an unused IPv6 address")
	v4Only.mu.Lock()
	v4Families := v4Only.outboundIPFamiliesLocked()
	v4Only.mu.Unlock()
	assert.Equal(t, []string{"IPv4"}, v4Families)
}
