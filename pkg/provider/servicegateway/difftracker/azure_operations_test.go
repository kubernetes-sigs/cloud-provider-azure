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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/natgatewayclient/mock_natgatewayclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/servicegatewayclient/mock_servicegatewayclient"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/servicegatewayclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/log"
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
		{Service: "egr1", ServiceType: Outbound, PublicNatGateway: NatGatewayDTO{ID: natID}},
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
			{ID: "/subscriptions/sub/resourceGroups/cluster-rg/providers/Microsoft.Network/loadBalancers/svc/backendAddressPools/svc"},
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

// responseError builds the shape azcore produces for the INITIAL updateServices/
// updateAddressLocations call: a POST. isSynchronousCompletion requires the POST, because a 200 is
// also how azcore reports a FAILED asynchronous operation (the terminal poll is a GET returning
// 200 {"status":"Failed"}).
func responseError(status int) error {
	req, _ := http.NewRequest(http.MethodPost, "https://example/sgw", nil)
	return &azcore.ResponseError{
		StatusCode:  status,
		RawResponse: &http.Response{StatusCode: status, Header: http.Header{}, Request: req},
	}
}

// TestIsSynchronousCompletion pins the discriminator that lets the provider drop the vendored SDK
// patch: NRP answers these long-running operations with 200 OK, which the generated client rejects
// even though the request succeeded. Only 200 may be tolerated; every other status is a real
// failure and must still propagate.
func TestIsSynchronousCompletion(t *testing.T) {
	assert.True(t, isSynchronousCompletion(responseError(http.StatusOK)))
	assert.True(t, isSynchronousCompletion(fmt.Errorf("wrapped: %w", responseError(http.StatusOK))))

	assert.False(t, isSynchronousCompletion(nil))
	assert.False(t, isSynchronousCompletion(errors.New("boom")))
	for _, status := range []int{
		http.StatusAccepted,
		http.StatusNoContent,
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusNotFound,
		http.StatusConflict,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	} {
		assert.False(t, isSynchronousCompletion(responseError(status)), "status %d must not be treated as success", status)
	}

	// A 200 that is not provably the initial call must NOT be tolerated: azcore reports a failed
	// long-running operation as a ResponseError built from the terminal poll, which is a GET
	// returning 200. Treating that as success silently records NRP state that does not exist.
	pollGET, _ := http.NewRequest(http.MethodGet, "https://poll.example/op/1", nil)
	assert.False(t, isSynchronousCompletion(&azcore.ResponseError{
		StatusCode:  http.StatusOK,
		RawResponse: &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Request: pollGET},
	}), "a 200 from an LRO poll (GET) is a failed async operation, not a synchronous completion")

	assert.False(t, isSynchronousCompletion(&azcore.ResponseError{StatusCode: http.StatusOK}),
		"a 200 with no RawResponse cannot be proven to be the initial call")

	postReq, _ := http.NewRequest(http.MethodPost, "https://example/sgw", nil)
	for _, header := range []string{"Location", "Azure-AsyncOperation"} {
		// Set canonicalises the key exactly as net/http does when parsing a real response.
		pollingHeader := http.Header{}
		pollingHeader.Set(header, "https://poll")
		assert.True(t, isSynchronousCompletion(&azcore.ResponseError{
			StatusCode: http.StatusOK,
			RawResponse: &http.Response{
				StatusCode: http.StatusOK,
				Header:     pollingHeader,
				Request:    postReq,
			},
		}), "NRP stamps %s on its already-complete synchronous 200; rejecting it fails every registration", header)
	}
}

func TestTolerateSynchronousCompletion(t *testing.T) {
	dt := &DiffTracker{logger: log.Noop()}

	assert.NoError(t, dt.tolerateSynchronousCompletion(responseError(http.StatusOK), "UpdateServices", "sgw"))
	assert.NoError(t, dt.tolerateSynchronousCompletion(nil, "UpdateServices", "sgw"))

	conflict := responseError(http.StatusConflict)
	assert.Same(t, conflict, dt.tolerateSynchronousCompletion(conflict, "UpdateServices", "sgw"))

	// NRP returns its synchronous 200 for updateServices with a Location header even though the
	// operation has already completed, so this must be tolerated. Rejecting it fails every
	// Service registration against the live provider.
	postReq, _ := http.NewRequest(http.MethodPost, "https://example/sgw", nil)
	async := &azcore.ResponseError{
		StatusCode: http.StatusOK,
		RawResponse: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Location": []string{"https://poll"}},
			Request:    postReq,
		},
	}
	assert.NoError(t, dt.tolerateSynchronousCompletion(async, "UpdateAddressLocations", "sgw"),
		"a POST 200 carrying a polling header is still NRP's synchronous completion")

	// A GET 200 is azcore's terminal poll for a FAILED async operation and must still propagate.
	pollGET, _ := http.NewRequest(http.MethodGet, "https://poll.example/op/1", nil)
	failedLRO := &azcore.ResponseError{
		StatusCode:  http.StatusOK,
		RawResponse: &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Request: pollGET},
	}
	assert.Same(t, failedLRO, dt.tolerateSynchronousCompletion(failedLRO, "UpdateServices", "sgw"),
		"a failed async LRO must not be reported as a synchronous completion")
}

type fixedStatusTransport struct {
	status int
	header http.Header
}

func (t fixedStatusTransport) Do(req *http.Request) (*http.Response, error) {
	header := t.header
	if header == nil {
		header = http.Header{}
	}
	header.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode: t.status,
		Status:     http.StatusText(t.status),
		Header:     header,
		Body:       io.NopCloser(bytes.NewReader([]byte(`{}`))),
		Request:    req,
	}, nil
}

type fakeCredential struct{}

func (fakeCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "fake", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func newServiceGatewayClient(t *testing.T, status int, header http.Header) servicegatewayclient.Interface {
	t.Helper()
	client, err := servicegatewayclient.New("subscription", fakeCredential{}, &arm.ClientOptions{
		ClientOptions: azcore.ClientOptions{Transport: fixedStatusTransport{status: status, header: header}},
	})
	if err != nil {
		t.Fatalf("building ServiceGateway client: %v", err)
	}
	return client
}

// TestServiceGatewayClientSynchronousCompletionEndToEnd drives the real generated SDK client, so it
// proves the provider no longer needs the vendored patch: NRP's 200 OK surfaces as an error that
// isSynchronousCompletion recognises, while a 409 still propagates.
func TestServiceGatewayClientSynchronousCompletionEndToEnd(t *testing.T) {
	ctx := context.Background()

	t.Run("200 OK is tolerated", func(t *testing.T) {
		client := newServiceGatewayClient(t, http.StatusOK, nil)

		err := client.UpdateServices(ctx, "rg", "sgw", armnetwork.ServiceGatewayUpdateServicesRequest{})
		assert.Error(t, err, "the generated client rejects 200 OK")
		assert.True(t, isSynchronousCompletion(err))

		err = client.UpdateAddressLocations(ctx, "rg", "sgw", armnetwork.ServiceGatewayUpdateAddressLocationsRequest{})
		assert.Error(t, err)
		assert.True(t, isSynchronousCompletion(err))
	})

	t.Run("409 Conflict still fails", func(t *testing.T) {
		client := newServiceGatewayClient(t, http.StatusConflict, nil)

		err := client.UpdateServices(ctx, "rg", "sgw", armnetwork.ServiceGatewayUpdateServicesRequest{})
		assert.Error(t, err)
		assert.False(t, isSynchronousCompletion(err), "a real ARM failure must never be tolerated")
	})
}

// asyncFailureTransport answers the initial call with 202 + Azure-AsyncOperation and every poll
// with 200 {"status":"Failed"} - the exact shape azcore turns into ResponseError{StatusCode:200}.
type asyncFailureTransport struct{ calls int }

func (t *asyncFailureTransport) Do(req *http.Request) (*http.Response, error) {
	t.calls++
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	if t.calls == 1 {
		header.Set("Azure-AsyncOperation", "https://poll.example/op/1")
		return &http.Response{StatusCode: http.StatusAccepted, Header: header, Request: req, Body: http.NoBody}, nil
	}
	body := `{"status":"Failed","error":{"code":"SGWUpdateFailed","message":"backend pool missing"}}`
	return &http.Response{
		StatusCode: http.StatusOK, Header: header, Request: req,
		Body: io.NopCloser(strings.NewReader(body)),
	}, nil
}

// TestServiceGatewayClientAsyncFailureIsNotSynchronousCompletion drives the real generated SDK
// client through a genuinely FAILED long-running operation.
//
// azcore reports such a failure as a ResponseError built from the terminal poll response, and for
// Azure-AsyncOperation polling that response is itself HTTP 200. Discriminating on the status code
// alone therefore reports a failed NRP operation as success, after which the tracker records
// LoadBalancer state NRP does not have and later address syncs referencing it are rejected. The
// initial call is a POST and every poll is a GET, which is what separates the two cases.
func TestServiceGatewayClientAsyncFailureIsNotSynchronousCompletion(t *testing.T) {
	transport := &asyncFailureTransport{}
	client, err := servicegatewayclient.New("subscription", fakeCredential{}, &arm.ClientOptions{
		ClientOptions: azcore.ClientOptions{Transport: transport},
	})
	assert.NoError(t, err)

	dt := &DiffTracker{logger: log.Noop()}
	err = client.UpdateServices(context.Background(), "rg", "sgw", armnetwork.ServiceGatewayUpdateServicesRequest{})

	assert.Error(t, err, "the SDK must surface the failed async operation")
	assert.False(t, isSynchronousCompletion(err),
		"a failed asynchronous operation reported as 200 must not be treated as a synchronous completion")
	assert.Error(t, dt.tolerateSynchronousCompletion(err, "UpdateServices", "sgw"),
		"a failed asynchronous operation must propagate so the caller retries instead of recording phantom NRP state")
}

// serviceClientWithTypeFieldSelector returns a fake clientset whose Service List honours a
// spec.type field selector the way the apiserver does. The default fake tracker ignores field
// selectors, so without this a filtered List is indistinguishable from an unfiltered one.
func serviceClientWithTypeFieldSelector(services ...*v1.Service) *fake.Clientset {
	c := fake.NewSimpleClientset()
	c.PrependReactor("list", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
		out := &v1.ServiceList{}
		wantType, filtered := action.(k8stesting.ListAction).GetListRestrictions().Fields.RequiresExactMatch("spec.type")
		for _, svc := range services {
			if filtered && string(svc.Spec.Type) != wantType {
				continue
			}
			out.Items = append(out.Items, *svc)
		}
		return true, out, nil
	})
	return c
}

// TestGetServiceByUIDViaList_FindsServiceRetypedAwayFromLoadBalancer pins that the UID scan is not
// narrowed by spec.type. Retyping a LoadBalancer to ClusterIP is a normal way to decommission it,
// and the delete that follows still has to tear down the PIP/LB and strip the finalizer. Hiding
// such a Service makes callers read the NotFound as "gone". The LoadBalancer case is the control.
func TestGetServiceByUIDViaList_FindsServiceRetypedAwayFromLoadBalancer(t *testing.T) {
	retyped := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "was-lb", Namespace: "ns", UID: "uid-retyped"},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeClusterIP},
	}
	stillLB := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "still-lb", Namespace: "ns", UID: "uid-lb"},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}

	dt := newTestDiffTracker()
	// No namespace/name recorded for either UID, so both reads take the List scan.
	dt.kubeClient = serviceClientWithTypeFieldSelector(retyped, stillLB)

	got, err := dt.getServiceByUID(context.Background(), "uid-retyped")
	assert.NoError(t, err,
		"a Service retyped away from LoadBalancer must still resolve: the tracker still owns its finalizer and Azure resources")
	if assert.NotNil(t, got) {
		assert.Equal(t, "uid-retyped", string(got.UID))
	}

	gotLB, err := dt.getServiceByUID(context.Background(), "uid-lb")
	assert.NoError(t, err, "control: a LoadBalancer Service resolves regardless of the selector")
	if assert.NotNil(t, gotLB) {
		assert.Equal(t, "uid-lb", string(gotLB.UID))
	}
}

// TestDisassociateNatGateway_ClearsReferencesOnTheWire pins the marshalled request bodies, not just
// that the calls happen. Assigning a plain typed nil to clear a field is silently dropped by the
// generated marshaller, and ARM reads an absent field as "leave unchanged", so both clears become
// no-ops that still report success.
func TestDisassociateNatGateway_ClearsReferencesOnTheWire(t *testing.T) {
	const sgwName, natName = "sgw", "egress-uid"
	sgwID := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/serviceGateways/" + sgwName
	natID := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/natGateways/" + natName

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	var sgwBody, natBody []byte
	mockFactory := mock_azclient.NewMockClientFactory(ctrl)

	mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
	mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
	mockSGW.EXPECT().GetServices(gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]*armnetwork.ServiceGatewayService{{
			Name: ptr.To(natName),
			Properties: &armnetwork.ServiceGatewayServicePropertiesFormat{
				ServiceType:        ptr.To(armnetwork.ServiceTypeOutbound),
				PublicNatGatewayID: ptr.To(natID),
			},
		}}, nil).AnyTimes()
	mockSGW.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, req armnetwork.ServiceGatewayUpdateServicesRequest) error {
			sgwBody, _ = json.Marshal(req)
			return nil
		}).AnyTimes()

	mockNAT := mock_natgatewayclient.NewMockInterface(ctrl)
	mockFactory.EXPECT().GetNatGatewayClient().Return(mockNAT).AnyTimes()
	mockNAT.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&armnetwork.NatGateway{
			Name: ptr.To(natName),
			Properties: &armnetwork.NatGatewayPropertiesFormat{
				IdleTimeoutInMinutes: ptr.To[int32](4),
				ServiceGateway:       &armnetwork.SubResource{ID: ptr.To(sgwID)},
			},
		}, nil).AnyTimes()
	mockNAT.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, ng armnetwork.NatGateway) (*armnetwork.NatGateway, error) {
			natBody, _ = json.Marshal(ng)
			return &ng, nil
		}).AnyTimes()

	dt := newTestDiffTracker()
	dt.networkClientFactory = mockFactory
	dt.config = testConfig()

	assert.NoError(t, dt.disassociateNatGatewayFromServiceGateway(context.Background(), sgwName, natName))

	assert.Contains(t, string(sgwBody), `"publicNatGatewayId":null`,
		"the ServiceGateway-side reference must be cleared explicitly, not omitted")
	assert.Contains(t, string(natBody), `"serviceGateway":null`,
		"the NAT-gateway-side reference must be cleared explicitly, not omitted")
	// Control: unrelated fields are still sent, so the null above is a real clear rather than an
	// empty body.
	assert.Contains(t, string(natBody), `"idleTimeoutInMinutes":4`,
		"control: sibling fields must survive the clear")
}

// TestIsSynchronousCompletion_RejectsErrorBearing200 pins that a 200 which names an error is not
// mistaken for NRP's synchronous completion.
//
// The completion shape is a bare 200 on the initial POST, so matching on status and method alone
// also swallows a 200 whose body reports failure. Tolerating that marks the service registered,
// adds its finalizer and publishes an external IP while NRP holds no entry, so no traffic is ever
// forwarded and nothing re-drives it. azcore fills ErrorCode from the body, so the empty case
// remains the real completion.
func TestIsSynchronousCompletion_RejectsErrorBearing200(t *testing.T) {
	errorBearing := func(code string) error {
		req, _ := http.NewRequest(http.MethodPost, "https://example/sgw", nil)
		return &azcore.ResponseError{
			StatusCode:  http.StatusOK,
			ErrorCode:   code,
			RawResponse: &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Request: req},
		}
	}

	assert.False(t, isSynchronousCompletion(errorBearing("OperationFailed")),
		"a 200 whose body reports a failure must propagate, not be tolerated as completion")
	assert.False(t, isSynchronousCompletion(fmt.Errorf("wrapped: %w", errorBearing("ServiceRegistrationFailed"))),
		"the same holds through a wrapped error")

	// Control: the documented completion shape carries no error code and stays tolerated.
	assert.True(t, isSynchronousCompletion(errorBearing("")),
		"control: a bare 200 on the initial POST is NRP's synchronous completion")
}

// TestConvertServiceDTOsToServiceRequests_MapsServiceType pins the field that decides whether NRP
// registers a service as an inbound LoadBalancer or an outbound NAT Gateway, and that an outbound
// service carries its NAT Gateway reference. Asserting the call happened while matching the payload
// with gomock.Any() cannot see either.
func TestConvertServiceDTOsToServiceRequests_MapsServiceType(t *testing.T) {
	requests, err := convertServiceDTOsToServiceRequests([]ServiceDTO{
		{Service: "lb-uid", ServiceType: Inbound},
		{Service: "nat-uid", ServiceType: Outbound, PublicNatGateway: NatGatewayDTO{ID: "/nat/id"}},
	}, testConfig())
	assert.NoError(t, err)

	if assert.Len(t, requests, 2) {
		assert.Equal(t, armnetwork.ServiceTypeInbound, *requests[0].Service.Properties.ServiceType,
			"an inbound service must register as a LoadBalancer")
		assert.Nil(t, requests[0].Service.Properties.PublicNatGatewayID,
			"an inbound service must not carry a NAT Gateway reference")

		assert.Equal(t, armnetwork.ServiceTypeOutbound, *requests[1].Service.Properties.ServiceType,
			"an outbound service must register as a NAT Gateway")
		if assert.NotNil(t, requests[1].Service.Properties.PublicNatGatewayID) {
			assert.Equal(t, "/nat/id", *requests[1].Service.Properties.PublicNatGatewayID)
		}
	}

	_, err = convertServiceDTOsToServiceRequests([]ServiceDTO{{Service: "x", ServiceType: "Sideways"}}, testConfig())
	assert.Error(t, err, "an unknown service type must not be silently mapped")
}
