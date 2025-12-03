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

package config

import (
	"strings"
	"testing"

	"sigs.k8s.io/cloud-provider-azure/pkg/credentialprovider"
)

func TestParseIdentityBindingsConfig(t *testing.T) {
	tests := []struct {
		name            string
		sniName         string
		defaultClientID string
		defaultTenantID string
		apiServerIP     string
		wantConfig      credentialprovider.IdentityBindingsConfig
		wantErr         bool
		errContains     string
	}{
		{
			name:       "empty config",
			wantConfig: credentialprovider.IdentityBindingsConfig{},
			wantErr:    false,
		},
		{
			name:            "valid config with all fields",
			sniName:         "api.example.com",
			defaultClientID: "client-123",
			defaultTenantID: "tenant-456",
			apiServerIP:     "10.0.0.1",
			wantConfig: credentialprovider.IdentityBindingsConfig{
				SNIName:         "api.example.com",
				DefaultClientID: "client-123",
				DefaultTenantID: "tenant-456",
				APIServerIP:     "10.0.0.1",
			},
			wantErr: false,
		},
		{
			name:        "valid config with SNI name and API server IP only",
			sniName:     "api.example.com",
			apiServerIP: "10.0.0.1",
			wantConfig: credentialprovider.IdentityBindingsConfig{
				SNIName:     "api.example.com",
				APIServerIP: "10.0.0.1",
			},
			wantErr: false,
		},
		{
			name:        "SNI name with https:// prefix",
			sniName:     "https://api.example.com",
			apiServerIP: "10.0.0.1",
			wantErr:     true,
			errContains: "must not contain protocol prefix",
		},
		{
			name:        "SNI name with http:// prefix",
			sniName:     "http://api.example.com",
			apiServerIP: "10.0.0.1",
			wantErr:     true,
			errContains: "must not contain protocol prefix",
		},
		{
			name:        "SNI name without API server IP",
			sniName:     "api.example.com",
			wantErr:     true,
			errContains: "ib-apiserver-ip must be set",
		},
		{
			name:        "API server IP without SNI name",
			apiServerIP: "10.0.0.1",
			wantErr:     true,
			errContains: "ib-sni-name must be set",
		},
		{
			name:            "client ID without SNI name",
			defaultClientID: "client-123",
			wantErr:         true,
			errContains:     "ib-sni-name must be set",
		},
		{
			name:            "tenant ID without SNI name",
			defaultTenantID: "tenant-456",
			wantErr:         true,
			errContains:     "ib-sni-name must be set",
		},
		{
			name:        "invalid API server IP - hostname",
			sniName:     "api.example.com",
			apiServerIP: "invalid-hostname",
			wantErr:     true,
			errContains: "must be a valid IP address",
		},
		{
			name:        "invalid API server IP - malformed",
			sniName:     "api.example.com",
			apiServerIP: "999.999.999.999",
			wantErr:     true,
			errContains: "must be a valid IP address",
		},
		{
			name:        "valid IPv6 address",
			sniName:     "api.example.com",
			apiServerIP: "2001:db8::1",
			wantConfig: credentialprovider.IdentityBindingsConfig{
				SNIName:     "api.example.com",
				APIServerIP: "2001:db8::1",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotConfig, err := ParseIdentityBindingsConfig(tt.sniName, tt.defaultClientID, tt.defaultTenantID, tt.apiServerIP)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseIdentityBindingsConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				if err == nil {
					t.Errorf("ParseIdentityBindingsConfig() expected error containing %q, got nil", tt.errContains)
				} else if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("ParseIdentityBindingsConfig() error = %v, want error containing %q", err, tt.errContains)
				}
				return
			}
			if gotConfig.SNIName != tt.wantConfig.SNIName {
				t.Errorf("ParseIdentityBindingsConfig() SNIName = %v, want %v", gotConfig.SNIName, tt.wantConfig.SNIName)
			}
			if gotConfig.DefaultClientID != tt.wantConfig.DefaultClientID {
				t.Errorf("ParseIdentityBindingsConfig() DefaultClientID = %v, want %v", gotConfig.DefaultClientID, tt.wantConfig.DefaultClientID)
			}
			if gotConfig.DefaultTenantID != tt.wantConfig.DefaultTenantID {
				t.Errorf("ParseIdentityBindingsConfig() DefaultTenantID = %v, want %v", gotConfig.DefaultTenantID, tt.wantConfig.DefaultTenantID)
			}
			if gotConfig.APIServerIP != tt.wantConfig.APIServerIP {
				t.Errorf("ParseIdentityBindingsConfig() APIServerIP = %v, want %v", gotConfig.APIServerIP, tt.wantConfig.APIServerIP)
			}
		})
	}
}
