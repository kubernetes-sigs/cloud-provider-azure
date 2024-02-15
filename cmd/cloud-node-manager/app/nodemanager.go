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
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/client-go/tools/cache"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/cli/globalflag"
	"k8s.io/component-base/config"
	"k8s.io/component-base/term"
	genericcontrollermanager "k8s.io/controller-manager/app"
	controllerhealthz "k8s.io/controller-manager/pkg/healthz"
	"k8s.io/klog/v2"

	cloudnodeconfig "sigs.k8s.io/cloud-provider-azure/cmd/cloud-node-manager/app/config"
	"sigs.k8s.io/cloud-provider-azure/cmd/cloud-node-manager/app/options"
	nodeprovider "sigs.k8s.io/cloud-provider-azure/pkg/node"
	"sigs.k8s.io/cloud-provider-azure/pkg/nodemanager"
	"sigs.k8s.io/cloud-provider-azure/pkg/version"
	"sigs.k8s.io/cloud-provider-azure/pkg/version/verflag"
)

// NewCloudNodeManagerCommand creates a *cobra.Command object with default parameters
func NewCloudNodeManagerCommand() *cobra.Command {
	s, err := options.NewCloudNodeManagerOptions()
	if err != nil {
		klog.Fatalf("unable to initialize command options: %v", err)
	}

	cmd := &cobra.Command{
		Use:  "cloud-node-manager",
		Long: `The Cloud node manager is a daemon that reconciles node information for its running node.`,
		Run: func(cmd *cobra.Command, args []string) {
			verflag.PrintAndExitIfRequested("Cloud Node Manager")
			cliflag.PrintFlags(cmd.Flags())

			c, err := s.Config()
			if err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(1)
			}

			if err := initForOS(c.WindowsService); err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(1)
			}

			if err := Run(context.Background(), c); err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(1)
			}

		},
	}

	fs := cmd.Flags()
	namedFlagSets := s.Flags()
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

// Run runs the ExternalCMServer.  This should never exit.
func Run(ctx context.Context, c *cloudnodeconfig.Config) error {
	// To help debugging, immediately log version and nodeName
	klog.Infof("Version: %+v", version.Get())
	klog.Infof("NodeName: %s", c.NodeName)
	defer utilruntime.HandleCrash()

	if c.SecureServing != nil {
		// Start the controller manager HTTP server
		var checks []healthz.HealthChecker
		healthzHandler := controllerhealthz.NewMutableHealthzHandler(checks...)
		healthzHandler.AddHealthChecker(controllerhealthz.NamedPingChecker(c.NodeName))
		unsecuredMux := genericcontrollermanager.NewBaseHandler(&config.DebuggingConfiguration{}, healthzHandler)
		handler := genericcontrollermanager.BuildHandlerChain(unsecuredMux, &c.Authorization, &c.Authentication)
		// TODO: handle stoppedCh returned by c.SecureServing.Serve
		if _, _, err := c.SecureServing.Serve(handler, 0, ctx.Done()); err != nil {
			return err
		}
		return nil
	}

	klog.V(1).Infof("Starting cloud-node-manager...")
	// If apiserver is not running we should wait for some time and fail only then. This is particularly
	// important when we start node manager before apiserver starts.
	if err := genericcontrollermanager.WaitForAPIServer(c.VersionedClient, 10*time.Second); err != nil {
		klog.Fatalf("Failed to wait for apiserver being healthy: %v", err)
	}
	nodeInformer := c.SharedInformers.Core().V1().Nodes()
	// Start the CloudNodeController
	nodeController := nodemanager.NewCloudNodeController(
		c.NodeName,
		nodeInformer.Lister(),
		// cloud node controller uses existing cluster role from node-controller
		c.ClientBuilder.ClientOrDie("node-controller"),
		nodeprovider.NewNodeProvider(ctx, c.UseInstanceMetadata, c.CloudConfigFilePath),
		c.NodeStatusUpdateFrequency.Duration,
		c.WaitForRoutes,
		c.EnableDeprecatedBetaTopologyLabels)

	// Use shared informer to listen to add/update of nodes. Note that any nodes
	// that exist before node controller starts will show up in the update method
	_, err := nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { nodeController.AddCloudNode(ctx, obj) },
		UpdateFunc: func(oldObj, newObj interface{}) { nodeController.UpdateCloudNode(ctx, oldObj, newObj) },
	})
	if err != nil {
		klog.Fatalf("Failed to register event handler with shared informer: %v", err)
		return err
	}

	c.SharedInformers.Start(ctx.Done())
	defer c.SharedInformers.Shutdown()
	c.SharedInformers.WaitForCacheSync(ctx.Done())
	klog.Infof("Started cloud-node-manager")
	// The following loops run communicate with the APIServer with a worst case complexity
	// of O(num_nodes) per cycle. These functions are justified here because these events fire
	// very infrequently. DO NOT MODIFY this to perform frequent operations.

	// Start a loop to periodically update the node addresses obtained from the cloud
	wait.UntilWithContext(ctx, func(ctx context.Context) { nodeController.UpdateNodeStatus(ctx) }, c.NodeStatusUpdateFrequency.Duration)
	return nil
}
