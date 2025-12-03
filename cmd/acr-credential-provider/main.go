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

// The acr credential provider is responsible for providing ACR credentials for kubelet.

package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/component-base/logs"
	"k8s.io/klog/v2"

	"sigs.k8s.io/cloud-provider-azure/cmd/acr-credential-provider/pkg/config"
	"sigs.k8s.io/cloud-provider-azure/pkg/version"
)

func main() {
	rand.Seed(time.Now().UnixNano())

	var (
		RegistryMirrorStr string
		IBSNIName         string
		IBDefaultClient   string
		IBDefaultTenant   string
		IBAPIIP           string
	)

	command := &cobra.Command{
		Use:   "acr-credential-provider configFile",
		Short: "Acr credential provider for Kubelet",
		Long:  `The acr credential provider is responsible for providing ACR credentials for kubelet`,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				return errors.New("config file is not specified")
			}
			if len(args) > 1 {
				return fmt.Errorf("expected exactly one argument (config file); Got arguments: %v", args)
			}
			return nil
		},
		Version: version.Get().GitVersion,
		RunE: func(_ *cobra.Command, args []string) error {
			ibConfig, err := config.ParseIdentityBindingsConfig(IBSNIName, IBDefaultClient, IBDefaultTenant, IBAPIIP)
			if err != nil {
				klog.Errorf("Error parsing identity bindings config: %v", err)
				return err
			}
			if err := NewCredentialProvider(args[0], RegistryMirrorStr, ibConfig).Run(context.TODO()); err != nil {
				klog.Errorf("Error running acr credential provider: %v", err)
				return err
			}
			return nil
		},
	}

	logs.InitLogs()
	defer logs.FlushLogs()

	// Flags
	command.Flags().StringVarP(&RegistryMirrorStr, "registry-mirror", "r", "",
		"Mirror a source registry host to a target registry host, and image pull credential will be requested to the target registry host when the image is from source registry host")
	command.Flags().StringVar(&IBSNIName, config.FlagIBSNIName, "",
		"SNI name for identity bindings")
	command.Flags().StringVar(&IBDefaultClient, config.FlagIBDefaultClient, "",
		"Default Azure AD client ID for identity bindings")
	command.Flags().StringVar(&IBDefaultTenant, config.FlagIBDefaultTenant, "",
		"Default Azure AD tenant ID for identity bindings")
	command.Flags().StringVar(&IBAPIIP, config.FlagIBAPIIP, "",
		"API server IP address for identity bindings endpoint")

	logs.AddFlags(command.Flags())
	if err := func() error {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		defer cancel()
		return command.ExecuteContext(ctx)
	}(); err != nil {
		os.Exit(1)
	}
}
