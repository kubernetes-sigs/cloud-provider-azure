/*
Copyright 2019 The Kubernetes Authors.

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

package mock

import (
	context "context"
	reflect "reflect"

	gomock "go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	types "k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
)

// NodeProvider is a mock of NodeProvider interface.
type NodeProvider struct {
	ctrl     *gomock.Controller
	recorder *NodeProviderMockRecorder
}

// NodeProviderMockRecorder is the mock recorder for NodeProvider.
type NodeProviderMockRecorder struct {
	mock *NodeProvider
}

// NewMockNodeProvider creates a new mock instance.
func NewMockNodeProvider(ctrl *gomock.Controller) *NodeProvider {
	mock := &NodeProvider{ctrl: ctrl}
	mock.recorder = &NodeProviderMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use.
func (m *NodeProvider) EXPECT() *NodeProviderMockRecorder {
	return m.recorder
}

// GetPlatformSubFaultDomain mocks base method.
func (m *NodeProvider) GetPlatformSubFaultDomain() (string, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetPlatformSubFaultDomain")
	ret0, _ := ret[0].(string)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// GetPlatformSubFaultDomain indicates an expected call of GetPlatformSubFaultDomain.
func (mr *NodeProviderMockRecorder) GetPlatformSubFaultDomain() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetPlatformSubFaultDomain", reflect.TypeOf((*NodeProvider)(nil).GetPlatformSubFaultDomain))
}

// GetZone mocks base method.
func (m *NodeProvider) GetZone(arg0 context.Context, arg1 types.NodeName) (cloudprovider.Zone, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetZone", arg0, arg1)
	ret0, _ := ret[0].(cloudprovider.Zone)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// GetZone indicates an expected call of GetZone.
func (mr *NodeProviderMockRecorder) GetZone(arg0, arg1 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetZone", reflect.TypeOf((*NodeProvider)(nil).GetZone), arg0, arg1)
}

// InstanceID mocks base method.
func (m *NodeProvider) InstanceID(arg0 context.Context, arg1 types.NodeName) (string, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "InstanceID", arg0, arg1)
	ret0, _ := ret[0].(string)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// InstanceID indicates an expected call of InstanceID.
func (mr *NodeProviderMockRecorder) InstanceID(arg0, arg1 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "InstanceID", reflect.TypeOf((*NodeProvider)(nil).InstanceID), arg0, arg1)
}

// InstanceType mocks base method.
func (m *NodeProvider) InstanceType(arg0 context.Context, arg1 types.NodeName) (string, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "InstanceType", arg0, arg1)
	ret0, _ := ret[0].(string)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// InstanceType indicates an expected call of InstanceType.
func (mr *NodeProviderMockRecorder) InstanceType(arg0, arg1 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "InstanceType", reflect.TypeOf((*NodeProvider)(nil).InstanceType), arg0, arg1)
}

// NodeAddresses mocks base method.
func (m *NodeProvider) NodeAddresses(arg0 context.Context, arg1 types.NodeName) ([]v1.NodeAddress, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "NodeAddresses", arg0, arg1)
	ret0, _ := ret[0].([]v1.NodeAddress)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// NodeAddresses indicates an expected call of NodeAddresses.
func (mr *NodeProviderMockRecorder) NodeAddresses(arg0, arg1 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "NodeAddresses", reflect.TypeOf((*NodeProvider)(nil).NodeAddresses), arg0, arg1)
}
