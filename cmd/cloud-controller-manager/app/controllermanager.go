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

package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/big"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server/healthz"
	cacheddiscovery "k8s.io/client-go/discovery/cached"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	cloudprovider "k8s.io/cloud-provider"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/cli/globalflag"
	"k8s.io/component-base/configz"
	controllersmetrics "k8s.io/component-base/metrics/prometheus/controllers"
	"k8s.io/component-base/term"
	genericcontrollermanager "k8s.io/controller-manager/app"
	"k8s.io/controller-manager/controller"
	"k8s.io/controller-manager/pkg/clientbuilder"
	controllerhealthz "k8s.io/controller-manager/pkg/healthz"
	"k8s.io/controller-manager/pkg/informerfactory"
	"k8s.io/klog/v2"

	cloudcontrollerconfig "sigs.k8s.io/cloud-provider-azure/cmd/cloud-controller-manager/app/config"
	"sigs.k8s.io/cloud-provider-azure/cmd/cloud-controller-manager/app/dynamic"
	"sigs.k8s.io/cloud-provider-azure/cmd/cloud-controller-manager/app/options"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider"
	"sigs.k8s.io/cloud-provider-azure/pkg/version"
	"sigs.k8s.io/cloud-provider-azure/pkg/version/verflag"
)

const (
	// ControllerStartJitter is the jitter value used when starting controller managers.
	ControllerStartJitter = 1.0
	// ConfigzName is the name used for register cloud-controller manager /configz, same with GroupName.
	ConfigzName = "cloudcontrollermanager.config.k8s.io"
)

// NewCloudControllerManagerCommand creates a *cobra.Command object with default parameters
func NewCloudControllerManagerCommand() *cobra.Command {
	s, err := options.NewCloudControllerManagerOptions()
	if err != nil {
		klog.Fatalf("unable to initialize command options: %v", err)
	}

	cmd := &cobra.Command{
		Use:  "cloud-controller-manager",
		Long: `The Cloud controller manager is a daemon that embeds the cloud specific control loops shipped with Kubernetes.`,
		Run: func(cmd *cobra.Command, args []string) {
			verflag.PrintAndExitIfRequested("Cloud Provider Azure")
			cliflag.PrintFlags(cmd.Flags())

			c, err := s.Config(KnownControllers(), ControllersDisabledByDefault.List())
			if err != nil {
				klog.Errorf("Run: failed to configure cloud controller manager: %v", err)
				os.Exit(1)
			}

			healthHandler, err := StartHTTPServer(c.Complete(), wait.NeverStop)
			if err != nil {
				klog.Errorf("Run: railed to start HTTP server: %v", err)
				os.Exit(1)
			}

			if c.ComponentConfig.Generic.LeaderElection.LeaderElect {
				// Identity used to distinguish between multiple cloud controller manager instances
				id, err := os.Hostname()
				if err != nil {
					klog.Errorf("Run: failed to get host name: %v", err)
					os.Exit(1)
				}
				// add a uniquifier so that two processes on the same host don't accidentally both become active
				id = id + "_" + string(uuid.NewUUID())

				// Lock required for leader election
				rl, err := resourcelock.NewFromKubeconfig(c.ComponentConfig.Generic.LeaderElection.ResourceLock,
					c.ComponentConfig.Generic.LeaderElection.ResourceNamespace,
					c.ComponentConfig.Generic.LeaderElection.ResourceName,
					resourcelock.ResourceLockConfig{
						Identity:      id,
						EventRecorder: c.EventRecorder,
					},
					c.Kubeconfig,
					c.ComponentConfig.Generic.LeaderElection.RenewDeadline.Duration)
				if err != nil {
					klog.Fatalf("error creating lock: %v", err)
				}

				// Try and become the leader and start cloud controller manager loops
				var electionChecker *leaderelection.HealthzAdaptor
				if c.ComponentConfig.Generic.LeaderElection.LeaderElect {
					electionChecker = leaderelection.NewLeaderHealthzAdaptor(time.Second * 20)
				}

				leaderelection.RunOrDie(context.TODO(), leaderelection.LeaderElectionConfig{
					Lock:          rl,
					LeaseDuration: c.ComponentConfig.Generic.LeaderElection.LeaseDuration.Duration,
					RenewDeadline: c.ComponentConfig.Generic.LeaderElection.RenewDeadline.Duration,
					RetryPeriod:   c.ComponentConfig.Generic.LeaderElection.RetryPeriod.Duration,
					Callbacks: leaderelection.LeaderCallbacks{
						OnStartedLeading: RunWrapper(s, c, healthHandler),
						OnStoppedLeading: func() {
							panic("leaderelection lost")
						},
					},
					WatchDog: electionChecker,
					Name:     "cloud-controller-manager",
				})

				panic("unreachable")
			}

			RunWrapper(s, c, healthHandler)(context.TODO())
			panic("unreachable")
		},
	}

	fs := cmd.Flags()
	namedFlagSets := s.Flags(KnownControllers(), ControllersDisabledByDefault.List())
	verflag.AddFlags(namedFlagSets.FlagSet("global"))
	globalflag.AddGlobalFlags(namedFlagSets.FlagSet("global"), cmd.Name())

	for _, f := range namedFlagSets.FlagSets {
		fs.AddFlagSet(f)
	}
	usageFmt := "Usage:\n  %s\n"
	cols, _, _ := term.TerminalSize(cmd.OutOrStdout())
	cmd.SetUsageFunc(func(cmd *cobra.Command) error {
		fmt.Fprintf(cmd.OutOrStderr(), usageFmt, cmd.UseLine())
		cliflag.PrintSections(cmd.OutOrStderr(), namedFlagSets, cols)
		return nil
	})
	cmd.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		fmt.Fprintf(cmd.OutOrStdout(), "%s\n\n"+usageFmt, cmd.Long, cmd.UseLine())
		cliflag.PrintSections(cmd.OutOrStdout(), namedFlagSets, cols)
	})

	return cmd
}

// RunWrapper adapts the ccm boot logic to the leader elector call back function
func RunWrapper(s *options.CloudControllerManagerOptions, c *cloudcontrollerconfig.Config, h *controllerhealthz.MutableHealthzHandler) func(ctx context.Context) {
	return func(ctx context.Context) {
		if !c.DynamicReloadingConfig.EnableDynamicReloading {
			klog.V(1).Infof("using static initialization from config file %s", c.ComponentConfig.KubeCloudShared.CloudProvider.CloudConfigFile)
			if err := Run(context.TODO(), c.Complete(), h); err != nil {
				klog.Errorf("RunWrapper: failed to start cloud controller manager: %v", err)
				os.Exit(1)
			}

			panic("unreachable")
		} else {
			var updateCh chan struct{}

			cloudConfigFile := c.ComponentConfig.KubeCloudShared.CloudProvider.CloudConfigFile
			if cloudConfigFile != "" {
				klog.V(1).Infof("RunWrapper: using dynamic initialization from config file %s, starting the file watcher", cloudConfigFile)
				updateCh = dynamic.RunFileWatcherOrDie(cloudConfigFile)
			} else {
				klog.V(1).Infof("RunWrapper: using dynamic initialization from secret %s/%s, starting the secret watcher", c.DynamicReloadingConfig.CloudConfigSecretNamespace, c.DynamicReloadingConfig.CloudConfigSecretName)
				updateCh = dynamic.RunSecretWatcherOrDie(c)
			}

			errCh := make(chan error, 1)
			cancelFunc := runAsync(s, errCh, h)
			for {
				select {
				case <-updateCh:
					klog.V(2).Info("RunWrapper: detected the cloud config has been updated, re-constructing the cloud controller manager")

					// stop the previous goroutines
					cancelFunc()

					var (
						shouldRemainStopped bool
						err                 error
					)
					if cloudConfigFile != "" {
						// start new goroutines if needed when using config file
						shouldRemainStopped, err = shouldDisableCloudProvider(cloudConfigFile)
						if err != nil {
							klog.Fatalf("RunWrapper: failed to determine if it is needed to restart all controllers: %s", err.Error())
						}
					}

					if !shouldRemainStopped {
						klog.Info("RunWrapper: restarting all controllers")
						cancelFunc = runAsync(s, errCh, h)
					} else {
						klog.Warningf("All controllers are stopped!")
					}

				case err := <-errCh:
					klog.Errorf("RunWrapper: failed to start cloud controller manager: %v", err)
					os.Exit(1)
				}
			}
		}
	}
}

func shouldDisableCloudProvider(configFilePath string) (bool, error) {
	configBytes, err := ioutil.ReadFile(configFilePath)
	if err != nil {
		klog.Errorf("shouldDisableCloudProvider: failed to read %s  %s", configFilePath, err.Error())
		return false, err
	}

	var c struct {
		DisableCloudProvider bool `json:"disableCloudProvider,omitempty"`
	}
	if err = json.Unmarshal(configBytes, &c); err != nil {
		klog.Errorf("shouldDisableCloudProvider: failed to unmarshal configBytes to struct: %s", err.Error())
		return false, err
	}

	klog.Infof("shouldDisableCloudProvider: should disable cloud provider: %t", c.DisableCloudProvider)
	return c.DisableCloudProvider, nil
}

func runAsync(s *options.CloudControllerManagerOptions, errCh chan error, h *controllerhealthz.MutableHealthzHandler) context.CancelFunc {
	ctx, cancelFunc := context.WithCancel(context.Background())

	go func() {
		c, err := s.Config(KnownControllers(), ControllersDisabledByDefault.List())
		if err != nil {
			klog.Errorf("RunAsync: failed to configure cloud controller manager: %v", err)
			os.Exit(1)
		}

		if err := Run(ctx, c.Complete(), h); err != nil {
			klog.Errorf("RunAsync: failed to run cloud controller manager: %v", err)
			errCh <- err
		}

		klog.V(1).Infof("RunAsync: stopping")
	}()

	return cancelFunc
}

// StartHTTPServer starts the controller manager HTTP server
func StartHTTPServer(c *cloudcontrollerconfig.CompletedConfig, stopCh <-chan struct{}) (*controllerhealthz.MutableHealthzHandler, error) {
	// Setup any healthz checks we will want to use.
	var checks []healthz.HealthChecker
	var electionChecker *leaderelection.HealthzAdaptor
	if c.ComponentConfig.Generic.LeaderElection.LeaderElect {
		electionChecker = leaderelection.NewLeaderHealthzAdaptor(time.Second * 20)
		checks = append(checks, electionChecker)
	}

	healthzHandler := controllerhealthz.NewMutableHealthzHandler(checks...)
	// Start the controller manager HTTP server
	if c.SecureServing != nil {
		unsecuredMux := genericcontrollermanager.NewBaseHandler(&c.ComponentConfig.Generic.Debugging, healthzHandler)
		handler := genericcontrollermanager.BuildHandlerChain(unsecuredMux, &c.Authorization, &c.Authentication)
		// TODO: handle stoppedCh returned by c.SecureServing.Serve
		if _, _, err := c.SecureServing.Serve(handler, 0, stopCh); err != nil {
			return nil, err
		}

		healthz.InstallReadyzHandler(unsecuredMux, checks...)
	}

	return healthzHandler, nil
}

// Run runs the ExternalCMServer.  This should never exit.
func Run(ctx context.Context, c *cloudcontrollerconfig.CompletedConfig, h *controllerhealthz.MutableHealthzHandler) error {
	// To help debugging, immediately log version
	klog.Infof("Version: %#v", version.Get())

	var (
		cloud cloudprovider.Interface
		err   error
	)

	var ipFamily provider.IPFamily
	cidrs, isDualStack, err := processCIDRs(c.ComponentConfig.KubeCloudShared.ClusterCIDR)
	if err != nil || len(cidrs) == 0 {
		klog.Fatalf("failed to check if it is a dual-stack cluster, cidrs: %v, err: %v", cidrs, err)
	}
	if isDualStack {
		ipFamily = provider.DualStack
	} else {
		if cidrs[0].IP.To4() != nil {
			ipFamily = provider.IPv4
		} else {
			ipFamily = provider.IPv6
		}
	}

	if c.ComponentConfig.KubeCloudShared.CloudProvider.CloudConfigFile != "" {
		cloud, err = provider.NewCloudFromConfigFile(ctx, c.ComponentConfig.KubeCloudShared.CloudProvider.CloudConfigFile, true, ipFamily)
		if err != nil {
			klog.Fatalf("Cloud provider azure could not be initialized: %v", err)
		}
	} else if c.DynamicReloadingConfig.EnableDynamicReloading && c.DynamicReloadingConfig.CloudConfigSecretName != "" {
		cloud, err = provider.NewCloudFromSecret(ctx, c.ClientBuilder, c.DynamicReloadingConfig.CloudConfigSecretName, c.DynamicReloadingConfig.CloudConfigSecretNamespace, c.DynamicReloadingConfig.CloudConfigKey)
		if err != nil {
			klog.Fatalf("Run: Cloud provider azure could not be initialized dynamically from secret %s/%s: %v", c.DynamicReloadingConfig.CloudConfigSecretNamespace, c.DynamicReloadingConfig.CloudConfigSecretName, err)
		}
	}

	if cloud == nil {
		klog.Fatalf("cloud provider is nil, please check if the --cloud-config is set properly")
	}

	if !cloud.HasClusterID() {
		if c.ComponentConfig.KubeCloudShared.AllowUntaggedCloud {
			klog.Warning("detected a cluster without a ClusterID.  A ClusterID will be required in the future.  Please tag your cluster to avoid any future issues")
		} else {
			klog.Fatalf("no ClusterID found.  A ClusterID is required for the cloud provider to function properly.  This check can be bypassed by setting the allow-untagged-cloud option")
		}
	}

	// setup /configz endpoint
	if cz, err := configz.New(ConfigzName); err == nil {
		cz.Set(c.ComponentConfig)
	} else {
		klog.Errorf("unable to register configz: %v", err)
	}
	clientBuilder := clientbuilder.SimpleControllerClientBuilder{
		ClientConfig: c.Kubeconfig,
	}
	controllerContext, err := CreateControllerContext(c, clientBuilder, ctx.Done())
	if err != nil {
		klog.Fatalf("error building controller context: %v", err)
	}

	if err := startControllers(ctx, controllerContext, c, ctx.Done(), cloud, newControllerInitializers(), h); err != nil {
		klog.Fatalf("error running controllers: %v", err)
	}

	return nil
}

// startControllers starts the cloud specific controller loops.
func startControllers(ctx context.Context, controllerContext genericcontrollermanager.ControllerContext, completedConfig *cloudcontrollerconfig.CompletedConfig, stopCh <-chan struct{},
	cloud cloudprovider.Interface, controllers map[string]initFunc, healthzHandler *controllerhealthz.MutableHealthzHandler) error {
	// Initialize the cloud provider with a reference to the clientBuilder
	cloud.Initialize(completedConfig.ClientBuilder, stopCh)
	// Set the informer on the user cloud object
	if informerUserCloud, ok := cloud.(cloudprovider.InformerUser); ok {
		informerUserCloud.SetInformers(completedConfig.SharedInformers)
	}

	var controllerChecks []healthz.HealthChecker
	for controllerName, initFn := range controllers {
		if !genericcontrollermanager.IsControllerEnabled(controllerName, ControllersDisabledByDefault, completedConfig.ComponentConfig.Generic.Controllers) {
			klog.Warningf("%q is disabled", controllerName)
			continue
		}

		klog.V(1).Infof("Starting %q", controllerName)
		ctrl, started, err := initFn(ctx, controllerContext, completedConfig, cloud, stopCh)
		if err != nil {
			klog.Errorf("Error starting %q: %s", controllerName, err.Error())
			return err
		}
		if !started {
			klog.Warningf("Skipping %q", controllerName)
			continue
		}
		check := controllerhealthz.NamedPingChecker(controllerName)
		if ctrl != nil {
			if healthCheckable, ok := ctrl.(controller.HealthCheckable); ok {
				if realCheck := healthCheckable.HealthChecker(); realCheck != nil {
					check = controllerhealthz.NamedHealthChecker(controllerName, realCheck)
				}
			}
		}
		controllerChecks = append(controllerChecks, check)
		klog.Infof("Started %q", controllerName)

		time.Sleep(wait.Jitter(completedConfig.ComponentConfig.Generic.ControllerStartInterval.Duration, ControllerStartJitter))
	}
	if healthzHandler != nil {
		healthzHandler.AddHealthChecker(controllerChecks...)
	}

	// If apiserver is not running we should wait for some time and fail only then. This is particularly
	// important when we start apiserver and controller manager at the same time.
	if err := genericcontrollermanager.WaitForAPIServer(completedConfig.VersionedClient, 10*time.Second); err != nil {
		klog.Fatalf("Failed to wait for apiserver being healthy: %v", err)
	}

	klog.V(2).Infof("startControllers: starting shared informers")
	completedConfig.SharedInformers.Start(stopCh)
	controllerContext.InformerFactory.Start(stopCh)

	<-stopCh
	klog.V(1).Infof("startControllers: received stopping signal, exiting")

	return nil
}

// initFunc is used to launch a particular controller.  It may run additional "should I activate checks".
// Any error returned will cause the controller process to `Fatal`
// The bool indicates whether the controller was enabled.
type initFunc func(ctx context.Context, controllerContext genericcontrollermanager.ControllerContext, completedConfig *cloudcontrollerconfig.CompletedConfig, cloud cloudprovider.Interface, stop <-chan struct{}) (debuggingHandler http.Handler, enabled bool, err error)

// KnownControllers indicate the default controller we are known.
func KnownControllers() []string {
	ret := sets.StringKeySet(newControllerInitializers())
	return ret.List()
}

// ControllersDisabledByDefault is the controller disabled default when starting cloud-controller managers.
var ControllersDisabledByDefault = sets.NewString()

// newControllerInitializers is a private map of named controller groups (you can start more than one in an init func)
// paired to their initFunc.  This allows for structured downstream composition and subdivision.
func newControllerInitializers() map[string]initFunc {
	controllers := map[string]initFunc{}
	controllers["cloud-node"] = startCloudNodeController
	controllers["cloud-node-lifecycle"] = startCloudNodeLifecycleController
	controllers["service"] = startServiceController
	controllers["route"] = startRouteController
	controllers["node-ipam"] = startNodeIpamController
	return controllers
}

// CreateControllerContext creates a context struct containing references to resources needed by the
// controllers such as the cloud provider and clientBuilder. rootClientBuilder is only used for
// the shared-informers client and token controller.
func CreateControllerContext(s *cloudcontrollerconfig.CompletedConfig, clientBuilder clientbuilder.ControllerClientBuilder, stop <-chan struct{}) (genericcontrollermanager.ControllerContext, error) {
	versionedClient := clientBuilder.ClientOrDie("shared-informers")
	sharedInformers := informers.NewSharedInformerFactory(versionedClient, ResyncPeriod(s)())

	metadataClient := metadata.NewForConfigOrDie(clientBuilder.ConfigOrDie("metadata-informers"))
	metadataInformers := metadatainformer.NewSharedInformerFactory(metadataClient, ResyncPeriod(s)())

	// If apiserver is not running we should wait for some time and fail only then. This is particularly
	// important when we start apiserver and controller manager at the same time.
	if err := genericcontrollermanager.WaitForAPIServer(versionedClient, 10*time.Second); err != nil {
		return genericcontrollermanager.ControllerContext{}, fmt.Errorf("failed to wait for apiserver being healthy: %w", err)
	}

	// Use a discovery client capable of being refreshed.
	discoveryClient := clientBuilder.ClientOrDie("controller-discovery")
	cachedClient := cacheddiscovery.NewMemCacheClient(discoveryClient.Discovery())
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(cachedClient)
	go wait.Until(func() {
		restMapper.Reset()
	}, 30*time.Second, stop)

	availableResources, err := GetAvailableResources(clientBuilder)
	if err != nil {
		return genericcontrollermanager.ControllerContext{}, err
	}

	ctx := genericcontrollermanager.ControllerContext{
		ClientBuilder:                   clientBuilder,
		InformerFactory:                 sharedInformers,
		ObjectOrMetadataInformerFactory: informerfactory.NewInformerFactory(sharedInformers, metadataInformers),
		RESTMapper:                      restMapper,
		AvailableResources:              availableResources,
		Stop:                            stop,
		InformersStarted:                make(chan struct{}),
		ResyncPeriod:                    ResyncPeriod(s),
		ControllerManagerMetrics:        controllersmetrics.NewControllerManagerMetrics("cloud-controller-manager"),
	}
	return ctx, nil
}

// ResyncPeriod returns a function which generates a duration each time it is
// invoked; this is so that multiple controllers don't get into lock-step and all
// hammer the apiserver with list requests simultaneously.
func ResyncPeriod(c *cloudcontrollerconfig.CompletedConfig) func() time.Duration {
	return func() time.Duration {
		n, _ := rand.Int(rand.Reader, big.NewInt(1000))
		factor := float64(n.Int64())/1000.0 + 1.0
		return time.Duration(float64(c.ComponentConfig.Generic.MinResyncPeriod.Nanoseconds()) * factor)
	}
}

// GetAvailableResources gets the map which contains all available resources of the apiserver
// TODO: In general, any controller checking this needs to be dynamic so
// users don't have to restart their controller manager if they change the apiserver.
// Until we get there, the structure here needs to be exposed for the construction of a proper ControllerContext.
func GetAvailableResources(clientBuilder clientbuilder.ControllerClientBuilder) (map[schema.GroupVersionResource]bool, error) {
	client := clientBuilder.ClientOrDie("controller-discovery")
	discoveryClient := client.Discovery()
	_, resourceMap, err := discoveryClient.ServerGroupsAndResources()
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("unable to get all supported resources from server: %w", err))
	}
	if len(resourceMap) == 0 {
		return nil, fmt.Errorf("unable to get any supported resources from server")
	}

	allResources := map[schema.GroupVersionResource]bool{}
	for _, apiResourceList := range resourceMap {
		version, err := schema.ParseGroupVersion(apiResourceList.GroupVersion)
		if err != nil {
			return nil, err
		}
		for _, apiResource := range apiResourceList.APIResources {
			allResources[version.WithResource(apiResource.Name)] = true
		}
	}

	return allResources, nil
}
