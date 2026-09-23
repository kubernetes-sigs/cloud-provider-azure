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
		name         string
		etag         bool
		skipEtagTest bool
	}{
		{name: "generated coverage", etag: true},
		{name: "custom coverage", etag: true, skipEtagTest: true},
		{name: "no ETag policy"},
		{name: "skip without ETag policy", skipEtagTest: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := ClientGenConfig{
				Verbs:        []string{"get", "createorupdate"},
				Resource:     "Interface",
				PackageAlias: "armnetwork",
				ClientName:   "InterfacesClient",
				Etag:         test.etag,
				SkipEtagTest: test.skipEtagTest,
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
			wantEtagTest := test.etag && !test.skipEtagTest
			if got := strings.Contains(tests.String(), `newResource.Etag = to.Ptr("invalid")`); got != wantEtagTest {
				t.Errorf("generic ETag test generated = %t, want %t", got, wantEtagTest)
			}
			if !strings.Contains(tests.String(), `ginkgo.When("update requests are raised"`) {
				t.Error("normal update coverage was omitted")
			}
		})
	}
}
