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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
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
