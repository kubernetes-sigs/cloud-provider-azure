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
	"fmt"
	"net"
	"strings"

	"sigs.k8s.io/cloud-provider-azure/pkg/credentialprovider"
)

const (
	// Flag names for identity bindings configuration
	FlagIBSNIName       = "ib-sni-name"
	FlagIBDefaultClient = "ib-default-client-id"
	FlagIBDefaultTenant = "ib-default-tenant-id"
	FlagIBAPIIP         = "ib-apiserver-ip"
)

// ParseIdentityBindingsConfig parses and validates identity bindings configuration from individual parameters
func ParseIdentityBindingsConfig(sniName, defaultClientID, defaultTenantID, apiServerIP string) (credentialprovider.IdentityBindingsConfig, error) {

	// Validate SNI name
	if sniName != "" {
		if strings.HasPrefix(sniName, "https://") || strings.HasPrefix(sniName, "http://") {
			return credentialprovider.IdentityBindingsConfig{}, fmt.Errorf("--%s must not contain protocol prefix (https:// or http://), got: %s",
				FlagIBSNIName, sniName)
		}
		if apiServerIP == "" {
			return credentialprovider.IdentityBindingsConfig{}, fmt.Errorf("--%s must be set when --%s is provided", FlagIBAPIIP, FlagIBSNIName)
		}
	}

	// Validate client ID requires SNI name
	if defaultClientID != "" && sniName == "" {
		return credentialprovider.IdentityBindingsConfig{}, fmt.Errorf("--%s must be set when --%s is provided", FlagIBSNIName, FlagIBDefaultClient)
	}

	// Validate tenant ID requires SNI name
	if defaultTenantID != "" && sniName == "" {
		return credentialprovider.IdentityBindingsConfig{}, fmt.Errorf("--%s must be set when --%s is provided", FlagIBSNIName, FlagIBDefaultTenant)
	}

	// Validate API server IP
	if apiServerIP != "" {
		if net.ParseIP(apiServerIP) == nil {
			return credentialprovider.IdentityBindingsConfig{}, fmt.Errorf("--%s must be a valid IP address, got: %s",
				FlagIBAPIIP, apiServerIP)
		}
		if sniName == "" {
			return credentialprovider.IdentityBindingsConfig{}, fmt.Errorf("--%s must be set when --%s is provided", FlagIBSNIName, FlagIBAPIIP)
		}
	}

	return credentialprovider.IdentityBindingsConfig{
		SNIName:         sniName,
		DefaultClientID: defaultClientID,
		DefaultTenantID: defaultTenantID,
		APIServerIP:     apiServerIP,
	}, nil
}
