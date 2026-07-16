package difftracker

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
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

	pip, natGw, servicesDTO := buildOutboundServiceResources("egress-123", nil, dtConfig)

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

func TestConfigValidation_AllFieldsRequired(t *testing.T) {
	tests := []struct {
		name        string
		config      Config
		expectError bool
	}{
		{
			name: "valid config",
			config: Config{
				SubscriptionID:             "sub-123",
				ResourceGroup:              "rg-456",
				Location:                   "eastus",
				VNetName:                   "test-vnet",
				ServiceGatewayResourceName: "sgw-789",
			},
			expectError: false,
		},
		{
			name: "missing subscription",
			config: Config{
				ResourceGroup:              "rg-456",
				Location:                   "eastus",
				ServiceGatewayResourceName: "sgw-789",
			},
			expectError: true,
		},
		{
			name: "missing resource group",
			config: Config{
				SubscriptionID:             "sub-123",
				Location:                   "eastus",
				ServiceGatewayResourceName: "sgw-789",
			},
			expectError: true,
		},
		{
			name: "missing location",
			config: Config{
				SubscriptionID:             "sub-123",
				ResourceGroup:              "rg-456",
				ServiceGatewayResourceName: "sgw-789",
			},
			expectError: true,
		},
		{
			name: "missing ServiceGatewayResourceName",
			config: Config{
				SubscriptionID: "sub-123",
				ResourceGroup:  "rg-456",
				Location:       "eastus",
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestNewIgnoreCaseSetFromSlice_PreservesOrder(t *testing.T) {
	// Order shouldn't matter for set membership
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
