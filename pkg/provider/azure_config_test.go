/*
Copyright 2020 The Kubernetes Authors.

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

package provider

import (
	"context"
	"testing"

	"sigs.k8s.io/cloud-provider-azure/pkg/provider/config"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"

	"github.com/golang/mock/gomock"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/zoneclient/mockzoneclient"

	"github.com/Azure/go-autorest/autorest/to"
	"github.com/stretchr/testify/assert"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakeclient "k8s.io/client-go/kubernetes/fake"

	"sigs.k8s.io/yaml"
)

func getTestConfig() *Config {
	return &Config{
		AzureAuthConfig: config.AzureAuthConfig{
			TenantID:        "TenantID",
			SubscriptionID:  "SubscriptionID",
			AADClientID:     "AADClientID",
			AADClientSecret: "AADClientSecret",
		},
		ResourceGroup:               "ResourceGroup",
		RouteTableName:              "RouteTableName",
		RouteTableResourceGroup:     "RouteTableResourceGroup",
		Location:                    "Location",
		SubnetName:                  "SubnetName",
		VnetName:                    "VnetName",
		PrimaryAvailabilitySetName:  "PrimaryAvailabilitySetName",
		PrimaryScaleSetName:         "PrimaryScaleSetName",
		LoadBalancerSku:             "LoadBalancerSku",
		ExcludeMasterFromStandardLB: to.BoolPtr(true),
	}
}

func getTestCloudConfigTypeSecretConfig() *Config {
	return &Config{
		AzureAuthConfig: config.AzureAuthConfig{
			TenantID:       "TenantID",
			SubscriptionID: "SubscriptionID",
		},
		ResourceGroup:           "ResourceGroup",
		RouteTableName:          "RouteTableName",
		RouteTableResourceGroup: "RouteTableResourceGroup",
		SecurityGroupName:       "SecurityGroupName",
		CloudConfigType:         cloudConfigTypeSecret,
	}
}

func getTestCloudConfigTypeMergeConfig() *Config {
	return &Config{
		AzureAuthConfig: config.AzureAuthConfig{
			TenantID:       "TenantID",
			SubscriptionID: "SubscriptionID",
		},
		ResourceGroup:           "ResourceGroup",
		RouteTableName:          "RouteTableName",
		RouteTableResourceGroup: "RouteTableResourceGroup",
		SecurityGroupName:       "SecurityGroupName",
		CloudConfigType:         cloudConfigTypeMerge,
	}
}

func getTestCloudConfigTypeMergeConfigExpected() *Config {
	config := getTestConfig()
	config.SecurityGroupName = "SecurityGroupName"
	config.CloudConfigType = cloudConfigTypeMerge
	return config
}

func TestGetConfigFromSecret(t *testing.T) {
	emptyConfig := &Config{}
	badConfig := &Config{ResourceGroup: "DuplicateColumnsIncloud-config"}
	tests := []struct {
		name           string
		existingConfig *Config
		secretConfig   *Config
		expected       *Config
		expectErr      bool
	}{
		{
			name: "Azure config shouldn't be override when cloud config type is file",
			existingConfig: &Config{
				ResourceGroup:   "ResourceGroup1",
				CloudConfigType: cloudConfigTypeFile,
			},
			secretConfig: getTestConfig(),
			expected:     nil,
		},
		{
			name:           "Azure config should be override when cloud config type is secret",
			existingConfig: getTestCloudConfigTypeSecretConfig(),
			secretConfig:   getTestConfig(),
			expected:       getTestConfig(),
		},
		{
			name:           "Azure config should be override when cloud config type is merge",
			existingConfig: getTestCloudConfigTypeMergeConfig(),
			secretConfig:   getTestConfig(),
			expected:       getTestCloudConfigTypeMergeConfigExpected(),
		},
		{
			name:           "Error should be reported when secret doesn't exists",
			existingConfig: getTestCloudConfigTypeMergeConfig(),
			expectErr:      true,
		},
		{
			name:           "Error should be reported when secret exists but cloud-config data is not provided",
			existingConfig: getTestCloudConfigTypeMergeConfig(),
			secretConfig:   emptyConfig,
			expectErr:      true,
		},
		{
			name:           "Error should be reported when it failed to parse Azure cloud-config",
			existingConfig: getTestCloudConfigTypeMergeConfig(),
			secretConfig:   badConfig,
			expectErr:      true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			az := &Cloud{
				KubeClient: fakeclient.NewSimpleClientset(),
				InitSecretConfig: InitSecretConfig{
					SecretName:      "azure-cloud-provider",
					SecretNamespace: "kube-system",
					CloudConfigKey:  "cloud-config",
				},
			}
			if test.existingConfig != nil {
				az.Config = *test.existingConfig
			}
			if test.secretConfig != nil {
				secret := &v1.Secret{
					Type: v1.SecretTypeOpaque,
					ObjectMeta: metav1.ObjectMeta{
						Name:      "azure-cloud-provider",
						Namespace: "kube-system",
					},
				}
				if test.secretConfig != emptyConfig && test.secretConfig != badConfig {
					secretData, err := yaml.Marshal(test.secretConfig)
					assert.NoError(t, err, test.name)
					secret.Data = map[string][]byte{
						"cloud-config": secretData,
					}
				}
				if test.secretConfig == badConfig {
					secret.Data = map[string][]byte{"cloud-config": []byte(`unknown: "hello",unknown: "hello"`)}
				}
				_, err := az.KubeClient.CoreV1().Secrets("kube-system").Create(context.TODO(), secret, metav1.CreateOptions{})
				assert.NoError(t, err, test.name)
			}

			real, err := az.GetConfigFromSecret()
			if test.expectErr {
				assert.Error(t, err, test.name)
				return
			}

			assert.NoError(t, err, test.name)
			assert.Equal(t, test.expected, real, test.name)
		})
	}
}

func TestInitializeCloudFromSecret(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	emptyConfig := &Config{}
	unknownConfigTypeConfig := getTestConfig()
	unknownConfigTypeConfig.CloudConfigType = "UnknownConfigType"
	tests := []struct {
		name           string
		existingConfig *Config
		secretConfig   *Config
		expected       *Config
		expectErr      bool
	}{
		{
			name: "Azure config shouldn't be override when cloud config type is file",
			existingConfig: &Config{
				ResourceGroup:   "ResourceGroup1",
				CloudConfigType: cloudConfigTypeFile,
			},
			secretConfig: getTestConfig(),
			expected:     nil,
		},
		{
			name: "Azure config shouldn't be override when cloud config type is unknown",
			existingConfig: &Config{
				ResourceGroup:   "ResourceGroup1",
				CloudConfigType: "UnknownConfigType",
			},
			secretConfig: unknownConfigTypeConfig,
			expected:     nil,
			expectErr:    true,
		},
		{
			name:           "Azure config should be override when cloud config type is secret",
			existingConfig: getTestCloudConfigTypeSecretConfig(),
			secretConfig:   getTestConfig(),
			expected:       getTestConfig(),
		},
		{
			name:           "Azure config should be override when cloud config type is merge",
			existingConfig: getTestCloudConfigTypeMergeConfig(),
			secretConfig:   getTestConfig(),
			expected:       getTestCloudConfigTypeMergeConfigExpected(),
		},
		{
			name:           "Error should be reported when secret doesn't exists",
			existingConfig: getTestCloudConfigTypeMergeConfig(),
			expectErr:      true,
		},
		{
			name:           "Error should be reported when secret exists but cloud-config data is not provided",
			existingConfig: getTestCloudConfigTypeMergeConfig(),
			secretConfig:   emptyConfig,
			expectErr:      true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			az := &Cloud{
				KubeClient: fakeclient.NewSimpleClientset(),
				InitSecretConfig: InitSecretConfig{
					SecretName:      "azure-cloud-provider",
					SecretNamespace: "kube-system",
					CloudConfigKey:  "cloud-config",
				},
			}
			if test.existingConfig != nil {
				az.Config = *test.existingConfig
			}
			if test.secretConfig != nil {
				secret := &v1.Secret{
					Type: v1.SecretTypeOpaque,
					ObjectMeta: metav1.ObjectMeta{
						Name:      "azure-cloud-provider",
						Namespace: "kube-system",
					},
				}
				if test.secretConfig != emptyConfig {
					secretData, err := yaml.Marshal(test.secretConfig)
					assert.NoError(t, err, test.name)
					secret.Data = map[string][]byte{
						"cloud-config": secretData,
					}
				}
				_, err := az.KubeClient.CoreV1().Secrets("kube-system").Create(context.TODO(), secret, metav1.CreateOptions{})
				assert.NoError(t, err, test.name)
			}

			mockZoneClient := mockzoneclient.NewMockInterface(ctrl)
			mockZoneClient.EXPECT().GetZones(gomock.Any(), gomock.Any()).Return(map[string][]string{"eastus": {"1", "2", "3"}}, nil).MaxTimes(1)
			az.ZoneClient = mockZoneClient

			err := az.InitializeCloudFromSecret(context.Background())
			assert.Equal(t, test.expectErr, err != nil)
		})
	}
}

func TestConfigSecretMetadata(t *testing.T) {
	for _, testCase := range []struct {
		description                                                         string
		secretName, secretNamespace, cloudConfigKey                         string
		expectedsecretName, expectedSsecretNamespace, expectedClouConfigKey string
	}{
		{
			description:              "configSecretMetadata should set the secret metadata from the given parameters",
			secretName:               "cloud-provider-config",
			secretNamespace:          "123456",
			cloudConfigKey:           "azure.json",
			expectedsecretName:       "cloud-provider-config",
			expectedSsecretNamespace: "123456",
			expectedClouConfigKey:    "azure.json",
		},
		{
			description:              "configSecretMetadata should set the secret metadata from the default values",
			expectedsecretName:       consts.DefaultCloudProviderConfigSecName,
			expectedSsecretNamespace: consts.DefaultCloudProviderConfigSecNamespace,
			expectedClouConfigKey:    consts.DefaultCloudProviderConfigSecKey,
		},
	} {
		t.Run(testCase.description, func(t *testing.T) {
			az := &Cloud{}
			az.configSecretMetadata(testCase.secretName, testCase.secretNamespace, testCase.cloudConfigKey)

			assert.Equal(t, testCase.expectedsecretName, az.SecretName)
			assert.Equal(t, testCase.expectedSsecretNamespace, az.SecretNamespace)
			assert.Equal(t, testCase.expectedClouConfigKey, az.CloudConfigKey)
		})
	}
}
