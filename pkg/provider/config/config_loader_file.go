/*
Copyright 2020 The Kubernetes Authors.

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
	"context"
	"io/ioutil"
	"strings"
)

func NewFileConfigLoader(loader ConfigLoader, configPath string) ConfigLoader {
	return &FileConfigLoader{
		ConfigLoader: loader,
		configPath:   configPath,
	}
}

// FileConfigLoader returns a parsed configuration for an Azure cloudprovider config file
type FileConfigLoader struct {
	ConfigLoader
	configPath string
}

func (loader *FileConfigLoader) LoadConfig(ctx context.Context) (*Config, error) {
	config, err := loader.ConfigLoader.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	if len(loader.configPath) == 0 {
		return config, nil
	}

	configContents, err := ioutil.ReadFile(loader.configPath)
	if err != nil {
		return nil, err
	}

	config, err = loader.ConfigLoader.ParseConfig(ctx, configContents)
	if err != nil {
		return nil, err
	}
	// The resource group name may be in different cases from different Azure APIs, hence it is converted to lower here.
	// See more context at https://github.com/kubernetes/kubernetes/issues/71994.
	config.ResourceGroup = strings.ToLower(config.ResourceGroup)
	return config, nil
}
