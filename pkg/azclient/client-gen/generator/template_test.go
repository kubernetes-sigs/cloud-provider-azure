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
	"strings"
	"testing"
)

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
