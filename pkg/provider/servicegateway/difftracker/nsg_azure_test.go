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
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/interfaceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/securitygroupclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachineclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachinescalesetvmclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

type nsgTransportFunc func(*http.Request) (*http.Response, error)

func (f nsgTransportFunc) Do(request *http.Request) (*http.Response, error) {
	return f(request)
}

func nsgResponse(request *http.Request, status int, value any) (*http.Response, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    request,
	}, nil
}

func TestNSGAzurePrimaryNICReadsRespectCancellation(t *testing.T) {
	for _, uniform := range []bool{false, true} {
		for _, phase := range []string{"success", "cancel VM read", "cancel NIC read"} {
			t.Run(fmt.Sprintf("uniform=%t/%s", uniform, phase), func(t *testing.T) {
				dt, cloud, node := newNSGTestTracker(t)
				vmID := *cloud.nic.Properties.VirtualMachine.ID
				if uniform {
					vmID = "/subscriptions/compute-sub/resourceGroups/node-rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0"
					cloud.nic.ID = ptr.To(vmID + "/networkInterfaces/primary")
					cloud.nic.Properties.VirtualMachine.ID = ptr.To(vmID)
					node.Spec.ProviderID = consts.ServiceGatewayNSGProviderIDPrefix + vmID
				}
				profile := &armcompute.NetworkProfile{NetworkInterfaces: []*armcompute.NetworkInterfaceReference{
					{ID: ptr.To(*cloud.nic.ID + "-secondary"), Properties: &armcompute.NetworkInterfaceReferenceProperties{Primary: ptr.To(false)}},
					{ID: cloud.nic.ID, Properties: &armcompute.NetworkInterfaceReferenceProperties{Primary: ptr.To(true)}},
				}}
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				requests := 0
				transport := nsgTransportFunc(func(request *http.Request) (*http.Response, error) {
					requests++
					assert.Equal(t, http.MethodGet, request.Method)
					vmRead := request.URL.Path == vmID
					if phase == "cancel VM read" && vmRead || phase == "cancel NIC read" && !vmRead {
						cancel()
						select {
						case <-request.Context().Done():
							return nil, request.Context().Err()
						case <-time.After(time.Second):
							return nil, fmt.Errorf("node read did not preserve cancellation")
						}
					}
					if vmRead {
						return nsgResponse(request, http.StatusOK, armcompute.VirtualMachine{
							ID: ptr.To(vmID), Location: ptr.To("eastus"),
							Properties: &armcompute.VirtualMachineProperties{NetworkProfile: profile},
						})
					}
					assert.Equal(t, *cloud.nic.ID, request.URL.Path)
					nic := nsgCopy(cloud.nic)
					nic.Location = nil
					return nsgResponse(request, http.StatusOK, nic)
				})
				options := func() *arm.ClientOptions {
					return &arm.ClientOptions{ClientOptions: azcore.ClientOptions{
						Transport: transport, Retry: policy.RetryOptions{MaxRetries: -1},
					}}
				}
				nics, err := interfaceclient.New("compute-sub", fakeCredential{}, options())
				assert.NoError(t, err)
				factory := mock_azclient.NewMockClientFactory(gomock.NewController(t))
				factory.EXPECT().GetInterfaceClient().Return(nics).AnyTimes()
				if uniform {
					vms, err := virtualmachinescalesetvmclient.New("compute-sub", fakeCredential{}, options())
					assert.NoError(t, err)
					factory.EXPECT().GetVirtualMachineScaleSetVMClient().Return(vms).AnyTimes()
				} else {
					vms, err := virtualmachineclient.New("compute-sub", fakeCredential{}, options())
					assert.NoError(t, err)
					factory.EXPECT().GetVirtualMachineClient().Return(vms).AnyTimes()
				}
				dt.computeClientFactory = factory
				target, nic, _, err := dt.primaryNIC(ctx, node)
				if phase == "success" {
					if assert.NoError(t, err) && assert.NotNil(t, nic) {
						assert.Equal(t, *cloud.nic.ID, target.String())
						assert.Equal(t, "eastus", ptr.Deref(nic.Location, ""))
					}
				} else {
					assert.ErrorIs(t, err, context.Canceled)
					assert.Nil(t, nic)
				}
				wantReads := 2
				if phase == "cancel VM read" {
					wantReads = 1
				}
				assert.Equal(t, wantReads, requests)
			})
		}
	}
}

func TestNSGPrimaryNICID(t *testing.T) {
	reference := func(id string, primary bool) *armcompute.NetworkInterfaceReference {
		return &armcompute.NetworkInterfaceReference{
			ID: ptr.To(id), Properties: &armcompute.NetworkInterfaceReferenceProperties{Primary: ptr.To(primary)},
		}
	}
	_, err := nsgPrimaryNICID(nil)
	assert.Error(t, err)
	for _, tt := range []struct {
		name string
		nics []*armcompute.NetworkInterfaceReference
		want string
	}{
		{name: "missing references"},
		{name: "nil reference", nics: []*armcompute.NetworkInterfaceReference{nil}},
		{name: "sole implicit primary", nics: []*armcompute.NetworkInterfaceReference{{ID: ptr.To("nic")}}, want: "nic"},
		{name: "explicit primary", nics: []*armcompute.NetworkInterfaceReference{reference("secondary", false), reference("primary", true)}, want: "primary"},
		{name: "no primary", nics: []*armcompute.NetworkInterfaceReference{reference("a", false), reference("b", false)}},
		{name: "multiple primaries", nics: []*armcompute.NetworkInterfaceReference{reference("a", true), reference("b", true)}},
		{name: "missing primary identity", nics: []*armcompute.NetworkInterfaceReference{reference("", true)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := nsgPrimaryNICID(&armcompute.NetworkProfile{NetworkInterfaces: tt.nics})
			if tt.want == "" {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNSGAzureConditionalNSGUpdate(t *testing.T) {
	for _, tt := range []struct {
		name        string
		status      int
		changedNRMS bool
	}{
		{"success", http.StatusOK, false},
		{"ETag conflict", http.StatusPreconditionFailed, false},
		{"changed NRMS response", http.StatusOK, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dt, cloud, _ := newNSGTestTracker(t)
			target, err := dt.targetFromID(*cloud.nic.ID)
			assert.NoError(t, err)
			rules, err := buildNICSecurityRules(nil, nil)
			assert.NoError(t, err)
			operational := nsgTestNRMSRule(103)
			rules = append(rules, operational)
			group, _, err := dt.desiredGroup(nil, target, "node", "eastus", rules)
			assert.NoError(t, err)
			group.Etag = ptr.To("nsg-etag")
			group.Properties.SecurityRules = []*armnetwork.SecurityRule{nsgCopy(operational)}
			group.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateSucceeded)
			group.Tags["unrelated"] = ptr.To("preserved")
			desired, changed, err := dt.desiredGroup(&group, target, "node", "eastus", rules)
			assert.NoError(t, err)
			assert.True(t, changed)
			assert.Equal(t, []*armnetwork.SecurityRule{operational}, group.Properties.SecurityRules)
			requests := 0
			client, err := securitygroupclient.New("network-sub", fakeCredential{}, &arm.ClientOptions{
				ClientOptions: azcore.ClientOptions{
					Retry: policy.RetryOptions{MaxRetries: -1},
					Transport: nsgTransportFunc(func(request *http.Request) (*http.Response, error) {
						requests++
						assert.Equal(t, http.MethodPut, request.Method)
						assert.Equal(t, *group.ID, request.URL.Path)
						assert.Equal(t, *group.Etag, request.Header.Get(consts.IfMatchHeader))
						var sent armnetwork.SecurityGroup
						if err := json.NewDecoder(request.Body).Decode(&sent); err != nil {
							return nil, err
						}
						assert.True(t, nsgRulesEqual(desired.Properties.SecurityRules, sent.Properties.SecurityRules))
						assert.Equal(t, group.Tags, sent.Tags)
						if tt.status != http.StatusOK {
							return nsgResponse(request, tt.status, map[string]any{"error": map[string]string{
								"code": "PreconditionFailed", "message": "NSG changed",
							}})
						}
						if tt.changedNRMS {
							for _, rule := range sent.Properties.SecurityRules {
								if ptr.Deref(rule.Name, "") == *operational.Name {
									rule.Properties.SourceAddressPrefix = ptr.To("Internet")
								}
							}
						}
						sent.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateSucceeded)
						return nsgResponse(request, tt.status, sent)
					}),
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			factory := mock_azclient.NewMockClientFactory(gomock.NewController(t))
			factory.EXPECT().GetSecurityGroupClient().Return(client).AnyTimes()
			dt.networkClientFactory = factory
			_, err = dt.writeGroup(context.Background(), desired)
			if tt.changedNRMS {
				assert.ErrorContains(t, err, "did not confirm")
			} else if tt.status == http.StatusOK {
				assert.NoError(t, err)
			} else {
				var response *azcore.ResponseError
				if assert.ErrorAs(t, err, &response) {
					assert.Equal(t, tt.status, response.StatusCode)
				}
			}
			assert.Equal(t, 1, requests)
		})
	}
}

func TestNSGAzureRetainsConcurrentNRMSRules(t *testing.T) {
	ctx := context.Background()
	service := nsgKubeService("svc-lb", "20.0.0.0/8")
	dt, cloud, _ := newNSGTestTracker(t, service)
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
	var remote *armnetwork.SecurityGroup
	for _, group := range cloud.groups {
		remote = nsgCopy(group)
	}
	if !assert.NotNil(t, remote) {
		return
	}
	initialETag := *remote.Etag
	operational := nsgTestNRMSRule(103)
	gets, puts := 0, 0
	client, err := securitygroupclient.New("network-sub", fakeCredential{}, &arm.ClientOptions{
		ClientOptions: azcore.ClientOptions{
			Retry: policy.RetryOptions{MaxRetries: -1},
			Transport: nsgTransportFunc(func(request *http.Request) (*http.Response, error) {
				assert.Equal(t, *remote.ID, request.URL.Path)
				switch request.Method {
				case http.MethodGet:
					gets++
					return nsgResponse(request, http.StatusOK, remote)
				case http.MethodPut:
					puts++
					var sent armnetwork.SecurityGroup
					if err := json.NewDecoder(request.Body).Decode(&sent); err != nil {
						return nil, err
					}
					if puts == 1 {
						assert.Equal(t, initialETag, request.Header.Get(consts.IfMatchHeader))
						remote.Properties.SecurityRules = append(remote.Properties.SecurityRules, nsgCopy(operational))
						remote.Etag = ptr.To("nrms-changed")
						return nsgResponse(request, http.StatusPreconditionFailed, map[string]any{"error": map[string]string{
							"code": "PreconditionFailed", "message": "NRMS changed the NSG after it was read",
						}})
					}
					assert.Equal(t, "nrms-changed", request.Header.Get(consts.IfMatchHeader))
					assert.Contains(t, sent.Properties.SecurityRules, operational)
					sent.Etag = ptr.To("reconciled")
					sent.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateSucceeded)
					remote = &sent
					return nsgResponse(request, http.StatusOK, remote)
				default:
					return nil, fmt.Errorf("unexpected NSG method %s", request.Method)
				}
			}),
		},
	})
	if !assert.NoError(t, err) {
		return
	}
	factory := mock_azclient.NewMockClientFactory(gomock.NewController(t))
	factory.EXPECT().GetSecurityGroupClient().Return(client).AnyTimes()
	dt.networkClientFactory = factory
	service.Annotations[consts.ServiceAnnotationAllowedIPRanges] = "40.0.0.0/8"
	assert.NoError(t, dt.ReconcileInboundService(service))
	updater := &LocationsUpdater{diffTracker: dt, ctx: ctx, logger: dt.logger}
	updater.process(ctx)
	assert.Equal(t, 1, puts)
	assert.NotEmpty(t, dt.dirtyNICPolicies, "a concurrent write must retain retry work")
	assert.Contains(t, remote.Properties.SecurityRules, operational)
	assert.True(t, dt.NRPResources.Locations["10.0.0.1"].Addresses["10.1.0.1"].Services.Has("svc-lb"))

	updater.process(ctx)
	assert.Equal(t, 2, gets)
	assert.Equal(t, 2, puts)
	assert.Empty(t, dt.dirtyNICPolicies)
	assert.Contains(t, remote.Properties.SecurityRules, operational)
	assert.Equal(t, armnetwork.SecurityRuleAccessAllow,
		nsgTestAccess(t, remote.Properties.SecurityRules, "40.1.2.3", "10.1.0.1", 8080, armnetwork.SecurityRuleProtocolTCP))
	assert.Equal(t, armnetwork.SecurityRuleAccessDeny,
		nsgTestAccess(t, remote.Properties.SecurityRules, "20.1.2.3", "10.1.0.1", 8080, armnetwork.SecurityRuleProtocolTCP))
	updater.process(ctx)
	assert.Equal(t, 2, gets, "a completed pass must not cause another NSG read")
	assert.Equal(t, 2, puts, "a completed pass must not rewrite the NSG")
	assert.Equal(t, 1, cloud.nicWrites, "rule co-management must not rewrite the NIC association")
}

func TestNSGAzureConditionalNICAssociation(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusPreconditionFailed} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			dt, cloud, node := newNSGTestTracker(t)
			target, err := dt.targetFromID(*cloud.nic.ID)
			if err != nil {
				t.Fatal(err)
			}
			original := nsgCopy(cloud.nic)
			requests := 0
			transport := nsgTransportFunc(func(request *http.Request) (*http.Response, error) {
				requests++
				assert.Equal(t, http.MethodPut, request.Method)
				assert.Equal(t, *original.ID, request.URL.Path)
				assert.Equal(t, *original.Etag, request.Header.Get(consts.IfMatchHeader))
				var sent armnetwork.Interface
				if err := json.NewDecoder(request.Body).Decode(&sent); err != nil {
					return nil, err
				}
				assert.Equal(t, dt.groupID(target), ptr.Deref(sent.Properties.NetworkSecurityGroup.ID, ""))
				assert.Equal(t, original.Properties.IPConfigurations, sent.Properties.IPConfigurations)
				assert.Equal(t, original.Properties.VirtualMachine, sent.Properties.VirtualMachine)
				assert.Equal(t, original.Properties.EnableIPForwarding, sent.Properties.EnableIPForwarding)
				assert.Equal(t, original.Properties.EnableAcceleratedNetworking, sent.Properties.EnableAcceleratedNetworking)
				assert.Equal(t, original.Tags, sent.Tags)
				if status != http.StatusOK {
					return nsgResponse(request, status, map[string]any{"error": map[string]string{
						"code": "PreconditionFailed", "message": "NIC changed",
					}})
				}
				sent.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateSucceeded)
				return nsgResponse(request, status, sent)
			})
			client, err := interfaceclient.New("compute-sub", fakeCredential{}, &arm.ClientOptions{
				ClientOptions: azcore.ClientOptions{Transport: transport, Retry: policy.RetryOptions{MaxRetries: -1}},
			})
			if err != nil {
				t.Fatal(err)
			}
			factory := mock_azclient.NewMockClientFactory(gomock.NewController(t))
			factory.EXPECT().GetInterfaceClient().Return(client).AnyTimes()
			dt.computeClientFactory = factory
			err = dt.associate(context.Background(), node.Name, target, original, nil)
			if status == http.StatusOK {
				assert.NoError(t, err)
			} else {
				var response *azcore.ResponseError
				if assert.ErrorAs(t, err, &response) {
					assert.Equal(t, status, response.StatusCode)
				}
			}
			assert.Equal(t, 1, requests)
			assert.Equal(t, 1, cloud.cacheInvalidations)
			assert.Nil(t, original.Properties.NetworkSecurityGroup, "the fetched NIC must not be mutated")
		})
	}
}

func TestNSGAzureConditionalVMSSAssociation(t *testing.T) {
	for _, scenario := range []string{"success", "conflict", "different primary NIC", "foreign association", "multiple primary configurations"} {
		t.Run(scenario, func(t *testing.T) {
			dt, cloud, node := newNSGTestTracker(t)
			vmID := "/subscriptions/compute-sub/resourceGroups/node-rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0"
			nicID := vmID + "/networkInterfaces/primary"
			target, err := dt.targetFromID(nicID)
			if err != nil {
				t.Fatal(err)
			}
			nic := nsgCopy(cloud.nic)
			nic.ID = ptr.To(nicID)
			nic.Properties.VirtualMachine.ID = ptr.To(vmID)
			vm := armcompute.VirtualMachineScaleSetVM{
				ID: ptr.To(vmID), Etag: ptr.To("vm-etag"), Location: ptr.To("eastus"),
				Properties: &armcompute.VirtualMachineScaleSetVMProperties{
					ProvisioningState: ptr.To("Succeeded"),
					HardwareProfile:   &armcompute.HardwareProfile{VMSize: ptr.To(armcompute.VirtualMachineSizeTypes("Standard_D2s_v5"))},
					StorageProfile:    &armcompute.StorageProfile{DataDisks: []*armcompute.DataDisk{{Name: ptr.To("disk"), Lun: ptr.To(int32(1))}}},
					NetworkProfile: &armcompute.NetworkProfile{NetworkInterfaces: []*armcompute.NetworkInterfaceReference{
						{ID: ptr.To(nicID), Properties: &armcompute.NetworkInterfaceReferenceProperties{Primary: ptr.To(true)}},
						{ID: ptr.To(vmID + "/networkInterfaces/secondary"), Properties: &armcompute.NetworkInterfaceReferenceProperties{Primary: ptr.To(false)}},
					}},
					NetworkProfileConfiguration: &armcompute.VirtualMachineScaleSetVMNetworkProfileConfiguration{
						NetworkInterfaceConfigurations: []*armcompute.VirtualMachineScaleSetNetworkConfiguration{
							{Name: ptr.To("primary"), Properties: &armcompute.VirtualMachineScaleSetNetworkConfigurationProperties{
								Primary: ptr.To(true), EnableIPForwarding: ptr.To(true), EnableAcceleratedNetworking: ptr.To(true),
								IPConfigurations: []*armcompute.VirtualMachineScaleSetIPConfiguration{
									{Name: ptr.To("v4"), Properties: &armcompute.VirtualMachineScaleSetIPConfigurationProperties{
										Primary: ptr.To(true), PrivateIPAddressVersion: ptr.To(armcompute.IPVersionIPv4),
										LoadBalancerBackendAddressPools: []*armcompute.SubResource{{ID: ptr.To("existing-pool")}},
									}},
									{Name: ptr.To("v6"), Properties: &armcompute.VirtualMachineScaleSetIPConfigurationProperties{
										PrivateIPAddressVersion: ptr.To(armcompute.IPVersionIPv6),
									}},
								},
							}},
							{Name: ptr.To("secondary"), Properties: &armcompute.VirtualMachineScaleSetNetworkConfigurationProperties{
								Primary: ptr.To(false), NetworkSecurityGroup: &armcompute.SubResource{ID: ptr.To("secondary-nsg")},
							}},
						},
					},
				},
			}
			switch scenario {
			case "different primary NIC":
				vm.Properties.NetworkProfile.NetworkInterfaces[0].ID = ptr.To(vmID + "/networkInterfaces/replacement")
			case "foreign association":
				vm.Properties.NetworkProfileConfiguration.NetworkInterfaceConfigurations[0].Properties.NetworkSecurityGroup = &armcompute.SubResource{ID: ptr.To("foreign")}
			case "multiple primary configurations":
				vm.Properties.NetworkProfileConfiguration.NetworkInterfaceConfigurations[1].Properties.Primary = ptr.To(true)
			}
			puts, gets := 0, 0
			transport := nsgTransportFunc(func(request *http.Request) (*http.Response, error) {
				assert.Equal(t, vmID, request.URL.Path)
				if request.Method == http.MethodGet {
					gets++
					return nsgResponse(request, http.StatusOK, vm)
				}
				assert.Equal(t, http.MethodPut, request.Method)
				puts++
				assert.Equal(t, "vm-etag", request.Header.Get(consts.IfMatchHeader))
				var sent armcompute.VirtualMachineScaleSetVM
				if err := json.NewDecoder(request.Body).Decode(&sent); err != nil {
					return nil, err
				}
				configs := sent.Properties.NetworkProfileConfiguration.NetworkInterfaceConfigurations
				if !assert.Len(t, configs, 2) {
					return nil, fmt.Errorf("incorrect NIC configuration count")
				}
				assert.Equal(t, dt.groupID(target), ptr.Deref(configs[0].Properties.NetworkSecurityGroup.ID, ""))
				assert.Equal(t, vm.Properties.NetworkProfileConfiguration.NetworkInterfaceConfigurations[0].Properties.IPConfigurations, configs[0].Properties.IPConfigurations)
				assert.Equal(t, vm.Properties.NetworkProfileConfiguration.NetworkInterfaceConfigurations[1], configs[1])
				assert.Equal(t, vm.Properties.HardwareProfile, sent.Properties.HardwareProfile)
				assert.Nil(t, sent.Properties.StorageProfile, "the update must not overwrite storage configuration")
				if scenario == "conflict" {
					return nsgResponse(request, http.StatusPreconditionFailed, map[string]any{"error": map[string]string{
						"code": "PreconditionFailed", "message": "VM changed",
					}})
				}
				sent.ID = ptr.To(vmID)
				sent.Properties.ProvisioningState = ptr.To("Succeeded")
				return nsgResponse(request, http.StatusOK, sent)
			})
			client, err := virtualmachinescalesetvmclient.New("compute-sub", fakeCredential{}, &arm.ClientOptions{
				ClientOptions: azcore.ClientOptions{Transport: transport, Retry: policy.RetryOptions{MaxRetries: -1}},
			})
			if err != nil {
				t.Fatal(err)
			}
			factory := mock_azclient.NewMockClientFactory(gomock.NewController(t))
			factory.EXPECT().GetVirtualMachineScaleSetVMClient().Return(client).AnyTimes()
			dt.computeClientFactory = factory
			original := nsgCopy(vm)
			err = dt.associate(context.Background(), node.Name, target, nic, &vm)
			switch scenario {
			case "success":
				assert.NoError(t, err)
				assert.Equal(t, 1, puts)
			case "conflict":
				var response *azcore.ResponseError
				if assert.ErrorAs(t, err, &response) {
					assert.Equal(t, http.StatusPreconditionFailed, response.StatusCode)
				}
				assert.Zero(t, gets, "association reuses the already-fetched VMSS model and ETag")
				assert.Equal(t, original, vm, "the fetched VMSS model must not be mutated")
				assert.Equal(t, 1, puts)
			default:
				assert.Error(t, err)
				assert.Equal(t, 0, puts)
			}
			assert.Equal(t, 1, cloud.cacheInvalidations)
		})
	}
}

func TestNSGAzureOwnership(t *testing.T) {
	dt, cloud, _ := newNSGTestTracker(t)
	target, err := dt.targetFromID(*cloud.nic.ID)
	if err != nil {
		t.Fatal(err)
	}
	group, _, err := dt.desiredGroup(nil, target, "node", "eastus", nil)
	if err != nil {
		t.Fatal(err)
	}
	assert.True(t, dt.owns(&group, target))
	cased := nsgCopy(group)
	cased.Name = ptr.To(strings.ToUpper(*cased.Name))
	cased.ID = ptr.To(strings.ToUpper(*cased.ID))
	cased.Tags = make(map[string]*string)
	for key, value := range group.Tags {
		cased.Tags[strings.ToUpper(key)] = value
	}
	assert.True(t, dt.owns(&cased, target))
	recovered, err := dt.targetFromTags(&cased)
	assert.NoError(t, err)
	assert.Equal(t, target, recovered)

	for _, key := range []string{consts.ServiceGatewayNSGOwnerTag, consts.ServiceGatewayNSGNICTag} {
		for _, value := range []*string{nil, ptr.To("different-owner")} {
			t.Run(key+"/"+ptr.Deref(value, "missing"), func(t *testing.T) {
				other := nsgCopy(group)
				if value == nil {
					delete(other.Tags, key)
				} else {
					other.Tags[key] = value
				}
				assert.False(t, dt.owns(&other, target))
				_, _, err := dt.desiredGroup(&other, target, "node", "eastus", nil)
				assert.Error(t, err)
			})
		}
	}
	markedOnly := nsgCopy(group)
	markedOnly.Tags = managedResourceTags()
	assert.False(t, dt.owns(&markedOnly, target), "a generic managed marker cannot replace the NIC and SGW ownership tags")
	_, _, err = dt.desiredGroup(&markedOnly, target, "node", "eastus", nil)
	assert.Error(t, err)
	conflicting := nsgCopy(group)
	conflicting.Tags[strings.ToUpper(consts.ServiceGatewayNSGOwnerTag)] = ptr.To("different-owner")
	assert.False(t, dt.owns(&conflicting, target))
	_, err = dt.targetFromID(strings.Replace(target.String(), "compute-sub", "other-sub", 1))
	assert.Error(t, err)
}

func TestNSGAzureReadsRequireSettledResources(t *testing.T) {
	for _, tt := range []struct {
		name  string
		state *armnetwork.ProvisioningState
		valid bool
	}{
		{name: "missing state"},
		{name: "empty state", state: ptr.To(armnetwork.ProvisioningState(""))},
		{name: "updating", state: ptr.To(armnetwork.ProvisioningStateUpdating)},
		{name: "deleting", state: ptr.To(armnetwork.ProvisioningStateDeleting)},
		{name: "succeeded", state: ptr.To(armnetwork.ProvisioningStateSucceeded), valid: true},
		{name: "failed can be repaired", state: ptr.To(armnetwork.ProvisioningStateFailed), valid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dt, cloud, _ := newNSGTestTracker(t)
			target, err := dt.targetFromID(*cloud.nic.ID)
			assert.NoError(t, err)
			group, _, err := dt.desiredGroup(nil, target, "node", "eastus", nil)
			assert.NoError(t, err)
			group.Properties.ProvisioningState = tt.state
			cloud.groups[*group.Name] = &group
			result, err := dt.readGroup(context.Background(), *group.Name)
			if tt.valid {
				assert.NoError(t, err)
				assert.NotNil(t, result)
			} else {
				assert.Error(t, err)
				assert.Nil(t, result)
			}
		})
	}
}

func TestNSGRulesEqual(t *testing.T) {
	rule := &armnetwork.SecurityRule{
		Name: ptr.To("rule"),
		Properties: &armnetwork.SecurityRulePropertiesFormat{
			Access: ptr.To(armnetwork.SecurityRuleAccessAllow), Direction: ptr.To(armnetwork.SecurityRuleDirectionInbound),
			Priority: ptr.To[int32](100), Protocol: ptr.To(armnetwork.SecurityRuleProtocolTCP),
			SourceAddressPrefix: ptr.To("20.0.0.0/8"), DestinationAddressPrefix: ptr.To("10.1.0.1"),
			SourcePortRange: ptr.To("*"), DestinationPortRange: ptr.To("8080"),
		},
	}
	for _, tt := range []struct {
		name   string
		change func(*armnetwork.SecurityRule)
		equal  bool
	}{
		{name: "unchanged", change: func(*armnetwork.SecurityRule) {}, equal: true},
		{name: "response metadata", equal: true, change: func(r *armnetwork.SecurityRule) {
			r.ID, r.Etag, r.Type = ptr.To("resource-id"), ptr.To("etag"), ptr.To("rule-type")
			r.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateSucceeded)
		}},
		{name: "plural addresses and ports", equal: true, change: func(r *armnetwork.SecurityRule) {
			r.Properties.SourceAddressPrefixes, r.Properties.SourceAddressPrefix = []*string{r.Properties.SourceAddressPrefix}, nil
			r.Properties.DestinationAddressPrefixes, r.Properties.DestinationAddressPrefix = []*string{r.Properties.DestinationAddressPrefix}, nil
			r.Properties.SourcePortRanges, r.Properties.SourcePortRange = []*string{r.Properties.SourcePortRange}, nil
			r.Properties.DestinationPortRanges, r.Properties.DestinationPortRange = []*string{r.Properties.DestinationPortRange}, nil
		}},
		{name: "changed port", change: func(r *armnetwork.SecurityRule) {
			r.Properties.DestinationPortRange = ptr.To("9090")
		}},
		{name: "changed source", change: func(r *armnetwork.SecurityRule) {
			r.Properties.SourceAddressPrefix = ptr.To("40.0.0.0/8")
		}},
		{name: "changed action", change: func(r *armnetwork.SecurityRule) {
			r.Properties.Access = ptr.To(armnetwork.SecurityRuleAccessDeny)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			changed := nsgCopy(rule)
			tt.change(changed)
			before := nsgCopy(changed)
			assert.Equal(t, tt.equal, nsgRulesEqual([]*armnetwork.SecurityRule{rule}, []*armnetwork.SecurityRule{changed}))
			assert.Equal(t, before, changed)
		})
	}
}
