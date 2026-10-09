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

package node

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	azureprovider "sigs.k8s.io/cloud-provider-azure/pkg/provider"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
)

func TestGetMetadataLabelsIMDS(t *testing.T) {
	for _, tc := range []struct {
		name         string
		response     string
		wantGroup    string
		wantSubgroup string
		wantError    bool
	}{
		{name: "M1 aliases", response: `{"compute":{"tagsList":[{"name":"Platform_Interconnect_Group","value":"group-123"}]}}`, wantGroup: "group-123", wantSubgroup: "group-123"},
		{name: "M2 labels", response: `{"compute":{"interconnectGroupId":"group-123","interconnectSubgroupId":"subgroup-123"}}`, wantGroup: "group-123", wantSubgroup: "subgroup-123"},
		{name: "M2 subgroup only", response: `{"compute":{"interconnectSubgroupId":"subgroup-123","tagsList":[{"name":"Platform_Interconnect_Group","value":"legacy"}]}}`, wantSubgroup: "subgroup-123"},
		{name: "metadata error", response: `{}`, wantError: true},
		{name: "evaluation error", response: `{"compute":{"tagsList":[{"name":"Platform_Interconnect_Group","value":"invalid/value"}]}}`, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, tc.response)
			}))
			defer server.Close()
			metadata, err := azureprovider.NewInstanceMetadataService(server.URL + "/")
			if !assert.NoError(t, err) {
				t.FailNow()
			}
			np := &IMDSNodeProvider{azure: &azureprovider.Cloud{
				Config: config.Config{UseInstanceMetadata: true}, Metadata: metadata,
			}}
			labels, err := np.GetMetadataLabels(context.Background())
			if tc.wantError {
				assert.Error(t, err)
				assert.Nil(t, labels)
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
				id, err := np.GetInterconnectGroupID(context.Background())
				assert.NoError(t, err)
				assert.Equal(t, tc.wantGroup, id)
			}
		})
	}
}

func TestGetMetadataLabelsARM(t *testing.T) {
	np := &ARMNodeProvider{}
	labels, err := np.GetMetadataLabels(context.Background())
	assert.NoError(t, err)
	assert.Empty(t, labels)
	id, err := np.GetInterconnectGroupID(context.Background())
	assert.NoError(t, err)
	assert.Empty(t, id)
}
