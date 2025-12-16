package difftracker

import "fmt"

// Config holds the configuration values needed by DiffTracker
// to perform Azure operations without depending on the full Cloud provider
type Config struct {
	// Azure subscription ID
	SubscriptionID string

	// Azure resource group name
	ResourceGroup string

	// Azure location/region
	Location string

	// Kubernetes cluster name
	ClusterName string

	// Service Gateway resource name
	ServiceGatewayResourceName string

	// Full Service Gateway resource ID
	ServiceGatewayID string
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
	if c.ClusterName == "" {
		return fmt.Errorf("config validation failed: ClusterName is required")
	}
	if c.ServiceGatewayResourceName == "" {
		return fmt.Errorf("config validation failed: ServiceGatewayResourceName is required")
	}
	if c.ServiceGatewayID == "" {
		return fmt.Errorf("config validation failed: ServiceGatewayID is required")
	}
	return nil
}
