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
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"

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
