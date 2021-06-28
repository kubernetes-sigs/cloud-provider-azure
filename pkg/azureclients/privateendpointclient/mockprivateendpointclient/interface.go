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

package mockprivateendpointclient

import (
	context "context"
	reflect "reflect"

	network "github.com/Azure/azure-sdk-for-go/services/network/mgmt/2021-02-01/network"
	gomock "github.com/golang/mock/gomock"
)

// MockInterface is a mock of Interface interface
type MockInterface struct {
	ctrl     *gomock.Controller
	recorder *MockInterfaceMockRecorder
}

// MockInterfaceMockRecorder is the mock recorder for MockInterface
type MockInterfaceMockRecorder struct {
	mock *MockInterface
}

// NewMockInterface creates a new mock instance
func NewMockInterface(ctrl *gomock.Controller) *MockInterface {
	mock := &MockInterface{ctrl: ctrl}
	mock.recorder = &MockInterfaceMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use
func (m *MockInterface) EXPECT() *MockInterfaceMockRecorder {
	return m.recorder
}

// Get mocks base method
func (m *MockInterface) Get(ctx context.Context, resourceGroupName, privateEndpointName, expand string) (network.PrivateEndpoint, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "Get", ctx, resourceGroupName, privateEndpointName, expand)
	ret0, _ := ret[0].(network.PrivateEndpoint)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// Get indicates an expected call of Get
func (mr *MockInterfaceMockRecorder) Get(ctx, resourceGroupName, privateEndpointName, expand interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Get", reflect.TypeOf((*MockInterface)(nil).Get), ctx, resourceGroupName, privateEndpointName, expand)
}

// CreateOrUpdate mocks base method
func (m *MockInterface) CreateOrUpdate(ctx context.Context, resourceGroupName, endpointName string, privateEndpoint network.PrivateEndpoint, waitForCompletion bool) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "CreateOrUpdate", ctx, resourceGroupName, endpointName, privateEndpoint, waitForCompletion)
	ret0, _ := ret[0].(error)
	return ret0
}

// CreateOrUpdate indicates an expected call of CreateOrUpdate
func (mr *MockInterfaceMockRecorder) CreateOrUpdate(ctx, resourceGroupName, endpointName, privateEndpoint, waitForCompletion interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "CreateOrUpdate", reflect.TypeOf((*MockInterface)(nil).CreateOrUpdate), ctx, resourceGroupName, endpointName, privateEndpoint, waitForCompletion)
}
