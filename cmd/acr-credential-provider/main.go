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
	"k8s.io/klog/v2"

	"sigs.k8s.io/cloud-provider-azure/pkg/credentialprovider"
)

func main() {
	rand.Seed(time.Now().UnixNano())
	command := &cobra.Command{
		Use:   "acr-credential-provider configFile",
		Short: "Acr credential provider for Kubelet",
		Long:  `The acr credential provider is responsible for providing ACR credentials for kubelet`,
		Args:  cobra.MinimumNArgs(1),
		Run: func(_ *cobra.Command, args []string) {
			if len(args) != 1 {
				klog.Errorf("Config file is not specified")
				os.Exit(1)
			}

			acrProvider, err := credentialprovider.NewAcrProvider(args[0])
			if err != nil {
				klog.Errorf("Failed to initialize ACR provider: %v", err)
				os.Exit(1)
			}

			if err := NewCredentialProvider(acrProvider).Run(context.TODO()); err != nil {
				klog.Errorf("Error running acr credential provider: %v", err)
				os.Exit(1)
			}
		},
	}

	logs.InitLogs()
	defer logs.FlushLogs()

	if err := command.Execute(); err != nil {
		os.Exit(1)
	}
}
