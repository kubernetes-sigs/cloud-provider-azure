package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	content, err := os.ReadFile("pkg/provider/azure_mock_vmsets.go")
	if err != nil {
		fmt.Println(err)
		return
	}

	mockCode := `// GetPlatformFaultDomainByNodeName mocks base method.
func (m *MockVMSet) GetPlatformFaultDomainByNodeName(ctx context.Context, name string) (string, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetPlatformFaultDomainByNodeName", ctx, name)
	ret0, _ := ret[0].(string)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// GetPlatformFaultDomainByNodeName indicates an expected call of GetPlatformFaultDomainByNodeName.
func (mr *MockVMSetMockRecorder) GetPlatformFaultDomainByNodeName(ctx, name any) *MockVMSetGetPlatformFaultDomainByNodeNameCall {
	mr.mock.ctrl.T.Helper()
	call := mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetPlatformFaultDomainByNodeName", reflect.TypeOf((*MockVMSet)(nil).GetPlatformFaultDomainByNodeName), ctx, name)
	return &MockVMSetGetPlatformFaultDomainByNodeNameCall{Call: call}
}

// MockVMSetGetPlatformFaultDomainByNodeNameCall wrap *gomock.Call
type MockVMSetGetPlatformFaultDomainByNodeNameCall struct {
	*gomock.Call
}

// Return rewrite *gomock.Call.Return
func (c *MockVMSetGetPlatformFaultDomainByNodeNameCall) Return(arg0 string, arg1 error) *MockVMSetGetPlatformFaultDomainByNodeNameCall {
	c.Call = c.Call.Return(arg0, arg1)
	return c
}

// Do rewrite *gomock.Call.Do
func (c *MockVMSetGetPlatformFaultDomainByNodeNameCall) Do(f func(context.Context, string) (string, error)) *MockVMSetGetPlatformFaultDomainByNodeNameCall {
	c.Call = c.Call.Do(f)
	return c
}

// DoAndReturn rewrite *gomock.Call.DoAndReturn
func (c *MockVMSetGetPlatformFaultDomainByNodeNameCall) DoAndReturn(f func(context.Context, string) (string, error)) *MockVMSetGetPlatformFaultDomainByNodeNameCall {
	c.Call = c.Call.DoAndReturn(f)
	return c
}
`
	strContent := string(content)
	strContent = strings.Replace(strContent, "// RefreshCaches mocks base method.", mockCode+"\n// RefreshCaches mocks base method.", 1)
	os.WriteFile("pkg/provider/azure_mock_vmsets.go", []byte(strContent), 0644)
}
