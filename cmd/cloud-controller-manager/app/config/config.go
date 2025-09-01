/*
Copyright 2018 The Kubernetes Authors.

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

package app

import (
	apiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	cloudprovider "k8s.io/cloud-provider"
	ccmconfig "k8s.io/cloud-provider/config"

	nodeipamconfig "sigs.k8s.io/cloud-provider-azure/pkg/nodeipam/config"
)

// Config is the main context object for the cloud controller manager.
type Config struct {
	ComponentConfig ccmconfig.CloudControllerManagerConfiguration

	SecureServing *apiserver.SecureServingInfo
	// LoopbackClientConfig is a config for a privileged loopback connection
	LoopbackClientConfig *restclient.Config

	Authentication apiserver.AuthenticationInfo
	Authorization  apiserver.AuthorizationInfo

	NodeIPAMControllerConfig nodeipamconfig.NodeIPAMControllerConfiguration

	// the general kube client
	Client *clientset.Clientset

	// the rest config for the master
	Kubeconfig *restclient.Config

	// the event sink
	EventRecorder record.EventRecorder

	// ClientBuilder will provide a client for this controller to use
	ClientBuilder cloudprovider.ControllerClientBuilder

	// VersionedClient will provide a client for informers
	VersionedClient clientset.Interface

	// SharedInformers gives access to informers for the controller.
	SharedInformers informers.SharedInformerFactory

	DynamicReloadingConfig DynamicReloadingConfig

	// Node filtering configuration
	NodeFilteringConfig NodeFilteringConfig
}

type DynamicReloadingConfig struct {
	EnableDynamicReloading     bool
	CloudConfigSecretName      string
	CloudConfigSecretNamespace string
	CloudConfigKey             string
}

// NodeFilteringConfig contains node filtering configuration
type NodeFilteringConfig struct {
	EnableNodeFiltering bool
	NodeLabelSelector   string
	NodeExcludeLabels   string
}

type completedConfig struct {
	*Config
}

// CompletedConfig same as Config, just to swap private object.
type CompletedConfig struct {
	// Embed a private pointer that cannot be instantiated outside of this package.
	*completedConfig
}

// Complete fills in any fields not set that are required to have valid data. It's mutating the receiver.
func (c *Config) Complete() *CompletedConfig {
	cc := completedConfig{c}

	apiserver.AuthorizeClientBearerToken(c.LoopbackClientConfig, &c.Authentication, &c.Authorization)

	return &CompletedConfig{&cc}
}
