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

package servicegateway

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
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
	utils.Logf("Creating kubernetes client for CCM cluster")

	kubeconfigPath := os.Getenv(CCMKubeconfigEnvVar)
	if kubeconfigPath == "" {
		return nil, fmt.Errorf("%s environment variable is not set - CCM crash tests require this to be set to the kubeconfig for the CCM cluster", CCMKubeconfigEnvVar)
	}

	utils.Logf("Using CCM kubeconfig from %s: %s", CCMKubeconfigEnvVar, kubeconfigPath)

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

	utils.Logf("Found %d CCM pods with prefix %q in namespace %s", len(ccmPods), CCMPodPrefix, c.Namespace)
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
		utils.Logf("Deleting CCM pod: %s", pod.Name)
		err := c.ClientSet.CoreV1().Pods(c.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
		if err != nil {
			return fmt.Errorf("failed to delete CCM pod %s: %w", pod.Name, err)
		}
	}

	utils.Logf("Deleted %d CCM pods", len(pods))
	return nil
}

// WaitForCCMReady waits for at least one CCM pod to be running and ready.
//
// excludeUIDs names pods that existed before a deliberate crash: a pod that is still terminating
// keeps reporting Running with ready containers for its whole grace period, so accepting one
// would let a caller conclude the CCM restarted when the original process is still serving.
func (c *CCMClusterClient) WaitForCCMReady(ctx context.Context, timeout time.Duration, excludeUIDs ...types.UID) error {
	utils.Logf("Waiting for CCM to be ready (timeout: %v)", timeout)

	excluded := make(map[types.UID]struct{}, len(excludeUIDs))
	for _, uid := range excludeUIDs {
		excluded[uid] = struct{}{}
	}

	return wait.PollUntilContextTimeout(ctx, CCMRecoveryPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		pods, err := c.GetCCMPods(ctx)
		if err != nil {
			utils.Logf("Error getting CCM pods: %v", err)
			return false, nil // Retry on error
		}

		for _, pod := range pods {
			if _, isOld := excluded[pod.UID]; isOld {
				continue
			}
			if pod.DeletionTimestamp != nil {
				continue
			}
			if pod.Status.Phase == v1.PodRunning {
				// Check if all containers are ready
				allPodsReady := true
				for _, containerStatus := range pod.Status.ContainerStatuses {
					if !containerStatus.Ready {
						allPodsReady = false
						break
					}
				}
				if allPodsReady {
					utils.Logf("CCM pod %s is running and ready", pod.Name)
					return true, nil
				}
			}
		}

		return false, nil
	})
}

// CrashCCMAndWaitForDown deletes every CCM pod and blocks until all of the pre-existing pods have
// actually terminated, returning their UIDs so a later WaitForCCMReady can require a genuinely new
// pod.
//
// DeleteAllCCMPods only ISSUES the deletes and returns, so a spec that calls it and immediately
// performs "while the CCM is down" actions may in fact be racing a still-running controller: the
// premise the spec depends on is never established, and the spec silently degrades into a
// steady-state test. Use this whenever the downtime window itself is the thing under test.
func (c *CCMClusterClient) CrashCCMAndWaitForDown(ctx context.Context, timeout time.Duration) ([]types.UID, error) {
	podsBefore, err := c.GetCCMPods(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get CCM pods before crash: %w", err)
	}
	oldUIDs := make([]types.UID, 0, len(podsBefore))
	for _, pod := range podsBefore {
		oldUIDs = append(oldUIDs, pod.UID)
	}
	utils.Logf("Crashing CCM and waiting for it to be fully down; pods before crash: %v", getPodNames(podsBefore))

	if err := c.DeleteAllCCMPods(ctx); err != nil {
		return nil, fmt.Errorf("failed to delete CCM pods: %w", err)
	}

	if err := wait.PollUntilContextTimeout(ctx, CCMRecoveryPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		pods, err := c.GetCCMPods(ctx)
		if err != nil {
			return false, nil
		}
		for _, pod := range pods {
			for _, oldUID := range oldUIDs {
				if pod.UID == oldUID {
					return false, nil
				}
			}
		}
		return true, nil
	}); err != nil {
		return nil, fmt.Errorf("pre-crash CCM pods did not terminate within %v: %w", timeout, err)
	}

	utils.Logf("CCM is fully down; every pre-crash pod has terminated")
	return oldUIDs, nil
}

// CrashCCMAndWaitForRecovery deletes all CCM pods and waits for recovery.
//
// Recovery is only accepted from a pod that did not exist before the delete, and the pods that
// did exist must all be gone. Without both checks the helper returns while the original CCM is
// still running out its grace period, and every spec built on it silently degrades into a test
// that never crashed the CCM at all.
func (c *CCMClusterClient) CrashCCMAndWaitForRecovery(ctx context.Context, recoveryTimeout time.Duration) error {
	utils.Logf("Crashing CCM by deleting all CCM pods...")

	// Get current pods before crash
	podsBefore, err := c.GetCCMPods(ctx)
	if err != nil {
		return fmt.Errorf("failed to get CCM pods before crash: %w", err)
	}
	utils.Logf("CCM pods before crash: %v", getPodNames(podsBefore))

	oldUIDs := make([]types.UID, 0, len(podsBefore))
	for _, pod := range podsBefore {
		oldUIDs = append(oldUIDs, pod.UID)
	}

	// Delete all CCM pods
	if err := c.DeleteAllCCMPods(ctx); err != nil {
		return fmt.Errorf("failed to delete CCM pods: %w", err)
	}

	// The crash is only real once every pre-crash pod has actually gone away.
	if err := wait.PollUntilContextTimeout(ctx, CCMRecoveryPollInterval, recoveryTimeout, true, func(ctx context.Context) (bool, error) {
		pods, err := c.GetCCMPods(ctx)
		if err != nil {
			return false, nil
		}
		for _, pod := range pods {
			for _, oldUID := range oldUIDs {
				if pod.UID == oldUID {
					return false, nil
				}
			}
		}
		return true, nil
	}); err != nil {
		return fmt.Errorf("pre-crash CCM pods did not terminate within %v: %w", recoveryTimeout, err)
	}

	// Wait for a genuinely new CCM pod to be ready
	if err := c.WaitForCCMReady(ctx, recoveryTimeout, oldUIDs...); err != nil {
		return fmt.Errorf("CCM failed to recover within %v: %w", recoveryTimeout, err)
	}

	// Get new pods after recovery
	podsAfter, err := c.GetCCMPods(ctx)
	if err != nil {
		return fmt.Errorf("failed to get CCM pods after recovery: %w", err)
	}
	utils.Logf("CCM pods after recovery: %v", getPodNames(podsAfter))

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
