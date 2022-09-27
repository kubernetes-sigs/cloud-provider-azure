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

package utils

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"

	providerazure "sigs.k8s.io/cloud-provider-azure/pkg/provider"
)

// GetConfigFromSecret gets cloud config from secret
func GetConfigFromSecret(cs clientset.Interface, ns, secretName, secretKey string) (*providerazure.Config, error) {
	secret, err := cs.CoreV1().Secrets(ns).Get(context.TODO(), secretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get secret %s/%s: %w", ns, secretName, err)
	}

	cloudConfigData, ok := secret.Data[secretKey]
	if !ok {
		return nil, fmt.Errorf("cloud-config is not set in the secret (%s/%s)", ns, secretName)
	}

	cloudConfig := providerazure.Config{}
	err = json.Unmarshal(cloudConfigData, &cloudConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to parse Azure cloud-config: %w", err)
	}
	return &cloudConfig, nil
}

// UpdateConfigFromSecret updates cloud config from secret
func UpdateConfigFromSecret(cs clientset.Interface, ns, secretName, secretKey string, config *providerazure.Config) error {
	cloudConfigData, err := json.MarshalIndent(config, "", "    ")
	if err != nil {
		return fmt.Errorf("failed to marshal Azure cloud-config: %w", err)
	}

	secret, err := cs.CoreV1().Secrets(ns).Get(context.TODO(), secretName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get secret %s/%s: %w", ns, secretName, err)
	}

	secret.Data[secretKey] = cloudConfigData
	_, err = cs.CoreV1().Secrets(ns).Update(context.TODO(), secret, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update secret %s/%s: %w", ns, secretName, err)
	}

	return nil
}
