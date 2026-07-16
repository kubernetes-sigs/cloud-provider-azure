/*
Copyright 2025 The Kubernetes Authors.

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
	"fmt"
	"os"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// CCM-related constants
const (
	// CCMKubeconfigEnvVar is the environment variable for the CCM cluster kubeconfig
	CCMKubeconfigEnvVar = "CCM_KUBECONFIG"

	// CCMNamespaceEnvVar is the environment variable for CCM namespace (defaults to kube-system)
	CCMNamespaceEnvVar = "CCM_NAMESPACE"

	// CCMPodPrefix is the prefix for CCM pod names
	CCMPodPrefix = "cloud-controller-manager"

	// CCMDefaultNamespace is the default namespace where CCM runs
	CCMDefaultNamespace = "69666476eebaaf0001bc891f"

	// CCMRecoveryTimeout is the default timeout for CCM recovery after crash
	CCMRecoveryTimeout = 60 * time.Second

	// CCMRecoveryPollInterval is the interval for polling CCM status
	CCMRecoveryPollInterval = 2 * time.Second
)

// CCMClusterClient provides access to the CCM cluster for crash testing
type CCMClusterClient struct {
	ClientSet clientset.Interface
	Namespace string
}

// CreateCCMKubeClientSet creates a Kubernetes client for the CCM cluster
// This is used for crash testing where we need to access the cluster where CCM runs
// (which may be different from the workload cluster).
// Returns an error if CCM_KUBECONFIG is not set.
func CreateCCMKubeClientSet() (clientset.Interface, error) {
	Logf("Creating kubernetes client for CCM cluster")

	kubeconfigPath := os.Getenv(CCMKubeconfigEnvVar)
	if kubeconfigPath == "" {
		return nil, fmt.Errorf("%s environment variable is not set - CCM crash tests require this to be set to the kubeconfig for the CCM cluster", CCMKubeconfigEnvVar)
	}

	Logf("Using CCM kubeconfig from %s: %s", CCMKubeconfigEnvVar, kubeconfigPath)

	c := clientcmd.GetConfigFromFileOrDie(kubeconfigPath)
	restConfig, err := clientcmd.NewDefaultClientConfig(*c, &clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{Server: ""}}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to create rest config from CCM kubeconfig: %w", err)
	}

	clientSet, err := clientset.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset for CCM cluster: %w", err)
	}

	return clientSet, nil
}

// NewCCMClusterClient creates a new CCM cluster client
func NewCCMClusterClient() (*CCMClusterClient, error) {
	cs, err := CreateCCMKubeClientSet()
	if err != nil {
		return nil, err
	}

	namespace := os.Getenv(CCMNamespaceEnvVar)
	if namespace == "" {
		namespace = CCMDefaultNamespace
	}

	return &CCMClusterClient{
		ClientSet: cs,
		Namespace: namespace,
	}, nil
}

// GetCCMPods returns all CCM pods in the CCM namespace
func (c *CCMClusterClient) GetCCMPods(ctx context.Context) ([]v1.Pod, error) {
	podList, err := c.ClientSet.CoreV1().Pods(c.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods in namespace %s: %w", c.Namespace, err)
	}

	var ccmPods []v1.Pod
	for _, pod := range podList.Items {
		if strings.HasPrefix(pod.Name, CCMPodPrefix) {
			ccmPods = append(ccmPods, pod)
		}
	}

	Logf("Found %d CCM pods with prefix %q in namespace %s", len(ccmPods), CCMPodPrefix, c.Namespace)
	return ccmPods, nil
}

// DeleteAllCCMPods deletes all CCM pods to simulate a crash
func (c *CCMClusterClient) DeleteAllCCMPods(ctx context.Context) error {
	pods, err := c.GetCCMPods(ctx)
	if err != nil {
		return err
	}

	if len(pods) == 0 {
		return fmt.Errorf("no CCM pods found with prefix %q in namespace %s", CCMPodPrefix, c.Namespace)
	}

	for _, pod := range pods {
		Logf("Deleting CCM pod: %s", pod.Name)
		err := c.ClientSet.CoreV1().Pods(c.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
		if err != nil {
			return fmt.Errorf("failed to delete CCM pod %s: %w", pod.Name, err)
		}
	}

	Logf("Deleted %d CCM pods", len(pods))
	return nil
}

// WaitForCCMReady waits for at least one CCM pod to be running and ready
func (c *CCMClusterClient) WaitForCCMReady(ctx context.Context, timeout time.Duration) error {
	Logf("Waiting for CCM to be ready (timeout: %v)", timeout)

	return wait.PollUntilContextTimeout(ctx, CCMRecoveryPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		pods, err := c.GetCCMPods(ctx)
		if err != nil {
			Logf("Error getting CCM pods: %v", err)
			return false, nil // Retry on error
		}

		for _, pod := range pods {
			if pod.Status.Phase == v1.PodRunning {
				// Check if all containers are ready
				allReady := true
				for _, containerStatus := range pod.Status.ContainerStatuses {
					if !containerStatus.Ready {
						allReady = false
						break
					}
				}
				if allReady {
					Logf("CCM pod %s is running and ready", pod.Name)
					return true, nil
				}
			}
		}

		return false, nil
	})
}

// CrashCCMAndWaitForRecovery deletes all CCM pods and waits for recovery
func (c *CCMClusterClient) CrashCCMAndWaitForRecovery(ctx context.Context, recoveryTimeout time.Duration) error {
	Logf("Crashing CCM by deleting all CCM pods...")

	// Get current pods before crash
	podsBefore, err := c.GetCCMPods(ctx)
	if err != nil {
		return fmt.Errorf("failed to get CCM pods before crash: %w", err)
	}
	Logf("CCM pods before crash: %v", getPodNames(podsBefore))

	// Delete all CCM pods
	if err := c.DeleteAllCCMPods(ctx); err != nil {
		return fmt.Errorf("failed to delete CCM pods: %w", err)
	}

	// Wait a brief moment for pods to start terminating
	time.Sleep(2 * time.Second)

	// Wait for new CCM pods to be ready
	if err := c.WaitForCCMReady(ctx, recoveryTimeout); err != nil {
		return fmt.Errorf("CCM failed to recover within %v: %w", recoveryTimeout, err)
	}

	// Get new pods after recovery
	podsAfter, err := c.GetCCMPods(ctx)
	if err != nil {
		return fmt.Errorf("failed to get CCM pods after recovery: %w", err)
	}
	Logf("CCM pods after recovery: %v", getPodNames(podsAfter))

	return nil
}

// GetCCMRecoveryTimeout returns the configured CCM recovery timeout
func GetCCMRecoveryTimeout() time.Duration {
	// Could add environment variable override here if needed
	return CCMRecoveryTimeout
}

// IsCCMClusterConfigured checks if the CCM cluster configuration is available
func IsCCMClusterConfigured() bool {
	return os.Getenv(CCMKubeconfigEnvVar) != ""
}

// getPodNames extracts pod names from a slice of pods
func getPodNames(pods []v1.Pod) []string {
	names := make([]string, len(pods))
	for i, pod := range pods {
		names[i] = pod.Name
	}
	return names
}
