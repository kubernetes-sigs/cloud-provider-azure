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

package azclient

import (
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

func TestNewAuthProvider(t *testing.T) {
	type args struct {
		armConfig          *ARMClientConfig
		config             *AzureAuthConfig
		clientOptionsMutFn []func(option *policy.ClientOptions)
	}
	tests := []struct {
		name    string
		args    args
		want    *AuthProvider
		wantErr bool
	}{
		{
			name: "no-config",
			args: args{
				armConfig: nil,
			},
			wantErr: true,
		},
		{
			name: "sp-password",
			args: args{
				armConfig: &ARMClientConfig{
					TenantID: "tenantID",
				},
				config: &AzureAuthConfig{
					AADClientID:     "aadClientID",
					AADClientSecret: "aadClientSecret",
				},
			},
			wantErr: false,
		},
		{
			name: "wrongconfig-msi",
			args: args{
				armConfig: &ARMClientConfig{
					TenantID: "tenantID",
				},
				config: &AzureAuthConfig{
					AADClientID:     "aadClientID",
					AADClientSecret: "msi",
				},
			},
			wantErr: true,
		},
		{
			name: "msi",
			args: args{
				armConfig: &ARMClientConfig{
					TenantID: "tenantID",
				},
				config: &AzureAuthConfig{
					AADClientID:                 "aadClientID",
					AADClientSecret:             "msi",
					UseManagedIdentityExtension: true,
				},
			},
			wantErr: false,
		},
		{
			name: "multi-tenant",
			args: args{
				armConfig: &ARMClientConfig{
					TenantID:                "tenantID",
					NetworkResourceTenantID: "networkResourceTenantID",
				},
				config: &AzureAuthConfig{
					AADClientID:     "aadClientID",
					AADClientSecret: "aadClientSecret",
				},
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewAuthProvider(tt.args.armConfig, tt.args.config, tt.args.clientOptionsMutFn...)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewAuthProvider() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
		})
	}
}
