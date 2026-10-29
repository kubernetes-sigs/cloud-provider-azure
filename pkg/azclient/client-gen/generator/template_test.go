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

package generator

import (
	"bytes"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestETagTestTemplate(t *testing.T) {
	for _, tt := range []struct {
		resource string
		etag     bool
	}{
		{resource: "PublicIPAddress", etag: true},
		{resource: "LoadBalancer", etag: true},
		{resource: "PublicIPAddress", etag: false},
	} {
		name := tt.resource
		if tt.etag {
			name += "/etag"
		}

		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			err := TestCaseTemplate.Execute(&out, ClientGenConfig{
				Resource:     tt.resource,
				PackageAlias: "armnetwork",
				ClientName:   tt.resource + "Client",
				Verbs:        []string{"get", "createorupdate", "delete", "list"},
				Etag:         tt.etag,
				Expand:       true,
			})
			if err != nil {
				t.Fatal(err)
			}
			source := out.String()
			if _, err := parser.ParseFile(token.NewFileSet(), "generated_test.go", "package generated\n"+source, parser.AllErrors); err != nil {
				t.Fatal(err)
			}
			wantStale := tt.resource == "PublicIPAddress" && tt.etag
			for _, fragment := range []string{
				"staleETag := *resource.Etag",
				"resource.Properties.IdleTimeoutInMinutes = to.Ptr(idleTimeout)",
				"if recorder.IsNewCassette()",
				"updated.Etag = to.Ptr(staleETag)",
				"updated.Properties.IdleTimeoutInMinutes = to.Ptr(idleTimeout + 1)",
				"errors.As(err, &responseError)",
				"http.StatusPreconditionFailed",
			} {
				if strings.Contains(source, fragment) != wantStale {
					t.Errorf("presence of %q should be %t", fragment, wantStale)
				}
			}
			wantInvalid := tt.etag && !wantStale
			if strings.Contains(source, `to.Ptr("invalid")`) != wantInvalid {
				t.Errorf("malformed ETag scenario should be present: %t", wantInvalid)
			}
		})
	}
}

func TestServiceGatewayChinaSuiteTemplate(t *testing.T) {
	for _, resource := range []string{"ServiceGateway", "PublicIPAddress", "VirtualNetworkLink"} {
		t.Run(resource, func(t *testing.T) {
			var out bytes.Buffer
			if err := TestSuiteTemplate.Execute(&out, ClientGenConfig{Resource: resource}); err != nil {
				t.Fatal(err)
			}
			source := out.String()
			if _, err := parser.ParseFile(token.NewFileSet(), "suite_test.go", "package generated\n"+source, parser.AllErrors); err != nil {
				t.Fatal(err)
			}
			wantSkip := resource == "ServiceGateway"
			for _, fragment := range []string{
				`if os.Getenv("AZURE_CLOUD") == "AZURECHINACLOUD"`,
				"t.Skip(",
				"Microsoft.Network/AllowServiceGateways",
			} {
				if strings.Contains(source, fragment) != wantSkip {
					t.Errorf("presence of %q should be %t", fragment, wantSkip)
				}
			}
			if wantSkip && strings.Index(source, "t.Skip(") > strings.Index(source, "ginkgo.RunSpecs(") {
				t.Error("China suite must be skipped before running Ginkgo setup or cleanup")
			}
		})
	}
}
