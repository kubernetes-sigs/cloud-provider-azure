/*
Copyright 2016 The Kubernetes Authors.

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
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	apiserveroptions "k8s.io/apiserver/pkg/server/options"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	cloudprovider "k8s.io/cloud-provider"
	ccmconfig "k8s.io/cloud-provider/config"
	ccmconfigscheme "k8s.io/cloud-provider/config/install"
	ccmconfigv1alpha1 "k8s.io/cloud-provider/config/v1alpha1"
	"k8s.io/cloud-provider/names"
	cpoptions "k8s.io/cloud-provider/options"
	cliflag "k8s.io/component-base/cli/flag"
	cmoptions "k8s.io/controller-manager/options"
	"k8s.io/controller-manager/pkg/clientbuilder"
	"k8s.io/klog/v2"

	cloudcontrollerconfig "sigs.k8s.io/cloud-provider-azure/cmd/cloud-controller-manager/app/config"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"

	// add the kubernetes feature gates
	_ "k8s.io/controller-manager/pkg/features/register"
)

const (
	// CloudControllerManagerUserAgent is the userAgent name when starting cloud-controller managers.
	CloudControllerManagerUserAgent = "cloud-controller-manager"

	// CloudControllerManagerPort is the default port for the cloud controller manager server.
	// This value may be overridden by a flag at startup.
	CloudControllerManagerPort = cloudprovider.CloudControllerManagerPort
)

// CloudControllerManagerOptions is the main context object for the controller manager.
type CloudControllerManagerOptions struct {
	Generic            *cmoptions.GenericControllerManagerConfigurationOptions
	KubeCloudShared    *cpoptions.KubeCloudSharedOptions
	ServiceController  *cpoptions.ServiceControllerOptions
	NodeIPAMController *NodeIPAMControllerOptions
	NodeController     *cpoptions.NodeControllerOptions

	SecureServing  *apiserveroptions.SecureServingOptionsWithLoopback
	Authentication *apiserveroptions.DelegatingAuthenticationOptions
	Authorization  *apiserveroptions.DelegatingAuthorizationOptions

	Master     string
	Kubeconfig string

	// NodeStatusUpdateFrequency is the frequency at which the controller updates nodes' status
	NodeStatusUpdateFrequency metav1.Duration

	DynamicReloading *DynamicReloadingOptions
}

// NewCloudControllerManagerOptions creates a new ExternalCMServer with a default config.
func NewCloudControllerManagerOptions() (*CloudControllerManagerOptions, error) {
	componentConfig, err := NewDefaultComponentConfig()
	if err != nil {
		return nil, err
	}

	s := CloudControllerManagerOptions{
		Generic:         cmoptions.NewGenericControllerManagerConfigurationOptions(&componentConfig.Generic),
		KubeCloudShared: cpoptions.NewKubeCloudSharedOptions(&componentConfig.KubeCloudShared),
		ServiceController: &cpoptions.ServiceControllerOptions{
			ServiceControllerConfiguration: &componentConfig.ServiceController,
		},
		NodeIPAMController: defaultNodeIPAMControllerOptions(),
		NodeController: &cpoptions.NodeControllerOptions{
			NodeControllerConfiguration: &componentConfig.NodeController,
		},
		SecureServing:             apiserveroptions.NewSecureServingOptions().WithLoopback(),
		Authentication:            apiserveroptions.NewDelegatingAuthenticationOptions(),
		Authorization:             apiserveroptions.NewDelegatingAuthorizationOptions(),
		NodeStatusUpdateFrequency: componentConfig.NodeStatusUpdateFrequency,
		DynamicReloading:          defaultDynamicReloadingOptions(),
	}

	s.Authentication.RemoteKubeConfigFileOptional = true
	s.Authorization.RemoteKubeConfigFileOptional = true

	// Set cloud provider name to Azure.
	s.KubeCloudShared.CloudProvider.Name = consts.CloudProviderName

	// Set the PairName but leave certificate directory blank to generate in-memory by default
	s.SecureServing.ServerCert.CertDirectory = ""
	s.SecureServing.ServerCert.PairName = "cloud-controller-manager"
	s.SecureServing.BindPort = CloudControllerManagerPort

	s.Generic.LeaderElection.ResourceName = "cloud-controller-manager"
	s.Generic.LeaderElection.ResourceNamespace = "kube-system"

	return &s, nil
}

// NewDefaultComponentConfig returns cloud-controller manager configuration object.
func NewDefaultComponentConfig() (*ccmconfig.CloudControllerManagerConfiguration, error) {
	versioned := &ccmconfigv1alpha1.CloudControllerManagerConfiguration{}
	ccmconfigscheme.Scheme.Default(versioned)

	internal := &ccmconfig.CloudControllerManagerConfiguration{}
	if err := ccmconfigscheme.Scheme.Convert(versioned, internal, nil); err != nil {
		return nil, err
	}
	return internal, nil
}

// Flags returns flags for a specific APIServer by section name
func (o *CloudControllerManagerOptions) Flags(allControllers, disabledByDefaultControllers []string) cliflag.NamedFlagSets {
	fss := cliflag.NamedFlagSets{}
	o.Generic.AddFlags(&fss, allControllers, disabledByDefaultControllers, names.CCMControllerAliases())
	o.KubeCloudShared.AddFlags(fss.FlagSet("generic"))
	o.ServiceController.AddFlags(fss.FlagSet("service controller"))
	o.NodeController.AddFlags(fss.FlagSet("node controller"))
	o.NodeIPAMController.AddFlags(fss.FlagSet("node ipam controller"))

	o.SecureServing.AddFlags(fss.FlagSet("secure serving"))
	o.Authentication.AddFlags(fss.FlagSet("authentication"))
	o.Authorization.AddFlags(fss.FlagSet("authorization"))

	o.DynamicReloading.AddFlags(fss.FlagSet("dynamic reloading"))

	fs := fss.FlagSet("misc")
	fs.StringVar(&o.Master, "master", o.Master, "The address of the Kubernetes API server (overrides any value in kubeconfig).")
	fs.StringVar(&o.Kubeconfig, "kubeconfig", o.Kubeconfig, "Path to kubeconfig file with authorization and master location information.")
	fs.DurationVar(&o.NodeStatusUpdateFrequency.Duration, "node-status-update-frequency", o.NodeStatusUpdateFrequency.Duration, "Specifies how often the controller updates nodes' status.")

	utilfeature.DefaultMutableFeatureGate.AddFlag(fss.FlagSet("generic"))

	return fss
}

// ApplyTo fills up cloud controller manager config with options.
func (o *CloudControllerManagerOptions) ApplyTo(
	c *cloudcontrollerconfig.Config,
	userAgent string,
	allControllers, disabledByDefaultControllers []string,
	controllerAliases map[string]string) error {
	var err error
	if err = o.Generic.ApplyTo(&c.ComponentConfig.Generic, allControllers, disabledByDefaultControllers, controllerAliases); err != nil {
		return err
	}
	if err = o.KubeCloudShared.ApplyTo(&c.ComponentConfig.KubeCloudShared); err != nil {
		return err
	}
	if err = o.ServiceController.ApplyTo(&c.ComponentConfig.ServiceController); err != nil {
		return err
	}
	if err = o.NodeIPAMController.ApplyTo(&c.NodeIPAMControllerConfig); err != nil {
		return err
	}
	if err = o.NodeController.ApplyTo(&c.ComponentConfig.NodeController); err != nil {
		return err
	}
	if err = o.SecureServing.ApplyTo(&c.SecureServing, &c.LoopbackClientConfig); err != nil {
		return err
	}
	if err = o.DynamicReloading.ApplyTo(&c.DynamicReloadingConfig); err != nil {
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
	c.Kubeconfig.ContentConfig.ContentType = o.Generic.ClientConnection.ContentType
	c.Kubeconfig.QPS = o.Generic.ClientConnection.QPS
	c.Kubeconfig.Burst = int(o.Generic.ClientConnection.Burst)

	c.Client, err = clientset.NewForConfig(restclient.AddUserAgent(c.Kubeconfig, userAgent))
	if err != nil {
		return err
	}

	c.EventRecorder = createRecorder(c.Client, userAgent)

	rootClientBuilder := clientbuilder.SimpleControllerClientBuilder{
		ClientConfig: c.Kubeconfig,
	}
	if c.ComponentConfig.KubeCloudShared.UseServiceAccountCredentials {
		c.ClientBuilder = clientbuilder.NewDynamicClientBuilder(
			restclient.AnonymousClientConfig(c.Kubeconfig),
			c.Client.CoreV1(),
			metav1.NamespaceSystem,
		)
	} else {
		c.ClientBuilder = rootClientBuilder
	}
	c.VersionedClient = rootClientBuilder.ClientOrDie("shared-informers")
	c.SharedInformers = informers.NewSharedInformerFactory(c.VersionedClient, ResyncPeriod(c)())

	// sync back to component config
	// TODO: find more elegant way than syncing back the values.

	c.ComponentConfig.NodeStatusUpdateFrequency = o.NodeStatusUpdateFrequency

	return nil
}

// Validate is used to validate config before launching the cloud controller manager
func (o *CloudControllerManagerOptions) Validate(allControllers, disabledByDefaultControllers []string, controllerAliases map[string]string) error {
	errors := []error{}

	errors = append(errors, o.Generic.Validate(allControllers, disabledByDefaultControllers, controllerAliases)...)
	errors = append(errors, o.KubeCloudShared.Validate()...)
	errors = append(errors, o.ServiceController.Validate()...)
	errors = append(errors, o.NodeIPAMController.Validate()...)
	errors = append(errors, o.NodeController.Validate()...)
	errors = append(errors, o.SecureServing.Validate()...)
	errors = append(errors, o.Authentication.Validate()...)
	errors = append(errors, o.Authorization.Validate()...)
	errors = append(errors, o.DynamicReloading.Validate()...)

	if len(o.KubeCloudShared.CloudProvider.Name) == 0 {
		errors = append(errors, fmt.Errorf("--cloud-provider cannot be empty"))
	}

	if o.ServiceController.ConcurrentServiceSyncs != 1 {
		errors = append(errors, fmt.Errorf("--concurrent-service-syncs is limited to 1 only"))
	}

	if !o.DynamicReloading.EnableDynamicReloading && o.KubeCloudShared.CloudProvider.CloudConfigFile == "" {
		errors = append(errors, fmt.Errorf("--cloud-config cannot be empty when --enable-dynamic-reloading is not set to true"))
	}

	return utilerrors.NewAggregate(errors)
}

// ResyncPeriod computes the time interval a shared informer waits before resyncing with the api server
func ResyncPeriod(c *cloudcontrollerconfig.Config) func() time.Duration {
	return func() time.Duration {
		factor := rand.Float64() + 1 // #nosec G404
		return time.Duration(float64(c.ComponentConfig.Generic.MinResyncPeriod.Nanoseconds()) * factor)
	}
}

// Config return a cloud controller manager config objective
func (o *CloudControllerManagerOptions) Config(
	allControllers, disabledByDefaultControllers []string,
	controllerAliases map[string]string,
) (*cloudcontrollerconfig.Config, error) {
	if err := o.Validate(allControllers, disabledByDefaultControllers, controllerAliases); err != nil {
		return nil, err
	}

	if err := o.SecureServing.MaybeDefaultWithSelfSignedCerts("localhost", nil, []net.IP{net.ParseIP("127.0.0.1")}); err != nil {
		return nil, fmt.Errorf("error creating self-signed certificates: %w", err)
	}

	c := &cloudcontrollerconfig.Config{}
	if err := o.ApplyTo(c, CloudControllerManagerUserAgent, allControllers, disabledByDefaultControllers, controllerAliases); err != nil {
		return nil, err
	}

	return c, nil
}

func createRecorder(kubeClient clientset.Interface, userAgent string) record.EventRecorder {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	// TODO: remove dependence on the legacyscheme
	return eventBroadcaster.NewRecorder(runtime.NewScheme(), v1.EventSource{Component: userAgent})
}
