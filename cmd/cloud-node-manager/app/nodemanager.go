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
	"fmt"
	"time"

	"github.com/spf13/cobra"

	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/cli/globalflag"
	"k8s.io/component-base/config"
	"k8s.io/component-base/term"
	genericcontrollermanager "k8s.io/controller-manager/app"
	controllerhealthz "k8s.io/controller-manager/pkg/healthz"
	"k8s.io/klog/v2"

	"sigs.k8s.io/cloud-provider-azure/cmd/cloud-node-manager/app/options"
	nodeprovider "sigs.k8s.io/cloud-provider-azure/pkg/node"
	"sigs.k8s.io/cloud-provider-azure/pkg/nodemanager"
	"sigs.k8s.io/cloud-provider-azure/pkg/version"
	"sigs.k8s.io/cloud-provider-azure/pkg/version/verflag"
)

var cloudNodeManagerOptions = options.NewCloudNodeManagerOptions()

func init() {
	fs := Rootcmd.Flags()
	namedFlagSets := cloudNodeManagerOptions.Flags()
	verflag.AddFlags(namedFlagSets.FlagSet("global"))
	globalflag.AddGlobalFlags(namedFlagSets.FlagSet("global"), Rootcmd.Name())
	for _, f := range namedFlagSets.FlagSets {
		fs.AddFlagSet(f)
	}

	usageFmt := "Usage:\n  %s\n"
	cols, _, _ := term.TerminalSize(Rootcmd.OutOrStdout())
	Rootcmd.SetUsageFunc(func(cmd *cobra.Command) error {
		fmt.Fprintf(cmd.OutOrStderr(), usageFmt, cmd.UseLine())
		cliflag.PrintSections(Rootcmd.OutOrStderr(), namedFlagSets, cols)
		return nil
	})
	Rootcmd.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		fmt.Fprintf(cmd.OutOrStdout(), "%s\n\n"+usageFmt, cmd.Long, cmd.UseLine())
		cliflag.PrintSections(cmd.OutOrStdout(), namedFlagSets, cols)
	})
}

// NewCloudNodeManagerCommand creates a *cobra.Command object with default parameters
var Rootcmd = &cobra.Command{
	Use:  "cloud-node-manager",
	Long: `The Cloud node manager is a daemon that reconciles node information for its running node.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		verflag.PrintAndExitIfRequested("Cloud Node Manager")
		cliflag.PrintFlags(cmd.Flags())

		c, err := cloudNodeManagerOptions.Config()
		if err != nil {
			return err
		}
		ctx := cmd.Context()
		// To help debugging, immediately log version and nodeName
		klog.Infof("Version: %+v", version.Get())
		klog.Infof("NodeName: %s", c.NodeName)

		// Start the controller manager HTTP server
		if c.SecureServing != nil {
			check := controllerhealthz.NamedPingChecker(c.NodeName)
			healthzHandler := controllerhealthz.NewMutableHealthzHandler()
			healthzHandler.AddHealthChecker(check)
			unsecuredMux := genericcontrollermanager.NewBaseHandler(&config.DebuggingConfiguration{}, healthzHandler)
			handler := genericcontrollermanager.BuildHandlerChain(unsecuredMux, &c.Authorization, &c.Authentication)
			// no need to handle stoppedCh returned by c.SecureServing.Serve because here is health checker.
			if _, _, err := c.SecureServing.Serve(handler, 0, ctx.Done()); err != nil {
				return err
			}
		}

		klog.V(1).Infof("Starting cloud-node-manager...")
		var nodeProvider nodemanager.NodeProvider

		if c.UseInstanceMetadata {
			nodeProvider = nodeprovider.NewIMDSNodeProvider(ctx)
		} else {
			nodeProvider = nodeprovider.NewARMNodeProvider(ctx, c.CloudConfigFilePath)
		}

		// Start the CloudNodeController
		nodeController := nodemanager.NewCloudNodeController(
			c.NodeName,
			c.SharedInformers.Core().V1().Nodes(),
			// cloud node controller uses existing cluster role from node-controller
			c.ClientBuilder.ClientOrDie("node-controller"),
			nodeProvider,
			c.NodeStatusUpdateFrequency.Duration,
			c.WaitForRoutes,
			c.EnableDeprecatedBetaTopologyLabels)

		// If apiserver is not running we should wait for some time and fail only then. This is particularly
		// important when we start node manager before apiserver starts.
		if err := genericcontrollermanager.WaitForAPIServer(c.VersionedClient, 10*time.Second); err != nil {
			klog.Fatalf("Failed to wait for apiserver being healthy: %v", err)
		}
		c.SharedInformers.Start(ctx.Done())
		go klog.Infof("Started cloud-node-manager")
		nodeController.Run(ctx)
		return nil
	},
}
