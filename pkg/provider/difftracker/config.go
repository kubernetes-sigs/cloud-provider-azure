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

package difftracker

import "fmt"

// Config holds the configuration values needed by DiffTracker
// to perform Azure operations without depending on the entire AzureCloud struct
// This allows DiffTracker to be more modular and testable
type Config struct {
	// Azure subscription ID
	SubscriptionID string

	// Azure resource group name
	ResourceGroup string

	// Azure location/region (e.g. "eastus2"). Distinct from the difftracker
	// Location type, which identifies a node by IP.
	Location string

	// Service Gateway resource name
	ServiceGatewayResourceName string

	// Full Service Gateway resource ID
	ServiceGatewayID string

	// Virtual Network name (required for backend pool configuration)
	VNetName string
}

// Validate checks if the configuration has all required fields
func (c *Config) Validate() error {
	if c.SubscriptionID == "" {
		return fmt.Errorf("config validation failed: SubscriptionID is required")
	}
	if c.ResourceGroup == "" {
		return fmt.Errorf("config validation failed: ResourceGroup is required")
	}
	if c.Location == "" {
		return fmt.Errorf("config validation failed: Location is required")
	}
	if c.ServiceGatewayResourceName == "" {
		return fmt.Errorf("config validation failed: ServiceGatewayResourceName is required")
	}
	if c.ServiceGatewayID == "" {
		return fmt.Errorf("config validation failed: ServiceGatewayID is required")
	}
	if c.VNetName == "" {
		return fmt.Errorf("config validation failed: VNetName is required")
	}
	return nil
}
