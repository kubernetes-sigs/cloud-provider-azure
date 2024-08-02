/*
Copyright 2017 The Kubernetes Authors.

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
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/spf13/pflag"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/diff"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/apis/apiserver"
	apiserveroptions "k8s.io/apiserver/pkg/server/options"
	cpconfig "k8s.io/cloud-provider/config"
	serviceconfig "k8s.io/cloud-provider/controllers/service/config"
	"k8s.io/cloud-provider/names"
	cpoptions "k8s.io/cloud-provider/options"
	componentbaseconfig "k8s.io/component-base/config"
	kubectrlmgrconfig "k8s.io/controller-manager/config"
	cmoptions "k8s.io/controller-manager/options"
	"k8s.io/controller-manager/pkg/leadermigration/options"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/nodeipam/config"
)

func TestDefaultFlags(t *testing.T) {
	s, _ := NewCloudControllerManagerOptions()

	expected := &CloudControllerManagerOptions{
		Generic: &cmoptions.GenericControllerManagerConfigurationOptions{
			GenericControllerManagerConfiguration: &kubectrlmgrconfig.GenericControllerManagerConfiguration{
				Address:         "0.0.0.0",
				MinResyncPeriod: metav1.Duration{Duration: 12 * time.Hour},
				ClientConnection: componentbaseconfig.ClientConnectionConfiguration{
					ContentType: "application/vnd.kubernetes.protobuf",
					QPS:         20.0,
					Burst:       30,
				},
				ControllerStartInterval: metav1.Duration{Duration: 0},
				LeaderElection: componentbaseconfig.LeaderElectionConfiguration{
					ResourceLock:      "leases",
					LeaderElect:       true,
					LeaseDuration:     metav1.Duration{Duration: 15 * time.Second},
					RenewDeadline:     metav1.Duration{Duration: 10 * time.Second},
					RetryPeriod:       metav1.Duration{Duration: 2 * time.Second},
					ResourceName:      "cloud-controller-manager",
					ResourceNamespace: "kube-system",
				},
				Controllers: []string{"*"},
			},
			Debugging: &cmoptions.DebuggingOptions{
				DebuggingConfiguration: &componentbaseconfig.DebuggingConfiguration{
					EnableProfiling:           true,
					EnableContentionProfiling: false,
				},
			},
			LeaderMigration: &options.LeaderMigrationOptions{},
		},
		KubeCloudShared: &cpoptions.KubeCloudSharedOptions{
			KubeCloudSharedConfiguration: &cpconfig.KubeCloudSharedConfiguration{
				RouteReconciliationPeriod: metav1.Duration{Duration: 10 * time.Second},
				NodeMonitorPeriod:         metav1.Duration{Duration: 5 * time.Second},
				ClusterName:               "kubernetes",
				ClusterCIDR:               "",
				AllocateNodeCIDRs:         false,
				CIDRAllocatorType:         "",
				ConfigureCloudRoutes:      true,
			},
			CloudProvider: &cpoptions.CloudProviderOptions{
				CloudProviderConfiguration: &cpconfig.CloudProviderConfiguration{
					Name:            "azure",
					CloudConfigFile: "",
				},
			},
		},
		ServiceController: &cpoptions.ServiceControllerOptions{
			ServiceControllerConfiguration: &serviceconfig.ServiceControllerConfiguration{
				ConcurrentServiceSyncs: 1,
			},
		},
		NodeIPAMController: &NodeIPAMControllerOptions{
			NodeIPAMControllerConfiguration: &config.NodeIPAMControllerConfiguration{
				NodeCIDRMaskSize: consts.DefaultNodeCIDRMaskSize,
			},
		},
		SecureServing: (&apiserveroptions.SecureServingOptions{
			BindPort:    10258,
			BindAddress: net.ParseIP("0.0.0.0"),
			ServerCert: apiserveroptions.GeneratableKeyCert{
				CertDirectory: "",
				PairName:      "cloud-controller-manager",
			},
			HTTP2MaxStreamsPerConnection: 0,
		}).WithLoopback(),
		Authentication: &apiserveroptions.DelegatingAuthenticationOptions{
			CacheTTL:            10 * time.Second,
			TokenRequestTimeout: 10 * time.Second,
			WebhookRetryBackoff: apiserveroptions.DefaultAuthWebhookRetryBackoff(),
			ClientCert:          apiserveroptions.ClientCertAuthenticationOptions{},
			RequestHeader: apiserveroptions.RequestHeaderAuthenticationOptions{
				UsernameHeaders:     []string{"x-remote-user"},
				GroupHeaders:        []string{"x-remote-group"},
				ExtraHeaderPrefixes: []string{"x-remote-extra-"},
			},
			RemoteKubeConfigFileOptional: true,
			Anonymous:                    &apiserver.AnonymousAuthConfig{Enabled: true},
		},
		Authorization: &apiserveroptions.DelegatingAuthorizationOptions{
			AllowCacheTTL:                10 * time.Second,
			DenyCacheTTL:                 10 * time.Second,
			RemoteKubeConfigFileOptional: true,
			AlwaysAllowPaths:             []string{"/healthz", "/readyz", "/livez"}, // note: this does not match /healthz/ or /healthz/*
			AlwaysAllowGroups:            []string{"system:masters"},
			WebhookRetryBackoff:          &wait.Backoff{Duration: 500 * time.Millisecond, Factor: 1.5, Jitter: 0.2, Steps: 5},
			ClientTimeout:                10 * time.Second,
		},
		Kubeconfig:                "",
		Master:                    "",
		NodeStatusUpdateFrequency: metav1.Duration{Duration: 5 * time.Minute},
		DynamicReloading: &DynamicReloadingOptions{
			EnableDynamicReloading:     false,
			CloudConfigSecretName:      "azure-cloud-provider",
			CloudConfigSecretNamespace: "kube-system",
			CloudConfigKey:             "",
		},
	}
	if !reflect.DeepEqual(expected, s) {
		t.Errorf("Got different run options than expected.\nDifference detected on:\n%s", diff.ObjectReflectDiff(expected, s))
	}
}

func TestAddFlags(t *testing.T) {
	fs := pflag.NewFlagSet("addflagstest", pflag.ContinueOnError)
	s, _ := NewCloudControllerManagerOptions()
	for _, f := range s.Flags([]string{""}, []string{""}).FlagSets {
		fs.AddFlagSet(f)
	}

	args := []string{
		"--allocate-node-cidrs=true",
		"--bind-address=192.168.4.21",
		"--cert-dir=/a/b/c",
		"--cloud-config=/cloud-config",
		"--cloud-provider=azure",
		"--cluster-cidr=1.2.3.4/24",
		"--cluster-name=k8s",
		"--configure-cloud-routes=false",
		"--contention-profiling=true",
		"--controller-start-interval=2m",
		"--controllers=foo,bar",
		"--http2-max-streams-per-connection=47",
		"--kube-api-burst=100",
		"--kube-api-content-type=application/vnd.kubernetes.protobuf",
		"--kube-api-qps=50.0",
		"--kubeconfig=/kubeconfig",
		"--leader-elect=false",
		"--leader-elect-lease-duration=30s",
		"--leader-elect-renew-deadline=15s",
		"--leader-elect-resource-lock=configmap",
		"--leader-elect-retry-period=5s",
		"--master=192.168.4.20",
		"--min-resync-period=100m",
		"--node-status-update-frequency=10m",
		"--profiling=false",
		"--route-reconciliation-period=30s",
		"--secure-port=10001",
		"--use-service-account-credentials=false",
		"--enable-dynamic-reloading=true",
		"--cloud-config-secret-name=test-secret",
	}
	err := fs.Parse(args)
	if err != nil {
		t.Fatalf("Parse flags failed: %v", err)
	}

	expected := &CloudControllerManagerOptions{
		Generic: &cmoptions.GenericControllerManagerConfigurationOptions{
			GenericControllerManagerConfiguration: &kubectrlmgrconfig.GenericControllerManagerConfiguration{
				Address:         "0.0.0.0",
				MinResyncPeriod: metav1.Duration{Duration: 100 * time.Minute},
				ClientConnection: componentbaseconfig.ClientConnectionConfiguration{
					ContentType: "application/vnd.kubernetes.protobuf",
					QPS:         50.0,
					Burst:       100,
				},
				ControllerStartInterval: metav1.Duration{Duration: 2 * time.Minute},
				LeaderElection: componentbaseconfig.LeaderElectionConfiguration{
					ResourceLock:      "configmap",
					LeaderElect:       false,
					LeaseDuration:     metav1.Duration{Duration: 30 * time.Second},
					RenewDeadline:     metav1.Duration{Duration: 15 * time.Second},
					RetryPeriod:       metav1.Duration{Duration: 5 * time.Second},
					ResourceName:      "cloud-controller-manager",
					ResourceNamespace: "kube-system",
				},
				Controllers: []string{"foo", "bar"},
			},
			Debugging: &cmoptions.DebuggingOptions{
				DebuggingConfiguration: &componentbaseconfig.DebuggingConfiguration{
					EnableContentionProfiling: true,
				},
			},
			LeaderMigration: &options.LeaderMigrationOptions{},
		},
		KubeCloudShared: &cpoptions.KubeCloudSharedOptions{
			KubeCloudSharedConfiguration: &cpconfig.KubeCloudSharedConfiguration{
				RouteReconciliationPeriod: metav1.Duration{Duration: 30 * time.Second},
				NodeMonitorPeriod:         metav1.Duration{Duration: 5 * time.Second},
				ClusterName:               "k8s",
				ClusterCIDR:               "1.2.3.4/24",
				AllocateNodeCIDRs:         true,
				CIDRAllocatorType:         "RangeAllocator",
				ConfigureCloudRoutes:      false,
			},
			CloudProvider: &cpoptions.CloudProviderOptions{
				CloudProviderConfiguration: &cpconfig.CloudProviderConfiguration{
					Name:            "azure",
					CloudConfigFile: "/cloud-config",
				},
			},
		},
		ServiceController: &cpoptions.ServiceControllerOptions{
			ServiceControllerConfiguration: &serviceconfig.ServiceControllerConfiguration{
				ConcurrentServiceSyncs: 1,
			},
		},
		NodeIPAMController: &NodeIPAMControllerOptions{
			NodeIPAMControllerConfiguration: &config.NodeIPAMControllerConfiguration{
				NodeCIDRMaskSize: consts.DefaultNodeCIDRMaskSize,
			},
		},
		SecureServing: (&apiserveroptions.SecureServingOptions{
			BindPort:    10001,
			BindAddress: net.ParseIP("192.168.4.21"),
			ServerCert: apiserveroptions.GeneratableKeyCert{
				CertDirectory: "/a/b/c",
				PairName:      "cloud-controller-manager",
			},
			HTTP2MaxStreamsPerConnection: 47,
		}).WithLoopback(),
		Authentication: &apiserveroptions.DelegatingAuthenticationOptions{
			CacheTTL:            10 * time.Second,
			TokenRequestTimeout: 10 * time.Second,
			WebhookRetryBackoff: apiserveroptions.DefaultAuthWebhookRetryBackoff(),
			ClientCert:          apiserveroptions.ClientCertAuthenticationOptions{},
			RequestHeader: apiserveroptions.RequestHeaderAuthenticationOptions{
				UsernameHeaders:     []string{"x-remote-user"},
				GroupHeaders:        []string{"x-remote-group"},
				ExtraHeaderPrefixes: []string{"x-remote-extra-"},
			},
			Anonymous:                    &apiserver.AnonymousAuthConfig{Enabled: true},
			RemoteKubeConfigFileOptional: true,
		},
		Authorization: &apiserveroptions.DelegatingAuthorizationOptions{
			AllowCacheTTL:                10 * time.Second,
			DenyCacheTTL:                 10 * time.Second,
			RemoteKubeConfigFileOptional: true,
			AlwaysAllowPaths:             []string{"/healthz", "/readyz", "/livez"}, // note: this does not match /healthz/ or /healthz/*// note: this does not match /healthz/ or
			AlwaysAllowGroups:            []string{"system:masters"},
			WebhookRetryBackoff:          &wait.Backoff{Duration: 500 * time.Millisecond, Factor: 1.5, Jitter: 0.2, Steps: 5},
			ClientTimeout:                10 * time.Second,
		},
		Kubeconfig:                "/kubeconfig",
		Master:                    "192.168.4.20",
		NodeStatusUpdateFrequency: metav1.Duration{Duration: 10 * time.Minute},
		DynamicReloading: &DynamicReloadingOptions{
			EnableDynamicReloading:     true,
			CloudConfigSecretName:      "test-secret",
			CloudConfigSecretNamespace: "kube-system",
			CloudConfigKey:             "cloud-config",
		},
	}
	if !reflect.DeepEqual(expected, s) {
		t.Errorf("Got different run options than expected.\nDifference detected on:\n%s", diff.ObjectReflectDiff(expected, s))
	}
}

func TestValidate(t *testing.T) {
	testCases := []struct {
		desc                                      string
		expected                                  string
		generateTestCloudControllerManagerOptions func() *CloudControllerManagerOptions
	}{
		{
			desc:     "should not return an error when validating default options",
			expected: "",
			generateTestCloudControllerManagerOptions: func() *CloudControllerManagerOptions {
				s, _ := NewCloudControllerManagerOptions()
				s.DynamicReloading.EnableDynamicReloading = true
				return s
			},
		},
		{
			desc:     "should return an error when validating options with empty cloud provider",
			expected: "--cloud-provider cannot be empty",
			generateTestCloudControllerManagerOptions: func() *CloudControllerManagerOptions {
				s, _ := NewCloudControllerManagerOptions()
				s.KubeCloudShared.CloudProvider.Name = ""
				s.KubeCloudShared.CloudProvider.CloudConfigFile = "azure.json"
				return s
			},
		},
		{
			desc:     "should return an error when validating options with concurrent service syncs not equal to 1",
			expected: "--concurrent-service-syncs is limited to 1 only",
			generateTestCloudControllerManagerOptions: func() *CloudControllerManagerOptions {
				s, _ := NewCloudControllerManagerOptions()
				s.ServiceController.ConcurrentServiceSyncs = 10
				s.KubeCloudShared.CloudProvider.CloudConfigFile = "azure.json"
				return s
			},
		},
		{
			desc:     "should return an error if the cloud config file is empty and the dynamic reloading is not enabled",
			expected: "--cloud-config cannot be empty when --enable-dynamic-reloading is not set to true",
			generateTestCloudControllerManagerOptions: func() *CloudControllerManagerOptions {
				s, _ := NewCloudControllerManagerOptions()
				return s
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			s := tc.generateTestCloudControllerManagerOptions()
			err := s.Validate([]string{}, []string{}, names.CCMControllerAliases())
			var errMsg string
			if err == nil {
				errMsg = ""
			} else {
				errMsg = err.Error()
			}
			if errMsg != tc.expected {
				t.Errorf("Expected error '%s' but got error '%s'", tc.expected, err)
			}
		})
	}
}
