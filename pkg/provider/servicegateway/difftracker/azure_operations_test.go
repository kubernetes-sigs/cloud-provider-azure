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
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
)

func TestConvertServiceDTOsToServiceRequests_OutboundRemovalHasNoNatGatewayID(t *testing.T) {
	reqs, err := convertServiceDTOsToServiceRequests([]ServiceDTO{
		{Service: "egr1", ServiceType: Outbound, IsDelete: true},
	}, Config{SubscriptionID: "sub", ResourceGroup: "rg", VNetName: "vnet"})
	assert.NoError(t, err)
	assert.Len(t, reqs, 1)
	assert.Nil(t, reqs[0].Service.Properties.PublicNatGatewayID)
}

func TestConvertServiceDTOsToServiceRequests_OutboundAddHasNatGatewayID(t *testing.T) {
	natID := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/natGateways/egr1"
	reqs, err := convertServiceDTOsToServiceRequests([]ServiceDTO{
		{Service: "egr1", ServiceType: Outbound, PublicNatGateway: NatGatewayDTO{Id: natID}},
	}, Config{SubscriptionID: "sub", ResourceGroup: "rg", VNetName: "vnet"})
	assert.NoError(t, err)
	assert.Len(t, reqs, 1)
	if assert.NotNil(t, reqs[0].Service.Properties.PublicNatGatewayID) {
		assert.Equal(t, natID, *reqs[0].Service.Properties.PublicNatGatewayID)
	}
}

func TestConvertServiceDTOsToServiceRequests_UnknownServiceTypeErrors(t *testing.T) {
	_, err := convertServiceDTOsToServiceRequests([]ServiceDTO{
		{Service: "x", ServiceType: ServiceType("")},
	}, Config{SubscriptionID: "sub", ResourceGroup: "rg", VNetName: "vnet"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown ServiceType")
}

func TestExtractResourceChildName(t *testing.T) {
	id := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb1/backendAddressPools/pool1"
	assert.Equal(t, "pool1", extractResourceChildName(id, "backendAddressPools"))
	assert.Equal(t, "", extractResourceChildName("/subscriptions/sub/loadBalancers/lb1", "backendAddressPools"))
	assert.Equal(t, "", extractResourceChildName("", "backendAddressPools"))
}

func TestCreateOrUpdatePIPWithResponse_NilPip(t *testing.T) {
	dt := &DiffTracker{}
	_, err := dt.createOrUpdatePIPWithResponse(context.Background(), "rg", nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "pip is nil")
}

func TestUpdateServiceLoadBalancerStatus_PreservesDualStackAndHostname(t *testing.T) {
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns", UID: "uid-1"},
		Status: v1.ServiceStatus{
			LoadBalancer: v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{
					{IP: "2001:db8::1"},
					{Hostname: "example.com"},
				},
			},
		},
	}
	kubeClient := fake.NewSimpleClientset(svc)
	dt := &DiffTracker{kubeClient: kubeClient}

	err := dt.updateServiceLoadBalancerStatus(context.Background(), "uid-1", "10.0.0.1")
	assert.NoError(t, err)

	got, err := kubeClient.CoreV1().Services("ns").Get(context.Background(), "svc", metav1.GetOptions{})
	assert.NoError(t, err)
	var ips, hosts []string
	for _, ing := range got.Status.LoadBalancer.Ingress {
		if ing.IP != "" {
			ips = append(ips, ing.IP)
		}
		if ing.Hostname != "" {
			hosts = append(hosts, ing.Hostname)
		}
	}
	assert.Contains(t, ips, "10.0.0.1")
	assert.Contains(t, ips, "2001:db8::1")
	assert.Contains(t, hosts, "example.com")
}

func TestUpdateServiceLoadBalancerStatus_ReplacesStaleSameFamily(t *testing.T) {
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns", UID: "uid-2"},
		Status: v1.ServiceStatus{
			LoadBalancer: v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{{IP: "10.0.0.9"}},
			},
		},
	}
	kubeClient := fake.NewSimpleClientset(svc)
	dt := &DiffTracker{kubeClient: kubeClient}

	err := dt.updateServiceLoadBalancerStatus(context.Background(), "uid-2", "10.0.0.1")
	assert.NoError(t, err)

	got, err := kubeClient.CoreV1().Services("ns").Get(context.Background(), "svc", metav1.GetOptions{})
	assert.NoError(t, err)
	var ips []string
	for _, ing := range got.Status.LoadBalancer.Ingress {
		ips = append(ips, ing.IP)
	}
	assert.Equal(t, []string{"10.0.0.1"}, ips)
}

// TestConvertServiceDTOsToServiceRequests_VNetResourceGroup verifies that the backend-pool VNet
// reference uses the configured VNet resource group (for BYO-VNet clusters), falling back to the
// cluster resource group when it is unset.
func TestConvertServiceDTOsToServiceRequests_VNetResourceGroup(t *testing.T) {
	dtos := []ServiceDTO{{
		Service:     "svc",
		ServiceType: Inbound,
		LoadBalancerBackendPools: []LoadBalancerBackendPoolDTO{
			{Id: "/subscriptions/sub/resourceGroups/cluster-rg/providers/Microsoft.Network/loadBalancers/svc/backendAddressPools/svc"},
		},
	}}
	vnetID := func(c Config) string {
		reqs, err := convertServiceDTOsToServiceRequests(dtos, c)
		assert.NoError(t, err)
		return *reqs[0].Service.Properties.LoadBalancerBackendPools[0].Properties.VirtualNetwork.ID
	}

	assert.Contains(t,
		vnetID(Config{SubscriptionID: "sub", ResourceGroup: "cluster-rg", VNetName: "vnet", VNetResourceGroup: "network-rg"}),
		"/resourceGroups/network-rg/", "a configured VNet resource group must be honored")
	assert.Contains(t,
		vnetID(Config{SubscriptionID: "sub", ResourceGroup: "cluster-rg", VNetName: "vnet"}),
		"/resourceGroups/cluster-rg/", "an unset VNet resource group must fall back to the cluster resource group")
}

// serviceListerWith builds a Service lister seeded with the given services by adding them
// directly to the informer indexer, so no informer needs to be started.
func serviceListerWith(t *testing.T, svcs ...*v1.Service) corelisters.ServiceLister {
	t.Helper()
	factory := informers.NewSharedInformerFactory(fake.NewSimpleClientset(), 0)
	indexer := factory.Core().V1().Services().Informer().GetIndexer()
	for _, s := range svcs {
		if err := indexer.Add(s); err != nil {
			t.Fatalf("seed service lister: %v", err)
		}
	}
	return factory.Core().V1().Services().Lister()
}

// trackService records a namespace/name for a UID so getServiceByUID can resolve it through
// the lister, mirroring what EnsureLoadBalancer populates on the ServiceConfig.
func (dt *DiffTracker) trackService(uid, namespace, name string) {
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     ServiceConfig{UID: uid, IsInbound: true, Namespace: namespace, Name: name},
	}
}

func TestGetServiceByUID_ResolvesThroughListerWithoutList(t *testing.T) {
	svc := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns", UID: "uid-1"}}
	dt := newTestDiffTracker()
	// Empty clientset: a List fallback would find nothing, so a successful resolve means the
	// cached lister answered the read.
	dt.kubeClient = fake.NewSimpleClientset()
	dt.serviceLister = serviceListerWith(t, svc)
	dt.trackService("uid-1", "ns", "svc")

	got, err := dt.getServiceByUID(context.Background(), "uid-1")
	assert.NoError(t, err)
	if assert.NotNil(t, got) {
		assert.Equal(t, "uid-1", string(got.UID))
	}
}

func TestGetServiceByUID_UIDMismatchReturnsNotFound(t *testing.T) {
	// The lister holds a different Service at the recorded namespace/name (a recreation that
	// reused the name). A clientset copy carrying the expected UID exists at another
	// namespace/name so a UID mismatch resolves to NotFound rather than falling through to the
	// List scan and returning it.
	current := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns", UID: "uid-current"}}
	stale := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "ns", UID: "uid-expected"}}
	dt := newTestDiffTracker()
	dt.kubeClient = fake.NewSimpleClientset(stale)
	dt.serviceLister = serviceListerWith(t, current)
	dt.trackService("uid-expected", "ns", "svc")

	got, err := dt.getServiceByUID(context.Background(), "uid-expected")
	assert.Nil(t, got)
	assert.True(t, apierrors.IsNotFound(err), "UID mismatch must surface as NotFound, got %v", err)
}

func TestGetServiceByUID_NilListerFallsBackToApiserverGet(t *testing.T) {
	svc := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns", UID: "uid-1"}}
	dt := newTestDiffTracker()
	dt.kubeClient = fake.NewSimpleClientset(svc)
	dt.serviceLister = nil
	dt.trackService("uid-1", "ns", "svc")

	got, err := dt.getServiceByUID(context.Background(), "uid-1")
	assert.NoError(t, err)
	if assert.NotNil(t, got) {
		assert.Equal(t, "uid-1", string(got.UID))
	}
}

func TestGetServiceByUID_UnknownIdentityFallsBackToList(t *testing.T) {
	svc := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns", UID: "uid-1"}}
	dt := newTestDiffTracker()
	dt.kubeClient = fake.NewSimpleClientset(svc)
	dt.serviceLister = serviceListerWith(t) // lister present but empty

	got, err := dt.getServiceByUID(context.Background(), "uid-1")
	assert.NoError(t, err)
	if assert.NotNil(t, got) {
		assert.Equal(t, "uid-1", string(got.UID))
	}
}

func TestGetServiceByUID_ColdListerCacheFallsBackToApiserverGet(t *testing.T) {
	// The Service exists on the apiserver but the lister cache has not observed it yet. The
	// read must fall back rather than reporting a false NotFound.
	svc := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns", UID: "uid-1"}}
	dt := newTestDiffTracker()
	dt.kubeClient = fake.NewSimpleClientset(svc)
	dt.serviceLister = serviceListerWith(t) // empty cache
	dt.trackService("uid-1", "ns", "svc")

	got, err := dt.getServiceByUID(context.Background(), "uid-1")
	assert.NoError(t, err)
	if assert.NotNil(t, got) {
		assert.Equal(t, "uid-1", string(got.UID))
	}
}

func TestUpdateServiceLoadBalancerStatus_DualStackSafeThroughLister(t *testing.T) {
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns", UID: "uid-1"},
		Status: v1.ServiceStatus{
			LoadBalancer: v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{
					{IP: "2001:db8::1"},
					{Hostname: "example.com"},
				},
			},
		},
	}
	dt := newTestDiffTracker()
	dt.kubeClient = fake.NewSimpleClientset(svc)
	dt.serviceLister = serviceListerWith(t, svc)
	dt.trackService("uid-1", "ns", "svc")

	err := dt.updateServiceLoadBalancerStatus(context.Background(), "uid-1", "10.0.0.1")
	assert.NoError(t, err)

	got, err := dt.kubeClient.CoreV1().Services("ns").Get(context.Background(), "svc", metav1.GetOptions{})
	assert.NoError(t, err)
	var ips, hosts []string
	for _, ing := range got.Status.LoadBalancer.Ingress {
		if ing.IP != "" {
			ips = append(ips, ing.IP)
		}
		if ing.Hostname != "" {
			hosts = append(hosts, ing.Hostname)
		}
	}
	assert.Contains(t, ips, "10.0.0.1")
	assert.Contains(t, ips, "2001:db8::1")
	assert.Contains(t, hosts, "example.com")
}
