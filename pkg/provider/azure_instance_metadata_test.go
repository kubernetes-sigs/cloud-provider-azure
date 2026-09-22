/*
Copyright 2023 The Kubernetes Authors.

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

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"

	"sigs.k8s.io/cloud-provider-azure/internal/testutil/imdstest"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
)

func TestCompileMetadataLabelRules(t *testing.T) {
	for _, tc := range []struct {
		name       string
		expression string
		wantError  string
	}{
		{name: "string", expression: "'group-123'"},
		{name: "syntax error", expression: "tags[", wantError: "compile metadata label rule"},
		{name: "undeclared variable", expression: "metadata.name", wantError: "undeclared reference"},
		{name: "wrong tag key type", expression: "tags[1]", wantError: "compile metadata label rule"},
		{name: "compute string", expression: "compute.interconnectGroupId"},
		{name: "wrong compute key type", expression: "compute[1]", wantError: "compile metadata label rule"},
		{name: "wrong compute value type", expression: "compute.interconnectGroupId + 1", wantError: "compile metadata label rule"},
		{name: "boolean result", expression: "true", wantError: "must return string"},
		{name: "dynamic result", expression: "dyn('group')", wantError: "must return string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rules, err := compileMetadataLabelRules([]metadataLabelRule{{
				name: "test-rule", label: consts.LabelPlatformInterconnectGroup, expression: tc.expression,
			}})
			if tc.wantError != "" {
				if !assert.ErrorContains(t, err, tc.wantError) {
					t.FailNow()
				}
				assert.Contains(t, err.Error(), "test-rule")
				assert.Contains(t, err.Error(), consts.LabelPlatformInterconnectGroup)
				assert.Nil(t, rules)
			} else {
				assert.NoError(t, err)
				assert.Len(t, rules, 1)
			}
		})
	}
}

func TestEvaluateMetadataLabels(t *testing.T) {
	rules, err := builtinMetadataLabelRules()
	if !assert.NoError(t, err) {
		t.FailNow()
	}
	for _, tc := range []struct {
		name      string
		tags      []Tag
		want      string
		wantError bool
	}{
		{name: "present", tags: []Tag{{Name: consts.TagNameInterconnectGroup, Value: "group-123"}}, want: "group-123"},
		{name: "missing"},
		{name: "case sensitive", tags: []Tag{{Name: "platform_interconnect_group", Value: "ignored"}}},
		{name: "empty", tags: []Tag{{Name: consts.TagNameInterconnectGroup}}},
		{name: "first duplicate wins", tags: []Tag{{Name: consts.TagNameInterconnectGroup, Value: "first"}, {Name: consts.TagNameInterconnectGroup, Value: "second"}}, want: "first"},
		{name: "empty first duplicate wins", tags: []Tag{{Name: consts.TagNameInterconnectGroup}, {Name: consts.TagNameInterconnectGroup, Value: "second"}}},
		{name: "invalid character", tags: []Tag{{Name: consts.TagNameInterconnectGroup, Value: "invalid/value"}}, wantError: true},
		{name: "too long", tags: []Tag{{Name: consts.TagNameInterconnectGroup, Value: strings.Repeat("a", 64)}}, wantError: true},
		{name: "maximum length", tags: []Tag{{Name: consts.TagNameInterconnectGroup, Value: strings.Repeat("a", 63)}}, want: strings.Repeat("a", 63)},
		{name: "unrelated invalid tag", tags: []Tag{{Name: "other", Value: "invalid/value"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			labels, err := evaluateMetadataLabels(context.Background(), rules, ComputeMetadata{TagsList: tc.tags})
			if tc.wantError {
				if !assert.ErrorContains(t, err, "invalid label value") {
					t.FailNow()
				}
				assert.Contains(t, err.Error(), consts.LabelPlatformInterconnectGroup)
				assert.NotContains(t, err.Error(), tc.tags[0].Value)
				assert.Nil(t, labels)
			} else {
				assert.NoError(t, err)
				want := map[string]string{}
				if tc.want != "" {
					want[consts.LabelPlatformInterconnectGroup] = tc.want
					want[consts.LabelPlatformInterconnectSubgroup] = tc.want
				}
				assert.Equal(t, want, labels)
			}
		})
	}
}

func TestEvaluateMetadataLabelsErrors(t *testing.T) {
	tags := make([]Tag, 1000)
	for i := range tags {
		tags[i] = Tag{Name: fmt.Sprintf("tag-%d", i), Value: "value"}
	}
	for _, tc := range []struct {
		name       string
		expression string
		wantError  string
	}{
		{name: "missing key", expression: "tags['missing']", wantError: "no such key"},
		{name: "unsupported compute key", expression: "compute.name", wantError: "no such key"},
		{name: "conversion error", expression: "string(int(tags['tag-0']))", wantError: "type conversion error"},
		{name: "cost limit", expression: "tags.all(key, tags[key] == 'value') ? 'group' : ''", wantError: "cost limit exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rules, err := compileMetadataLabelRules([]metadataLabelRule{
				{name: "successful", label: "test.example/first", expression: "'first'"},
				{name: tc.name, label: consts.LabelPlatformInterconnectGroup, expression: tc.expression},
			})
			if !assert.NoError(t, err) {
				t.FailNow()
			}
			labels, err := evaluateMetadataLabels(context.Background(), rules, ComputeMetadata{TagsList: tags})
			if !assert.ErrorContains(t, err, tc.wantError) {
				t.FailNow()
			}
			assert.Contains(t, err.Error(), tc.name)
			assert.Nil(t, labels, "do not return partial labels on error")
		})
	}
}

func TestEvaluateMetadataLabelsBatch(t *testing.T) {
	rules, err := compileMetadataLabelRules([]metadataLabelRule{
		{name: "first", label: "test.example/first", expression: "tags['source']"},
		{name: "second", label: "test.example/second", expression: "tags['source'] + '-second'"},
		{name: "omitted", label: "test.example/omitted", expression: "''"},
	})
	if !assert.NoError(t, err) {
		t.FailNow()
	}
	labels, err := evaluateMetadataLabels(context.Background(), rules, ComputeMetadata{TagsList: []Tag{{Name: "source", Value: "group"}}})
	assert.NoError(t, err)
	assert.Equal(t, map[string]string{
		"test.example/first":  "group",
		"test.example/second": "group-second",
	}, labels)
}

func TestEvaluateMetadataLabelsNonString(t *testing.T) {
	// Bypass the compile-time guard to exercise the defensive runtime check.
	env, err := cel.NewEnv()
	if !assert.NoError(t, err) {
		t.FailNow()
	}
	ast, issues := env.Compile("true")
	if !assert.NoError(t, issues.Err()) {
		t.FailNow()
	}
	program, err := env.Program(ast)
	if !assert.NoError(t, err) {
		t.FailNow()
	}
	labels, err := evaluateMetadataLabels(context.Background(), []compiledMetadataLabelRule{{
		name: "non-string", label: consts.LabelPlatformInterconnectGroup, program: program,
	}}, ComputeMetadata{})
	assert.ErrorContains(t, err, "returned a non-string value")
	assert.Nil(t, labels)
}

func TestBuiltinMetadataLabelRulesRegistered(t *testing.T) {
	rules, err := builtinMetadataLabelRules()
	if !assert.NoError(t, err) {
		t.FailNow()
	}
	assert.NotEmpty(t, rules)
	for _, rule := range rules {
		_, ok := consts.ManagedMetadataLabelKeys[rule.label]
		assert.Truef(t, ok, "built-in rule %q produces label %q that is not registered in consts.ManagedMetadataLabelKeys", rule.name, rule.label)
	}
}

// TestFillNetInterfacePublicIPs tests if IPv6 IPs from imds load balancer are
// properly handled.
func TestFillNetInterfacePublicIPs(t *testing.T) {
	testcases := []struct {
		desc                 string
		publicIPs            []PublicIPMetadata
		netInterface         *NetworkInterface
		expectedNetInterface *NetworkInterface
	}{
		{
			desc: "IPv6/DualStack",
			publicIPs: []PublicIPMetadata{
				{
					FrontendIPAddress: "20.0.0.0",
					PrivateIPAddress:  "10.244.0.0",
				},
				{
					FrontendIPAddress: "[2001::1]",
					PrivateIPAddress:  "[fd00::1]",
				},
			},
			netInterface: &NetworkInterface{
				IPV4: NetworkData{
					IPAddress: []IPAddress{
						{
							PrivateIP: "10.244.0.0",
						},
					},
				},
				IPV6: NetworkData{
					IPAddress: []IPAddress{
						{
							PrivateIP: "fd00::1",
						},
					},
				},
			},
			expectedNetInterface: &NetworkInterface{
				IPV4: NetworkData{
					IPAddress: []IPAddress{
						{
							PrivateIP: "10.244.0.0",
							PublicIP:  "20.0.0.0",
						},
					},
				},
				IPV6: NetworkData{
					IPAddress: []IPAddress{
						{
							PrivateIP: "fd00::1",
							PublicIP:  "2001::1",
						},
					},
				},
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			fillNetInterfacePublicIPs(tc.publicIPs, tc.netInterface)
			assert.Equal(t, tc.expectedNetInterface, tc.netInterface)
		})
	}
}

func TestGetPlatformSubFaultDomain(t *testing.T) {
	for _, testCase := range []struct {
		description string
		nilCompute  bool
		expectedErr error
	}{
		{
			description: "GetPlatformSubFaultDomain should parse the correct platformSubFaultDomain",
		},
		{
			description: "GetPlatformSubFaultDomain should report an error if the compute is nil",
			nilCompute:  true,
			expectedErr: errors.New("failure of getting compute information from instance metadata"),
		},
	} {
		t.Run(testCase.description, func(t *testing.T) {
			cloud := &Cloud{
				Config: config.Config{
					Location:            "eastus",
					UseInstanceMetadata: true,
				},
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Errorf("Test [%s] unexpected error: %v", testCase.description, err)
			}

			respString := `{"compute":{"zone":"1", "platformFaultDomain":"1", "location":"westus", "platformSubFaultDomain": "2"}}`
			if testCase.nilCompute {
				respString = "{}"
			}
			mux := http.NewServeMux()
			mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, respString)
			}))
			go func() {
				_ = http.Serve(listener, mux)
			}()
			defer listener.Close()

			cloud.Metadata, err = NewInstanceMetadataService("http://" + listener.Addr().String() + "/")
			if err != nil {
				t.Errorf("Test [%s] unexpected error: %v", testCase.description, err)
			}

			fd, err := cloud.GetPlatformSubFaultDomain(context.TODO())
			if testCase.expectedErr != nil {
				assert.Equal(t, testCase.expectedErr, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, "2", fd)
			}
		})
	}
}

func TestGetInterconnectGroupID(t *testing.T) {
	testCases := []struct {
		name                string
		useInstanceMetadata bool
		respString          string
		expectedID          string
		expectedErr         bool
	}{
		{
			name:                "InterconnectGroup tag present",
			useInstanceMetadata: true,
			respString:          `{"compute":{"tagsList":[{"name":"Platform_Interconnect_Group","value":"group-123"},{"name":"Other_Tag","value":"other"}]}}`,
			expectedID:          "group-123",
			expectedErr:         false,
		},
		{
			name:                "InterconnectGroup tag absent",
			useInstanceMetadata: true,
			respString:          `{"compute":{"tagsList":[{"name":"Other_Tag","value":"other"}]}}`,
			expectedID:          "",
			expectedErr:         false,
		},
		{
			name:                "InterconnectGroup tag present with empty value",
			useInstanceMetadata: true,
			respString:          `{"compute":{"tagsList":[{"name":"Platform_Interconnect_Group","value":""}]}}`,
			expectedID:          "",
			expectedErr:         false,
		},
		{
			name:                "First duplicate tag wins",
			useInstanceMetadata: true,
			respString:          `{"compute":{"tagsList":[{"name":"Platform_Interconnect_Group","value":"first"},{"name":"Platform_Interconnect_Group","value":"second"}]}}`,
			expectedID:          "first",
		},
		{
			name:                "Empty first duplicate stops search",
			useInstanceMetadata: true,
			respString:          `{"compute":{"tagsList":[{"name":"Platform_Interconnect_Group","value":""},{"name":"Platform_Interconnect_Group","value":"second"}]}}`,
		},
		{
			name:                "Invalid label value",
			useInstanceMetadata: true,
			respString:          `{"compute":{"tagsList":[{"name":"Platform_Interconnect_Group","value":"invalid/value"}]}}`,
			expectedErr:         true,
		},
		{
			name:                "Empty tagsList",
			useInstanceMetadata: true,
			respString:          `{"compute":{"tagsList":[]}}`,
			expectedID:          "",
			expectedErr:         false,
		},
		{
			name:                "Compute metadata is nil",
			useInstanceMetadata: true,
			respString:          `{}`,
			expectedID:          "",
			expectedErr:         true,
		},
		{
			name:                "UseInstanceMetadata false",
			useInstanceMetadata: false,
			respString:          `{"compute":{"tagsList":[{"name":"Platform_Interconnect_Group","value":"group-123"}]}}`,
			expectedID:          "",
			expectedErr:         false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cloud := &Cloud{
				Config: config.Config{
					Location:            "eastus",
					UseInstanceMetadata: tc.useInstanceMetadata,
				},
			}

			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				fmt.Fprint(w, tc.respString)
			}))
			defer server.Close()

			var err error
			cloud.Metadata, err = NewInstanceMetadataService(server.URL + "/")
			assert.NoError(t, err)

			id, err := cloud.GetInterconnectGroupID(context.TODO())

			if tc.expectedErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expectedID, id)
				labels, err := cloud.GetMetadataLabels(context.Background())
				assert.NoError(t, err)
				if tc.expectedID == "" {
					assert.Empty(t, labels)
				} else {
					assert.Equal(t, map[string]string{
						consts.LabelPlatformInterconnectGroup:    tc.expectedID,
						consts.LabelPlatformInterconnectSubgroup: tc.expectedID,
					}, labels)
				}
				if tc.useInstanceMetadata {
					assert.EqualValues(t, 1, requests.Load(), "batch and compatibility API reuse the metadata cache")
				} else {
					assert.Zero(t, requests.Load())
				}
			}
		})
	}
}

func TestGetMetadataLabelsDisabled(t *testing.T) {
	cloud := &Cloud{}
	labels, err := cloud.GetMetadataLabels(context.Background())
	assert.NoError(t, err)
	assert.Empty(t, labels)
}

func TestGetMetadataLabelsInvalidatesMissingCompute(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			fmt.Fprint(w, `{}`)
		} else {
			fmt.Fprint(w, `{"compute":{"tagsList":[{"name":"Platform_Interconnect_Group","value":"group-123"}]}}`)
		}
	}))
	defer server.Close()
	metadata, err := NewInstanceMetadataService(server.URL + "/")
	if !assert.NoError(t, err) {
		t.FailNow()
	}
	cloud := &Cloud{Config: config.Config{UseInstanceMetadata: true}, Metadata: metadata}
	labels, err := cloud.GetMetadataLabels(context.Background())
	assert.EqualError(t, err, "failure of getting compute information from instance metadata")
	assert.Nil(t, labels)
	labels, err = cloud.GetMetadataLabels(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, map[string]string{
		consts.LabelPlatformInterconnectGroup:    "group-123",
		consts.LabelPlatformInterconnectSubgroup: "group-123",
	}, labels)
	assert.EqualValues(t, 2, requests.Load())
}

func TestGetMetadataLabelsAcquisitionError(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		response   string
		disconnect bool
	}{
		{name: "invalid JSON", status: http.StatusOK, response: `invalid-json`},
		{name: "unsupported version", status: http.StatusBadRequest, response: `{"compute":{"tagsList":[{"name":"Platform_Interconnect_Group","value":"legacy"}]}}`},
		{name: "server error", status: http.StatusInternalServerError},
		{name: "request timeout", status: http.StatusRequestTimeout},
		{name: "transport error", disconnect: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				assert.Equal(t, "2025-11-15", r.URL.Query().Get("api-version"))
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.response)
			}))
			defer server.Close()
			if tc.disconnect {
				server.Close()
			}
			metadata, err := NewInstanceMetadataService(server.URL)
			if !assert.NoError(t, err) {
				t.FailNow()
			}
			cloud := &Cloud{Config: config.Config{UseInstanceMetadata: true}, Metadata: metadata}
			labels, err := cloud.GetMetadataLabels(context.Background())
			assert.Error(t, err)
			assert.Nil(t, labels)
			if tc.disconnect {
				assert.Zero(t, requests.Load())
			} else {
				assert.EqualValues(t, 1, requests.Load(), "no fallback request")
			}
		})
	}
}

const (
	testIMDSNodeName        = "vm1"
	testIMDSResourceID      = "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1"
	testIMDSVMSize          = "Standard_D2s_v3"
	testLBMetadataErrPrefix = "load balancer metadata unavailable: "
	testLBEmptyPublicIPBody = `{"loadbalancer":{"publicIpAddresses":[]}}`
)

// fakeIMDS is a local IMDS server that counts instance and load balancer requests.
type fakeIMDS struct {
	*imdstest.Server
}

func newFakeIMDS(t *testing.T, instanceBody string, lbHandler http.HandlerFunc) *fakeIMDS {
	instance := func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, instanceBody)
	}
	// Disable connection reuse so that transport-level retries cannot hide request counts.
	return &fakeIMDS{Server: imdstest.NewServer(t, instance, lbHandler, true)}
}

func (f *fakeIMDS) assertCalls(t *testing.T, instanceCalls, lbCalls int32) {
	t.Helper()
	assert.Equal(t, instanceCalls, f.InstanceCalls(), "instance metadata requests")
	assert.Equal(t, lbCalls, f.LBCalls(), "loadbalancer metadata requests")
}

func newTestIMDSService(t *testing.T, f *fakeIMDS) *InstanceMetadataService {
	ims, err := NewInstanceMetadataService(f.URL)
	if err != nil {
		t.Fatalf("failed to create instance metadata service: %v", err)
	}
	return ims
}

func imdsIPEntry(privateIP, publicIP string) string {
	if privateIP == "" {
		return ""
	}
	return fmt.Sprintf(`{"privateIpAddress":%q,"publicIpAddress":%q}`, privateIP, publicIP)
}

// testIMDSInstanceBody returns instance metadata for the local node with the given addresses.
func testIMDSInstanceBody(ipv4, ipv4Public, ipv6 string) string {
	return fmt.Sprintf(`{"compute":{"name":%q,"vmSize":%q,"subscriptionId":"sub","resourceGroupName":"rg","resourceId":%q},`+
		`"network":{"interface":[{"ipv4":{"ipAddress":[%s]},"ipv6":{"ipAddress":[%s]}}]}}`,
		testIMDSNodeName, testIMDSVMSize, testIMDSResourceID, imdsIPEntry(ipv4, ipv4Public), imdsIPEntry(ipv6, ""))
}

func testIMDSLBBody(frontendIP, privateIP string) string {
	return fmt.Sprintf(`{"loadbalancer":{"publicIpAddresses":[{"frontendIpAddress":%q,"privateIpAddress":%q}]}}`, frontendIP, privateIP)
}

func imdsLBStatus(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
	}
}

func imdsLBBody(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body)
	}
}

// imdsLBConnectionReset closes the connection without writing a response.
func imdsLBConnectionReset(w http.ResponseWriter, _ *http.Request) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	conn, _, err := hijacker.Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

// imdsLBTruncatedBody declares a longer body than it writes, causing a read error on the client.
func imdsLBTruncatedBody(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Length", "100")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "{")
}

// expireMetadataCache marks the cached instance metadata as expired without sleeping.
func expireMetadataCache(t *testing.T, ims *InstanceMetadataService) {
	t.Helper()
	timedCache, ok := ims.imsCache.(*azcache.TimedCache)
	if !ok {
		t.Fatalf("unexpected metadata cache type %T", ims.imsCache)
	}
	obj, exists, err := timedCache.Store.GetByKey(consts.MetadataCacheKey)
	if err != nil || !exists {
		t.Fatalf("metadata cache entry not found: exists=%t, err=%v", exists, err)
	}
	entry := obj.(*azcache.AzureCacheEntry)
	entry.Lock.Lock()
	entry.CreatedOn = entry.CreatedOn.Add(-consts.MetadataCacheTTL - time.Second)
	entry.Lock.Unlock()
}

func TestIsTransientIMDSError(t *testing.T) {
	assert.Equal(t, "failure of getting loadbalancer metadata, status code 503", (&imdsResponseError{statusCode: http.StatusServiceUnavailable}).Error())

	for _, tc := range []struct {
		desc     string
		err      error
		expected bool
	}{
		{desc: "500 should be transient", err: &imdsResponseError{statusCode: http.StatusInternalServerError}, expected: true},
		{desc: "502 should be transient", err: &imdsResponseError{statusCode: http.StatusBadGateway}, expected: true},
		{desc: "503 should be transient", err: &imdsResponseError{statusCode: http.StatusServiceUnavailable}, expected: true},
		{desc: "429 should be transient", err: &imdsResponseError{statusCode: http.StatusTooManyRequests}, expected: true},
		{desc: "408 should be transient", err: &imdsResponseError{statusCode: http.StatusRequestTimeout}, expected: true},
		{desc: "410 should be transient", err: &imdsResponseError{statusCode: http.StatusGone}, expected: true},
		{desc: "HTTP 400 is not classified as transient", err: &imdsResponseError{statusCode: http.StatusBadRequest}, expected: false},
		{desc: "HTTP 403 is not classified as transient", err: &imdsResponseError{statusCode: http.StatusForbidden}, expected: false},
		{desc: "HTTP 404 is not classified as transient", err: &imdsResponseError{statusCode: http.StatusNotFound}, expected: false},
		{desc: "wrapped HTTP 404 is not classified as transient", err: fmt.Errorf("wrapped: %w", &imdsResponseError{statusCode: http.StatusNotFound}), expected: false},
		{desc: "wrapped 503 should be transient", err: fmt.Errorf("wrapped: %w", &imdsResponseError{statusCode: http.StatusServiceUnavailable}), expected: true},
		{desc: "wrapped 410 should be transient", err: fmt.Errorf("wrapped: %w", &imdsResponseError{statusCode: http.StatusGone}), expected: true},
		{desc: "network error should be transient", err: errors.New("connection reset by peer"), expected: true},
		{desc: "read error should be transient", err: io.ErrUnexpectedEOF, expected: true},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			assert.Equal(t, tc.expected, isTransientIMDSError(tc.err))
		})
	}
}

func TestGetMetadataLoadBalancerStatus(t *testing.T) {
	for _, tc := range []struct {
		desc              string
		instanceBody      string
		lbHandler         http.HandlerFunc
		expectedPublicIP  string
		expectedErr       string
		expectedErrPrefix string
		expectedCause     error
		expectedStatus    int
		expectedLBCalls   int32
	}{
		{
			desc:             "healthy loadbalancer metadata should fill the public IP",
			instanceBody:     testIMDSInstanceBody("10.0.0.5", "", ""),
			lbHandler:        imdsLBBody(testIMDSLBBody("20.0.0.9", "10.0.0.5")),
			expectedPublicIP: "20.0.0.9",
			expectedLBCalls:  1,
		},
		{
			desc:            "healthy loadbalancer metadata without public IPs should not set an error",
			instanceBody:    testIMDSInstanceBody("10.0.0.5", "", ""),
			lbHandler:       imdsLBBody(testLBEmptyPublicIPBody),
			expectedLBCalls: 1,
		},
		{
			desc:            "loadbalancer metadata without a loadbalancer profile should not set an error",
			instanceBody:    testIMDSInstanceBody("10.0.0.5", "", ""),
			lbHandler:       imdsLBBody(`{}`),
			expectedLBCalls: 1,
		},
		{
			desc:            "404 should not record a load balancer metadata error",
			instanceBody:    testIMDSInstanceBody("10.0.0.5", "", ""),
			lbHandler:       imdsLBStatus(http.StatusNotFound),
			expectedLBCalls: 1,
		},
		{
			desc:            "400 should not record a load balancer metadata error",
			instanceBody:    testIMDSInstanceBody("10.0.0.5", "", ""),
			lbHandler:       imdsLBStatus(http.StatusBadRequest),
			expectedLBCalls: 1,
		},
		{
			desc:            "500 should record a load balancer metadata error",
			instanceBody:    testIMDSInstanceBody("10.0.0.5", "", ""),
			lbHandler:       imdsLBStatus(http.StatusInternalServerError),
			expectedErr:     testLBMetadataErrPrefix + "failure of getting loadbalancer metadata, status code 500",
			expectedStatus:  http.StatusInternalServerError,
			expectedLBCalls: 1,
		},
		{
			desc:            "503 should record a load balancer metadata error",
			instanceBody:    testIMDSInstanceBody("10.0.0.5", "", ""),
			lbHandler:       imdsLBStatus(http.StatusServiceUnavailable),
			expectedErr:     testLBMetadataErrPrefix + "failure of getting loadbalancer metadata, status code 503",
			expectedStatus:  http.StatusServiceUnavailable,
			expectedLBCalls: 1,
		},
		{
			desc:            "429 should record a load balancer metadata error",
			instanceBody:    testIMDSInstanceBody("10.0.0.5", "", ""),
			lbHandler:       imdsLBStatus(http.StatusTooManyRequests),
			expectedErr:     testLBMetadataErrPrefix + "failure of getting loadbalancer metadata, status code 429",
			expectedStatus:  http.StatusTooManyRequests,
			expectedLBCalls: 1,
		},
		{
			desc:            "408 should record a load balancer metadata error",
			instanceBody:    testIMDSInstanceBody("10.0.0.5", "", ""),
			lbHandler:       imdsLBStatus(http.StatusRequestTimeout),
			expectedErr:     testLBMetadataErrPrefix + "failure of getting loadbalancer metadata, status code 408",
			expectedStatus:  http.StatusRequestTimeout,
			expectedLBCalls: 1,
		},
		{
			desc:            "410 should record a load balancer metadata error",
			instanceBody:    testIMDSInstanceBody("10.0.0.5", "", ""),
			lbHandler:       imdsLBStatus(http.StatusGone),
			expectedErr:     testLBMetadataErrPrefix + "failure of getting loadbalancer metadata, status code 410",
			expectedStatus:  http.StatusGone,
			expectedLBCalls: 1,
		},
		{
			desc:              "network failure should record a load balancer metadata error",
			instanceBody:      testIMDSInstanceBody("10.0.0.5", "", ""),
			lbHandler:         imdsLBConnectionReset,
			expectedErrPrefix: testLBMetadataErrPrefix + "Get ",
			expectedCause:     io.EOF,
			expectedLBCalls:   1,
		},
		{
			desc:            "body read failure should record a load balancer metadata error",
			instanceBody:    testIMDSInstanceBody("10.0.0.5", "", ""),
			lbHandler:       imdsLBTruncatedBody,
			expectedErr:     testLBMetadataErrPrefix + "unexpected EOF",
			expectedCause:   io.ErrUnexpectedEOF,
			expectedLBCalls: 1,
		},
		{
			desc:            "invalid JSON should record a load balancer metadata error",
			instanceBody:    testIMDSInstanceBody("10.0.0.5", "", ""),
			lbHandler:       imdsLBBody("not-json"),
			expectedErr:     testLBMetadataErrPrefix + "invalid character 'o' in literal null (expecting 'u')",
			expectedLBCalls: 1,
		},
		{
			desc:             "public IP in instance metadata should skip loadbalancer metadata",
			instanceBody:     testIMDSInstanceBody("10.0.0.5", "20.0.0.1", ""),
			lbHandler:        imdsLBStatus(http.StatusServiceUnavailable),
			expectedPublicIP: "20.0.0.1",
			expectedLBCalls:  0,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			f := newFakeIMDS(t, tc.instanceBody, tc.lbHandler)
			ims := newTestIMDSService(t, f)

			first, err := ims.GetMetadata(context.Background(), azcache.CacheReadTypeDefault)
			assert.NoError(t, err)
			firstLBErr := first.LBMetadataError
			second, err := ims.GetMetadata(context.Background(), azcache.CacheReadTypeDefault)
			assert.NoError(t, err)
			assert.True(t, first == second, "cached metadata should be returned")

			assert.Equal(t, testIMDSNodeName, first.Compute.Name)
			assert.Equal(t, tc.expectedPublicIP, first.Network.Interface[0].IPV4.IPAddress[0].PublicIP)
			if tc.expectedErr == "" && tc.expectedErrPrefix == "" {
				assert.NoError(t, first.LBMetadataError)
			} else {
				lbErr := firstLBErr
				if tc.expectedErr != "" {
					assert.EqualError(t, lbErr, tc.expectedErr)
				} else {
					assert.Error(t, lbErr)
					if lbErr != nil {
						assert.True(t, strings.HasPrefix(lbErr.Error(), tc.expectedErrPrefix), lbErr.Error())
					}
				}
				assert.True(t, errors.Is(lbErr, providererrors.ErrLoadBalancerMetadataUnavailable))
				if tc.expectedCause != nil {
					assert.True(t, errors.Is(lbErr, tc.expectedCause))
				}
				if tc.expectedStatus != 0 {
					var respErr *imdsResponseError
					assert.True(t, errors.As(lbErr, &respErr))
					if respErr != nil {
						assert.Equal(t, tc.expectedStatus, respErr.statusCode)
					}
				}
				assert.Same(t, firstLBErr, second.LBMetadataError, "cached error should be returned unchanged")
			}
			f.assertCalls(t, 1, tc.expectedLBCalls)
		})
	}
}

func TestGetMetadataLoadBalancerRecoveryAfterCacheExpiry(t *testing.T) {
	staleErr := testLBMetadataErrPrefix + "failure of getting loadbalancer metadata, status code 503"
	for _, tc := range []struct {
		desc             string
		recoveredLB      http.HandlerFunc
		expectedPublicIP string
		expectedErr      string
	}{
		{
			desc:             "recovered loadbalancer metadata should fill the public IP and clear the error",
			recoveredLB:      imdsLBBody(testIMDSLBBody("20.0.0.9", "10.0.0.5")),
			expectedPublicIP: "20.0.0.9",
		},
		{
			desc:        "recovered loadbalancer metadata without public IPs should clear the error",
			recoveredLB: imdsLBBody(testLBEmptyPublicIPBody),
		},
		{
			desc:        "benign loadbalancer response after expiry should clear the error",
			recoveredLB: imdsLBStatus(http.StatusNotFound),
		},
		{
			desc:        "repeated transient failure should record a new error",
			recoveredLB: imdsLBStatus(http.StatusServiceUnavailable),
			expectedErr: staleErr,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			f := newFakeIMDS(t, testIMDSInstanceBody("10.0.0.5", "", ""), imdsLBStatus(http.StatusServiceUnavailable))
			ims := newTestIMDSService(t, f)

			stale, err := ims.GetMetadata(context.Background(), azcache.CacheReadTypeDefault)
			assert.NoError(t, err)
			assert.EqualError(t, stale.LBMetadataError, staleErr)
			staleLBErr := stale.LBMetadataError

			f.SetLoadBalancerHandler(tc.recoveredLB)
			expireMetadataCache(t, ims)

			fresh, err := ims.GetMetadata(context.Background(), azcache.CacheReadTypeDefault)
			assert.NoError(t, err)
			assert.True(t, fresh != stale, "expired metadata should be refreshed")
			assert.Equal(t, tc.expectedPublicIP, fresh.Network.Interface[0].IPV4.IPAddress[0].PublicIP)
			if tc.expectedErr == "" {
				assert.NoError(t, fresh.LBMetadataError)
			} else {
				assert.EqualError(t, fresh.LBMetadataError, tc.expectedErr)
				assert.NotSame(t, staleLBErr, fresh.LBMetadataError, "a new error should be created per cache fill")
			}

			// The previously returned metadata must not be mutated by the refresh.
			assert.Same(t, staleLBErr, stale.LBMetadataError)
			assert.Equal(t, "", stale.Network.Interface[0].IPV4.IPAddress[0].PublicIP)
			f.assertCalls(t, 2, 2)
		})
	}
}

func TestInstanceMetadataJSONOmitsLBMetadataError(t *testing.T) {
	for _, tc := range []struct {
		desc            string
		lbMetadataError error
	}{
		{
			desc: "nil LBMetadataError should not be serialized",
		},
		{
			desc:            "wrapped LBMetadataError should not be serialized",
			lbMetadataError: fmt.Errorf("%w: %w", providererrors.ErrLoadBalancerMetadataUnavailable, &imdsResponseError{statusCode: http.StatusServiceUnavailable}),
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			metadata := &InstanceMetadata{
				Compute:         &ComputeMetadata{Name: "vm1", VMSize: "Standard_D2s_v3"},
				Network:         &NetworkMetadata{Interface: []*NetworkInterface{}},
				LBMetadataError: tc.lbMetadataError,
			}

			data, err := json.Marshal(metadata)
			assert.NoError(t, err)
			assert.JSONEq(t, `{"compute":{"name":"vm1","vmSize":"Standard_D2s_v3"},"network":{"interface":[]}}`, string(data))
		})
	}
}

func TestGetMetadataLabelsInterconnect(t *testing.T) {
	for _, tc := range []struct {
		name         string
		fields       string
		legacy       string
		wantGroup    string
		wantSubgroup string
		wantError    bool
	}{
		{name: "M1 aliases", legacy: "legacy", wantGroup: "legacy", wantSubgroup: "legacy"},
		{name: "M2 both", fields: `"interconnectGroupId":"group","interconnectSubgroupId":"subgroup"`, wantGroup: "group", wantSubgroup: "subgroup"},
		{name: "M2 group only", fields: `"interconnectGroupId":"group"`, wantGroup: "group"},
		{name: "M2 subgroup only", fields: `"interconnectSubgroupId":"subgroup"`, wantSubgroup: "subgroup"},
		{name: "M2 group overrides M1", fields: `"interconnectGroupId":"group"`, legacy: "legacy", wantGroup: "group"},
		{name: "M2 subgroup overrides M1", fields: `"interconnectSubgroupId":"subgroup"`, legacy: "legacy", wantSubgroup: "subgroup"},
		{name: "M2 empty subgroup overrides M1", fields: `"interconnectGroupId":"group","interconnectSubgroupId":""`, legacy: "legacy", wantGroup: "group"},
		{name: "M2 empty group overrides M1", fields: `"interconnectGroupId":"","interconnectSubgroupId":"subgroup"`, legacy: "legacy", wantSubgroup: "subgroup"},
		{name: "M2 conflict wins", fields: `"interconnectGroupId":"group","interconnectSubgroupId":"subgroup"`, legacy: "legacy", wantGroup: "group", wantSubgroup: "subgroup"},
		{name: "M2 ignores invalid M1", fields: `"interconnectGroupId":"group","interconnectSubgroupId":"subgroup"`, legacy: "invalid/value", wantGroup: "group", wantSubgroup: "subgroup"},
		{name: "M2 empty falls back", fields: `"interconnectGroupId":"","interconnectSubgroupId":""`, legacy: "legacy", wantGroup: "legacy", wantSubgroup: "legacy"},
		{name: "M2 group empty falls back", fields: `"interconnectGroupId":""`, legacy: "legacy", wantGroup: "legacy", wantSubgroup: "legacy"},
		{name: "M2 subgroup empty falls back", fields: `"interconnectSubgroupId":""`, legacy: "legacy", wantGroup: "legacy", wantSubgroup: "legacy"},
		{name: "M2 empty without M1", fields: `"interconnectGroupId":"","interconnectSubgroupId":""`},
		{name: "no values"},
		{name: "numeric group", fields: `"interconnectGroupId":123`, legacy: "legacy", wantError: true},
		{name: "boolean subgroup", fields: `"interconnectGroupId":"group","interconnectSubgroupId":true`, legacy: "legacy", wantError: true},
		{name: "object group", fields: `"interconnectGroupId":{}`, legacy: "legacy", wantError: true},
		{name: "array subgroup", fields: `"interconnectSubgroupId":[]`, legacy: "legacy", wantError: true},
		{name: "null group", fields: `"interconnectGroupId":null`, legacy: "legacy", wantError: true},
		{name: "null subgroup", fields: `"interconnectGroupId":"group","interconnectSubgroupId":null`, legacy: "legacy", wantError: true},
		{name: "invalid group", fields: `"interconnectGroupId":"invalid/value"`, legacy: "legacy", wantError: true},
		{name: "invalid subgroup after valid group", fields: `"interconnectGroupId":"group","interconnectSubgroupId":"invalid/value"`, legacy: "legacy", wantError: true},
		{name: "long subgroup", fields: fmt.Sprintf(`"interconnectSubgroupId":%q`, strings.Repeat("a", 64)), legacy: "legacy", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				assert.Equal(t, "/metadata/instance", r.URL.Path)
				assert.Equal(t, "2025-11-15", r.URL.Query().Get("api-version"))
				assert.Equal(t, "json", r.URL.Query().Get("format"))
				assert.Equal(t, "True", r.Header.Get("Metadata"))
				compute := tc.fields
				if tc.legacy != "" {
					if compute != "" {
						compute += ","
					}
					compute += fmt.Sprintf(`"tagsList":[{"name":"Platform_Interconnect_Group","value":%q}]`, tc.legacy)
				}
				fmt.Fprintf(w, `{"compute":{%s}}`, compute)
			}))
			defer server.Close()
			metadata, err := NewInstanceMetadataService(server.URL)
			if !assert.NoError(t, err) {
				t.FailNow()
			}
			cloud := &Cloud{Config: config.Config{UseInstanceMetadata: true}, Metadata: metadata}
			labels, err := cloud.GetMetadataLabels(context.Background())
			if tc.wantError {
				assert.Error(t, err)
				assert.Nil(t, labels, "never return fallback or partial labels on error")
			} else {
				assert.NoError(t, err)
				want := map[string]string{}
				if tc.wantGroup != "" {
					want[consts.LabelPlatformInterconnectGroup] = tc.wantGroup
				}
				if tc.wantSubgroup != "" {
					want[consts.LabelPlatformInterconnectSubgroup] = tc.wantSubgroup
				}
				assert.Equal(t, want, labels)
				group, err := cloud.GetInterconnectGroupID(context.Background())
				assert.NoError(t, err)
				assert.Equal(t, tc.wantGroup, group)
			}
			assert.EqualValues(t, 1, requests.Load(), "labels and compatibility getter share one snapshot")
		})
	}
}
