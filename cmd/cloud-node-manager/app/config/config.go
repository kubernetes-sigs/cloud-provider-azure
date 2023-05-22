/*
Copyright 2019 The Kubernetes Authors.

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	cloudprovider "k8s.io/cloud-provider"
	componentbaseconfig "k8s.io/component-base/config"
)

// Config is the main context object for the cloud node manager.
type Config struct {
	// the node name
	NodeName string

	// the path to the config file (azure.json) for use in ARM mode.
	CloudConfigFilePath string

	// NodeStatusUpdateFrequency is the frequency at which the controller updates nodes' status
	NodeStatusUpdateFrequency metav1.Duration

	SecureServing *apiserver.SecureServingInfo
	// LoopbackClientConfig is a config for a privileged loopback connection
	LoopbackClientConfig *restclient.Config

	Authentication apiserver.AuthenticationInfo
	Authorization  apiserver.AuthorizationInfo

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

	// minResyncPeriod is the resync period in reflectors; will be random between
	// minResyncPeriod and 2*minResyncPeriod.
	MinResyncPeriod metav1.Duration

	// ClientConnection specifies the kubeconfig file and client connection
	// settings for the proxy server to use when communicating with the apiserver.
	ClientConnection componentbaseconfig.ClientConnectionConfiguration

	// WaitForRoutes indicates whether the node should wait for routes to be created on Azure.
	// If true, the node condition "NodeNetworkUnavailable" would be set to true on initialization.
	WaitForRoutes bool

	// Specifies if node information is retrieved via IMDS or ARM.
	UseInstanceMetadata bool

	// WindowsService should be set to true if cloud-node-manager is running as a service on Windows.
	// Its corresponding flag only gets registered in Windows builds
	WindowsService bool

	// EnableDeprecatedBetaTopologyLabels indicates whether the node should apply beta topology labels.
	// If true, the node will apply beta topology labels.
	// DEPRECATED: This flag will be removed in a future release.
	EnableDeprecatedBetaTopologyLabels bool
}
