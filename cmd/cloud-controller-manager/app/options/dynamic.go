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

package options

import (
	"github.com/spf13/pflag"

	app "sigs.k8s.io/cloud-provider-azure/cmd/cloud-controller-manager/app/config"
)

// DynamicReloadingOptions holds the configurations of the dynamic reloading logics
type DynamicReloadingOptions struct {
	EnableDynamicReloading     bool
	CloudConfigSecretName      string
	CloudConfigSecretNamespace string
	CloudConfigKey             string
}

// AddFlags adds flags related to dynamic reloading for controller manager to the specified FlagSet
func (o *DynamicReloadingOptions) AddFlags(fs *pflag.FlagSet) {
	if o == nil {
		return
	}

	fs.BoolVar(&o.EnableDynamicReloading, "enable-dynamic-reloading", false, "Enable re-configuring cloud controller manager from secret without restarting.")
	fs.StringVar(&o.CloudConfigSecretName, "cloud-config-secret-name", "", "The name of the cloud config secret.")
	fs.StringVar(&o.CloudConfigSecretNamespace, "cloud-config-secret-namespace", "kube-system", "The k8s namespace of the cloud config secret, default to 'kube-system'.")
	fs.StringVar(&o.CloudConfigKey, "cloud-config-key", "cloud-config", "The key of the config data in the cloud config secret, default to 'cloud-config'.")
}

// ApplyTo fills up dynamic reloading config with options
func (o *DynamicReloadingOptions) ApplyTo(cfg *app.DynamicReloadingConfig) error {
	if o == nil {
		return nil
	}

	cfg.EnableDynamicReloading = o.EnableDynamicReloading
	cfg.CloudConfigSecretName = o.CloudConfigSecretName
	cfg.CloudConfigSecretNamespace = o.CloudConfigSecretNamespace
	cfg.CloudConfigKey = o.CloudConfigKey

	return nil
}

// Validate checks validation of DynamicReloadingOptions
func (o *DynamicReloadingOptions) Validate() []error {
	return nil
}

func defaultDynamicReloadingOptions() *DynamicReloadingOptions {
	return &DynamicReloadingOptions{
		EnableDynamicReloading:     false,
		CloudConfigSecretName:      "azure-cloud-provider",
		CloudConfigSecretNamespace: "kube-system",
		CloudConfigKey:             "",
	}
}
