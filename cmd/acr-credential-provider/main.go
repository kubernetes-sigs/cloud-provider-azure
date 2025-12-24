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
	"math/rand"
	"os"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/component-base/logs"

	"sigs.k8s.io/cloud-provider-azure/pkg/credentialprovider"
	"sigs.k8s.io/cloud-provider-azure/pkg/log"
	"sigs.k8s.io/cloud-provider-azure/pkg/version"
)

func main() {
	logger := log.Background().WithName("main")
	rand.Seed(time.Now().UnixNano())

	var RegistryMirrorStr string

	command := &cobra.Command{
		Use:     "acr-credential-provider configFile",
		Short:   "Acr credential provider for Kubelet",
		Long:    `The acr credential provider is responsible for providing ACR credentials for kubelet`,
		Args:    cobra.MinimumNArgs(1),
		Version: version.Get().GitVersion,
		Run: func(_ *cobra.Command, args []string) {
			if len(args) != 1 {
				logger.Error(nil, "Config file is not specified")
				os.Exit(1)
			}

			acrProvider, err := credentialprovider.NewAcrProviderFromConfig(args[0], RegistryMirrorStr)
			if err != nil {
				logger.Error(err, "Failed to initialize ACR provider")
				os.Exit(1)
			}

			if err := NewCredentialProvider(acrProvider).Run(context.TODO()); err != nil {
				logger.Error(err, "Error running acr credential provider")
				os.Exit(1)
			}
		},
	}

	logs.InitLogs()
	defer logs.FlushLogs()

	// Flags
	command.Flags().StringVarP(&RegistryMirrorStr, "registry-mirror", "r", "",
		"Mirror a source registry host to a target registry host, and image pull credential will be requested to the target registry host when the image is from source registry host")

	logs.AddFlags(command.Flags())
	if err := command.Execute(); err != nil {
		os.Exit(1)
	}
}
