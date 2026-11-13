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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/interfaceclient/mock_interfaceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/loadbalancerclient/mock_loadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/securitygroupclient/mock_securitygroupclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachineclient/mock_virtualmachineclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

type nsgTestCloud struct {
	mu                 sync.Mutex
	groups             map[string]*armnetwork.SecurityGroup
	nic                *armnetwork.Interface
	groupWrites        int
	groupReads         int
	nicWrites          int
	groupDeletes       int
	cacheInvalidations int
	groupError         error
	groupDeleteError   error
	nicError           error
	applyOnError       bool
	beforeWrite        func(context.Context)
	incompleteAck      bool
	afterDelete        func()
}

func nsgCopy[T any](value T) T {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var result T
	if err := json.Unmarshal(data, &result); err != nil {
		panic(err)
	}
	return result
}

func (c *nsgTestCloud) nicSnapshot() (*armnetwork.Interface, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.nic == nil {
		return nil, notFoundError()
	}
	return nsgCopy(c.nic), nil
}

func (c *nsgTestCloud) DeleteCacheForNode(context.Context, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cacheInvalidations++
	return nil
}

func nsgKubeService(name, sources string) *v1.Service {
	uid, kind, valid := serviceIdentity(name)
	if !valid || kind != Inbound {
		panic("NSG fixture requires a canonical inbound ServiceGateway name")
	}
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			UID: types.UID(uid), Name: name, Namespace: "test",
			Annotations: map[string]string{consts.ServiceAnnotationAllowedIPRanges: sources},
		},
		Spec: v1.ServiceSpec{
			Type:  v1.ServiceTypeLoadBalancer,
			Ports: []v1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(8080), Protocol: v1.ProtocolTCP}},
		},
	}
}

func newNSGTestTracker(t *testing.T, services ...*v1.Service) (*DiffTracker, *nsgTestCloud, *v1.Node) {
	t.Helper()
	ctrl := gomock.NewController(t)
	networkFactory := mock_azclient.NewMockClientFactory(ctrl)
	computeFactory := mock_azclient.NewMockClientFactory(ctrl)
	groups := mock_securitygroupclient.NewMockInterface(ctrl)
	interfaces := mock_interfaceclient.NewMockInterface(ctrl)
	machines := mock_virtualmachineclient.NewMockInterface(ctrl)
	networkFactory.EXPECT().GetSecurityGroupClient().Return(groups).AnyTimes()
	computeFactory.EXPECT().GetInterfaceClient().Return(interfaces).AnyTimes()
	computeFactory.EXPECT().GetVirtualMachineClient().Return(machines).AnyTimes()
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node", UID: "node-uid"},
		Spec: v1.NodeSpec{
			ProviderID: "azure:///subscriptions/compute-sub/resourceGroups/node-rg/providers/Microsoft.Compute/virtualMachines/node",
		},
		Status: v1.NodeStatus{Addresses: []v1.NodeAddress{
			{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
			{Type: v1.NodeInternalIP, Address: "fd00::1"},
		}},
	}
	cloud := &nsgTestCloud{
		groups: make(map[string]*armnetwork.SecurityGroup),
		nic: &armnetwork.Interface{
			ID:       ptr.To("/subscriptions/compute-sub/resourceGroups/node-rg/providers/Microsoft.Network/networkInterfaces/node-nic"),
			Name:     ptr.To("node-nic"),
			Location: ptr.To("eastus"),
			Etag:     ptr.To("nic-0"),
			Tags:     map[string]*string{"existing": ptr.To("tag")},
			Properties: &armnetwork.InterfacePropertiesFormat{
				ProvisioningState: ptr.To(armnetwork.ProvisioningStateSucceeded),
				VirtualMachine: &armnetwork.SubResource{
					ID: ptr.To("/subscriptions/compute-sub/resourceGroups/node-rg/providers/Microsoft.Compute/virtualMachines/node"),
				},
				EnableIPForwarding:          ptr.To(true),
				EnableAcceleratedNetworking: ptr.To(true),
				IPConfigurations: []*armnetwork.InterfaceIPConfiguration{
					{Name: ptr.To("primary"), Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
						Primary: ptr.To(true), PrivateIPAddress: ptr.To("10.0.0.1"),
					}},
					{Name: ptr.To("ipv6"), Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
						PrivateIPAddress: ptr.To("fd00::1"),
					}},
				},
			},
		},
	}
	machines.EXPECT().Get(gomock.Any(), "node-rg", gomock.Any(), gomock.Any()).Return(&armcompute.VirtualMachine{
		ID: cloud.nic.Properties.VirtualMachine.ID, Location: cloud.nic.Location,
		Properties: &armcompute.VirtualMachineProperties{
			NetworkProfile: &armcompute.NetworkProfile{NetworkInterfaces: []*armcompute.NetworkInterfaceReference{
				{ID: cloud.nic.ID},
			}},
		},
	}, nil).AnyTimes()
	groups.EXPECT().Get(gomock.Any(), "network-rg", gomock.Any()).DoAndReturn(
		func(ctx context.Context, _, name string) (*armnetwork.SecurityGroup, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			cloud.mu.Lock()
			defer cloud.mu.Unlock()
			cloud.groupReads++
			if group := cloud.groups[name]; group != nil {
				return nsgCopy(group), nil
			}
			return nil, notFoundError()
		}).AnyTimes()
	groups.EXPECT().List(gomock.Any(), "network-rg").DoAndReturn(
		func(context.Context, string) ([]*armnetwork.SecurityGroup, error) {
			cloud.mu.Lock()
			defer cloud.mu.Unlock()
			var result []*armnetwork.SecurityGroup
			for _, group := range cloud.groups {
				result = append(result, nsgCopy(group))
			}
			return result, nil
		}).AnyTimes()
	groups.EXPECT().CreateOrUpdate(gomock.Any(), "network-rg", gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, _, name string, value armnetwork.SecurityGroup) (*armnetwork.SecurityGroup, error) {
			cloud.mu.Lock()
			hook := cloud.beforeWrite
			cloud.mu.Unlock()
			if hook != nil {
				hook(ctx)
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			cloud.mu.Lock()
			defer cloud.mu.Unlock()
			cloud.groupWrites++
			if cloud.groupError != nil && !cloud.applyOnError {
				return nil, cloud.groupError
			}
			current := nsgCopy(&value)
			current.Etag = ptr.To(fmt.Sprintf("nsg-%d", cloud.groupWrites))
			current.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateSucceeded)
			cloud.groups[name] = current
			if cloud.groupError != nil {
				return nil, cloud.groupError
			}
			response := nsgCopy(current)
			if cloud.incompleteAck {
				response.Properties.SecurityRules = nil
			}
			return response, nil
		}).AnyTimes()
	groups.EXPECT().Delete(gomock.Any(), "network-rg", gomock.Any()).DoAndReturn(
		func(ctx context.Context, _, name string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			cloud.mu.Lock()
			cloud.groupDeletes++
			if err := cloud.groupDeleteError; err != nil {
				cloud.mu.Unlock()
				return err
			}
			delete(cloud.groups, name)
			after := cloud.afterDelete
			cloud.mu.Unlock()
			if after != nil {
				after()
			}
			return ctx.Err()
		}).AnyTimes()
	interfaces.EXPECT().Get(gomock.Any(), "node-rg", gomock.Any(), gomock.Any()).DoAndReturn(
		func(context.Context, string, string, *string) (*armnetwork.Interface, error) {
			return cloud.nicSnapshot()
		}).AnyTimes()
	interfaces.EXPECT().CreateOrUpdate(gomock.Any(), "node-rg", "node-nic", gomock.Any()).DoAndReturn(
		func(_ context.Context, _, _ string, value armnetwork.Interface) (*armnetwork.Interface, error) {
			cloud.mu.Lock()
			defer cloud.mu.Unlock()
			cloud.nicWrites++
			if cloud.nicError != nil && !cloud.applyOnError {
				return nil, cloud.nicError
			}
			if cloud.nic == nil {
				return nil, &azcore.ResponseError{StatusCode: http.StatusPreconditionFailed}
			}
			if ptr.Deref(value.Etag, "") != ptr.Deref(cloud.nic.Etag, "") {
				return nil, &azcore.ResponseError{StatusCode: http.StatusPreconditionFailed}
			}
			cloud.nic = nsgCopy(&value)
			cloud.nic.Etag = ptr.To(fmt.Sprintf("nic-%d", cloud.nicWrites))
			for _, group := range cloud.groups {
				if value.Properties.NetworkSecurityGroup != nil && *group.ID == *value.Properties.NetworkSecurityGroup.ID {
					group.Properties.NetworkInterfaces = []*armnetwork.Interface{{ID: cloud.nic.ID}}
				}
			}
			if cloud.nicError != nil {
				return nil, cloud.nicError
			}
			cloud.nic.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateSucceeded)
			return nsgCopy(cloud.nic), nil
		}).AnyTimes()
	dt := newTestDiffTracker()
	dt.logger = logr.Discard()
	dt.config = Config{
		SubscriptionID: "compute-sub", NetworkResourceSubscriptionID: "network-sub",
		ResourceGroup: "network-rg", Location: "eastus", VNetName: "vnet", ServiceGatewayResourceName: "sgw",
	}
	dt.networkClientFactory = networkFactory
	dt.computeClientFactory = computeFactory
	dt.nodeProvider = cloud
	kube := fake.NewSimpleClientset(node)
	dt.kubeClient = kube
	for _, service := range services {
		if _, err := kube.CoreV1().Services(service.Namespace).Create(context.Background(), service, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		uid := ServiceName(service)
		config, err := AdmitInboundService(service)
		if err != nil {
			t.Fatal(err)
		}
		serviceConfig := NewInboundServiceConfig(uid, config)
		serviceConfig.Namespace, serviceConfig.Name = service.Namespace, service.Name
		dt.pendingServiceOps[uid] = &ServiceOperationState{
			ServiceName: uid, Config: serviceConfig, LastAppliedConfig: &serviceConfig, State: StateCreated,
		}
		dt.K8sResources.Services.Insert(uid)
		dt.NRPResources.LoadBalancers.Insert(uid)
		dt.mayHaveNICPolicies = dt.mayHaveNICPolicies || nsgPolicyEnabled(config)
	}
	return dt, cloud, node
}

func addNSGTestBackend(dt *DiffTracker, uid, location, address, egress string) {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	node, exists := dt.K8sResources.Nodes[location]
	if !exists {
		node = newNode()
	}
	pod, exists := node.Pods[address]
	if !exists {
		pod = newPod()
	}
	pod.InboundIdentities.Insert(uid)
	pod.PublicOutboundIdentity = egress
	node.Pods[address] = pod
	dt.K8sResources.Nodes[location] = node
	if dt.mayHaveNICPolicies {
		dt.dirtyNICPolicies.Insert(location)
	}
	if egress != "" {
		dt.NRPResources.NATGateways.Insert(egress)
	}
}

func prepareNSGTest(t *testing.T, tracker *DiffTracker) map[string]*nsgPreparedNIC {
	t.Helper()
	prepared, err := tracker.prepareNICPolicies(context.Background(), tracker.takeNICPolicyWork())
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func nsgTestLocations(tracker *DiffTracker, prepared map[string]*nsgPreparedNIC) LocationData {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	locations := sets.New[string]()
	for location := range tracker.K8sResources.Nodes {
		locations.Insert(location)
	}
	for _, resources := range []map[string]NRPLocation{tracker.NRPResources.Locations, tracker.pendingLocationUpdates} {
		for location := range resources {
			locations.Insert(location)
		}
	}
	return tracker.getSyncNICPolicyLocationsLocked(prepared, locations)
}

func nsgTestLocationSync(tracker *DiffTracker, prepared map[string]*nsgPreparedNIC) LocationData {
	baseline := tracker.GetSyncLocationsAddresses()
	return tracker.applyNICPoliciesToLocationSync(baseline, prepared)
}

func nsgTestDrainingLocations(tracker *DiffTracker) []string {
	var locations []string
	for _, resources := range []map[string]NRPLocation{tracker.NRPResources.Locations, tracker.pendingLocationUpdates} {
		for location, current := range resources {
			if current.nodeOwner != nil && current.nodeOwner.draining {
				locations = append(locations, location)
			}
		}
	}
	return locations
}

func TestNICPoliciesPreparesOneNICForBothFamilies(t *testing.T) {
	first := nsgKubeService("v4-lb", "20.0.0.0/8")
	second := nsgKubeService("v6-lb", "2001:db8::/32")
	second.Spec.IPFamilies = []v1.IPFamily{v1.IPv6Protocol}
	dt, cloud, _ := newNSGTestTracker(t, first, second)
	addNSGTestBackend(dt, "v4-lb", "10.0.0.1", "10.1.0.1", "egress-natgw")
	addNSGTestBackend(dt, "v6-lb", "fd00::1", "fd00:1::1", "")

	before := nsgTestLocations(dt, nil)
	assert.True(t, before.Locations["10.0.0.1"].Addresses["10.1.0.1"].ServiceRef.Equals(utilsets.NewString("egress-natgw")))
	_, v6Published := before.Locations["fd00::1"]
	assert.False(t, v6Published)
	originalNIC := nsgCopy(cloud.nic)
	prepared := prepareNSGTest(t, dt)
	after := nsgTestLocations(dt, prepared)
	assert.True(t, after.Locations["10.0.0.1"].Addresses["10.1.0.1"].ServiceRef.Equals(utilsets.NewString("v4-lb", "egress-natgw")))
	assert.True(t, after.Locations["fd00::1"].Addresses["fd00:1::1"].ServiceRef.Has("v6-lb"))
	assert.Len(t, cloud.groups, 1)
	assert.Equal(t, 1, cloud.groupWrites)
	assert.Equal(t, 1, cloud.nicWrites)
	assert.Equal(t, originalNIC.Properties.IPConfigurations, cloud.nic.Properties.IPConfigurations)
	assert.Equal(t, originalNIC.Properties.EnableIPForwarding, cloud.nic.Properties.EnableIPForwarding)
	assert.Equal(t, originalNIC.Properties.EnableAcceleratedNetworking, cloud.nic.Properties.EnableAcceleratedNetworking)
	assert.Equal(t, originalNIC.Tags, cloud.nic.Tags)
	for _, group := range cloud.groups {
		assert.Len(t, group.Properties.SecurityRules, 6)
		assert.Equal(t, *group.ID, *cloud.nic.Properties.NetworkSecurityGroup.ID)
		assert.Contains(t, *group.ID, "/subscriptions/network-sub/")
	}
	dt.dirtyNICPolicies.Insert("10.0.0.1")
	prepareNSGTest(t, dt)
	assert.Equal(t, 1, cloud.groupWrites, "unchanged rules must not be rewritten")
	assert.Equal(t, 1, cloud.nicWrites, "rule reconciliation must not repeatedly rewrite the NIC")
}

func TestNICPolicyWarningUsesKubernetesUID(t *testing.T) {
	service := nsgKubeService("service-uid-lb", "20.0.0.0/8")
	dt, _, _ := newNSGTestTracker(t, service)
	addNSGTestBackend(dt, "service-uid-lb", "10.0.0.1", "10.1.0.1", "")
	recorder := &captureRecorder{FakeRecorder: *record.NewFakeRecorder(1)}
	dt.SetEventRecorder(recorder)

	dt.reportNICPolicyError([]string{"10.0.0.1"}, errors.New("NSG temporarily unavailable"))

	if assert.Len(t, recorder.objects, 1) {
		object, ok := recorder.objects[0].(*v1.Service)
		if assert.True(t, ok) {
			assert.Equal(t, types.UID("service-uid"), object.UID)
			assert.Equal(t, service.Name, object.Name)
			assert.Equal(t, service.Namespace, object.Namespace)
		}
	}
	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, consts.ServiceGatewayNSGReconcileFailedReason)
	default:
		t.Fatal("the NIC policy warning was not emitted")
	}
}

func TestNICPoliciesSourceOnlyUpdatesAndRemoval(t *testing.T) {
	service := nsgKubeService("svc-lb", "20.0.0.0/8")
	dt, cloud, _ := newNSGTestTracker(t, service)
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	prepared := prepareNSGTest(t, dt)
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepared))
	service.Annotations[consts.ServiceAnnotationAllowedIPRanges] = "40.0.0.0/8"
	if !assert.NoError(t, dt.ReconcileInboundService(service)) {
		return
	}
	assert.Equal(t, StateCreated, dt.pendingServiceOps["svc-lb"].State)
	assert.Empty(t, dt.serviceUpdaterTrigger, "source-only updates must not schedule an LB update")
	cloud.groupError = errors.New("temporary NSG error")
	failed, err := dt.prepareNICPolicies(context.Background(), dt.takeNICPolicyWork())
	assert.Error(t, err)
	assert.Empty(t, nsgTestLocations(dt, failed).Locations, "existing bindings are not withdrawn")
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.2", "")
	assert.Empty(t, nsgTestLocations(dt, failed).Locations, "new bindings wait for the requested policy")
	cloud.groupError = nil
	prepared = prepareNSGTest(t, dt)
	next := nsgTestLocations(dt, prepared)
	assert.True(t, next.Locations["10.0.0.1"].Addresses["10.1.0.2"].ServiceRef.Has("svc-lb"))
	assert.Equal(t, 1, cloud.nicWrites)
	for _, group := range cloud.groups {
		assert.Equal(t, armnetwork.SecurityRuleAccessDeny,
			nsgTestAccess(t, group.Properties.SecurityRules, "20.1.2.3", "10.1.0.1", 8080, armnetwork.SecurityRuleProtocolTCP))
		assert.Equal(t, armnetwork.SecurityRuleAccessAllow,
			nsgTestAccess(t, group.Properties.SecurityRules, "40.1.2.3", "10.1.0.1", 8080, armnetwork.SecurityRuleProtocolTCP))
	}

	delete(service.Annotations, consts.ServiceAnnotationAllowedIPRanges)
	assert.NoError(t, dt.ReconcileInboundService(service))
	prepareNSGTest(t, dt)
	for _, group := range cloud.groups {
		assert.Len(t, group.Properties.SecurityRules, 2)
	}
	assert.Equal(t, 0, cloud.groupDeletes)
	assert.Equal(t, 1, cloud.nicWrites)
}

func TestNICPoliciesPreservesNRMSAcrossPolicyLifecycle(t *testing.T) {
	ctx := context.Background()
	service := nsgKubeService("svc-lb", "20.0.0.0/8")
	dt, cloud, _ := newNSGTestTracker(t, service)
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
	operational := nsgTestNRMSRule(103)
	for _, group := range cloud.groups {
		group.Properties.SecurityRules = append(group.Properties.SecurityRules, nsgCopy(operational))
		group.Etag = ptr.To("nrms-added")
	}
	assertOperational := func() {
		t.Helper()
		if !assert.Len(t, cloud.groups, 1) {
			return
		}
		for _, group := range cloud.groups {
			index := slices.IndexFunc(group.Properties.SecurityRules, func(rule *armnetwork.SecurityRule) bool {
				return ptr.Deref(rule.Name, "") == *operational.Name
			})
			if assert.NotEqual(t, -1, index) {
				assert.Equal(t, operational, group.Properties.SecurityRules[index])
				assert.False(t, isNSGOwnedRule(group.Properties.SecurityRules[index]))
			}
		}
	}

	dt.dirtyNICPolicies.Insert("10.0.0.1")
	prepareNSGTest(t, dt)
	assert.Equal(t, 1, cloud.groupWrites, "NRMS appearing alone must not rewrite unchanged managed rules")
	assertOperational()

	service.Annotations[consts.ServiceAnnotationAllowedIPRanges] = "40.0.0.0/8"
	assert.NoError(t, dt.ReconcileInboundService(service))
	prepareNSGTest(t, dt)
	assert.Empty(t, dt.serviceUpdaterTrigger, "source-only changes must not schedule an LB update")
	assertOperational()

	service.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
	assert.NoError(t, dt.ReconcileInboundService(service))
	prepareNSGTest(t, dt)
	assertOperational()
	for _, group := range cloud.groups {
		for _, port := range []int32{8080, 9090} {
			assert.Equal(t, armnetwork.SecurityRuleAccessDeny,
				nsgTestAccess(t, group.Properties.SecurityRules, "20.1.2.3", "10.1.0.1", port, armnetwork.SecurityRuleProtocolTCP))
		}
	}
	dt.mu.Lock()
	config := dt.pendingServiceOps["svc-lb"].Config
	dt.pendingServiceOps["svc-lb"].InFlightConfig = &config
	dt.mu.Unlock()
	dt.OnServiceCreationComplete("svc-lb", true, nil)
	prepareNSGTest(t, dt)
	assertOperational()
	for _, group := range cloud.groups {
		assert.Equal(t, armnetwork.SecurityRuleAccessAllow,
			nsgTestAccess(t, group.Properties.SecurityRules, "20.1.2.3", "10.1.0.1", 8080, armnetwork.SecurityRuleProtocolTCP))
		assert.Equal(t, armnetwork.SecurityRuleAccessDeny,
			nsgTestAccess(t, group.Properties.SecurityRules, "20.1.2.3", "10.1.0.1", 9090, armnetwork.SecurityRuleProtocolTCP))
	}

	service.Annotations = nil
	assert.NoError(t, dt.ReconcileInboundService(service))
	prepareNSGTest(t, dt)
	assertOperational()
	for _, group := range cloud.groups {
		assert.Len(t, group.Properties.SecurityRules, 3)
	}
	service.Annotations = map[string]string{consts.ServiceAnnotationAllowedIPRanges: "40.0.0.0/8"}
	assert.NoError(t, dt.ReconcileInboundService(service))
	prepareNSGTest(t, dt)
	assertOperational()
	dt.DeleteService("svc-lb", true, false)
	prepared := prepareNSGTest(t, dt)
	for _, group := range cloud.groups {
		assert.Len(t, group.Properties.SecurityRules, 5, "old protection remains until withdrawal is acknowledged")
	}
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepared))
	assert.NoError(t, dt.pruneNICPolicies(ctx, prepared, sets.New[string]()))
	assertOperational()
	for _, group := range cloud.groups {
		assert.Len(t, group.Properties.SecurityRules, 3)
	}
	assert.Zero(t, cloud.groupDeletes)
	assert.Equal(t, 1, cloud.nicWrites)
}

func TestNICPoliciesRecoversAppliedButFailedWrites(t *testing.T) {
	for _, stage := range []string{"NSG", "NIC"} {
		t.Run(stage, func(t *testing.T) {
			dt, cloud, _ := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
			addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
			cloud.applyOnError = true
			if stage == "NSG" {
				cloud.groupError = errors.New("response lost")
			} else {
				cloud.nicError = errors.New("response lost")
			}
			prepared, err := dt.prepareNICPolicies(context.Background(), dt.takeNICPolicyWork())
			assert.Error(t, err)
			assert.Empty(t, nsgTestLocations(dt, prepared).Locations)
			cloud.groupError, cloud.nicError = nil, nil
			prepared = prepareNSGTest(t, dt)
			assert.True(t, nsgTestLocations(dt, prepared).Locations["10.0.0.1"].Addresses["10.1.0.1"].ServiceRef.Has("svc-lb"))
			assert.Equal(t, 1, cloud.groupWrites)
			assert.Equal(t, 1, cloud.nicWrites)
		})
	}
}

func TestNICPoliciesDrainsBeforePruningAndDefersOrphanCleanup(t *testing.T) {
	service := nsgKubeService("svc-lb", "20.0.0.0/8")
	dt, cloud, node := newNSGTestTracker(t, service)
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	prepared := prepareNSGTest(t, dt)
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepared))
	loadBalancer := NewLoadBalancer(dt)
	_, exists, err := loadBalancer.GetLoadBalancer(context.Background(), "cluster", service)
	assert.NoError(t, err)
	assert.True(t, exists)
	assert.NoError(t, loadBalancer.EnsureLoadBalancerDeleted(context.Background(), "cluster", service))
	prepared = prepareNSGTest(t, dt)
	assert.Equal(t, 1, cloud.groupWrites, "published backends retain protection until withdrawal succeeds")
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepared))
	assert.NoError(t, dt.pruneNICPolicies(context.Background(), prepared, sets.New[string]()))
	for _, group := range cloud.groups {
		assert.Len(t, group.Properties.SecurityRules, 2)
	}
	assert.Equal(t, 0, cloud.groupDeletes)
	assert.NoError(t, dt.kubeClient.CoreV1().Nodes().Delete(context.Background(), node.Name, metav1.DeleteOptions{}))
	dt.ReconcileNodeChange(node, nil)
	prepareNSGTest(t, dt)
	assert.Empty(t, nsgTestDrainingLocations(dt))
	assert.Equal(t, 0, cloud.groupDeletes, "Node deletion is not NIC deletion")
	cloud.nic = nil
	for _, group := range cloud.groups {
		group.Properties.NetworkInterfaces = nil
	}
	prepareNSGTest(t, dt)
	assert.Equal(t, 0, cloud.groupDeletes, "old resource deletion belongs to startup")
	assert.NoError(t, dt.discoverNICPolicies(context.Background(), nil))
	assert.Equal(t, 1, cloud.groupDeletes)
	assert.Empty(t, cloud.groups)
}

func TestNICPoliciesLeavesUnrestrictedNodesUntouched(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprintf("NSG policies active elsewhere=%t", active), func(t *testing.T) {
			service := nsgKubeService("svc-lb", "")
			service.Annotations = nil
			dt, cloud, _ := newNSGTestTracker(t, service)
			dt.mayHaveNICPolicies = active
			cloud.nic.Properties.NetworkSecurityGroup = &armnetwork.SecurityGroup{ID: ptr.To("unrelated-nsg")}
			addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
			prepared := prepareNSGTest(t, dt)
			assert.Empty(t, cloud.groups)
			assert.Equal(t, 0, cloud.nicWrites)
			assert.True(t, nsgTestLocations(dt, prepared).Locations["10.0.0.1"].Addresses["10.1.0.1"].ServiceRef.Has("svc-lb"))
		})
	}
	t.Run("retired owned rules behind a foreign association", func(t *testing.T) {
		service := nsgKubeService("svc-lb", "20.0.0.0/8")
		dt, cloud, _ := newNSGTestTracker(t, service)
		addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
		dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
		service.Annotations = nil
		assert.NoError(t, dt.ReconcileInboundService(service))
		cloud.nic.Properties.NetworkSecurityGroup = &armnetwork.SecurityGroup{ID: ptr.To("unrelated-nsg")}
		addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.2", "")
		prepared := prepareNSGTest(t, dt)
		assert.True(t, nsgTestLocationSync(dt, prepared).Locations["10.0.0.1"].Addresses["10.1.0.2"].ServiceRef.Has("svc-lb"))
		assert.Equal(t, "unrelated-nsg", *cloud.nic.Properties.NetworkSecurityGroup.ID)
		assert.Equal(t, 1, cloud.nicWrites, "removing the policy must not overwrite the foreign association")
		assert.Zero(t, cloud.groupDeletes)
		for _, group := range cloud.groups {
			assert.Len(t, group.Properties.SecurityRules, 2)
		}
	})
}

func TestNICPoliciesForgetsDeletedUnrestrictedServices(t *testing.T) {
	service := nsgKubeService("svc-lb", "")
	service.Annotations = nil
	dt, _, _ := newNSGTestTracker(t, service)
	service.Spec.Type = v1.ServiceTypeClusterIP
	loadBalancer := NewLoadBalancer(dt)
	_, exists, err := loadBalancer.GetLoadBalancer(context.Background(), "cluster", service)
	assert.NoError(t, err)
	assert.True(t, exists, "retyping still reaches the existing provider deletion path")
	assert.NoError(t, loadBalancer.EnsureLoadBalancerDeleted(context.Background(), "cluster", service))
	dt.mu.Lock()
	dt.NRPResources.LoadBalancers.Delete("svc-lb")
	dt.mu.Unlock()
	dt.OnServiceCreationComplete("svc-lb", true, nil)
	dt.mu.Lock()
	_, retained := dt.pendingServiceOps["svc-lb"]
	dt.mu.Unlock()
	assert.False(t, retained, "deletion retires the single shared Service record")
}

func TestNICPoliciesDeletesUnpublishedRules(t *testing.T) {
	for _, family := range []struct {
		name, location, backend, sources, deniedSource string
		ipFamily                                       v1.IPFamily
	}{
		{"IPv4", "10.0.0.1", "10.1.0.1", "20.0.0.0/8", "40.1.2.3", v1.IPv4Protocol},
		{"IPv6", "fd00::1", "fd00:1::1", "2001:db8::/32", "2001:db9::1", v1.IPv6Protocol},
	} {
		for _, retype := range []bool{false, true} {
			for _, cleanupBeforeACK := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/retype=%t/cleanup-before-ACK=%t", family.name, retype, cleanupBeforeACK), func(t *testing.T) {
					ctx := context.Background()
					service := nsgKubeService("svc-lb", family.sources)
					service.Spec.IPFamilies = []v1.IPFamily{family.ipFamily}
					service.Spec.Ports[0].Port = 65535
					dt, cloud, _ := newNSGTestTracker(t, service)
					delete(dt.pendingServiceOps, "svc-lb")
					dt.prepareNICPolicyInitialization(map[string]*v1.Service{"svc-lb": service}, nil)
					assert.Nil(t, dt.pendingServiceOps["svc-lb"].LastAppliedConfig)
					addNSGTestBackend(dt, "svc-lb", family.location, family.backend, "")
					mockNSGLocationUpdates(t, dt, func(context.Context, armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
						return nil
					})
					updater := &LocationsUpdater{diffTracker: dt, ctx: ctx, logger: dt.logger}
					updater.process(ctx)
					if !assert.Len(t, cloud.groups, 1) {
						return
					}
					assert.Empty(t, dt.NRPResources.Locations)
					assert.Empty(t, dt.pendingLocationUpdates)
					for _, group := range cloud.groups {
						assert.Equal(t, armnetwork.SecurityRuleAccessDeny, nsgTestAccess(t,
							group.Properties.SecurityRules, family.deniedSource, family.backend, 8080, armnetwork.SecurityRuleProtocolTCP))
					}
					worker := NewServiceUpdater(ctx, dt, dt.OnServiceCreationComplete, dt.serviceUpdaterTrigger)
					t.Cleanup(worker.Stop)
					worker.processBatch()
					worker.wg.Wait()
					if !assert.True(t, dt.pendingServiceOps["svc-lb"].CreationFailedTerminal) {
						return
					}
					assert.Nil(t, dt.pendingServiceOps["svc-lb"].LastAppliedConfig)
					assert.Empty(t, dt.dirtyNICPolicies)
					for len(dt.locationsUpdaterTrigger) > 0 {
						<-dt.locationsUpdaterTrigger
					}
					if retype {
						service.Spec.Type = v1.ServiceTypeClusterIP
						_, err := dt.kubeClient.CoreV1().Services(service.Namespace).Update(ctx, service, metav1.UpdateOptions{})
						assert.NoError(t, err)
					} else {
						assert.NoError(t, dt.kubeClient.CoreV1().Services(service.Namespace).Delete(ctx, service.Name, metav1.DeleteOptions{}))
					}
					loadBalancer := NewLoadBalancer(dt)
					_, exists, err := loadBalancer.GetLoadBalancer(ctx, "cluster", service)
					assert.NoError(t, err)
					assert.True(t, exists)
					assert.NoError(t, loadBalancer.EnsureLoadBalancerDeleted(ctx, "cluster", service))
					assert.Equal(t, StateDeletionInProgress, dt.pendingServiceOps["svc-lb"].State)
					assert.Contains(t, dt.dirtyNICPolicies, family.location)
					cleanup := func() {
						select {
						case <-dt.locationsUpdaterTrigger:
							updater.process(ctx)
						default:
							t.Fatal("deletion left installed NIC restrictions without scheduling LocationsUpdater")
						}
						assert.Empty(t, dt.dirtyNICPolicies)
						for _, group := range cloud.groups {
							assert.Len(t, group.Properties.SecurityRules, 2)
							assert.Equal(t, armnetwork.SecurityRuleAccessAllow, nsgTestAccess(t,
								group.Properties.SecurityRules, family.deniedSource, family.backend, 8080, armnetwork.SecurityRuleProtocolTCP))
						}
					}
					if cleanupBeforeACK {
						cleanup()
					}
					dt.NRPResources.LoadBalancers.Delete("svc-lb")
					dt.OnServiceCreationComplete("svc-lb", true, nil)
					assert.NotContains(t, dt.pendingServiceOps, "svc-lb")
					if !cleanupBeforeACK {
						cleanup()
					}
				})
			}
		}
	}
}

func TestNICPoliciesProtectsRetiringNodeLocations(t *testing.T) {
	dt, cloud, node := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.2", "")
	prepared := prepareNSGTest(t, dt)
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepared))
	changed := node.DeepCopy()
	changed.Status.Addresses[0].Address = "10.0.0.2"
	_, err := dt.kubeClient.CoreV1().Nodes().Update(context.Background(), changed, metav1.UpdateOptions{})
	assert.NoError(t, err)
	cloud.nic.Properties.IPConfigurations[0].Properties.PrivateIPAddress = ptr.To("10.0.0.2")
	dt.ReconcileNodeChange(node, changed)
	dt.UpdateEndpoints("svc-lb", map[string]string{
		"10.1.0.1": "10.0.0.1",
		"10.1.0.2": "10.0.0.1",
	}, map[string]string{"10.1.0.2": "10.0.0.2"})
	prepared = prepareNSGTest(t, dt)
	assert.Equal(t, 1, cloud.groupWrites, "old host-location references still require their source rules")
	for _, group := range cloud.groups {
		assert.Equal(t, armnetwork.SecurityRuleAccessDeny,
			nsgTestAccess(t, group.Properties.SecurityRules, "40.1.2.3", "10.1.0.1", 8080, armnetwork.SecurityRuleProtocolTCP))
	}
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepared))
	assert.NoError(t, dt.pruneNICPolicies(context.Background(), prepared, sets.New[string]()))
	assert.Empty(t, nsgTestDrainingLocations(dt))
	for _, group := range cloud.groups {
		assert.Equal(t, armnetwork.SecurityRuleAccessAllow,
			nsgTestAccess(t, group.Properties.SecurityRules, "40.1.2.3", "10.1.0.1", 8080, armnetwork.SecurityRuleProtocolTCP))
		assert.Equal(t, armnetwork.SecurityRuleAccessDeny,
			nsgTestAccess(t, group.Properties.SecurityRules, "40.1.2.3", "10.1.0.2", 8080, armnetwork.SecurityRuleProtocolTCP))
	}
}

func TestNICPoliciesKeepLocationOwnerAcrossLateACK(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		t.Run(fmt.Sprintf("full-recovery=%t", recovery), func(t *testing.T) {
			dt, cloud, node := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
			addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
			addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.2", "")
			prepared := prepareNSGTest(t, dt)
			attempt := nsgTestLocationSync(dt, prepared)
			if recovery {
				dt.UpdateLocationsAddresses(attempt)
				dt.locationRecoveryPending = true
				attempt = nsgTestLocationSync(dt, nil)
			}
			changed := node.DeepCopy()
			changed.Status.Addresses[0].Address = "10.0.0.2"
			_, err := dt.kubeClient.CoreV1().Nodes().Update(context.Background(), changed, metav1.UpdateOptions{})
			assert.NoError(t, err)
			cloud.nic.Properties.IPConfigurations[0].Properties.PrivateIPAddress = ptr.To("10.0.0.2")
			dt.ReconcileNodeChange(node, changed)
			dt.UpdateEndpoints("svc-lb", map[string]string{
				"10.1.0.1": "10.0.0.1", "10.1.0.2": "10.0.0.1",
			}, map[string]string{"10.1.0.2": "10.0.0.2"})
			dt.UpdateLocationsAddresses(attempt)
			assert.NotNil(t, dt.NRPResources.Locations["10.0.0.1"].nodeOwner)
			prepared = prepareNSGTest(t, dt)
			assert.Equal(t, 1, cloud.groupWrites, "late publication keeps old-location protection until withdrawal is acknowledged")
			for _, group := range cloud.groups {
				assert.Equal(t, armnetwork.SecurityRuleAccessDeny,
					nsgTestAccess(t, group.Properties.SecurityRules, "40.1.2.3", "10.1.0.1", 8080, armnetwork.SecurityRuleProtocolTCP))
			}
			dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepared))
			assert.NoError(t, dt.pruneNICPolicies(context.Background(), prepared, sets.New[string]()))
			assert.Empty(t, nsgTestDrainingLocations(dt))
			assert.Equal(t, 2, cloud.groupWrites)
		})
	}
}

func TestNICPoliciesDrainsHistoricalOwnerBeforePreparingReplacement(t *testing.T) {
	for _, egress := range []string{"", "default-natgw", "egress-natgw"} {
		t.Run(fmt.Sprintf("retained-egress=%s", egress), func(t *testing.T) {
			ctx := context.Background()
			dt, cloud, node := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
			addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
			dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
			if egress != "" {
				dt.NRPResources.Locations["10.0.0.1"].Addresses["10.1.0.9"] = NRPAddress{Services: utilsets.NewString(egress)}
				dt.NRPResources.NATGateways.Insert(egress)
				if egress == "egress-natgw" {
					pod := newPod()
					pod.PublicOutboundIdentity = egress
					dt.K8sResources.Nodes["10.0.0.1"].Pods["10.1.0.9"] = pod
				}
			}
			moved := node.DeepCopy()
			moved.Status.Addresses[0].Address = "10.0.0.2"
			_, err := dt.kubeClient.CoreV1().Nodes().Update(ctx, moved, metav1.UpdateOptions{})
			assert.NoError(t, err)
			cloud.nic.Properties.IPConfigurations[0].Properties.PrivateIPAddress = ptr.To("10.0.0.2")
			dt.ReconcileNodeChange(node, moved)

			replacement := node.DeepCopy()
			replacement.Name, replacement.UID = "node-b", "node-b"
			replacement.Spec.ProviderID += "-b"
			replacement.Status.Addresses = []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: "10.0.0.1"}}
			_, err = dt.kubeClient.CoreV1().Nodes().Create(ctx, replacement, metav1.CreateOptions{})
			assert.NoError(t, err)
			dt.ReconcileNodeChange(nil, replacement)
			dt.UpdateEndpoints("svc-lb", map[string]string{"10.1.0.1": "10.0.0.1"}, map[string]string{"10.1.0.2": "10.0.0.1"})

			mockNSGLocationUpdates(t, dt, func(_ context.Context, request armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
				for _, location := range request.AddressLocations {
					for _, address := range location.Addresses {
						for _, uid := range address.Services {
							assert.NotEqual(t, "svc-lb", ptr.Deref(uid, ""), "the old owner's NIC cannot authorize the replacement")
						}
					}
				}
				return nil
			})
			updater := &LocationsUpdater{diffTracker: dt, ctx: ctx, logger: dt.logger}
			updater.process(ctx)
			assert.Empty(t, nsgTestDrainingLocations(dt), "non-inbound references must not retain old ownership")
			if egress != "" {
				assert.True(t, dt.NRPResources.Locations["10.0.0.1"].Addresses["10.1.0.9"].Services.Has(egress))
			}
			assert.True(t, dt.dirtyNICPolicies.Has("10.0.0.2"), "old-NIC cleanup must survive reuse of its previous location")

			updater.process(ctx)
			assert.Equal(t, 1, updater.failureCount, "the replacement's own Azure identity is unavailable in this fixture")
			assert.False(t, dt.NRPResources.Locations["10.0.0.1"].Addresses["10.1.0.2"].Services.Has("svc-lb"))
			assert.True(t, dt.dirtyNICPolicies.Has("10.0.0.1"))
			for _, group := range cloud.groups {
				assert.Len(t, group.Properties.SecurityRules, 2, "the old NIC's retired protection is pruned")
			}
		})
	}
}

func TestNICPoliciesRevalidatesReplacementWithReusedAddresses(t *testing.T) {
	ctx := context.Background()
	service := nsgKubeService("svc-lb", "20.0.0.0/8")
	dt, cloud, node := newNSGTestTracker(t, service)
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
	assert.Equal(t, node.UID, dt.NRPResources.Locations["10.0.0.1"].nodeOwner.uid)

	replacement := node.DeepCopy()
	replacement.UID = "replacement"
	_, err := dt.kubeClient.CoreV1().Nodes().Update(ctx, replacement, metav1.UpdateOptions{})
	assert.NoError(t, err)
	dt.ReconcileNodeChange(node, replacement)
	assert.True(t, dt.NRPResources.Locations["10.0.0.1"].nodeOwner.draining)
	cloud.nic.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateUpdating)
	calls := 0
	mockNSGLocationUpdates(t, dt, func(_ context.Context, request armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
		calls++
		for _, location := range request.AddressLocations {
			for _, address := range location.Addresses {
				if calls == 1 {
					assert.Empty(t, address.Services, "the replacement cannot inherit the old owner's permission")
				} else {
					assert.Len(t, address.Services, 1)
					assert.Equal(t, "svc-lb", ptr.Deref(address.Services[0], ""))
				}
			}
		}
		return nil
	})
	updater := &LocationsUpdater{diffTracker: dt, ctx: ctx, logger: dt.logger}
	updater.process(ctx)
	assert.Equal(t, 1, calls)
	assert.Empty(t, dt.NRPResources.Locations)
	cloud.nic.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateSucceeded)
	updater.process(ctx)
	assert.Equal(t, 2, calls)
	ack := dt.NRPResources.Locations["10.0.0.1"]
	assert.True(t, ack.Addresses["10.1.0.1"].Services.Has("svc-lb"))
	assert.Equal(t, replacement.UID, ack.nodeOwner.uid)
	assert.False(t, ack.nodeOwner.draining)

	dt.ReconcileNodeChange(node, nil)
	assert.Empty(t, dt.dirtyNICPolicies, "a stale tombstone must not invalidate the replacement")
	service.Annotations[consts.ServiceAnnotationAllowedIPRanges] = "40.0.0.0/8"
	assert.NoError(t, dt.ReconcileInboundService(service))
	cloud.groupError = errors.New("policy update failed")
	updater.process(ctx)
	assert.Equal(t, 2, calls, "an ordinary policy failure must not withdraw the unchanged binding")
	assert.True(t, dt.NRPResources.Locations["10.0.0.1"].Addresses["10.1.0.1"].Services.Has("svc-lb"))
}

func TestNICPoliciesRejectsAmbiguousCurrentNodeLocations(t *testing.T) {
	ctx := context.Background()
	dt, cloud, node := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	other := node.DeepCopy()
	other.Name, other.UID = "other", "other"
	other.Spec.ProviderID += "-other"
	other.Status.Addresses = []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: "10.0.0.1"}}
	_, err := dt.kubeClient.CoreV1().Nodes().Create(ctx, other, metav1.CreateOptions{})
	assert.NoError(t, err)
	dt.ReconcileNodeChange(nil, other)
	work := dt.takeNICPolicyWork()
	prepared, err := dt.prepareNICPolicies(ctx, work)
	assert.ErrorContains(t, err, "multiple current Node owners")
	assert.Empty(t, nsgTestLocationSync(dt, prepared).Locations)
	assert.Zero(t, cloud.nicWrites)
	assert.Zero(t, cloud.groupWrites)
	dt.mu.Lock()
	dt.finishNICPolicyWorkLocked(work, prepared)
	dt.mu.Unlock()
	assert.NoError(t, dt.kubeClient.CoreV1().Nodes().Delete(ctx, other.Name, metav1.DeleteOptions{}))
	dt.ReconcileNodeChange(other, nil)
	prepared = prepareNSGTest(t, dt)
	assert.True(t, nsgTestLocationSync(dt, prepared).Locations["10.0.0.1"].Addresses["10.1.0.1"].ServiceRef.Has("svc-lb"))
}

func TestNICPoliciesRetainsStartupServiceMetadata(t *testing.T) {
	service := nsgKubeService("svc-lb", "20.0.0.0/8")
	dt, _, _ := newNSGTestTracker(t, service)
	config, err := AdmitInboundService(service)
	assert.NoError(t, err)
	assert.NoError(t, dt.AddService(NewInboundServiceConfig("svc-lb", config)))
	dt.mu.Lock()
	assert.Equal(t, service.Namespace, dt.pendingServiceOps["svc-lb"].Config.Namespace)
	assert.Equal(t, service.Name, dt.pendingServiceOps["svc-lb"].Config.Name)
	dt.mu.Unlock()
}

func TestNICPoliciesReconstructsOwnedResourcesAfterRestart(t *testing.T) {
	service := nsgKubeService("svc-lb", "20.0.0.0/8")
	before, oldCloud, _ := newNSGTestTracker(t, service)
	addNSGTestBackend(before, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	prepareNSGTest(t, before)
	for _, scenario := range []string{"unchanged", "changed policy", "deleted service"} {
		t.Run(scenario, func(t *testing.T) {
			var currentServices []*v1.Service
			deleted := scenario == "deleted service"
			if !deleted {
				current := service.DeepCopy()
				if scenario == "changed policy" {
					current.Annotations[consts.ServiceAnnotationAllowedIPRanges] = "40.0.0.0/8"
				}
				currentServices = append(currentServices, current)
			}
			dt, cloud, node := newNSGTestTracker(t, currentServices...)
			cloud.nic = nsgCopy(oldCloud.nic)
			cloud.groups = nsgCopy(oldCloud.groups)
			if !deleted {
				addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
			}
			assert.NoError(t, dt.discoverNICPolicies(context.Background(), map[string][]string{node.Name: nodeInternalIPs(node)}))
			assert.Equal(t, 0, cloud.groupWrites)
			prepared := prepareNSGTest(t, dt)
			if deleted {
				assert.Zero(t, cloud.groupWrites, "observed protection stays until the corrective membership ACK")
			}
			data := nsgTestLocationSync(dt, prepared)
			assert.Equal(t, FullUpdate, data.Action)
			dt.UpdateLocationsAddresses(data)
			assert.NoError(t, dt.pruneNICPolicies(context.Background(), prepared, sets.New[string]()))
			assert.Equal(t, 0, cloud.nicWrites)
			if scenario == "unchanged" {
				assert.Equal(t, 0, cloud.groupWrites)
			} else {
				assert.Equal(t, 1, cloud.groupWrites)
			}
			for _, group := range cloud.groups {
				if deleted {
					assert.Len(t, group.Properties.SecurityRules, 2)
				} else {
					assert.True(t, dt.NRPResources.Locations["10.0.0.1"].Addresses["10.1.0.1"].Services.Has("svc-lb"))
				}
			}
		})
	}
}

func TestNICPoliciesDropsRemovedNodeState(t *testing.T) {
	dt, cloud, node := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	prepared := prepareNSGTest(t, dt)
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepared))
	assert.NoError(t, dt.kubeClient.CoreV1().Nodes().Delete(context.Background(), node.Name, metav1.DeleteOptions{}))
	dt.ReconcileNodeChange(node, nil)
	dt.DeleteService("svc-lb", true, false)
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, nil))
	dt.CheckPendingServiceDeletions()
	dt.NRPResources.LoadBalancers.Delete("svc-lb")
	dt.OnServiceCreationComplete("svc-lb", true, nil)
	prepareNSGTest(t, dt)
	assert.NotContains(t, dt.pendingServiceOps, "svc-lb")
	assert.Empty(t, nsgTestDrainingLocations(dt))
	assert.Empty(t, dt.dirtyNICPolicies)
	assert.Len(t, cloud.groups, 1, "the retained NIC NSG does not require runtime bookkeeping")
}

func TestNICPoliciesIgnoresUninitializedUninvolvedNode(t *testing.T) {
	dt, cloud, _ := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	prepared := prepareNSGTest(t, dt)
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepared))
	unrelated := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated", UID: "unrelated"},
		Status:     v1.NodeStatus{Addresses: []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: "10.0.0.2"}}},
	}
	_, err := dt.kubeClient.CoreV1().Nodes().Create(context.Background(), unrelated, metav1.CreateOptions{})
	assert.NoError(t, err)
	dt.ReconcileNodeChange(nil, unrelated)
	prepareNSGTest(t, dt)
	assert.Empty(t, dt.dirtyNICPolicies)
	assert.Equal(t, 1, cloud.groupWrites)
	assert.Equal(t, 1, cloud.nicWrites)
}

func TestNICPoliciesIgnoresReadinessOnlyChanges(t *testing.T) {
	dt, cloud, node := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
	node.Status.Conditions = []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}}
	_, err := dt.kubeClient.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{})
	assert.NoError(t, err)
	dt.ReconcileNodeChange(nil, node)
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	prepared := prepareNSGTest(t, dt)
	dt.dirtyNICPolicies.Clear()
	previous := node.DeepCopy()
	node.Status.Conditions[0].Status = v1.ConditionFalse
	dt.ReconcileNodeChange(previous, node)
	assert.Zero(t, dt.dirtyNICPolicies.Len())
	assert.Equal(t, 1, cloud.groupWrites)
	assert.True(t, nsgTestLocations(dt, prepared).Locations["10.0.0.1"].Addresses["10.1.0.1"].ServiceRef.Has("svc-lb"))
}

func TestNICPoliciesRejoinedNodeFindsExistingGroup(t *testing.T) {
	before, oldCloud, _ := newNSGTestTracker(t, nsgKubeService("old-lb", "20.0.0.0/8"))
	addNSGTestBackend(before, "old-lb", "10.0.0.1", "10.1.0.1", "")
	prepareNSGTest(t, before)

	dt, cloud, node := newNSGTestTracker(t)
	assert.NoError(t, dt.kubeClient.CoreV1().Nodes().Delete(context.Background(), node.Name, metav1.DeleteOptions{}))
	cloud.nic, cloud.groups = nsgCopy(oldCloud.nic), nsgCopy(oldCloud.groups)
	assert.NoError(t, dt.discoverNICPolicies(context.Background(), nil))
	assert.Empty(t, nsgTestDrainingLocations(dt))
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, nil))

	node.UID = "rejoined"
	_, err := dt.kubeClient.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{})
	assert.NoError(t, err)
	dt.ReconcileNodeChange(nil, node)
	prepareNSGTest(t, dt)
	for _, group := range cloud.groups {
		assert.Len(t, group.Properties.SecurityRules, 2, "the current NIC's obsolete service rules are still removed")
	}
	assert.Equal(t, 1, cloud.groupWrites)
	assert.Zero(t, cloud.groupDeletes)
}

func TestNICPoliciesRetainsPendingNodesOnCancellation(t *testing.T) {
	for _, duringWrite := range []bool{false, true} {
		t.Run(fmt.Sprintf("duringWrite=%t", duringWrite), func(t *testing.T) {
			dt, cloud, node := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
			addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
			for index := 2; index <= 3; index++ {
				other := node.DeepCopy()
				other.Name = fmt.Sprintf("node-%d", index)
				other.UID = types.UID(other.Name)
				other.Spec.ProviderID += fmt.Sprintf("-%d", index)
				other.Status.Addresses = []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: fmt.Sprintf("10.0.0.%d", index)}}
				_, err := dt.kubeClient.CoreV1().Nodes().Create(context.Background(), other, metav1.CreateOptions{})
				assert.NoError(t, err)
			}
			dt.dirtyNICPolicies.Insert("10.0.0.2", "10.0.0.3")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if duringWrite {
				cloud.beforeWrite = func(context.Context) { cancel() }
			} else {
				cancel()
			}
			updater := &LocationsUpdater{diffTracker: dt, ctx: ctx, logger: dt.logger}
			updater.process(ctx)
			assert.ErrorIs(t, ctx.Err(), context.Canceled)
			for _, location := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"} {
				assert.True(t, dt.dirtyNICPolicies.Has(location), location)
			}
			assert.Zero(t, cloud.groupWrites)
			assert.Zero(t, cloud.nicWrites)
			assert.Empty(t, dt.NRPResources.Locations)
			assert.Empty(t, dt.locationsUpdaterTrigger, "failed NSG work must use LocationsUpdater backoff, not self-trigger")
		})
	}
}

func TestNSGDiscoveryCleansOrphansDuringInitialization(t *testing.T) {
	dt, cloud, _ := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	prepareNSGTest(t, dt)
	cloud.nic = nil
	for _, group := range cloud.groups {
		group.Properties.NetworkInterfaces = nil
	}
	assert.NoError(t, dt.discoverNICPolicies(context.Background(), nil))
	assert.Empty(t, cloud.groups)
	assert.Empty(t, dt.dirtyNICPolicies)
	assert.Equal(t, 1, cloud.groupDeletes)
	assert.NoError(t, dt.discoverNICPolicies(context.Background(), nil), "repeating startup cleanup is harmless")
	assert.Equal(t, 1, cloud.groupDeletes)
}

func TestNICPoliciesRejectsForeignAssociationBeforeCreatingGroup(t *testing.T) {
	dt, cloud, _ := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	cloud.nic.Properties.NetworkSecurityGroup = &armnetwork.SecurityGroup{
		ID: ptr.To("/subscriptions/network-sub/resourceGroups/network-rg/providers/Microsoft.Network/networkSecurityGroups/foreign"),
	}
	prepared, err := dt.prepareNICPolicies(context.Background(), dt.takeNICPolicyWork())
	assert.ErrorContains(t, err, "different NIC NSG")
	assert.Equal(t, 0, cloud.groupWrites)
	assert.Equal(t, 0, cloud.nicWrites)
	assert.Empty(t, nsgTestLocations(dt, prepared).Locations)
}

func TestNICPoliciesRejectsUnownedRulesWithoutWriting(t *testing.T) {
	service := nsgKubeService("svc-lb", "20.0.0.0/8")
	dt, cloud, _ := newNSGTestTracker(t, service)
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	prepared := prepareNSGTest(t, dt)
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepared))
	for _, group := range cloud.groups {
		group.Properties.SecurityRules = append(group.Properties.SecurityRules, &armnetwork.SecurityRule{
			Name: ptr.To("external-rule"),
			Properties: &armnetwork.SecurityRulePropertiesFormat{
				Direction: ptr.To(armnetwork.SecurityRuleDirectionInbound), Access: ptr.To(armnetwork.SecurityRuleAccessDeny),
				Protocol: ptr.To(armnetwork.SecurityRuleProtocolTCP), Priority: ptr.To[int32](200),
				SourceAddressPrefix: ptr.To("*"), DestinationAddressPrefix: ptr.To("*"),
				SourcePortRange: ptr.To("*"), DestinationPortRange: ptr.To("22"),
			},
		})
	}
	before := nsgCopy(cloud.groups)
	service.Annotations[consts.ServiceAnnotationAllowedIPRanges] = "40.0.0.0/8"
	assert.NoError(t, dt.ReconcileInboundService(service))
	prepared, err := dt.prepareNICPolicies(context.Background(), dt.takeNICPolicyWork())
	assert.ErrorContains(t, err, "unexpected custom rule")
	assert.Equal(t, 1, cloud.groupWrites)
	assert.Equal(t, 1, cloud.nicWrites)
	assert.Equal(t, before, cloud.groups)
	assert.Empty(t, nsgTestLocations(dt, prepared).Locations, "existing published backends remain unchanged")
}

func TestNICPoliciesReplacesOnlyTheConfiguredClusterAssociation(t *testing.T) {
	for _, resourceGroup := range []string{"", "shared-rg"} {
		t.Run("cluster NSG RG="+resourceGroup, func(t *testing.T) {
			dt, cloud, _ := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
			dt.config.SecurityGroupName = "cluster-nsg"
			dt.config.SecurityGroupResourceGroup = resourceGroup
			if resourceGroup == "" {
				resourceGroup = dt.config.ResourceGroup
			}
			cloud.nic.Properties.NetworkSecurityGroup = &armnetwork.SecurityGroup{
				ID: ptr.To(fmt.Sprintf("/subscriptions/network-sub/resourceGroups/%s/providers/Microsoft.Network/networkSecurityGroups/CLUSTER-NSG", resourceGroup)),
			}
			addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
			prepared := prepareNSGTest(t, dt)
			assert.Equal(t, 1, cloud.groupWrites)
			assert.Equal(t, 1, cloud.nicWrites)
			assert.True(t, nsgTestLocations(dt, prepared).Locations["10.0.0.1"].Addresses["10.1.0.1"].ServiceRef.Has("svc-lb"))
		})
	}
}

func TestNICPoliciesNodeIdentityChanges(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		t.Run(fmt.Sprintf("NSG already prepared=%t", prepared), func(t *testing.T) {
			dt, cloud, node := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
			addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
			dt.ReconcileNodeChange(nil, node)
			var proof map[string]*nsgPreparedNIC
			if prepared {
				proof = prepareNSGTest(t, dt)
			}
			replacement := node.DeepCopy()
			replacement.UID = "replacement"
			_, err := dt.kubeClient.CoreV1().Nodes().Update(context.Background(), replacement, metav1.UpdateOptions{})
			assert.NoError(t, err)
			dt.ReconcileNodeChange(node, replacement)
			dt.ReconcileNodeChange(node, nil)
			assert.Empty(t, nsgTestLocations(dt, proof).Locations, "a replaced Node must be revalidated")
			proof = prepareNSGTest(t, dt)
			assert.Equal(t, 1, cloud.groupWrites)
			assert.Equal(t, 1, cloud.nicWrites)
			assert.True(t, nsgTestLocations(dt, proof).Locations["10.0.0.1"].Addresses["10.1.0.1"].ServiceRef.Has("svc-lb"))

			previous := replacement.DeepCopy()
			replacement.Spec.ProviderID += "-different"
			_, err = dt.kubeClient.CoreV1().Nodes().Update(context.Background(), replacement, metav1.UpdateOptions{})
			assert.NoError(t, err)
			dt.ReconcileNodeChange(previous, replacement)
			proof, err = dt.prepareNICPolicies(context.Background(), dt.takeNICPolicyWork())
			assert.ErrorContains(t, err, "provider identity does not match")
			assert.Empty(t, nsgTestLocations(dt, proof).Locations)
			assert.Equal(t, 1, cloud.nicWrites)
		})
	}
}

func TestNICPoliciesRequiresCompleteWriteAcknowledgment(t *testing.T) {
	dt, cloud, _ := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	cloud.incompleteAck = true
	prepared, err := dt.prepareNICPolicies(context.Background(), dt.takeNICPolicyWork())
	assert.Error(t, err)
	assert.Equal(t, 0, cloud.nicWrites)
	assert.Empty(t, nsgTestLocations(dt, prepared).Locations)
	cloud.incompleteAck = false
	prepared = prepareNSGTest(t, dt)
	assert.True(t, nsgTestLocations(dt, prepared).Locations["10.0.0.1"].Addresses["10.1.0.1"].ServiceRef.Has("svc-lb"))
}

func TestNICPoliciesRevokesLBPreparationOnLiveOwnerChanges(t *testing.T) {
	for _, scenario := range []string{"cold replacement", "stale replacement", "unchanged"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			service := nsgKubeService("svc-lb", "20.0.0.0/8")
			dt, _, node := newNSGTestTracker(t, service, nsgKubeService("unrelated-lb", "40.0.0.0/8"))
			addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
			dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
			service.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
			assert.NoError(t, dt.ReconcileInboundService(service))
			updater := &LocationsUpdater{diffTracker: dt, ctx: ctx, logger: dt.logger}
			updater.process(ctx)
			assert.True(t, dt.pendingServiceOps["svc-lb"].NSGPrepared)
			dt.pendingServiceOps["unrelated-lb"].NSGPrepared = true

			current := node.DeepCopy()
			if scenario != "unchanged" {
				current.UID = "replacement"
				current.Spec.ProviderID += "-replacement"
			}
			_, err := dt.kubeClient.CoreV1().Nodes().Update(ctx, current, metav1.UpdateOptions{})
			assert.NoError(t, err)
			if scenario == "stale replacement" {
				setTestNodeLister(t, dt, node)
			} else {
				setTestNodeLister(t, dt)
			}
			dt.mu.Lock()
			dt.markServiceNICPoliciesDirtyLocked("svc-lb")
			dt.mu.Unlock()
			_, err = dt.prepareNICPolicies(ctx, dt.takeNICPolicyWork())
			if scenario == "unchanged" {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
				if assert.NotNil(t, dt.NRPResources.Locations["10.0.0.1"].nodeOwner) {
					assert.True(t, dt.NRPResources.Locations["10.0.0.1"].nodeOwner.draining)
				}
			}
			dt.mu.Lock()
			allowed := dt.readyForLBUpdateLocked("svc-lb")
			dt.mu.Unlock()
			assert.Equal(t, scenario == "unchanged", allowed, "LB preparation permission belongs to the confirmed physical owner")
			assert.True(t, dt.pendingServiceOps["unrelated-lb"].NSGPrepared)
		})
	}
}

func TestNICPoliciesRetainsFailedWorkAcrossLocationReuse(t *testing.T) {
	for _, currentB := range []string{"10.0.0.1", "10.0.0.3"} {
		for _, restrictedA := range []bool{false, true} {
			t.Run(fmt.Sprintf("replacement=%s/restricted-first-owner=%t", currentB, restrictedA), func(t *testing.T) {
				ctx := context.Background()
				const p, q = "10.0.0.1", "10.0.0.2"
				serviceA := nsgKubeService("a-lb", "20.0.0.0/8")
				if !restrictedA {
					serviceA.Annotations = nil
				}
				dt, cloud, a := newNSGTestTracker(t, serviceA, nsgKubeService("b-lb", "20.0.0.0/8"))
				// The VM fixture retains pointers to the initial NIC's resource IDs.
				vmNIC := cloud.nic
				addNSGTestBackend(dt, "a-lb", p, "10.1.0.1", "")
				dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
				b := a.DeepCopy()
				b.Name, b.UID = "node-b", "node-b"
				b.Spec.ProviderID += "-b"
				b.Status.Addresses = []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: q}}
				_, err := dt.kubeClient.CoreV1().Nodes().Create(ctx, b, metav1.CreateOptions{})
				assert.NoError(t, err)
				dt.NRPResources.Locations[q] = NRPLocation{
					Addresses: map[string]NRPAddress{"10.2.0.1": {Services: utilsets.NewString("b-lb")}},
					nodeOwner: &nsgLocationOwner{name: b.Name, uid: b.UID, providerID: b.Spec.ProviderID},
				}
				target, err := dt.targetFromID("/subscriptions/compute-sub/resourceGroups/node-rg/providers/Microsoft.Network/networkInterfaces/node-b-nic")
				assert.NoError(t, err)
				rules, err := buildNICSecurityRules(map[string]nsgService{
					"b-lb": nsgTestService(8080, "TCP", "20.0.0.0/8", "10.2.0.1"),
				}, nil)
				assert.NoError(t, err)
				group, _, err := dt.desiredGroup(nil, target, b.Name, "eastus", rules)
				assert.NoError(t, err)
				group.Etag = ptr.To("b-etag")
				group.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateSucceeded)
				cloud.groups[*group.Name] = &group

				movedA, movedB := a.DeepCopy(), b.DeepCopy()
				movedA.Status.Addresses[0].Address = q
				movedB.Status.Addresses[0].Address = currentB
				for _, node := range []*v1.Node{movedA, movedB} {
					_, err := dt.kubeClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
					assert.NoError(t, err)
				}
				cloud.nic.Properties.IPConfigurations[0].Properties.PrivateIPAddress = ptr.To(q)
				dt.ReconcileNodeChange(a, movedA)
				dt.ReconcileNodeChange(b, movedB)
				dt.UpdateEndpoints("a-lb", map[string]string{"10.1.0.1": p}, nil)
				mockNSGLocationUpdates(t, dt, func(context.Context, armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
					return nil
				})
				updater := &LocationsUpdater{diffTracker: dt, ctx: ctx, logger: dt.logger}
				updater.process(ctx)
				assert.Empty(t, dt.NRPResources.Locations)
				assert.Equal(t, 1, updater.failureCount)
				assert.True(t, dt.dirtyNICPolicies.Has(currentB), "successful cleanup must not remove another NIC's retry location")
				assert.Equal(t, armnetwork.SecurityRuleAccessDeny,
					nsgTestAccess(t, cloud.groups[*group.Name].Properties.SecurityRules, "40.1.2.3", "10.2.0.1", 8080, armnetwork.SecurityRuleProtocolTCP))

				*vmNIC.ID = target.String()
				vmNIC.Name = ptr.To(target.Name)
				*vmNIC.Properties.VirtualMachine.ID += "-b"
				vmNIC.Properties.IPConfigurations[0].Properties.PrivateIPAddress = ptr.To(currentB)
				vmNIC.Properties.NetworkSecurityGroup = &armnetwork.SecurityGroup{ID: ptr.To(dt.groupID(target))}
				cloud.nic = vmNIC
				updater.process(ctx)
				assert.Zero(t, updater.failureCount)
				assert.Empty(t, dt.dirtyNICPolicies)
				for _, current := range cloud.groups {
					assert.Len(t, current.Properties.SecurityRules, 2, "both NICs finish cleanup without another Node or Service event")
				}
			})
		}
	}
}

func TestNICPoliciesRetriesUnresolvedOwnerCleanup(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		for _, partialCache := range []bool{false, true} {
			t.Run(fmt.Sprintf("unavailable=%t/partial-cache=%t", unavailable, partialCache), func(t *testing.T) {
				ctx := context.Background()
				dt, cloud, _ := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
				addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
				dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
				dt.UpdateEndpoints("svc-lb", map[string]string{"10.1.0.1": "10.0.0.1"}, nil)
				if partialCache {
					setTestNodeLister(t, dt, &v1.Node{
						ObjectMeta: metav1.ObjectMeta{Name: "unrelated", UID: "unrelated"},
						Status: v1.NodeStatus{Addresses: []v1.NodeAddress{
							{Type: v1.NodeInternalIP, Address: "10.9.0.1"},
						}},
					})
				} else {
					setTestNodeLister(t, dt)
				}
				blocked := unavailable
				client := dt.kubeClient.(*fake.Clientset)
				client.ClearActions()
				client.PrependReactor("get", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
					if blocked {
						return true, nil, errors.New("Node API unavailable")
					}
					return false, nil, nil
				})
				mockNSGLocationUpdates(t, dt, func(context.Context, armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
					return nil
				})
				updater := &LocationsUpdater{diffTracker: dt, ctx: ctx, logger: dt.logger}
				updater.process(ctx)
				assert.Empty(t, dt.NRPResources.Locations)
				if unavailable {
					assert.Equal(t, 1, updater.failureCount)
					assert.True(t, dt.dirtyNICPolicies.Has("10.0.0.1"), "unresolved owner cleanup must survive the withdrawal ACK")
					blocked = false
					updater.process(ctx)
				}
				assert.Zero(t, updater.failureCount)
				assert.Empty(t, dt.dirtyNICPolicies)
				for _, group := range cloud.groups {
					assert.Len(t, group.Properties.SecurityRules, 2, "recovery must remove obsolete rules without another event")
				}
				lists := 0
				for _, action := range client.Actions() {
					if action.Matches("list", "nodes") {
						lists++
					}
				}
				if unavailable {
					assert.Equal(t, 1, lists, "only the unresolved post-ACK location needs a live list")
				} else {
					assert.Zero(t, lists, "known owners use targeted reads")
				}
			})
		}
	}
}

func TestNICPoliciesGatesUnrestrictedPortChanges(t *testing.T) {
	for _, port := range []int32{8080, 10080} {
		t.Run(fmt.Sprintf("port=%d", port), func(t *testing.T) {
			a, b := nsgKubeService("a-lb", "20.0.0.0/8"), nsgKubeService("b-lb", "")
			b.Annotations = nil
			b.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
			dt, cloud, _ := newNSGTestTracker(t, a, b)
			addNSGTestBackend(dt, "a-lb", "10.0.0.1", "10.1.0.1", "")
			addNSGTestBackend(dt, "b-lb", "10.0.0.1", "10.1.0.1", "")
			dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
			b.Spec.Ports[0].TargetPort = intstr.FromInt32(port)
			assert.NoError(t, dt.ReconcileInboundService(b))
			dt.mu.Lock()
			allowed := dt.readyForLBUpdateLocked("b-lb")
			dt.mu.Unlock()
			assert.False(t, allowed, "an unrestricted port change must first check the shared NIC policy")
			updater := &LocationsUpdater{diffTracker: dt, ctx: context.Background(), logger: dt.logger}
			updater.process(context.Background())
			dt.mu.Lock()
			allowed = dt.readyForLBUpdateLocked("b-lb")
			dt.mu.Unlock()
			assert.Equal(t, port != 8080, allowed)
			assert.Equal(t, 1, cloud.groupWrites)
			assert.True(t, dt.NRPResources.Locations["10.0.0.1"].Addresses["10.1.0.1"].Services.Has("b-lb"))
		})
	}
}

func TestNICPoliciesAllowsCorrectiveUnrestrictedLBUpdates(t *testing.T) {
	for _, previouslyRestricted := range []bool{false, true} {
		for _, unknownResult := range []bool{false, true} {
			t.Run(fmt.Sprintf("previously-restricted=%t/unknown-result=%t", previouslyRestricted, unknownResult), func(t *testing.T) {
				ctx := context.Background()
				a, b := nsgKubeService("a-lb", "20.0.0.0/8"), nsgKubeService("b-lb", "40.0.0.0/8")
				if !previouslyRestricted {
					a.Annotations, b.Annotations = nil, nil
				}
				dt, cloud, _ := newNSGTestTracker(t, a, b)
				if !previouslyRestricted {
					addNSGTestBackend(dt, "a-lb", "10.0.0.1", "10.1.0.1", "")
				}
				addNSGTestBackend(dt, "b-lb", "10.0.0.1", "10.1.0.1", "")
				dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
				writes := cloud.groupWrites
				b.Spec.Ports[0].Port = 81
				assert.NoError(t, dt.ReconcileInboundService(b))
				op := dt.pendingServiceOps["b-lb"]
				previous := op.Config
				op.InFlightConfig = &previous
				dt.OnServiceCreationComplete("b-lb", false, errors.New("old LB response lost"))
				assert.Contains(t, op.RetainedBackendPorts, PortMapping{Port: 8080, Protocol: "TCP"})

				a.Annotations = map[string]string{consts.ServiceAnnotationAllowedIPRanges: "20.0.0.0/8"}
				assert.NoError(t, dt.ReconcileInboundService(a))
				addNSGTestBackend(dt, "a-lb", "10.0.0.1", "10.1.0.1", "")
				b.Annotations = nil
				b.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
				assert.NoError(t, dt.ReconcileInboundService(b))
				addNSGTestBackend(dt, "b-lb", "10.0.0.1", "10.1.0.2", "")
				mockNSGLocationUpdates(t, dt, func(context.Context, armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
					return nil
				})
				updater := &LocationsUpdater{diffTracker: dt, ctx: ctx, logger: dt.logger}
				updater.process(ctx)
				assert.True(t, op.NSGPrepared, "the current policy permits the corrective target even while old ports remain protected")
				assert.NotEmpty(t, dt.serviceUpdaterTrigger)
				assert.NotEmpty(t, dt.dirtyNICPolicies, "observational permission does not finish NIC work")
				assert.Equal(t, writes, cloud.groupWrites, "observation-only preparation must not prune or install A's refused policy")
				assert.NotContains(t, dt.NRPResources.Locations["10.0.0.1"].Addresses, "10.1.0.2")
				assert.Equal(t, !previouslyRestricted, dt.NRPResources.Locations["10.0.0.1"].Addresses["10.1.0.1"].Services.Has("a-lb"))
				if unknownResult {
					attempt := op.Config
					op.InFlightConfig = &attempt
					dt.OnServiceCreationComplete("b-lb", false, errors.New("corrective LB response lost"))
					updater.process(ctx)
					assert.True(t, op.NSGPrepared, "an uncertain corrective write can retry without discarding its old obligations")
					assert.Contains(t, op.RetainedBackendPorts, PortMapping{Port: 8080, Protocol: "TCP"})
					assert.Equal(t, writes, cloud.groupWrites)
				}
				applied := op.Config
				op.InFlightConfig = &applied
				dt.OnServiceCreationComplete("b-lb", true, nil)
				updater.process(ctx)
				assert.Empty(t, op.RetainedBackendPorts)
				assert.Empty(t, dt.dirtyNICPolicies)
				assert.Greater(t, cloud.groupWrites, writes)
				assert.True(t, dt.NRPResources.Locations["10.0.0.1"].Addresses["10.1.0.1"].Services.Has("a-lb"))
				assert.True(t, dt.NRPResources.Locations["10.0.0.1"].Addresses["10.1.0.2"].Services.Has("b-lb"))
				for _, group := range cloud.groups {
					assert.Equal(t, armnetwork.SecurityRuleAccessDeny,
						nsgTestAccess(t, group.Properties.SecurityRules, "60.1.2.3", "10.1.0.1", 8080, armnetwork.SecurityRuleProtocolTCP))
					assert.Equal(t, armnetwork.SecurityRuleAccessAllow,
						nsgTestAccess(t, group.Properties.SecurityRules, "60.1.2.3", "10.1.0.1", 9090, armnetwork.SecurityRuleProtocolTCP))
				}
			})
		}
	}
}

func TestNICPoliciesRequiresAllNICsForObservedLBUpdates(t *testing.T) {
	ctx := context.Background()
	a, b := nsgKubeService("a-lb", ""), nsgKubeService("b-lb", "")
	a.Annotations, b.Annotations = nil, nil
	dt, cloud, _ := newNSGTestTracker(t, a, b)
	addNSGTestBackend(dt, "a-lb", "10.0.0.1", "10.1.0.1", "")
	addNSGTestBackend(dt, "b-lb", "10.0.0.1", "10.1.0.1", "")
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, nil))
	dt.NRPResources.Locations["10.0.0.2"] = NRPLocation{
		Addresses: map[string]NRPAddress{"10.2.0.1": {Services: utilsets.NewString("b-lb")}},
	}
	a.Annotations = map[string]string{consts.ServiceAnnotationAllowedIPRanges: "20.0.0.0/8"}
	assert.NoError(t, dt.ReconcileInboundService(a))
	b.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
	assert.NoError(t, dt.ReconcileInboundService(b))
	calls := 0
	mockNSGLocationUpdates(t, dt, func(context.Context, armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
		calls++
		if calls == 1 {
			return errors.New("withdrawal response lost")
		}
		return nil
	})
	updater := &LocationsUpdater{diffTracker: dt, ctx: ctx, logger: dt.logger}
	updater.process(ctx)
	assert.False(t, dt.pendingServiceOps["b-lb"].NSGPrepared, "one observed NIC cannot authorize an unconfirmed binding on another NIC")
	assert.True(t, dt.dirtyNICPolicies.Has("10.0.0.2"))
	updater.process(ctx)
	assert.True(t, dt.pendingServiceOps["b-lb"].NSGPrepared, "withdrawal ACK removes the unconfirmed NIC's obligation")
	assert.Equal(t, 2, calls)
	assert.Zero(t, cloud.groupWrites)
}

func TestNICPoliciesValidatesObservedLBPermission(t *testing.T) {
	for _, scenario := range []string{
		"attached", "unattached", "absent", "old owner", "draining owner", "missing deny", "missing allow-other",
		"custom rule", "wrong region", "NSG updating", "NSG failed", "NIC updating", "NIC failed",
		"missing owned group", "foreign association", "NIC missing", "identity changed",
	} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			a, b := nsgKubeService("a-lb", "20.0.0.0/8"), nsgKubeService("b-lb", "40.0.0.0/8")
			dt, cloud, node := newNSGTestTracker(t, a, b)
			addNSGTestBackend(dt, "b-lb", "10.0.0.1", "10.1.0.1", "")
			dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
			addNSGTestBackend(dt, "a-lb", "10.0.0.1", "10.1.0.1", "")
			b.Annotations = nil
			b.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
			assert.NoError(t, dt.ReconcileInboundService(b))
			for name, group := range cloud.groups {
				switch scenario {
				case "missing deny":
					group.Properties.SecurityRules = slices.DeleteFunc(group.Properties.SecurityRules, func(rule *armnetwork.SecurityRule) bool {
						return *rule.Properties.Access == armnetwork.SecurityRuleAccessDeny
					})
				case "missing allow-other":
					group.Properties.SecurityRules = slices.DeleteFunc(group.Properties.SecurityRules, func(rule *armnetwork.SecurityRule) bool {
						return nsgRuleServiceUID(rule) == ""
					})
				case "custom rule":
					rule := nsgCopy(group.Properties.SecurityRules[0])
					rule.Name, rule.Properties.Description = ptr.To("customer-rule"), ptr.To("custom")
					group.Properties.SecurityRules = append(group.Properties.SecurityRules, rule)
				case "wrong region":
					group.Location = ptr.To("different-region")
				case "NSG updating":
					group.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateUpdating)
				case "NSG failed":
					group.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateFailed)
				case "missing owned group", "absent":
					delete(cloud.groups, name)
				}
			}
			switch scenario {
			case "unattached", "absent":
				cloud.nic.Properties.NetworkSecurityGroup = nil
			case "NIC updating":
				cloud.nic.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateUpdating)
			case "NIC failed":
				cloud.nic.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateFailed)
			case "foreign association":
				cloud.nic.Properties.NetworkSecurityGroup = &armnetwork.SecurityGroup{ID: ptr.To("/foreign/nsg")}
			case "NIC missing":
				cloud.nic = nil
			case "identity changed":
				setTestNodeLister(t, dt, node)
				replacement := node.DeepCopy()
				replacement.UID = "replacement"
				_, err := dt.kubeClient.CoreV1().Nodes().Update(ctx, replacement, metav1.UpdateOptions{})
				assert.NoError(t, err)
			}
			work := dt.takeNICPolicyWork()
			prepared, err := dt.prepareNICPolicies(ctx, work)
			assert.Error(t, err, "observational permission must retain the original reconciliation failure")
			want := scenario == "attached" || scenario == "unattached" || scenario == "absent" ||
				scenario == "old owner" || scenario == "draining owner"
			plan := prepared["10.0.0.1"]
			assert.Equal(t, want, plan != nil && plan.lbOnly)
			assert.False(t, dt.pendingServiceOps["b-lb"].NSGPrepared, "a single preparation does not bypass all-NIC finalization")
			switch scenario {
			case "old owner":
				dt.NRPResources.Locations["10.0.0.1"].nodeOwner.uid = "old-owner"
				want = false
			case "draining owner":
				dt.NRPResources.Locations["10.0.0.1"].nodeOwner.draining = true
				want = false
			}
			dt.mu.Lock()
			dt.finishNICPolicyWorkLocked(work, prepared)
			dt.mu.Unlock()
			assert.Equal(t, want, dt.pendingServiceOps["b-lb"].NSGPrepared)
			assert.NotEmpty(t, dt.dirtyNICPolicies)
			assert.Equal(t, 1, cloud.groupWrites, "observation does not repair or overwrite the fetched model")
		})
	}
}

func TestNICPoliciesDiscardsFailedWriteProofForLBUpdates(t *testing.T) {
	for _, scenario := range []string{"write error", "applied with lost response", "incomplete ACK", "identity changed after write"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			a, b := nsgKubeService("a-lb", "20.0.0.0/8"), nsgKubeService("b-lb", "")
			b.Annotations = nil
			b.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
			dt, cloud, node := newNSGTestTracker(t, a, b)
			addNSGTestBackend(dt, "a-lb", "10.0.0.1", "10.1.0.1", "")
			addNSGTestBackend(dt, "b-lb", "10.0.0.1", "10.1.0.1", "")
			dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
			a.Annotations[consts.ServiceAnnotationAllowedIPRanges] = "40.0.0.0/8"
			assert.NoError(t, dt.ReconcileInboundService(a))
			b.Spec.Ports[0].TargetPort = intstr.FromInt32(10080)
			assert.NoError(t, dt.ReconcileInboundService(b))
			switch scenario {
			case "write error", "applied with lost response":
				cloud.groupError = errors.New("NSG response lost")
				cloud.applyOnError = scenario == "applied with lost response"
			case "incomplete ACK":
				cloud.incompleteAck = true
			case "identity changed after write":
				cloud.beforeWrite = func(context.Context) {
					replacement := node.DeepCopy()
					replacement.UID = "replacement"
					_, err := dt.kubeClient.CoreV1().Nodes().Update(ctx, replacement, metav1.UpdateOptions{})
					assert.NoError(t, err)
				}
			}
			work := dt.takeNICPolicyWork()
			prepared, err := dt.prepareNICPolicies(ctx, work)
			assert.Error(t, err)
			assert.Empty(t, prepared, "a failed write or identity check is not an observational plan")
			dt.mu.Lock()
			dt.finishNICPolicyWorkLocked(work, prepared)
			dt.mu.Unlock()
			assert.False(t, dt.pendingServiceOps["b-lb"].NSGPrepared)
			assert.NotEmpty(t, dt.dirtyNICPolicies)
		})
	}
}

func TestNICPoliciesReadsUnrestrictedLivePortsForConflicts(t *testing.T) {
	for _, livePort := range []int32{8080, 9090} {
		t.Run(fmt.Sprintf("live-port=%d", livePort), func(t *testing.T) {
			ctx := context.Background()
			a, b := nsgKubeService("a-lb", "20.0.0.0/8"), nsgKubeService("b-lb", "")
			b.Annotations = nil
			b.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
			dt, cloud, _ := newNSGTestTracker(t, a, b)
			addNSGTestBackend(dt, "a-lb", "10.0.0.1", "10.1.0.1", "")
			addNSGTestBackend(dt, "b-lb", "10.0.0.1", "10.1.0.1", "")
			dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
			b.Spec.Ports[0].TargetPort = intstr.FromInt32(10080)
			assert.NoError(t, dt.ReconcileInboundService(b))
			op := dt.pendingServiceOps["b-lb"]
			op.LastAppliedConfig = nil
			addNSGTestBackend(dt, "a-lb", "10.0.0.1", "10.1.0.2", "")
			addNSGTestBackend(dt, "b-lb", "10.0.0.1", "10.1.0.2", "")
			_, live, _, err := buildInboundServiceResources("b-lb", &InboundConfig{
				FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}},
				BackendPorts:  []PortMapping{{Port: livePort, Protocol: "TCP"}},
			}, dt.config)
			assert.NoError(t, err)
			live.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateSucceeded)
			client := mock_loadbalancerclient.NewMockInterface(gomock.NewController(t))
			dt.networkClientFactory.(*mock_azclient.MockClientFactory).EXPECT().GetLoadBalancerClient().Return(client).AnyTimes()
			client.EXPECT().Get(gomock.Any(), "network-rg", "b-lb", nil).Return(&live, nil).Times(1)
			mockNSGLocationUpdates(t, dt, func(context.Context, armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
				return nil
			})
			updater := &LocationsUpdater{diffTracker: dt, ctx: ctx, logger: dt.logger}
			updater.process(ctx)
			assert.Contains(t, op.RetainedBackendPorts, PortMapping{Port: livePort, Protocol: "TCP"})
			assert.True(t, op.NSGPrepared)
			if livePort == 8080 {
				assert.Equal(t, 1, cloud.groupWrites)
				assert.NotContains(t, dt.NRPResources.Locations["10.0.0.1"].Addresses, "10.1.0.2")
			} else {
				assert.Equal(t, 2, cloud.groupWrites)
				assert.True(t, dt.NRPResources.Locations["10.0.0.1"].Addresses["10.1.0.2"].Services.Has("b-lb"))
			}
		})
	}
}

func TestNICPoliciesChecksRecoveredUnpublishedUnrestrictedPorts(t *testing.T) {
	for _, tt := range []struct {
		name      string
		state     armnetwork.ProvisioningState
		port      int32
		applied   bool
		published bool
	}{
		{"failed", armnetwork.ProvisioningStateFailed, 9090, false, false},
		{"updating", armnetwork.ProvisioningStateUpdating, 9090, false, false},
		{"settled conflicting port", armnetwork.ProvisioningStateSucceeded, 8080, false, false},
		{"settled safe port", armnetwork.ProvisioningStateSucceeded, 9090, false, true},
		{"applied control", armnetwork.ProvisioningStateFailed, 9090, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			a, b := nsgKubeService("a-lb", "20.0.0.0/8"), nsgKubeService("b-lb", "")
			b.Annotations = nil
			b.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
			dt, _, _ := newNSGTestTracker(t, a, b)
			addNSGTestBackend(dt, "a-lb", "10.0.0.1", "10.1.0.1", "")
			dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
			_, listed, _, err := buildInboundServiceResources("b-lb", dt.pendingServiceOps["b-lb"].Config.InboundConfig, dt.config)
			assert.NoError(t, err)
			listed.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateFailed)
			if !tt.applied {
				delete(dt.pendingServiceOps, "b-lb")
				dt.prepareNICPolicyInitialization(map[string]*v1.Service{"b-lb": b}, map[string]*armnetwork.LoadBalancer{"b-lb": &listed})
				assert.Nil(t, dt.pendingServiceOps["b-lb"].LastAppliedConfig)
			}
			addNSGTestBackend(dt, "b-lb", "10.0.0.1", "10.1.0.1", "")
			live := nsgCopy(listed)
			live.Properties.ProvisioningState = ptr.To(tt.state)
			live.Properties.LoadBalancingRules[0].Properties.BackendPort = ptr.To(tt.port)
			client := mock_loadbalancerclient.NewMockInterface(gomock.NewController(t))
			dt.networkClientFactory.(*mock_azclient.MockClientFactory).EXPECT().GetLoadBalancerClient().Return(client).AnyTimes()
			reads := 0
			client.EXPECT().Get(gomock.Any(), "network-rg", "b-lb", nil).
				DoAndReturn(func(context.Context, string, string, *string) (*armnetwork.LoadBalancer, error) {
					reads++
					return &live, nil
				}).AnyTimes()
			mockNSGLocationUpdates(t, dt, func(context.Context, armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
				return nil
			})
			updater := &LocationsUpdater{diffTracker: dt, ctx: ctx, logger: dt.logger}
			updater.process(ctx)
			published := dt.NRPResources.Locations["10.0.0.1"].Addresses["10.1.0.1"].Services.Has("b-lb")
			if !assert.Equal(t, tt.published, published, "unsettled or conflicting live ports cannot authorize a new unrestricted binding") {
				return
			}
			if tt.applied {
				assert.Zero(t, reads)
			} else {
				assert.Equal(t, 1, reads)
			}
			if tt.published {
				return
			}
			assert.Positive(t, updater.failureCount)
			assert.NoError(t, dt.ReconcileInboundService(b))
			client.EXPECT().CreateOrUpdate(gomock.Any(), "network-rg", "b-lb", gomock.Any()).
				DoAndReturn(func(_ context.Context, _, _ string, value armnetwork.LoadBalancer) (*armnetwork.LoadBalancer, error) {
					assert.False(t, dt.NRPResources.Locations["10.0.0.1"].Addresses["10.1.0.1"].Services.Has("b-lb"))
					value.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateSucceeded)
					return &value, nil
				}).Times(1)
			worker := NewServiceUpdater(ctx, dt, dt.OnServiceCreationComplete, dt.serviceUpdaterTrigger)
			t.Cleanup(worker.Stop)
			worker.processBatch()
			worker.wg.Wait()
			assert.NotNil(t, dt.pendingServiceOps["b-lb"].LastAppliedConfig)
			updater.process(ctx)
			assert.True(t, dt.NRPResources.Locations["10.0.0.1"].Addresses["10.1.0.1"].Services.Has("b-lb"))
			assert.Empty(t, dt.dirtyNICPolicies)
		})
	}
}

func TestNICPoliciesResolvesMovedOwnerAfterNodeObservation(t *testing.T) {
	ctx := context.Background()
	dt, cloud, node := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
	dt.UpdateEndpoints("svc-lb", map[string]string{"10.1.0.1": "10.0.0.1"}, nil)
	moved := node.DeepCopy()
	moved.Status.Addresses[0].Address = "10.0.0.2"
	_, err := dt.kubeClient.CoreV1().Nodes().Update(ctx, moved, metav1.UpdateOptions{})
	assert.NoError(t, err)
	cloud.nic.Properties.IPConfigurations[0].Properties.PrivateIPAddress = ptr.To("10.0.0.2")
	setTestNodeLister(t, dt)
	blocked := true
	dt.kubeClient.(*fake.Clientset).PrependReactor("get", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		if blocked {
			return true, nil, errors.New("Node API unavailable")
		}
		return false, nil, nil
	})
	mockNSGLocationUpdates(t, dt, func(context.Context, armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
		return nil
	})
	updater := &LocationsUpdater{diffTracker: dt, ctx: ctx, logger: dt.logger}
	updater.process(ctx)
	assert.Empty(t, dt.NRPResources.Locations)
	blocked = false
	updater.process(ctx)
	dt.ReconcileNodeChange(node, moved)
	updater.process(ctx)
	assert.Empty(t, dt.dirtyNICPolicies)
	for _, group := range cloud.groups {
		assert.Len(t, group.Properties.SecurityRules, 2)
	}
}

func TestNICPoliciesUpdatesSourcePolicyWhileLBParked(t *testing.T) {
	for _, parked := range []bool{false, true} {
		t.Run(fmt.Sprintf("parked=%t", parked), func(t *testing.T) {
			ctx := context.Background()
			service := nsgKubeService("svc-lb", "")
			service.Annotations = nil
			dt, cloud, _ := newNSGTestTracker(t, service)
			addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
			dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, nil))
			worker := NewServiceUpdater(ctx, dt, dt.OnServiceCreationComplete, dt.serviceUpdaterTrigger)
			t.Cleanup(worker.Stop)
			if parked {
				service.Spec.Ports[0].Port = 65535
				assert.NoError(t, dt.ReconcileInboundService(service))
				worker.processBatch()
				worker.wg.Wait()
				assert.True(t, dt.pendingServiceOps["svc-lb"].CreationFailedTerminal)
				assert.True(t, dt.NRPResources.LoadBalancers.Has("svc-lb"))
			}
			for index, sources := range []string{"20.0.0.0/8", "40.0.0.0/8", ""} {
				service.Annotations = nil
				if sources != "" {
					service.Annotations = map[string]string{consts.ServiceAnnotationAllowedIPRanges: sources}
				}
				assert.NoError(t, dt.ReconcileInboundService(service))
				prepareNSGTest(t, dt)
				op := dt.pendingServiceOps["svc-lb"]
				assert.Equal(t, parked, op.CreationFailedTerminal)
				if sources == "" {
					assert.Nil(t, op.Config.InboundConfig.AllowedIPRanges)
				} else {
					want, err := parseNSGSourceRanges(sources)
					assert.NoError(t, err)
					assert.Equal(t, want, op.Config.InboundConfig.AllowedIPRanges)
				}
				assert.Equal(t, index+1, cloud.groupWrites)
				for _, group := range cloud.groups {
					applied, err := decodeNSGServices(group.Properties.SecurityRules)
					assert.NoError(t, err)
					assert.Equal(t, op.Config.InboundConfig.AllowedIPRanges, applied["svc-lb"].sources)
					want := armnetwork.SecurityRuleAccessDeny
					if sources == "" {
						want = armnetwork.SecurityRuleAccessAllow
					}
					assert.Equal(t, want, nsgTestAccess(t, group.Properties.SecurityRules,
						"60.1.2.3", "10.1.0.1", 8080, armnetwork.SecurityRuleProtocolTCP))
				}
				worker.processBatch()
				worker.wg.Wait()
				assert.Equal(t, parked, op.CreationFailedTerminal, "source changes must not rearm the invalid LB shape")
				assert.Equal(t, int32(80), op.LastAppliedConfig.InboundConfig.FrontendPorts[0].Port)
			}
		})
	}
}

func TestNICPoliciesDefersLBDispatchAndRetainsUnconfirmedPorts(t *testing.T) {
	service := nsgKubeService("svc-lb", "20.0.0.0/8")
	dt, cloud, _ := newNSGTestTracker(t, service)
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	prepared := prepareNSGTest(t, dt)
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepared))
	service.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
	assert.NoError(t, dt.ReconcileInboundService(service))
	worker := newTestServiceUpdater(dt)
	worker.processBatch()
	worker.wg.Wait()
	assert.Nil(t, dt.pendingServiceOps["svc-lb"].InFlightConfig, "the LB write must wait for new-port protection")
	assert.Empty(t, worker.activeOps)
	updater := &LocationsUpdater{diffTracker: dt, ctx: context.Background(), logger: dt.logger}
	updater.process(context.Background())
	dt.mu.Lock()
	config := dt.pendingServiceOps["svc-lb"].Config
	assert.True(t, dt.readyForLBUpdateLocked("svc-lb"))
	dt.pendingServiceOps["svc-lb"].InFlightConfig = &config
	dt.mu.Unlock()
	service.Spec.Ports[0].TargetPort = intstr.FromInt32(10080)
	assert.NoError(t, dt.ReconcileInboundService(service))
	dt.OnServiceCreationComplete("svc-lb", false, errors.New("LB response lost"))
	prepareNSGTest(t, dt)
	for _, group := range cloud.groups {
		for _, port := range []int32{8080, 9090, 10080} {
			assert.Equal(t, armnetwork.SecurityRuleAccessDeny,
				nsgTestAccess(t, group.Properties.SecurityRules, "40.1.2.3", "10.1.0.1", port, armnetwork.SecurityRuleProtocolTCP))
		}
	}
	dt.mu.Lock()
	latest := dt.pendingServiceOps["svc-lb"].Config
	dt.pendingServiceOps["svc-lb"].InFlightConfig = &latest
	dt.mu.Unlock()
	dt.OnServiceCreationComplete("svc-lb", true, nil)
	prepareNSGTest(t, dt)
	for _, group := range cloud.groups {
		assert.Equal(t, armnetwork.SecurityRuleAccessAllow,
			nsgTestAccess(t, group.Properties.SecurityRules, "40.1.2.3", "10.1.0.1", 9090, armnetwork.SecurityRuleProtocolTCP))
		assert.Equal(t, armnetwork.SecurityRuleAccessDeny,
			nsgTestAccess(t, group.Properties.SecurityRules, "40.1.2.3", "10.1.0.1", 10080, armnetwork.SecurityRuleProtocolTCP))
	}
}

func TestNICPoliciesProtectsUnconfirmedLocationWrites(t *testing.T) {
	dt, cloud, _ := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	prepared := prepareNSGTest(t, dt)
	attempt := nsgTestLocationSync(dt, prepared)
	assert.True(t, attempt.Locations["10.0.0.1"].Addresses["10.1.0.1"].ServiceRef.Has("svc-lb"))
	dt.DeleteService("svc-lb", true, false)
	prepared = prepareNSGTest(t, dt)
	assert.Equal(t, 1, cloud.groupWrites, "an ambiguous address write must retain its protection")
	correction := nsgTestLocationSync(dt, prepared)
	change, exists := correction.Locations["10.0.0.1"]
	if !assert.True(t, exists, "the possibly applied address needs an explicit corrective removal") {
		return
	}
	for _, address := range change.Addresses {
		assert.False(t, address.ServiceRef.Has("svc-lb"))
	}
	dt.UpdateLocationsAddresses(correction)
	assert.NoError(t, dt.pruneNICPolicies(context.Background(), prepared, sets.New[string]()))
	for _, group := range cloud.groups {
		assert.Len(t, group.Properties.SecurityRules, 2)
	}
}

func TestNICPoliciesDoesNotPublishDuringProtectionRemoval(t *testing.T) {
	dt, cloud, _ := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	prepareNSGTest(t, dt)
	dt.UpdateEndpoints("svc-lb", map[string]string{"10.1.0.1": "10.0.0.1"}, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	cloud.beforeWrite = func(ctx context.Context) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	done := make(chan error, 1)
	var prepared map[string]*nsgPreparedNIC
	go func() {
		var err error
		prepared, err = dt.prepareNICPolicies(context.Background(), dt.takeNICPolicyWork())
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("NSG removal did not start")
	}
	dt.UpdateEndpoints("svc-lb", nil, map[string]string{"10.1.0.1": "10.0.0.1"})
	assert.Empty(t, nsgTestLocations(dt, nil).Locations, "a pruning write cannot authorize a re-added backend")
	close(release)
	assert.NoError(t, <-done)
	cloud.mu.Lock()
	cloud.beforeWrite = nil
	cloud.mu.Unlock()
	assert.Empty(t, nsgTestLocations(dt, prepared).Locations)
	prepared = prepareNSGTest(t, dt)
	assert.True(t, nsgTestLocations(dt, prepared).Locations["10.0.0.1"].Addresses["10.1.0.1"].ServiceRef.Has("svc-lb"))
}

func TestNICPoliciesDoesNotAuthorizeAReplacedNodeAfterAnInFlightWrite(t *testing.T) {
	dt, cloud, node := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	entered, release := make(chan struct{}), make(chan struct{})
	cloud.beforeWrite = func(ctx context.Context) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	done := make(chan error, 1)
	var prepared map[string]*nsgPreparedNIC
	go func() {
		var err error
		prepared, err = dt.prepareNICPolicies(context.Background(), dt.takeNICPolicyWork())
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("NSG write did not start")
	}
	replacement := node.DeepCopy()
	replacement.Spec.ProviderID += "-replacement"
	_, err := dt.kubeClient.CoreV1().Nodes().Update(context.Background(), replacement, metav1.UpdateOptions{})
	assert.NoError(t, err)
	dt.ReconcileNodeChange(node, replacement)
	close(release)
	assert.ErrorContains(t, <-done, "superseded")
	assert.Empty(t, nsgTestLocations(dt, prepared).Locations, "a completed write for the old VM cannot authorize the new VM")
}

func TestNICPoliciesKeepsOldBackendPortsUntilLoadBalancerUpdateCompletes(t *testing.T) {
	service := nsgKubeService("svc-lb", "20.0.0.0/8")
	dt, cloud, _ := newNSGTestTracker(t, service)
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	prepared := prepareNSGTest(t, dt)
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepared))
	service.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
	assert.NoError(t, dt.ReconcileInboundService(service))
	prepareNSGTest(t, dt)
	for _, group := range cloud.groups {
		for _, port := range []int32{8080, 9090} {
			assert.Equal(t, armnetwork.SecurityRuleAccessDeny,
				nsgTestAccess(t, group.Properties.SecurityRules, "40.1.2.3", "10.1.0.1", port, armnetwork.SecurityRuleProtocolTCP))
		}
	}
	dt.mu.Lock()
	config := dt.pendingServiceOps["svc-lb"].Config
	dt.pendingServiceOps["svc-lb"].InFlightConfig = &config
	dt.mu.Unlock()
	dt.OnServiceCreationComplete("svc-lb", true, nil)
	prepareNSGTest(t, dt)
	for _, group := range cloud.groups {
		assert.Equal(t, armnetwork.SecurityRuleAccessAllow,
			nsgTestAccess(t, group.Properties.SecurityRules, "40.1.2.3", "10.1.0.1", 8080, armnetwork.SecurityRuleProtocolTCP))
		assert.Equal(t, armnetwork.SecurityRuleAccessDeny,
			nsgTestAccess(t, group.Properties.SecurityRules, "40.1.2.3", "10.1.0.1", 9090, armnetwork.SecurityRuleProtocolTCP))
	}
}

func TestNICPoliciesPublishesOnlyConfirmedLBPorts(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(fmt.Sprintf("confirmed-old-configuration=%t", known), func(t *testing.T) {
			service := nsgKubeService("svc-lb", "20.0.0.0/8")
			dt, cloud, _ := newNSGTestTracker(t, service)
			addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
			service.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
			assert.NoError(t, dt.ReconcileInboundService(service))
			if !known {
				dt.pendingServiceOps["svc-lb"].LastAppliedConfig = nil
			}
			prepared := prepareNSGTest(t, dt)
			if known {
				assert.True(t, nsgTestLocations(dt, prepared).Locations["10.0.0.1"].Addresses["10.1.0.1"].ServiceRef.Has("svc-lb"))
				for _, group := range cloud.groups {
					assert.Equal(t, armnetwork.SecurityRuleAccessDeny,
						nsgTestAccess(t, group.Properties.SecurityRules, "40.1.2.3", "10.1.0.1", 8080, armnetwork.SecurityRuleProtocolTCP))
				}
			} else {
				assert.Empty(t, nsgTestLocations(dt, prepared).Locations, "desired ports are not proof of the live LB configuration")
			}
			confirmed := dt.pendingServiceOps["svc-lb"].Config
			dt.pendingServiceOps["svc-lb"].InFlightConfig = &confirmed
			dt.OnServiceCreationComplete("svc-lb", true, nil)
			prepared = prepareNSGTest(t, dt)
			assert.True(t, nsgTestLocations(dt, prepared).Locations["10.0.0.1"].Addresses["10.1.0.1"].ServiceRef.Has("svc-lb"))
		})
	}
}

func TestNICPoliciesKeepsUnrestrictedAttemptedPorts(t *testing.T) {
	service := nsgKubeService("svc-lb", "")
	service.Annotations = nil
	dt, cloud, _ := newNSGTestTracker(t, service)
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, nil))
	service.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
	assert.NoError(t, dt.ReconcileInboundService(service))
	dt.mu.Lock()
	attempt := dt.pendingServiceOps["svc-lb"].Config
	dt.pendingServiceOps["svc-lb"].InFlightConfig = &attempt
	dt.mu.Unlock()
	service.Spec.Ports[0].TargetPort = intstr.FromInt32(10080)
	service.Annotations = map[string]string{consts.ServiceAnnotationAllowedIPRanges: "20.0.0.0/8"}
	assert.NoError(t, dt.ReconcileInboundService(service))
	prepareNSGTest(t, dt)
	assert.Empty(t, dt.pendingServiceOps["svc-lb"].RetainedBackendPorts, "the running request already has an InFlightConfig")
	for _, group := range cloud.groups {
		assert.Equal(t, armnetwork.SecurityRuleAccessDeny,
			nsgTestAccess(t, group.Properties.SecurityRules, "40.1.2.3", "10.1.0.1", 9090, armnetwork.SecurityRuleProtocolTCP))
	}
	dt.OnServiceCreationComplete("svc-lb", false, errors.New("LB response lost"))
	assert.Equal(t, []PortMapping{{Port: 9090, Protocol: "TCP"}}, dt.pendingServiceOps["svc-lb"].RetainedBackendPorts)
	prepareNSGTest(t, dt)
	assert.Len(t, cloud.groups, 1)
	for _, group := range cloud.groups {
		for _, port := range []int32{8080, 9090, 10080} {
			assert.Equal(t, armnetwork.SecurityRuleAccessDeny,
				nsgTestAccess(t, group.Properties.SecurityRules, "40.1.2.3", "10.1.0.1", port, armnetwork.SecurityRuleProtocolTCP))
		}
	}
}

func TestNICPoliciesKeepsInFlightPortsThroughDeletion(t *testing.T) {
	for _, success := range []bool{false, true} {
		t.Run(fmt.Sprintf("success=%t", success), func(t *testing.T) {
			service := nsgKubeService("svc-lb", "20.0.0.0/8")
			dt, cloud, _ := newNSGTestTracker(t, service)
			addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
			dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
			service.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
			assert.NoError(t, dt.ReconcileInboundService(service))
			prepareNSGTest(t, dt)
			attempt := dt.pendingServiceOps["svc-lb"].Config
			dt.pendingServiceOps["svc-lb"].InFlightConfig = &attempt
			service.Spec.Ports[0].TargetPort = intstr.FromInt32(10080)
			assert.NoError(t, dt.ReconcileInboundService(service))
			dt.DeleteService("svc-lb", true, false)
			var outcome error
			if !success {
				outcome = errors.New("LB response lost during deletion")
			}
			dt.OnServiceCreationComplete("svc-lb", success, outcome)
			op := dt.pendingServiceOps["svc-lb"]
			assert.Nil(t, op.InFlightConfig)
			assert.Equal(t, StateDeletionPending, op.State)
			assert.Contains(t, op.RetainedBackendPorts, PortMapping{Port: 9090, Protocol: "TCP"})
			prepared := prepareNSGTest(t, dt)
			for _, group := range cloud.groups {
				assert.Equal(t, armnetwork.SecurityRuleAccessDeny,
					nsgTestAccess(t, group.Properties.SecurityRules, "40.1.2.3", "10.1.0.1", 9090, armnetwork.SecurityRuleProtocolTCP))
			}
			dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepared))
			assert.NoError(t, dt.pruneNICPolicies(context.Background(), prepared, sets.New[string]()))
			dt.CheckPendingServiceDeletions()
			dt.NRPResources.LoadBalancers.Delete("svc-lb")
			dt.OnServiceCreationComplete("svc-lb", true, nil)
			assert.NotContains(t, dt.pendingServiceOps, "svc-lb")
		})
	}
}

func TestNICPoliciesReadsLivePortsForRecoveredFreshNIC(t *testing.T) {
	service := nsgKubeService("svc-lb", "20.0.0.0/8")
	service.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
	dt, cloud, _ := newNSGTestTracker(t, service)
	op := dt.pendingServiceOps["svc-lb"]
	op.LastAppliedConfig, op.State = nil, StateUpdateInProgress
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{
		Addresses: map[string]NRPAddress{"10.1.0.1": {Services: utilsets.NewString("svc-lb")}},
	}
	_, live, _, err := buildInboundServiceResources("svc-lb", &InboundConfig{
		FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}},
		BackendPorts:  []PortMapping{{Port: 8080, Protocol: "TCP"}},
	}, dt.config)
	assert.NoError(t, err)
	live.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateSucceeded)
	client := mock_loadbalancerclient.NewMockInterface(gomock.NewController(t))
	dt.networkClientFactory.(*mock_azclient.MockClientFactory).EXPECT().GetLoadBalancerClient().Return(client).AnyTimes()
	client.EXPECT().Get(gomock.Any(), "network-rg", "svc-lb", nil).Return(&live, nil).Times(2)
	prepareNSGTest(t, dt)
	assert.Equal(t, []PortMapping{{Port: 8080, Protocol: "TCP"}}, op.RetainedBackendPorts)
	for _, group := range cloud.groups {
		for _, port := range []int32{8080, 9090} {
			assert.Equal(t, armnetwork.SecurityRuleAccessDeny,
				nsgTestAccess(t, group.Properties.SecurityRules, "40.1.2.3", "10.1.0.1", port, armnetwork.SecurityRuleProtocolTCP))
		}
	}
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.2", "")
	prepared := prepareNSGTest(t, dt)
	assert.NotContains(t, nsgTestLocationSync(dt, prepared).Locations["10.0.0.1"].Addresses, "10.1.0.2")
	applied := op.Config
	op.InFlightConfig = &applied
	dt.OnServiceCreationComplete("svc-lb", true, nil)
	prepared = prepareNSGTest(t, dt)
	assert.True(t, nsgTestLocationSync(dt, prepared).Locations["10.0.0.1"].Addresses["10.1.0.2"].ServiceRef.Has("svc-lb"))
	for _, group := range cloud.groups {
		assert.Equal(t, armnetwork.SecurityRuleAccessAllow,
			nsgTestAccess(t, group.Properties.SecurityRules, "40.1.2.3", "10.1.0.1", 8080, armnetwork.SecurityRuleProtocolTCP))
	}
}

func TestNICPoliciesChecksLivePortsDespiteHistoricalRules(t *testing.T) {
	for _, retained := range []bool{false, true} {
		t.Run(fmt.Sprintf("retained-attempt=%t", retained), func(t *testing.T) {
			service := nsgKubeService("svc-lb", "20.0.0.0/8")
			dt, cloud, _ := newNSGTestTracker(t, service)
			addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
			dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
			service.Spec.Ports[0].TargetPort = intstr.FromInt32(10080)
			assert.NoError(t, dt.ReconcileInboundService(service))
			op := dt.pendingServiceOps["svc-lb"]
			op.LastAppliedConfig, op.RetainedBackendPorts = nil, nil
			if retained {
				op.RetainedBackendPorts = []PortMapping{{Port: 11080, Protocol: "TCP"}}
			}
			_, live, _, err := buildInboundServiceResources("svc-lb", &InboundConfig{
				FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}},
				BackendPorts:  []PortMapping{{Port: 9090, Protocol: "TCP"}},
			}, dt.config)
			assert.NoError(t, err)
			live.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateSucceeded)
			client := mock_loadbalancerclient.NewMockInterface(gomock.NewController(t))
			dt.networkClientFactory.(*mock_azclient.MockClientFactory).EXPECT().GetLoadBalancerClient().Return(client).AnyTimes()
			client.EXPECT().Get(gomock.Any(), "network-rg", "svc-lb", nil).Return(&live, nil).Times(1)
			prepareNSGTest(t, dt)
			for _, group := range cloud.groups {
				for _, port := range []int32{8080, 9090, 10080} {
					assert.Equal(t, armnetwork.SecurityRuleAccessDeny,
						nsgTestAccess(t, group.Properties.SecurityRules, "60.1.2.3", "10.1.0.1", port, armnetwork.SecurityRuleProtocolTCP))
				}
			}
		})
	}
}

func TestNICPoliciesRequiresOldOwnerWithdrawalForLBUpdates(t *testing.T) {
	for _, ack := range []bool{false, true} {
		t.Run(fmt.Sprintf("ack=%t", ack), func(t *testing.T) {
			ctx := context.Background()
			service := nsgKubeService("svc-lb", "20.0.0.0/8")
			dt, cloud, node := newNSGTestTracker(t, service)
			vmNIC := cloud.nic
			addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
			dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
			oldTarget, err := dt.targetFromID(*cloud.nic.ID)
			assert.NoError(t, err)
			service.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
			assert.NoError(t, dt.ReconcileInboundService(service))
			dt.UpdateEndpoints("svc-lb", map[string]string{"10.1.0.1": "10.0.0.1"}, nil)
			*vmNIC.ID += "-replacement"
			*vmNIC.Properties.VirtualMachine.ID += "-replacement"
			vmNIC.Name = ptr.To("node-nic-replacement")
			replacement := node.DeepCopy()
			replacement.UID = "replacement"
			replacement.Spec.ProviderID = consts.ServiceGatewayNSGProviderIDPrefix + *vmNIC.Properties.VirtualMachine.ID
			_, err = dt.kubeClient.CoreV1().Nodes().Update(ctx, replacement, metav1.UpdateOptions{})
			assert.NoError(t, err)
			dt.ReconcileNodeChange(node, replacement)
			target, err := dt.targetFromID(*vmNIC.ID)
			assert.NoError(t, err)
			rules, err := buildNICSecurityRules(nil, nil)
			assert.NoError(t, err)
			group, _, err := dt.desiredGroup(nil, target, replacement.Name, "eastus", rules)
			assert.NoError(t, err)
			group.Etag = ptr.To("replacement-etag")
			group.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateSucceeded)
			cloud.groups[*group.Name] = &group
			cloud.nic = nsgCopy(vmNIC)
			cloud.nic.Properties.NetworkSecurityGroup = &armnetwork.SecurityGroup{ID: group.ID}
			mockNSGLocationUpdates(t, dt, func(context.Context, armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
				if !ack {
					return errors.New("withdrawal not applied")
				}
				return nil
			})
			updater := &LocationsUpdater{diffTracker: dt, ctx: ctx, logger: dt.logger}
			updater.process(ctx)
			if !ack {
				old := dt.NRPResources.Locations["10.0.0.1"]
				assert.True(t, old.nodeOwner.draining)
				assert.True(t, old.Addresses["10.1.0.1"].Services.Has("svc-lb"))
				assert.Equal(t, armnetwork.SecurityRuleAccessAllow,
					nsgTestAccess(t, cloud.groups[dt.groupName(oldTarget)].Properties.SecurityRules,
						"40.1.2.3", "10.1.0.1", 9090, armnetwork.SecurityRuleProtocolTCP))
			}
			assert.Equal(t, ack, dt.pendingServiceOps["svc-lb"].NSGPrepared, "replacement proof cannot discharge the old owner's obligation")
		})
	}
}

func TestNICPoliciesWaitsForSettledLiveLBPorts(t *testing.T) {
	for _, tt := range []struct {
		name  string
		state *armnetwork.ProvisioningState
	}{
		{"succeeded", ptr.To(armnetwork.ProvisioningStateSucceeded)},
		{"updating", ptr.To(armnetwork.ProvisioningStateUpdating)},
		{"failed", ptr.To(armnetwork.ProvisioningStateFailed)},
		{"missing state", nil},
		{"empty state", ptr.To(armnetwork.ProvisioningState(""))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			a, b := nsgKubeService("a-lb", ""), nsgKubeService("b-lb", "")
			a.Annotations, b.Annotations = nil, nil
			dt, cloud, _ := newNSGTestTracker(t, a, b)
			addNSGTestBackend(dt, "a-lb", "10.0.0.1", "10.1.0.1", "")
			addNSGTestBackend(dt, "b-lb", "10.0.0.1", "10.1.0.1", "")
			dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, nil))
			b.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
			assert.NoError(t, dt.ReconcileInboundService(b))
			dt.pendingServiceOps["b-lb"].LastAppliedConfig = nil
			a.Annotations = map[string]string{consts.ServiceAnnotationAllowedIPRanges: "20.0.0.0/8"}
			assert.NoError(t, dt.ReconcileInboundService(a))
			_, live, _, err := buildInboundServiceResources("b-lb", dt.pendingServiceOps["b-lb"].Config.InboundConfig, dt.config)
			assert.NoError(t, err)
			live.Properties.ProvisioningState = tt.state
			client := mock_loadbalancerclient.NewMockInterface(gomock.NewController(t))
			dt.networkClientFactory.(*mock_azclient.MockClientFactory).EXPECT().GetLoadBalancerClient().Return(client).AnyTimes()
			gets := 1
			if tt.name != "succeeded" {
				gets++
			}
			client.EXPECT().Get(gomock.Any(), "network-rg", "b-lb", nil).Return(&live, nil).Times(gets)
			work := dt.takeNICPolicyWork()
			prepared, err := dt.prepareNICPolicies(ctx, work)
			if tt.name != "succeeded" {
				assert.ErrorContains(t, err, "not in Succeeded provisioning state")
				assert.Zero(t, cloud.groupWrites, "unfinished LB response cannot erase the old live-port obligation")
				dt.mu.Lock()
				dt.finishNICPolicyWorkLocked(work, prepared)
				dt.mu.Unlock()
				assert.True(t, dt.pendingServiceOps["b-lb"].NSGPrepared, "a safe target-only observation can repair the LB without establishing its old port baseline")
				if assert.NotNil(t, prepared["10.0.0.1"]) {
					assert.True(t, prepared["10.0.0.1"].lbOnly)
				}
				live.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateSucceeded)
				prepared, err = dt.prepareNICPolicies(ctx, dt.takeNICPolicyWork())
			}
			assert.NoError(t, err)
			assert.NotEmpty(t, prepared)
			assert.Equal(t, 1, cloud.groupWrites)
		})
	}
}

func TestNICPoliciesRecoversFailedLBFromObservedTarget(t *testing.T) {
	for _, tt := range []struct {
		name      string
		observed  string
		requested string
		port      int32
		want      bool
	}{
		{"protected target", "20.0.0.0/8", "20.0.0.0/8", 9090, true},
		{"unrestricted target", "20.0.0.0/8", "", 10080, true},
		{"no owned policy", "", "", 9090, true},
		{"unprotected target", "20.0.0.0/8", "20.0.0.0/8", 10080, false},
		{"old restriction blocks target", "20.0.0.0/8", "", 9090, false},
		{"changed source policy", "20.0.0.0/8", "40.0.0.0/8", 9090, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			service := nsgKubeService("svc-lb", tt.observed)
			if tt.observed == "" {
				service.Annotations = nil
			}
			dt, cloud, _ := newNSGTestTracker(t, service, nsgKubeService("other-lb", "20.0.0.0/8"))
			addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
			dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
			service.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
			assert.NoError(t, dt.ReconcileInboundService(service))
			prepareNSGTest(t, dt)
			op := dt.pendingServiceOps["svc-lb"]
			attempt := op.Config
			op.InFlightConfig = &attempt
			dt.OnServiceCreationComplete("svc-lb", false, errors.New("LB update failed"))
			op.LastAppliedConfig, op.NSGPrepared = nil, false
			service.Spec.Ports[0].TargetPort = intstr.FromInt32(tt.port)
			service.Annotations = nil
			if tt.requested != "" {
				service.Annotations = map[string]string{consts.ServiceAnnotationAllowedIPRanges: tt.requested}
			}
			assert.NoError(t, dt.ReconcileInboundService(service))
			addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.2", "")
			_, live, _, err := buildInboundServiceResources("svc-lb", op.Config.InboundConfig, dt.config)
			assert.NoError(t, err)
			live.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateFailed)
			client := mock_loadbalancerclient.NewMockInterface(gomock.NewController(t))
			dt.networkClientFactory.(*mock_azclient.MockClientFactory).EXPECT().GetLoadBalancerClient().Return(client).AnyTimes()
			client.EXPECT().Get(gomock.Any(), "network-rg", "svc-lb", nil).Return(&live, nil).Times(1)
			writes := cloud.groupWrites
			puts := 0
			if tt.want {
				client.EXPECT().CreateOrUpdate(gomock.Any(), "network-rg", "svc-lb", gomock.Any()).
					DoAndReturn(func(_ context.Context, _, _ string, lb armnetwork.LoadBalancer) (*armnetwork.LoadBalancer, error) {
						puts++
						assert.Equal(t, writes, cloud.groupWrites, "repair permission must not rewrite the NSG first")
						ports, err := loadBalancerBackendPorts(&lb)
						assert.NoError(t, err)
						assert.Equal(t, []PortMapping{{Port: tt.port, Protocol: "TCP"}}, ports)
						return &lb, nil
					})
			} else {
				client.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
			}
			mockNSGLocationUpdates(t, dt, func(context.Context, armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
				return nil
			})
			updater := &LocationsUpdater{diffTracker: dt, ctx: ctx, logger: dt.logger}
			updater.process(ctx)
			if !assert.Equal(t, tt.want, op.NSGPrepared) {
				return
			}
			assert.Equal(t, 1, updater.failureCount, "the baseline read remains a reconciliation failure")
			assert.Equal(t, writes, cloud.groupWrites)
			assert.NotContains(t, dt.NRPResources.Locations["10.0.0.1"].Addresses, "10.1.0.2")
			assert.NotEmpty(t, dt.dirtyNICPolicies)
			op.State, op.NextRetryAt = StateUpdateInProgress, time.Time{}
			worker := NewServiceUpdater(ctx, dt, dt.OnServiceCreationComplete, dt.serviceUpdaterTrigger)
			t.Cleanup(worker.Stop)
			worker.processBatch()
			worker.wg.Wait()
			if !tt.want {
				assert.Zero(t, puts)
				assert.Nil(t, op.LastAppliedConfig)
				return
			}
			assert.Equal(t, 1, puts)
			assert.NotNil(t, op.LastAppliedConfig, "actual ServiceUpdater completion establishes the applied configuration")
			assert.Empty(t, op.RetainedBackendPorts)
			assert.NotEmpty(t, dt.locationsUpdaterTrigger)
			updater.process(ctx)
			assert.Zero(t, updater.failureCount)
			assert.Empty(t, dt.dirtyNICPolicies)
			assert.True(t, dt.NRPResources.Locations["10.0.0.1"].Addresses["10.1.0.2"].Services.Has("svc-lb"))
		})
	}
}

func TestNICPoliciesReusesFailedPortReadObservation(t *testing.T) {
	ctx := context.Background()
	service := nsgKubeService("svc-lb", "20.0.0.0/8")
	dt, cloud, node := newNSGTestTracker(t, service)
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
	service.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
	assert.NoError(t, dt.ReconcileInboundService(service))
	prepareNSGTest(t, dt)
	dt.pendingServiceOps["svc-lb"].LastAppliedConfig = nil
	client := mock_loadbalancerclient.NewMockInterface(gomock.NewController(t))
	dt.networkClientFactory.(*mock_azclient.MockClientFactory).EXPECT().GetLoadBalancerClient().Return(client).AnyTimes()
	client.EXPECT().Get(gomock.Any(), "network-rg", "svc-lb", nil).Return(nil, errors.New("LB read unavailable")).Times(1)
	reads := make(map[string]error)
	writes := cloud.groupWrites
	for range 2 {
		plan, err := dt.prepareNICPolicy(ctx, node, nodeInternalIPs(node), nodeInternalIPs(node), reads)
		assert.ErrorContains(t, err, "LB read unavailable")
		if assert.NotNil(t, plan) {
			assert.True(t, plan.lbOnly)
			assert.Equal(t, []PortMapping{{Port: 9090, Protocol: "TCP"}}, plan.services["svc-lb"].ports)
		}
	}
	assert.Equal(t, writes, cloud.groupWrites)
	assert.False(t, dt.pendingServiceOps["svc-lb"].NSGPrepared, "observations still require the all-NIC finalizer")
}

func TestNICPoliciesVerifiesMissingOrStaleNodeCache(t *testing.T) {
	for _, scenario := range []string{"empty cache", "stale UID", "stale addresses", "live read error", "confirmed deletion", "live replacement behind matching cache"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			dt, _, node := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
			addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
			dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
			switch scenario {
			case "stale UID":
				stale := node.DeepCopy()
				stale.UID = "obsolete"
				setTestNodeLister(t, dt, stale)
			case "stale addresses":
				stale := node.DeepCopy()
				stale.Status.Addresses = []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: "10.0.0.99"}}
				setTestNodeLister(t, dt, stale)
			case "live replacement behind matching cache":
				setTestNodeLister(t, dt, node)
				replacement := node.DeepCopy()
				replacement.UID = "replacement"
				replacement.Spec.ProviderID += "-replacement"
				_, err := dt.kubeClient.CoreV1().Nodes().Update(ctx, replacement, metav1.UpdateOptions{})
				assert.NoError(t, err)
			default:
				setTestNodeLister(t, dt)
			}
			if scenario == "live read error" {
				dt.kubeClient.(*fake.Clientset).PrependReactor("get", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewInternalError(errors.New("Node API unavailable"))
				})
			}
			if scenario == "confirmed deletion" {
				assert.NoError(t, dt.kubeClient.CoreV1().Nodes().Delete(ctx, node.Name, metav1.DeleteOptions{}))
			}
			dt.mu.Lock()
			dt.markServiceNICPoliciesDirtyLocked("svc-lb")
			dt.mu.Unlock()
			prepared, err := dt.prepareNICPolicies(ctx, dt.takeNICPolicyWork())
			switch scenario {
			case "live read error":
				assert.ErrorContains(t, err, "Node API unavailable")
			case "confirmed deletion", "live replacement behind matching cache":
				assert.Error(t, err)
			default:
				assert.NoError(t, err)
			}
			draining := dt.NRPResources.Locations["10.0.0.1"].nodeOwner.draining
			departed := scenario == "confirmed deletion" || scenario == "live replacement behind matching cache"
			assert.Equal(t, departed, draining)
			if departed {
				assert.NotEmpty(t, nsgTestLocationSync(dt, prepared).Locations)
			} else {
				assert.Empty(t, nsgTestLocationSync(dt, prepared).Locations, "cache state alone must not withdraw an unchanged owner")
			}
		})
	}
}

func TestNICPoliciesKeepsMembershipAttemptsBeforeFirstPolicy(t *testing.T) {
	service := nsgKubeService("svc-lb", "")
	service.Annotations = nil
	dt, cloud, _ := newNSGTestTracker(t, service)
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	attempt := nsgTestLocationSync(dt, nil)
	assert.True(t, attempt.Locations["10.0.0.1"].Addresses["10.1.0.1"].ServiceRef.Has("svc-lb"))
	service.Annotations = map[string]string{consts.ServiceAnnotationAllowedIPRanges: "20.0.0.0/8"}
	assert.NoError(t, dt.ReconcileInboundService(service))
	dt.UpdateEndpoints("svc-lb", map[string]string{"10.1.0.1": "10.0.0.1"}, nil)
	prepared := prepareNSGTest(t, dt)
	assert.Len(t, cloud.groups, 1)
	for _, group := range cloud.groups {
		assert.Equal(t, armnetwork.SecurityRuleAccessDeny,
			nsgTestAccess(t, group.Properties.SecurityRules, "40.1.2.3", "10.1.0.1", 8080, armnetwork.SecurityRuleProtocolTCP))
	}
	correction := nsgTestLocationSync(dt, prepared)
	assert.Contains(t, correction.Locations, "10.0.0.1", "the pre-policy attempt still requires an acknowledged withdrawal")
	dt.UpdateLocationsAddresses(correction)
	assert.NoError(t, dt.pruneNICPolicies(context.Background(), prepared, sets.New[string]()))
	assert.Empty(t, dt.pendingLocationUpdates)
	for _, group := range cloud.groups {
		assert.Len(t, group.Properties.SecurityRules, 2)
	}
}

func TestNICPoliciesRestoresMembershipAfterUnconfirmedWithdrawal(t *testing.T) {
	for _, restricted := range []bool{false, true} {
		for _, kind := range []string{"whole location", "one address", "default-preserving full location"} {
			t.Run(fmt.Sprintf("restricted=%t/%s", restricted, kind), func(t *testing.T) {
				service := nsgKubeService("svc-lb", "20.0.0.0/8")
				if !restricted {
					service.Annotations = nil
				}
				dt, _, _ := newNSGTestTracker(t, service)
				old := map[string]string{"10.1.0.1": "10.0.0.1"}
				addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
				if kind == "one address" {
					old["10.1.0.2"] = "10.0.0.1"
					addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.2", "")
				}
				dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
				if kind == "default-preserving full location" {
					dt.NRPResources.Locations["10.0.0.1"].Addresses["10.1.0.9"] = NRPAddress{Services: utilsets.NewString("default-natgw")}
				}
				remaining := map[string]string{}
				if kind == "one address" {
					remaining["10.1.0.2"] = "10.0.0.1"
				}
				dt.UpdateEndpoints("svc-lb", old, remaining)
				withdrawal := nsgTestLocationSync(dt, prepareNSGTest(t, dt))
				assert.Contains(t, withdrawal.Locations, "10.0.0.1")
				dt.UpdateEndpoints("svc-lb", remaining, old)
				restore := nsgTestLocationSync(dt, prepareNSGTest(t, dt))
				refs := restore.Locations["10.0.0.1"].Addresses["10.1.0.1"].ServiceRef
				if assert.NotNil(t, refs, "an uncertain withdrawal requires restoration even when desired equals the last ACK") {
					assert.True(t, refs.Has("svc-lb"))
				}
				dt.UpdateLocationsAddresses(restore)
				assert.Empty(t, dt.pendingLocationUpdates)
				assert.Empty(t, nsgTestLocationSync(dt, nil).Locations)
			})
		}
	}
}

func TestNICPoliciesRequiresSettledAssociationAfterUnknownWrite(t *testing.T) {
	for _, tc := range []struct {
		state        armnetwork.ProvisioningState
		repairFails  bool
		removePolicy bool
		foreign      bool
		publish      bool
		nicWrites    int
	}{
		{state: armnetwork.ProvisioningStateSucceeded, publish: true, nicWrites: 1},
		{state: armnetwork.ProvisioningStateUpdating, nicWrites: 1},
		{state: armnetwork.ProvisioningStateFailed, repairFails: true, nicWrites: 2},
		{state: armnetwork.ProvisioningStateFailed, publish: true, nicWrites: 2},
		{nicWrites: 1},
		{state: armnetwork.ProvisioningStateSucceeded, removePolicy: true, publish: true, nicWrites: 1},
		{state: armnetwork.ProvisioningStateUpdating, removePolicy: true, nicWrites: 1},
		{state: armnetwork.ProvisioningStateFailed, removePolicy: true, repairFails: true, nicWrites: 2},
		{state: armnetwork.ProvisioningStateFailed, removePolicy: true, publish: true, nicWrites: 2},
		{state: armnetwork.ProvisioningStateFailed, removePolicy: true, foreign: true, nicWrites: 1},
	} {
		t.Run(fmt.Sprintf("%s/repairFails=%t/removePolicy=%t/foreign=%t", tc.state, tc.repairFails, tc.removePolicy, tc.foreign), func(t *testing.T) {
			service := nsgKubeService("svc-lb", "20.0.0.0/8")
			dt, cloud, _ := newNSGTestTracker(t, service)
			addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
			cloud.nicError, cloud.applyOnError = context.DeadlineExceeded, true
			prepared, err := dt.prepareNICPolicies(context.Background(), dt.takeNICPolicyWork())
			assert.ErrorIs(t, err, context.DeadlineExceeded)
			assert.Empty(t, nsgTestLocations(dt, prepared).Locations)
			cloud.nicError, cloud.applyOnError = nil, false
			cloud.nic.Properties.ProvisioningState = ptr.To(tc.state)
			if tc.state == "" {
				cloud.nic.Properties.ProvisioningState = nil
			}
			if tc.repairFails {
				cloud.nicError = errors.New("association repair failed")
			}
			if tc.removePolicy {
				service.Annotations = nil
				assert.NoError(t, dt.ReconcileInboundService(service))
			}
			if tc.foreign {
				cloud.nic.Properties.NetworkSecurityGroup.ID = ptr.To("unrelated-nsg")
			}
			prepared, err = dt.prepareNICPolicies(context.Background(), dt.takeNICPolicyWork())
			if tc.publish {
				assert.NoError(t, err)
				assert.True(t, nsgTestLocations(dt, prepared).Locations["10.0.0.1"].Addresses["10.1.0.1"].ServiceRef.Has("svc-lb"))
			} else {
				assert.Error(t, err)
				assert.Empty(t, nsgTestLocations(dt, prepared).Locations)
			}
			assert.Equal(t, tc.nicWrites, cloud.nicWrites)
			if tc.foreign {
				assert.Equal(t, "unrelated-nsg", *cloud.nic.Properties.NetworkSecurityGroup.ID)
			}
		})
	}
}
