package config

import (
	"context"
	"fmt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

func NewSecretConfigLoader(configLoader ConfigLoader, kubeClient clientset.Interface, secretName, secretNamespace, cloudConfigKey string) ConfigLoader {
	if secretName == "" {
		secretName = consts.DefaultCloudProviderConfigSecName
	}
	if secretNamespace == "" {
		secretNamespace = consts.DefaultCloudProviderConfigSecNamespace
	}
	if cloudConfigKey == "" {
		cloudConfigKey = consts.DefaultCloudProviderConfigSecKey
	}

	return &SecretConfigLoader{
		ConfigLoader:    configLoader,
		kubeClient:      kubeClient,
		SecretName:      secretName,
		SecretNamespace: secretNamespace,
		CloudConfigKey:  cloudConfigKey,
	}
}

type SecretConfigLoader struct {
	ConfigLoader
	kubeClient      clientset.Interface
	SecretName      string `json:"secretName,omitempty" yaml:"secretName,omitempty"`
	SecretNamespace string `json:"secretNamespace,omitempty" yaml:"secretNamespace,omitempty"`
	CloudConfigKey  string `json:"cloudConfigKey,omitempty" yaml:"cloudConfigKey,omitempty"`
}

func (loader *SecretConfigLoader) LoadConfig(ctx context.Context) (*Config, error) {
	secret, err := loader.kubeClient.CoreV1().Secrets(loader.SecretNamespace).Get(ctx, loader.SecretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get secret %s/%s: %w", loader.SecretNamespace, loader.SecretName, err)
	}

	cloudConfigData, ok := secret.Data[loader.CloudConfigKey]
	if !ok {
		return nil, fmt.Errorf("cloud-config is not set in the secret (%s/%s)", loader.SecretNamespace, loader.SecretName)
	}
	return loader.ParseConfig(ctx, cloudConfigData)
}
