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
	"time"

	"github.com/spf13/cobra"

	"k8s.io/apiserver/pkg/server/healthz"
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
		RunE: func(cmd *cobra.Command, _ []string) error {
			verflag.PrintAndExitIfRequested("Cloud Node Manager")
			cliflag.PrintFlags(cmd.Flags())

			c, err := s.Config()
			if err != nil {
				return err
			}

			if err := initForOS(c.WindowsService); err != nil {
				return err
			}

			if err := Run(cmd.Context(), c); err != nil {
				return err
			}
			return nil
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
	cmd.SetHelpFunc(func(cmd *cobra.Command, _ []string) {
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

	// Start the controller manager HTTP server
	var checks []healthz.HealthChecker
	healthzHandler := controllerhealthz.NewMutableHealthzHandler(checks...)
	if c.SecureServing != nil {
		unsecuredMux := genericcontrollermanager.NewBaseHandler(&config.DebuggingConfiguration{}, healthzHandler)
		handler := genericcontrollermanager.BuildHandlerChain(unsecuredMux, &c.Authorization, &c.Authentication)
		// TODO: handle stoppedCh returned by c.SecureServing.Serve
		if _, _, err := c.SecureServing.Serve(handler, 0, ctx.Done()); err != nil {
			return err
		}
	}

	run := func(ctx context.Context) {
		if err := startControllers(ctx, c, healthzHandler); err != nil {
			klog.Fatalf("error running controllers: %v", err)
		}
	}

	run(context.TODO())
	panic("unreachable")
}

// startControllers starts the cloud specific controller loops.
func startControllers(ctx context.Context, c *cloudnodeconfig.Config, healthzHandler *controllerhealthz.MutableHealthzHandler) error {
	klog.V(1).Infof("Starting cloud-node-manager...")

	// Start the CloudNodeController
	nodeController := nodemanager.NewCloudNodeController(
		c.NodeName,
		c.SharedInformers.Core().V1().Nodes(),
		// cloud node controller uses existing cluster role from node-controller
		c.ClientBuilder.ClientOrDie("node-controller"),
		nodeprovider.NewNodeProvider(ctx, c.UseInstanceMetadata, c.CloudConfigFilePath),
		c.NodeStatusUpdateFrequency.Duration,
		c.WaitForRoutes,
		c.EnableDeprecatedBetaTopologyLabels)

	go nodeController.Run(ctx)

	check := controllerhealthz.NamedPingChecker(c.NodeName)
	healthzHandler.AddHealthChecker(check)

	klog.Infof("Started cloud-node-manager")

	// If apiserver is not running we should wait for some time and fail only then. This is particularly
	// important when we start node manager before apiserver starts.
	if err := genericcontrollermanager.WaitForAPIServer(c.VersionedClient, 10*time.Second); err != nil {
		klog.Fatalf("Failed to wait for apiserver being healthy: %v", err)
	}

	c.SharedInformers.Start(ctx.Done())

	select {}
}
