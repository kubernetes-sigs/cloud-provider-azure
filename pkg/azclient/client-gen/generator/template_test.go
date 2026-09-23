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
			if strings.Contains(source, `req.Raw().Header.Get("If-Match")`) != tt.etag {
				t.Errorf("ETag header scenario should be present: %t", tt.etag)
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

func TestEtagTestGeneration(t *testing.T) {
	for _, test := range []struct {
		name  string
		etag  bool
		verbs []string
	}{
		{name: "conditional writes", etag: true, verbs: []string{"get", "createorupdate"}},
		{name: "unconditional writes", verbs: []string{"get", "createorupdate"}},
		{name: "read-only client", etag: true, verbs: []string{"get"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := ClientGenConfig{
				Verbs:        test.verbs,
				Resource:     "Interface",
				PackageAlias: "armnetwork",
				ClientName:   "InterfacesClient",
				Etag:         test.etag,
			}
			var client bytes.Buffer
			if err := ClientFactoryTemplate.Execute(&client, config); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(client.String(), "etag.AppendEtag"); got != test.etag {
				t.Errorf("ETag policy generated = %t, want %t", got, test.etag)
			}

			var tests bytes.Buffer
			if err := TestCaseTemplate.Execute(&tests, config); err != nil {
				t.Fatal(err)
			}
			hasWrites := false
			for _, verb := range test.verbs {
				hasWrites = hasWrites || verb == "createorupdate"
			}
			wantEtagTest := test.etag && hasWrites
			for _, assertion := range []string{
				`req.Raw().Header.Get("If-Match")`,
				`errors.Is(err, intercepted)`,
				`&fake.TokenCredential{}`,
			} {
				if got := strings.Contains(tests.String(), assertion); got != wantEtagTest {
					t.Errorf("generated %q = %t, want %t", assertion, got, wantEtagTest)
				}
			}
			if strings.Contains(tests.String(), "newResource.Etag =") {
				t.Error("ETag test must not mutate the shared resource fixture")
			}
			if got := strings.Contains(tests.String(), `ginkgo.When("update requests are raised"`); got != hasWrites {
				t.Errorf("normal update coverage generated = %t, want %t", got, hasWrites)
			}
		})
	}
}
