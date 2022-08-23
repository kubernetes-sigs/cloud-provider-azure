/*
Copyright 2022 The Kubernetes Authors.

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

package deployer

import (
	"context"
	"flag"
	"os"
	"path/filepath"

	"github.com/octago/sflags/gen/gpflag"
	"github.com/spf13/pflag"
	"k8s.io/klog"
	"sigs.k8s.io/kubetest2/pkg/types"
)

// Name is the name of the deployer
const Name = "aks"

var (
	GitTag string

	subscriptionID = os.Getenv("AZURE_SUBSCRIPTION_ID")
	tenantID       = os.Getenv("AZURE_TENANT_ID")
	clientID       = os.Getenv("AZURE_CLIENT_ID")
	clientSecret   = os.Getenv("AZURE_CLIENT_SECRET")
	imageRegistry  = os.Getenv("IMAGE_REGISTRY")
	ctx            = context.Background()
)

type deployer struct {
	// generic parts
	commonOptions types.Options

	*BuildOptions
	*UpOptions

	// aks specific details
	KubeconfigPath    string `flag:"kubeconfig" desc:"--kubeconfig flag for aks create cluster"`
	ResourceGroupName string `flag:"rgName" desc:"--rgName flag for resource group name"`
}

// New implements deployer.New for aks
func New(opts types.Options) (types.Deployer, *pflag.FlagSet) {
	// create a deployer object and set fields that are not flag controlled
	d := &deployer{
		commonOptions: opts,
		// logsDir:       filepath.Join(opts.RunDir(), "logs"),
	}
	// register flags and return
	return d, bindFlags(d)
}

func (d *deployer) DumpClusterLogs() error {
	return nil
}

func (d *deployer) Kubeconfig() (string, error) {
	if d.KubeconfigPath != "" {
		return d.KubeconfigPath, nil
	}
	if kconfig, ok := os.LookupEnv("KUBECONFIG"); ok {
		return kconfig, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kube", "config"), nil
}

func (d *deployer) Version() string {
	return GitTag
}

// bindFlags is a helper used to create & bind a flagset to the deployer
func bindFlags(d *deployer) *pflag.FlagSet {
	flags, err := gpflag.Parse(d)
	if err != nil {
		klog.Fatalf("unable to generate flags from deployer")
		return nil
	}

	klog.InitFlags(nil)
	flags.AddGoFlagSet(flag.CommandLine)

	return flags
}

// assert that deployer implements types.DeployerWithKubeconfig
var _ types.DeployerWithKubeconfig = &deployer{}
