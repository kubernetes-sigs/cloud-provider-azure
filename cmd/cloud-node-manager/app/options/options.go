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

package options

import (
	"fmt"
	"math/rand"
	"net"
	"os"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	apiserveroptions "k8s.io/apiserver/pkg/server/options"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	cliflag "k8s.io/component-base/cli/flag"
	componentbaseconfig "k8s.io/component-base/config"
	"k8s.io/controller-manager/pkg/clientbuilder"
	"k8s.io/klog/v2"

	cloudnodeconfig "sigs.k8s.io/cloud-provider-azure/cmd/cloud-node-manager/app/config"

	// add the related feature gates
	_ "k8s.io/controller-manager/pkg/features/register"
)

const (
	// cloudNodeManagerUserAgent is the userAgent name when starting cloud-node managers.
	cloudNodeManagerUserAgent = "cloud-node-manager"
	// defaultCloudNodeManagerPort is the default cloud-node manager port.
	defaultCloudNodeManagerPort = 0
	// CloudControllerManagerPort is the default port for the cloud controller manager server.
	// This value may be overridden by a flag at startup.
	CloudControllerManagerPort = 10263
	// defaultNodeStatusUpdateFrequencyInMinute is the default frequency at which the manager updates nodes' status.
	defaultNodeStatusUpdateFrequencyInMinute = 5
)

// CloudNodeManagerOptions is the main context object for the controller manager.
type CloudNodeManagerOptions struct {
	Master              string
	Kubeconfig          string
	NodeName            string
	CloudConfigFilePath string

	SecureServing  *apiserveroptions.SecureServingOptionsWithLoopback
	Authentication *apiserveroptions.DelegatingAuthenticationOptions
	Authorization  *apiserveroptions.DelegatingAuthorizationOptions

	// NodeStatusUpdateFrequency is the frequency at which the manager updates nodes' status
	NodeStatusUpdateFrequency metav1.Duration
	// minResyncPeriod is the resync period in reflectors; will be random between
	// minResyncPeriod and 2*minResyncPeriod.
	MinResyncPeriod metav1.Duration
	// ClientConnection specifies the kubeconfig file and client connection
	// settings for the proxy server to use when communicating with the apiserver.
	ClientConnection componentbaseconfig.ClientConnectionConfiguration
	// WaitForRoutes indicates whether the node should wait for routes to be created on Azure.
	// If true, the node condition "NodeNetworkUnavailable" would be set to true on initialization.
	WaitForRoutes bool

	UseInstanceMetadata bool

	// WindowsService should be set to true if cloud-node-manager is running as a service on Windows.
	// Its corresponding flag only gets registered in Windows builds
	WindowsService bool

	// EnableDeprecatedBetaTopologyLabels indicates whether the node should apply beta topology labels.
	// If true, the node will apply beta topology labels.
	// DEPRECATED: This flag will be removed in a future release.
	EnableDeprecatedBetaTopologyLabels bool
}

// NewCloudNodeManagerOptions creates a new CloudNodeManagerOptions with a default config.
func NewCloudNodeManagerOptions() *CloudNodeManagerOptions {
	s := CloudNodeManagerOptions{
		SecureServing:  apiserveroptions.NewSecureServingOptions().WithLoopback(),
		Authentication: apiserveroptions.NewDelegatingAuthenticationOptions(),
		Authorization:  apiserveroptions.NewDelegatingAuthorizationOptions(),
		NodeStatusUpdateFrequency: metav1.Duration{
			Duration: defaultNodeStatusUpdateFrequencyInMinute * time.Minute,
		},
	}

	s.Authentication.RemoteKubeConfigFileOptional = true
	s.Authorization.RemoteKubeConfigFileOptional = true

	// Set the PairName but leave certificate directory blank to generate in-memory by default
	s.SecureServing.ServerCert.CertDirectory = ""
	s.SecureServing.ServerCert.PairName = "cloud-node-manager"
	s.SecureServing.BindPort = defaultCloudNodeManagerPort
	return &s
}

// Flags returns flags for a specific APIServer by section name
func (o *CloudNodeManagerOptions) Flags() cliflag.NamedFlagSets {
	fss := cliflag.NamedFlagSets{}

	o.SecureServing.AddFlags(fss.FlagSet("secure serving"))
	o.Authentication.AddFlags(fss.FlagSet("authentication"))
	o.Authorization.AddFlags(fss.FlagSet("authorization"))

	fs := fss.FlagSet("misc")
	o.addOSFlags(fs)

	fs.StringVar(&o.Master, "master", o.Master, "The address of the Kubernetes API server (overrides any value in kubeconfig).")
	fs.StringVar(&o.Kubeconfig, "kubeconfig", o.Kubeconfig, "Path to kubeconfig file with authorization and master location information.")
	fs.StringVar(&o.NodeName, "node-name", o.NodeName, "Name of the Node (default is hostname).")
	fs.DurationVar(&o.NodeStatusUpdateFrequency.Duration, "node-status-update-frequency", o.NodeStatusUpdateFrequency.Duration, "Specifies how often the controller updates nodes' status.")
	fs.DurationVar(&o.MinResyncPeriod.Duration, "min-resync-period", o.MinResyncPeriod.Duration, "The resync period in reflectors will be random between MinResyncPeriod and 2*MinResyncPeriod.")
	fs.StringVar(&o.ClientConnection.ContentType, "kube-api-content-type", o.ClientConnection.ContentType, "Content type of requests sent to apiserver.")
	fs.Float32Var(&o.ClientConnection.QPS, "kube-api-qps", 20, "QPS to use while talking with kubernetes apiserver.")
	fs.Int32Var(&o.ClientConnection.Burst, "kube-api-burst", 30, "Burst to use while talking with kubernetes apiserver.")
	fs.BoolVar(&o.WaitForRoutes, "wait-routes", false, "Whether the nodes should wait for routes created on Azure route table. It should be set to true when using kubenet plugin.")
	fs.BoolVar(&o.UseInstanceMetadata, "use-instance-metadata", true, "Should use Instance Metadata Service for fetching node information; if false will use ARM instead.")
	fs.StringVar(&o.CloudConfigFilePath, "cloud-config", o.CloudConfigFilePath, "The path to the cloud config file to be used when using ARM to fetch node information.")
	fs.BoolVar(&o.EnableDeprecatedBetaTopologyLabels, "enable-deprecated-beta-topology-labels", o.EnableDeprecatedBetaTopologyLabels, "DEPRECATED: This flag will be removed in a future release. If true, the node will apply beta topology labels.")
	return fss
}

// ApplyTo fills up cloud controller manager config with options.
func (o *CloudNodeManagerOptions) ApplyTo(c *cloudnodeconfig.Config, userAgent string) error {
	var err error
	if err = o.SecureServing.ApplyTo(&c.SecureServing, &c.LoopbackClientConfig); err != nil {
		return err
	}
	if o.SecureServing.BindPort != 0 || o.SecureServing.Listener != nil {
		o.Authentication.RemoteKubeConfigFile = o.Kubeconfig
		o.Authorization.RemoteKubeConfigFile = o.Kubeconfig

		if err = o.Authentication.ApplyTo(&c.Authentication, c.SecureServing, nil); err != nil {
			return err
		}
		if err = o.Authorization.ApplyTo(&c.Authorization); err != nil {
			return err
		}
	}

	c.Kubeconfig, err = clientcmd.BuildConfigFromFlags(o.Master, o.Kubeconfig)
	if err != nil {
		return err
	}
	c.Kubeconfig.DisableCompression = true
	c.Kubeconfig.ContentConfig.ContentType = o.ClientConnection.ContentType
	c.Kubeconfig.QPS = o.ClientConnection.QPS
	c.Kubeconfig.Burst = int(o.ClientConnection.Burst)
	c.WaitForRoutes = o.WaitForRoutes

	c.Client, err = clientset.NewForConfig(restclient.AddUserAgent(c.Kubeconfig, userAgent))
	if err != nil {
		return err
	}

	// Default NodeName is hostname.
	c.NodeName = strings.ToLower(o.NodeName)
	if c.NodeName == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return err
		}

		c.NodeName = strings.ToLower(hostname)
	}

	c.EventRecorder = createRecorder(c.Client, userAgent)
	c.ClientBuilder = clientbuilder.SimpleControllerClientBuilder{
		ClientConfig: c.Kubeconfig,
	}
	c.VersionedClient = c.ClientBuilder.ClientOrDie("shared-informers")
	// Only need to watch the node itself. There is no need to set up a watch on the nodes in the cluster.
	c.SharedInformers = informers.NewSharedInformerFactoryWithOptions(c.VersionedClient, resyncPeriod(c)(), informers.WithTweakListOptions(func(options *metav1.ListOptions) {
		options.FieldSelector = fields.OneTermEqualSelector("metadata.name", c.NodeName).String()
	}))
	c.NodeStatusUpdateFrequency = o.NodeStatusUpdateFrequency
	c.UseInstanceMetadata = o.UseInstanceMetadata
	c.CloudConfigFilePath = o.CloudConfigFilePath

	c.WindowsService = o.WindowsService

	// Allow users to choose to apply beta topology labels until they are removed by all cloud providers.
	c.EnableDeprecatedBetaTopologyLabels = o.EnableDeprecatedBetaTopologyLabels

	return nil
}

// resyncPeriod computes the time interval a shared informer waits before resyncing with the api server
func resyncPeriod(c *cloudnodeconfig.Config) func() time.Duration {
	return func() time.Duration {
		factor := rand.Float64() + 1 // #nosec G404
		return time.Duration(float64(c.MinResyncPeriod.Nanoseconds()) * factor)
	}
}

// Config return a cloud controller manager config objective
func (o *CloudNodeManagerOptions) Config() (*cloudnodeconfig.Config, error) {
	if err := o.SecureServing.MaybeDefaultWithSelfSignedCerts("localhost", nil, []net.IP{net.ParseIP("127.0.0.1")}); err != nil {
		return nil, fmt.Errorf("error creating self-signed certificates: %w", err)
	}

	c := &cloudnodeconfig.Config{}
	if err := o.ApplyTo(c, cloudNodeManagerUserAgent); err != nil {
		return nil, err
	}

	return c, nil
}

func createRecorder(kubeClient clientset.Interface, userAgent string) record.EventRecorder {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	// TODO: remove dependence on the legacyscheme
	return eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: userAgent})
}
