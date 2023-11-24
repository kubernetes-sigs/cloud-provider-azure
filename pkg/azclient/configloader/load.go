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

package configloader

import (
	"context"
	"fmt"
	"strings"

	clientset "k8s.io/client-go/kubernetes"
)

type decoderFactory[Type any] func(content []byte, loader ConfigLoader[Type]) ConfigLoader[Type]
type K8sSecretLoaderConfig struct {
	K8sSecretConfig
	KubeClient clientset.Interface
}

type FileLoaderConfig struct {
	FilePath string
}

// The config type for Azure cloud provider secret. Supported values are:
// * file   : The values are read from local cloud-config file.
// * secret : The values from secret would override all configures from local cloud-config file.
// * merge  : The values from secret would override only configurations that are explicitly set in the secret. This is the default value.
type CloudConfigType string

const (
	CloudConfigTypeFile   CloudConfigType = "file"
	CloudConfigTypeSecret CloudConfigType = "secret"
	CloudConfigTypeMerge  CloudConfigType = "merge"
)

type ConfigMergeConfig struct {
	// The cloud configure type for Azure cloud provider. Supported values are file, secret and merge.
	CloudConfigType CloudConfigType `json:"cloudConfigType,omitempty" yaml:"cloudConfigType,omitempty"`
}

func Load[Type any](ctx context.Context, secretLoaderConfig *K8sSecretLoaderConfig, fileLoaderConfig *FileLoaderConfig) (*Type, error) {

	if fileLoaderConfig == nil {
		return nil, fmt.Errorf("config file load config is nil")
	}
	//by default the config load type  is merge
	loadConfigloader := NewEmptyLoader[ConfigMergeConfig](&ConfigMergeConfig{CloudConfigType: CloudConfigTypeMerge})
	loadConfigloader = NewFileLoader(fileLoaderConfig.FilePath, loadConfigloader, NewYamlByteLoader[ConfigMergeConfig])
	loadConfig, err := loadConfigloader.Load(ctx)
	if err != nil {
		return nil, err
	}

	configloader := NewFileLoader(fileLoaderConfig.FilePath, nil, NewYamlByteLoader[Type])
	if strings.EqualFold(string(loadConfig.CloudConfigType), string(CloudConfigTypeFile)) {
		return configloader.Load(ctx)
	}

	if secretLoaderConfig == nil {
		return nil, fmt.Errorf("secret load config is nil")
	}

	if strings.EqualFold(string(loadConfig.CloudConfigType), string(CloudConfigTypeSecret)) {
		configloader = NewK8sSecretLoader(&secretLoaderConfig.K8sSecretConfig, secretLoaderConfig.KubeClient, nil, NewYamlByteLoader[Type])
		return configloader.Load(ctx)
	}
	configloader = NewK8sSecretLoader(&secretLoaderConfig.K8sSecretConfig, secretLoaderConfig.KubeClient, configloader, NewYamlByteLoader[Type])
	return configloader.Load(ctx)
}
