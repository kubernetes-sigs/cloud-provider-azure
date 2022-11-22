package config

import (
	"context"
	"fmt"
	"sigs.k8s.io/yaml"
)

// The config type for Azure cloud provider secret. Supported values are:
// * file   : The values are read from local cloud-config file.
// * secret : The values from secret would override all configures from local cloud-config file.
// * merge  : The values from secret would override only configurations that are explicitly set in the secret. This is the default value.
type cloudConfigType string

const (
	cloudConfigTypeFile   cloudConfigType = "file"
	cloudConfigTypeSecret cloudConfigType = "secret"
	cloudConfigTypeMerge  cloudConfigType = "merge"
)

type ConfigLoader interface {
	LoadConfig(ctx context.Context) (*Config, error)
	ParseConfig(ctx context.Context, content []byte) (*Config, error)
}

type BaseConfigLoader struct {
	Config *Config
}

func (loader *BaseConfigLoader) ParseConfig(ctx context.Context, content []byte) (*Config, error) {
	err := yaml.Unmarshal(content, loader.Config)
	if err != nil {
		return nil, fmt.Errorf("failed to parse Azure cloud-config: %w", err)
	}

	return loader.Config, nil
}

func (loader *BaseConfigLoader) LoadConfig(ctx context.Context) (*Config, error) {
	return loader.Config, nil
}
