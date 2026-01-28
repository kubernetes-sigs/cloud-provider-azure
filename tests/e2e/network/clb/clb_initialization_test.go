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

package network

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

// CCM Initialization test label
const clbInitTestLabel = "CLB-Init"

// Constants for initialization tests
const (
	ccmDeploymentName           = "cloud-controller-manager"
	ccmContainerName            = "cloud-controller-manager"
	cloudProviderConfigSecret   = "cloud-provider-config"
	initReconciliationTimeout   = 5 * time.Minute
	initReconciliationPollDelay = 5 * time.Second
	// CCMImageEnvVar is the environment variable to specify the custom CCM image
	CCMImageEnvVar = "CCM_IMAGE"
)

// getCCMImage returns the custom CCM image from environment variable or empty string if not set
func getCCMImage() string {
	return os.Getenv(CCMImageEnvVar)
}

// getCloudProviderConfig retrieves the cloud-provider-config secret and returns the azure.json content
func getCloudProviderConfig(ctx context.Context, ccmClient *utils.CCMClusterClient) (map[string]interface{}, error) {
	secret, err := ccmClient.ClientSet.CoreV1().Secrets(ccmClient.Namespace).Get(
		ctx,
		cloudProviderConfigSecret,
		metav1.GetOptions{},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get cloud-provider-config secret: %w", err)
	}

	azureJSON, ok := secret.Data["azure.json"]
	if !ok {
		return nil, fmt.Errorf("azure.json not found in cloud-provider-config secret")
	}

	var config map[string]interface{}
	if err := json.Unmarshal(azureJSON, &config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal azure.json: %w", err)
	}

	return config, nil
}

// updateCloudProviderConfig updates the cloud-provider-config secret with the given config
func updateCloudProviderConfig(ctx context.Context, ccmClient *utils.CCMClusterClient, config map[string]interface{}) error {
	utils.Logf("Updating cloud-provider-config secret with CLB settings")

	azureJSON, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal azure.json: %w", err)
	}

	secret, err := ccmClient.ClientSet.CoreV1().Secrets(ccmClient.Namespace).Get(
		ctx,
		cloudProviderConfigSecret,
		metav1.GetOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to get cloud-provider-config secret: %w", err)
	}

	secret.Data["azure.json"] = azureJSON

	_, err = ccmClient.ClientSet.CoreV1().Secrets(ccmClient.Namespace).Update(
		ctx,
		secret,
		metav1.UpdateOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to update cloud-provider-config secret: %w", err)
	}

	utils.Logf("Successfully updated cloud-provider-config secret")
	return nil
}

// enableCLBConfig modifies the cloud-provider-config to enable CLB/Service Gateway features
func enableCLBConfig(config map[string]interface{}, serviceGatewayName string) map[string]interface{} {
	config["loadBalancerSku"] = "service"
	config["loadBalancerBackendPoolConfigurationType"] = "podIP"
	config["disableOutboundSNAT"] = false
	config["serviceGatewayEnabled"] = true
	config["serviceGatewayResourceName"] = serviceGatewayName
	return config
}

// getOriginalCloudProviderConfigJSON returns the raw JSON bytes from the secret for backup
func getOriginalCloudProviderConfigJSON(ctx context.Context, ccmClient *utils.CCMClusterClient) ([]byte, error) {
	secret, err := ccmClient.ClientSet.CoreV1().Secrets(ccmClient.Namespace).Get(
		ctx,
		cloudProviderConfigSecret,
		metav1.GetOptions{},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get cloud-provider-config secret: %w", err)
	}

	azureJSON, ok := secret.Data["azure.json"]
	if !ok {
		return nil, fmt.Errorf("azure.json not found in cloud-provider-config secret")
	}

	return azureJSON, nil
}

// restoreCloudProviderConfig restores the original cloud-provider-config secret
func restoreCloudProviderConfig(ctx context.Context, ccmClient *utils.CCMClusterClient, originalJSON []byte) error {
	utils.Logf("Restoring original cloud-provider-config secret")

	secret, err := ccmClient.ClientSet.CoreV1().Secrets(ccmClient.Namespace).Get(
		ctx,
		cloudProviderConfigSecret,
		metav1.GetOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to get cloud-provider-config secret: %w", err)
	}

	secret.Data["azure.json"] = originalJSON

	_, err = ccmClient.ClientSet.CoreV1().Secrets(ccmClient.Namespace).Update(
		ctx,
		secret,
		metav1.UpdateOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to restore cloud-provider-config secret: %w", err)
	}

	utils.Logf("Successfully restored original cloud-provider-config secret")
	return nil
}

// logCloudProviderConfig logs the current cloud-provider-config for debugging
func logCloudProviderConfig(config map[string]interface{}) {
	utils.Logf("Cloud Provider Config:")
	utils.Logf("  loadBalancerSku: %v", config["loadBalancerSku"])
	utils.Logf("  loadBalancerBackendPoolConfigurationType: %v", config["loadBalancerBackendPoolConfigurationType"])
	utils.Logf("  disableOutboundSNAT: %v", config["disableOutboundSNAT"])
	utils.Logf("  serviceGatewayEnabled: %v", config["serviceGatewayEnabled"])
	utils.Logf("  serviceGatewayResourceName: %v", config["serviceGatewayResourceName"])
}

// Ensure base64 import is used (for potential future use)
var _ = base64.StdEncoding

// updateCCMImage updates the CCM deployment to use a specific image
func updateCCMImage(ctx context.Context, ccmClient *utils.CCMClusterClient, image string) error {
	utils.Logf("Updating CCM deployment %s to use image: %s", ccmDeploymentName, image)

	deployment, err := ccmClient.ClientSet.AppsV1().Deployments(ccmClient.Namespace).Get(
		ctx,
		ccmDeploymentName,
		metav1.GetOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to get CCM deployment: %w", err)
	}

	// Find and update the CCM container image
	found := false
	for i, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name == ccmContainerName {
			deployment.Spec.Template.Spec.Containers[i].Image = image
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("CCM container %s not found in deployment", ccmContainerName)
	}

	_, err = ccmClient.ClientSet.AppsV1().Deployments(ccmClient.Namespace).Update(
		ctx,
		deployment,
		metav1.UpdateOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to update CCM deployment image: %w", err)
	}

	utils.Logf("Successfully updated CCM deployment to use image: %s", image)
	return nil
}

// getCCMCurrentImage returns the current image of the CCM container
func getCCMCurrentImage(ctx context.Context, ccmClient *utils.CCMClusterClient) (string, error) {
	deployment, err := ccmClient.ClientSet.AppsV1().Deployments(ccmClient.Namespace).Get(
		ctx,
		ccmDeploymentName,
		metav1.GetOptions{},
	)
	if err != nil {
		return "", fmt.Errorf("failed to get CCM deployment: %w", err)
	}

	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name == ccmContainerName {
			return container.Image, nil
		}
	}
	return "", fmt.Errorf("CCM container %s not found in deployment", ccmContainerName)
}

// scaleCCMDeployment scales the CCM deployment to the specified number of replicas
func scaleCCMDeployment(ctx context.Context, ccmClient *utils.CCMClusterClient, replicas int32) error {
	utils.Logf("Scaling CCM deployment %s to %d replicas", ccmDeploymentName, replicas)

	scale := &autoscalingv1.Scale{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ccmDeploymentName,
			Namespace: ccmClient.Namespace,
		},
		Spec: autoscalingv1.ScaleSpec{
			Replicas: replicas,
		},
	}

	_, err := ccmClient.ClientSet.AppsV1().Deployments(ccmClient.Namespace).UpdateScale(
		ctx,
		ccmDeploymentName,
		scale,
		metav1.UpdateOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to scale CCM deployment to %d: %w", replicas, err)
	}

	utils.Logf("Successfully scaled CCM deployment to %d replicas", replicas)
	return nil
}

// waitForCCMFullyDown waits for all CCM pods to be terminated
func waitForCCMFullyDown(ctx context.Context, ccmClient *utils.CCMClusterClient, timeout time.Duration) error {
	utils.Logf("Waiting for CCM to be fully down (timeout: %v)", timeout)

	return wait.PollUntilContextTimeout(ctx, initReconciliationPollDelay, timeout, true, func(ctx context.Context) (bool, error) {
		pods, err := ccmClient.GetCCMPods(ctx)
		if err != nil {
			utils.Logf("Error getting CCM pods: %v", err)
			return false, nil // Retry on error
		}

		if len(pods) == 0 {
			utils.Logf("CCM is fully down (0 pods)")
			return true, nil
		}

		utils.Logf("CCM still has %d pods running, waiting...", len(pods))
		return false, nil
	})
}

// waitForCCMFullyUp waits for at least one CCM pod to be running and ready
func waitForCCMFullyUp(ctx context.Context, ccmClient *utils.CCMClusterClient, timeout time.Duration) error {
	utils.Logf("Waiting for CCM to be fully up (timeout: %v)", timeout)

	return wait.PollUntilContextTimeout(ctx, initReconciliationPollDelay, timeout, true, func(ctx context.Context) (bool, error) {
		pods, err := ccmClient.GetCCMPods(ctx)
		if err != nil {
			utils.Logf("Error getting CCM pods: %v", err)
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
					utils.Logf("CCM pod %s is running and ready", pod.Name)
					return true, nil
				}
			}
		}

		utils.Logf("No ready CCM pods found yet, waiting...")
		return false, nil
	})
}

var _ = Describe("Container Load Balancer Initialization Tests", Label(clbTestLabel, clbInitTestLabel), func() {
	basename := "clb-init"

	var (
		cs                       clientset.Interface
		ccmClient                *utils.CCMClusterClient
		ns                       *v1.Namespace
		ccmOriginalReplicas      int32
		ccmOriginalImage         string
		ccmCustomImage           string
		originalCloudProviderCfg []byte
	)

	BeforeEach(func() {
		var err error

		// First check if CCM cluster is configured
		if !utils.IsCCMClusterConfigured() {
			Skip(fmt.Sprintf("Skipping CCM initialization tests: %s environment variable not set", utils.CCMKubeconfigEnvVar))
		}

		// Create workload cluster client
		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())

		// Create CCM cluster client
		ccmClient, err = utils.NewCCMClusterClient()
		Expect(err).NotTo(HaveOccurred())

		// Create test namespace in workload cluster
		ns, err = utils.CreateTestingNamespace(basename, cs)
		Expect(err).NotTo(HaveOccurred())

		// Get and store original CCM deployment replica count
		ctx := context.Background()
		deployment, err := ccmClient.ClientSet.AppsV1().Deployments(ccmClient.Namespace).Get(
			ctx,
			ccmDeploymentName,
			metav1.GetOptions{},
		)
		Expect(err).NotTo(HaveOccurred())
		ccmOriginalReplicas = *deployment.Spec.Replicas
		utils.Logf("CCM deployment original replica count: %d", ccmOriginalReplicas)

		// Get and store original CCM image, and check for custom image override
		ccmOriginalImage, err = getCCMCurrentImage(ctx, ccmClient)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("CCM deployment original image: %s", ccmOriginalImage)

		ccmCustomImage = getCCMImage()
		if ccmCustomImage != "" {
			utils.Logf("Custom CCM image specified via %s: %s", CCMImageEnvVar, ccmCustomImage)

			// Backup original cloud-provider-config
			originalCloudProviderCfg, err = getOriginalCloudProviderConfigJSON(ctx, ccmClient)
			Expect(err).NotTo(HaveOccurred())
			utils.Logf("Backed up original cloud-provider-config")

			// Get and modify cloud-provider-config to enable CLB
			config, err := getCloudProviderConfig(ctx, ccmClient)
			Expect(err).NotTo(HaveOccurred())

			// Get the serviceGatewayResourceName from existing config or use default
			serviceGatewayName := "my-service-gateway"
			if sgName, ok := config["serviceGatewayResourceName"].(string); ok && sgName != "" {
				serviceGatewayName = sgName
			}

			// Enable CLB settings
			config = enableCLBConfig(config, serviceGatewayName)
			logCloudProviderConfig(config)

			// Update the secret
			err = updateCloudProviderConfig(ctx, ccmClient, config)
			Expect(err).NotTo(HaveOccurred())

			// Update CCM to use custom image
			err = updateCCMImage(ctx, ccmClient, ccmCustomImage)
			Expect(err).NotTo(HaveOccurred())

			// Wait for the new pod to be ready (deployment update triggers rollout)
			utils.Logf("Waiting for CCM pod with new image and config to be ready...")
			time.Sleep(10 * time.Second) // Give deployment time to start rollout
			err = ccmClient.WaitForCCMReady(ctx, utils.CCMRecoveryTimeout)
			Expect(err).NotTo(HaveOccurred())
		} else {
			utils.Logf("No custom CCM image specified, using existing image and config")
		}

		// Verify CCM is running before starting test
		pods, err := ccmClient.GetCCMPods(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(len(pods)).To(BeNumerically(">", 0), "CCM should be running before test starts")
	})

	AfterEach(func() {
		if cs != nil && ns != nil {
			err := utils.DeleteNamespace(cs, ns.Name)
			Expect(err).NotTo(HaveOccurred())

			utils.Logf("Waiting 360 seconds for Azure cleanup (egress gateway cleanup is slower)...")
			time.Sleep(360 * time.Second)

			By("Verifying Service Gateway cleanup")
			verifyServiceGatewayCleanup()

			By("Verifying Address Locations cleanup")
			verifyAddressLocationsCleanup()
		}

		// Restore original configuration and ensure CCM is running
		if ccmClient != nil {
			ctx := context.Background()

			// Restore original cloud-provider-config if it was modified
			if originalCloudProviderCfg != nil {
				err := restoreCloudProviderConfig(ctx, ccmClient, originalCloudProviderCfg)
				if err != nil {
					utils.Logf("Warning: Failed to restore cloud-provider-config: %v", err)
				}
			}

			// Restore original CCM image if it was changed
			if ccmOriginalImage != "" && ccmCustomImage != "" {
				err := updateCCMImage(ctx, ccmClient, ccmOriginalImage)
				if err != nil {
					utils.Logf("Warning: Failed to restore CCM image: %v", err)
				}
			}

			// Scale back to original replicas
			err := scaleCCMDeployment(ctx, ccmClient, ccmOriginalReplicas)
			if err != nil {
				utils.Logf("Warning: Failed to restore CCM replicas: %v", err)
			}

			err = ccmClient.WaitForCCMReady(ctx, utils.CCMRecoveryTimeout)
			if err != nil {
				utils.Logf("Warning: CCM may not be fully recovered after test: %v", err)
			}
		}

		cs = nil
		ccmClient = nil
		ns = nil
	})

	It("should reconcile 5 LoadBalancer services added during CCM downtime", func() {
		const (
			numServices = 5
			podsPerSvc  = 4
			servicePort = int32(8080)
			targetPort  = 8080
		)

		ctx := context.Background()

		By("Scaling CCM to 0 replicas")
		err := scaleCCMDeployment(ctx, ccmClient, 0)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for CCM to be fully down")
		err = waitForCCMFullyDown(ctx, ccmClient, utils.CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())

		By(fmt.Sprintf("Creating %d services in parallel while CCM is down", numServices))
		var wg sync.WaitGroup
		for i := 1; i <= numServices; i++ {
			wg.Add(1)
			go func(svcNum int) {
				defer wg.Done()
				defer GinkgoRecover()

				serviceName := fmt.Sprintf("init-lb-svc-%d", svcNum)
				serviceLabels := map[string]string{
					"app": serviceName,
				}

				// Create pods for this service
				for j := 0; j < podsPerSvc; j++ {
					pod := &v1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:      fmt.Sprintf("%s-pod-%d", serviceName, j),
							Namespace: ns.Name,
							Labels:    serviceLabels,
						},
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:            "test-app",
									Image:           utils.AgnhostImage,
									ImagePullPolicy: v1.PullIfNotPresent,
									Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)},
								},
							},
						},
					}
					_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())
				}

				// Create service
				service := &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      serviceName,
						Namespace: ns.Name,
						Annotations: map[string]string{
							"service.beta.kubernetes.io/azure-load-balancer-backend-pool-type": "clb",
						},
					},
					Spec: v1.ServiceSpec{
						Type:     v1.ServiceTypeLoadBalancer,
						Selector: serviceLabels,
						Ports: []v1.ServicePort{
							{
								Name:       "http",
								Protocol:   v1.ProtocolTCP,
								Port:       servicePort,
								TargetPort: intstr.FromInt(targetPort),
							},
						},
					},
				}
				_, err := cs.CoreV1().Services(ns.Name).Create(ctx, service, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				utils.Logf("Created service %s with %d pods", serviceName, podsPerSvc)
			}(i)
		}
		wg.Wait()

		By("Scaling CCM to 1 replica")
		err = scaleCCMDeployment(ctx, ccmClient, 1)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for CCM to be fully up")
		err = waitForCCMFullyUp(ctx, ccmClient, utils.CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting 3 minutes for CCM to reconcile all services")
		time.Sleep(3 * time.Minute)

		By("Verifying Azure resources are reconciled (K8s external IP not expected)")
		for i := 1; i <= numServices; i++ {
			serviceName := fmt.Sprintf("init-lb-svc-%d", i)

			// Verify service still exists
			svc, err := cs.CoreV1().Services(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			// Note: K8s service.Status.LoadBalancer.Ingress will NOT be set - this is the known limitation
			// But Azure resources (PIPs, LBs) and Service Gateway endpoints SHOULD be created
			if len(svc.Status.LoadBalancer.Ingress) > 0 {
				utils.Logf("Service %s has K8s external IP: %s (unexpected)",
					serviceName, svc.Status.LoadBalancer.Ingress[0].IP)
			} else {
				utils.Logf("Service %s has no K8s external IP (expected - status not updated)", serviceName)
			}

			// Verify Azure resources (PIPs, LBs) were created
			serviceUID := string(svc.UID)
			err = verifyAzureResources(serviceUID)
			Expect(err).NotTo(HaveOccurred(), "Azure resources should exist for service %s", serviceName)
			utils.Logf("Service %s has Azure PIPs and Load Balancer", serviceName)

			// Verify endpoints match pod count in Service Gateway
			alResponse, err := queryServiceGatewayAddressLocations()
			Expect(err).NotTo(HaveOccurred())

			registeredPods := 0
			for _, location := range alResponse.Value {
				for _, addr := range location.Addresses {
					for _, svcName := range addr.Services {
						if svcName == serviceUID {
							registeredPods++
						}
					}
				}
			}
			utils.Logf("Service %s has %d registered endpoints in Service Gateway (expected %d)",
				serviceName, registeredPods, podsPerSvc)
			Expect(registeredPods).To(Equal(podsPerSvc),
				"Service %s should have %d endpoints in Address Locations", serviceName, podsPerSvc)
		}
	})

	It("should reconcile 4 LoadBalancer services deleted during CCM downtime", func() {
		const (
			numServices = 4
			podsPerSvc  = 3
			servicePort = int32(8080)
			targetPort  = 8080
		)

		ctx := context.Background()
		var serviceUIDs []string

		By(fmt.Sprintf("Creating %d services while CCM is up", numServices))
		for i := 1; i <= numServices; i++ {
			serviceName := fmt.Sprintf("init-lb-del-%d", i)
			serviceLabels := map[string]string{
				"app": serviceName,
			}

			// Create pods
			for j := 0; j < podsPerSvc; j++ {
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("%s-pod-%d", serviceName, j),
						Namespace: ns.Name,
						Labels:    serviceLabels,
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name:            "test-app",
								Image:           utils.AgnhostImage,
								ImagePullPolicy: v1.PullIfNotPresent,
								Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)},
							},
						},
					},
				}
				_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
			}

			// Create service
			service := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName,
					Namespace: ns.Name,
					Annotations: map[string]string{
						"service.beta.kubernetes.io/azure-load-balancer-backend-pool-type": "clb",
					},
				},
				Spec: v1.ServiceSpec{
					Type:     v1.ServiceTypeLoadBalancer,
					Selector: serviceLabels,
					Ports: []v1.ServicePort{
						{
							Name:       "http",
							Protocol:   v1.ProtocolTCP,
							Port:       servicePort,
							TargetPort: intstr.FromInt(targetPort),
						},
					},
				},
			}
			svc, err := cs.CoreV1().Services(ns.Name).Create(ctx, service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			serviceUIDs = append(serviceUIDs, string(svc.UID))
		}

		By("Waiting for services to be established")
		time.Sleep(90 * time.Second)

		By("Verifying all Service Gateway services exist")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())
		for _, serviceUID := range serviceUIDs {
			found := false
			for _, svc := range sgResponse.Value {
				if svc.Name == serviceUID {
					found = true
					break
				}
			}
			Expect(found).To(BeTrue(), "Service Gateway service for %s should exist before deletion", serviceUID)
		}

		By("Scaling CCM to 0 replicas")
		err = scaleCCMDeployment(ctx, ccmClient, 0)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for CCM to be fully down")
		err = waitForCCMFullyDown(ctx, ccmClient, utils.CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())

		By("Deleting all services in parallel while CCM is down")
		var wg sync.WaitGroup
		for i := 1; i <= numServices; i++ {
			wg.Add(1)
			go func(svcNum int) {
				defer wg.Done()
				defer GinkgoRecover()

				serviceName := fmt.Sprintf("init-lb-del-%d", svcNum)
				err := cs.CoreV1().Services(ns.Name).Delete(ctx, serviceName, metav1.DeleteOptions{})
				Expect(err).NotTo(HaveOccurred())
				utils.Logf("Deleted service %s", serviceName)
			}(i)
		}
		wg.Wait()

		By("Scaling CCM to 1 replica")
		err = scaleCCMDeployment(ctx, ccmClient, 1)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for CCM to be fully up")
		err = waitForCCMFullyUp(ctx, ccmClient, utils.CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting 5 minutes for deletion reconciliation")
		time.Sleep(5 * time.Minute)

		By("Verifying all services are deleted from K8s")
		for i := 1; i <= numServices; i++ {
			serviceName := fmt.Sprintf("init-lb-del-%d", i)
			_, err := cs.CoreV1().Services(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
			Expect(err).To(HaveOccurred(), "Service %s should be deleted from K8s", serviceName)
		}

		By("Verifying all Service Gateway services are gone")
		sgResponse, err = queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())
		for _, serviceUID := range serviceUIDs {
			for _, svc := range sgResponse.Value {
				Expect(svc.Name).NotTo(Equal(serviceUID),
					"Service Gateway service for %s should be deleted", serviceUID)
			}
		}

		By("Verifying Service Gateway cleanup")
		verifyServiceGatewayCleanup()
	})

	It("should reconcile 5 egress gateways added during CCM downtime", func() {
		const (
			podsPerGateway = 5
			targetPort     = 8080
		)

		egressGateways := []string{
			"init-egress-alpha",
			"init-egress-beta",
			"init-egress-gamma",
			"init-egress-delta",
			"init-egress-epsilon",
		}

		ctx := context.Background()

		By("Scaling CCM to 0 replicas")
		err := scaleCCMDeployment(ctx, ccmClient, 0)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for CCM to be fully down")
		err = waitForCCMFullyDown(ctx, ccmClient, utils.CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())

		By(fmt.Sprintf("Creating %d egress gateways in parallel while CCM is down", len(egressGateways)))
		var wg sync.WaitGroup
		for _, egressName := range egressGateways {
			wg.Add(1)
			go func(egress string) {
				defer wg.Done()
				defer GinkgoRecover()

				for i := 0; i < podsPerGateway; i++ {
					pod := &v1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:      fmt.Sprintf("%s-pod-%d", egress, i),
							Namespace: ns.Name,
							Labels: map[string]string{
								egressLabel: egress,
							},
						},
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:            "test-app",
									Image:           utils.AgnhostImage,
									ImagePullPolicy: v1.PullIfNotPresent,
									Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)},
								},
							},
						},
					}
					_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())
				}
				utils.Logf("Created egress gateway %s with %d pods", egress, podsPerGateway)
			}(egressName)
		}
		wg.Wait()

		By("Scaling CCM to 1 replica")
		err = scaleCCMDeployment(ctx, ccmClient, 1)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for CCM to be fully up")
		err = waitForCCMFullyUp(ctx, ccmClient, utils.CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting 3 minutes for CCM to reconcile egress gateways")
		time.Sleep(3 * time.Minute)

		By("Verifying all egress gateway services exist in Service Gateway with NAT Gateways")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		foundGateways := 0
		for _, svc := range sgResponse.Value {
			if svc.Properties.ServiceType == "Outbound" {
				for _, egressName := range egressGateways {
					if svc.Name == egressName {
						foundGateways++
						utils.Logf("Found egress gateway '%s' with NAT Gateway: %s",
							egressName, svc.Properties.PublicNatGatewayID)
						Expect(svc.Properties.PublicNatGatewayID).NotTo(BeEmpty(),
							"NAT Gateway ID should exist for %s", egressName)
					}
				}
			}
		}
		Expect(foundGateways).To(Equal(len(egressGateways)),
			"All %d egress gateways should exist in Service Gateway", len(egressGateways))

		By("Verifying all egress gateways have correct number of registered pods")
		alResponse, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())

		for _, egressName := range egressGateways {
			registeredPods := 0
			for _, location := range alResponse.Value {
				for _, addr := range location.Addresses {
					for _, svcName := range addr.Services {
						if svcName == egressName {
							registeredPods++
						}
					}
				}
			}
			utils.Logf("Egress gateway '%s' has %d registered pods (expected %d)",
				egressName, registeredPods, podsPerGateway)
			Expect(registeredPods).To(Equal(podsPerGateway),
				"Egress gateway %s should have %d registered pods", egressName, podsPerGateway)
		}
	})

	It("should reconcile 4 egress gateways deleted during CCM downtime", func() {
		const (
			podsPerGateway = 4
			targetPort     = 8080
		)

		egressGateways := []string{
			"init-egress-del-w",
			"init-egress-del-x",
			"init-egress-del-y",
			"init-egress-del-z",
		}

		ctx := context.Background()

		By(fmt.Sprintf("Creating %d egress gateways while CCM is up", len(egressGateways)))
		for _, egressName := range egressGateways {
			for i := 0; i < podsPerGateway; i++ {
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("%s-pod-%d", egressName, i),
						Namespace: ns.Name,
						Labels: map[string]string{
							egressLabel: egressName,
						},
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name:            "test-app",
								Image:           utils.AgnhostImage,
								ImagePullPolicy: v1.PullIfNotPresent,
								Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)},
							},
						},
					},
				}
				_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
			}
		}

		By("Waiting for NAT Gateways to be provisioned")
		time.Sleep(90 * time.Second)

		By("Verifying all egress gateways exist before deletion")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())
		for _, egressName := range egressGateways {
			found := false
			for _, svc := range sgResponse.Value {
				if svc.Properties.ServiceType == "Outbound" && svc.Name == egressName {
					found = true
					utils.Logf("Egress gateway '%s' exists with NAT Gateway: %s",
						egressName, svc.Properties.PublicNatGatewayID)
					break
				}
			}
			Expect(found).To(BeTrue(), "Egress gateway %s should exist before deletion", egressName)
		}

		By("Scaling CCM to 0 replicas")
		err = scaleCCMDeployment(ctx, ccmClient, 0)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for CCM to be fully down")
		err = waitForCCMFullyDown(ctx, ccmClient, utils.CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())

		By("Deleting all pods from all egress gateways in parallel while CCM is down")
		var wg sync.WaitGroup
		for _, egressName := range egressGateways {
			wg.Add(1)
			go func(egress string) {
				defer wg.Done()
				defer GinkgoRecover()

				for i := 0; i < podsPerGateway; i++ {
					podName := fmt.Sprintf("%s-pod-%d", egress, i)
					err := cs.CoreV1().Pods(ns.Name).Delete(ctx, podName, metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())
				}
				utils.Logf("Deleted all pods from egress gateway %s", egress)
			}(egressName)
		}
		wg.Wait()

		By("Scaling CCM to 1 replica")
		err = scaleCCMDeployment(ctx, ccmClient, 1)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for CCM to be fully up")
		err = waitForCCMFullyUp(ctx, ccmClient, utils.CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting 5 minutes for deletion reconciliation")
		time.Sleep(5 * time.Minute)

		By("Verifying all pods are deleted from K8s")
		for _, egressName := range egressGateways {
			for i := 0; i < podsPerGateway; i++ {
				podName := fmt.Sprintf("%s-pod-%d", egressName, i)
				_, err := cs.CoreV1().Pods(ns.Name).Get(ctx, podName, metav1.GetOptions{})
				Expect(err).To(HaveOccurred(), "Pod %s should be deleted", podName)
			}
		}

		By("Verifying all outbound services are deleted from Service Gateway")
		sgResponse, err = queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())
		for _, egressName := range egressGateways {
			for _, svc := range sgResponse.Value {
				if svc.Properties.ServiceType == "Outbound" {
					Expect(svc.Name).NotTo(Equal(egressName),
						"Outbound service for %s should be deleted", egressName)
				}
			}
		}

		By("Verifying NAT Gateway cleanup")
		verifyNATGatewayCleanup(egressGateways)
	})

	It("should handle mixed operations during extended downtime", func() {
		const (
			baselinePods   = 3
			newServicePods = 4
			egressPods     = 3
			newEgressPods  = 4
			additionalPods = 3
			servicePort    = int32(8080)
			targetPort     = 8080
		)

		ctx := context.Background()
		baselineServices := []string{"init-mixed-exist-1", "init-mixed-exist-2"}
		newServices := []string{"init-mixed-new-a", "init-mixed-new-b", "init-mixed-new-c"}
		baselineEgress := "init-mixed-egress-exist"
		newEgressGateways := []string{"init-mixed-egress-new-1", "init-mixed-egress-new-2"}

		By("Creating 2 baseline services while CCM is up")
		for _, serviceName := range baselineServices {
			serviceLabels := map[string]string{
				"app": serviceName,
			}

			// Create pods
			for j := 0; j < baselinePods; j++ {
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("%s-pod-%d", serviceName, j),
						Namespace: ns.Name,
						Labels:    serviceLabels,
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name:            "test-app",
								Image:           utils.AgnhostImage,
								ImagePullPolicy: v1.PullIfNotPresent,
								Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)},
							},
						},
					},
				}
				_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
			}

			// Create service
			service := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName,
					Namespace: ns.Name,
					Annotations: map[string]string{
						"service.beta.kubernetes.io/azure-load-balancer-backend-pool-type": "clb",
					},
				},
				Spec: v1.ServiceSpec{
					Type:     v1.ServiceTypeLoadBalancer,
					Selector: serviceLabels,
					Ports: []v1.ServicePort{
						{
							Name:       "http",
							Protocol:   v1.ProtocolTCP,
							Port:       servicePort,
							TargetPort: intstr.FromInt(targetPort),
						},
					},
				},
			}
			_, err := cs.CoreV1().Services(ns.Name).Create(ctx, service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("Creating 1 baseline egress gateway while CCM is up")
		for i := 0; i < egressPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-pod-%d", baselineEgress, i),
					Namespace: ns.Name,
					Labels: map[string]string{
						egressLabel: baselineEgress,
					},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:            "test-app",
							Image:           utils.AgnhostImage,
							ImagePullPolicy: v1.PullIfNotPresent,
							Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)},
						},
					},
				},
			}
			_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("Waiting for all baseline resources to be established")
		time.Sleep(90 * time.Second)

		By("Scaling CCM to 0 replicas")
		err := scaleCCMDeployment(ctx, ccmClient, 0)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for CCM to be fully down")
		err = waitForCCMFullyDown(ctx, ccmClient, utils.CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())

		By("Performing mixed operations in parallel while CCM is down")
		var wg sync.WaitGroup

		// Create 3 new services
		for _, serviceName := range newServices {
			wg.Add(1)
			go func(svcName string) {
				defer wg.Done()
				defer GinkgoRecover()

				serviceLabels := map[string]string{
					"app": svcName,
				}

				// Create pods
				for j := 0; j < newServicePods; j++ {
					pod := &v1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:      fmt.Sprintf("%s-pod-%d", svcName, j),
							Namespace: ns.Name,
							Labels:    serviceLabels,
						},
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:            "test-app",
									Image:           utils.AgnhostImage,
									ImagePullPolicy: v1.PullIfNotPresent,
									Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)},
								},
							},
						},
					}
					_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())
				}

				// Create service
				service := &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      svcName,
						Namespace: ns.Name,
						Annotations: map[string]string{
							"service.beta.kubernetes.io/azure-load-balancer-backend-pool-type": "clb",
						},
					},
					Spec: v1.ServiceSpec{
						Type:     v1.ServiceTypeLoadBalancer,
						Selector: serviceLabels,
						Ports: []v1.ServicePort{
							{
								Name:       "http",
								Protocol:   v1.ProtocolTCP,
								Port:       servicePort,
								TargetPort: intstr.FromInt(targetPort),
							},
						},
					},
				}
				_, err := cs.CoreV1().Services(ns.Name).Create(ctx, service, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				utils.Logf("Created new service %s", svcName)
			}(serviceName)
		}

		// Delete 1 existing service
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer GinkgoRecover()

			err := cs.CoreV1().Services(ns.Name).Delete(ctx, baselineServices[0], metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
			utils.Logf("Deleted service %s", baselineServices[0])
		}()

		// Add 3 more pods to existing service
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer GinkgoRecover()

			existingSvc := baselineServices[1]
			serviceLabels := map[string]string{
				"app": existingSvc,
			}

			for j := baselinePods; j < baselinePods+additionalPods; j++ {
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("%s-pod-%d", existingSvc, j),
						Namespace: ns.Name,
						Labels:    serviceLabels,
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name:            "test-app",
								Image:           utils.AgnhostImage,
								ImagePullPolicy: v1.PullIfNotPresent,
								Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)},
							},
						},
					},
				}
				_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
			}
			utils.Logf("Added %d pods to service %s", additionalPods, existingSvc)
		}()

		// Create 2 new egress gateways
		for _, egressName := range newEgressGateways {
			wg.Add(1)
			go func(egress string) {
				defer wg.Done()
				defer GinkgoRecover()

				for i := 0; i < newEgressPods; i++ {
					pod := &v1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:      fmt.Sprintf("%s-pod-%d", egress, i),
							Namespace: ns.Name,
							Labels: map[string]string{
								egressLabel: egress,
							},
						},
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:            "test-app",
									Image:           utils.AgnhostImage,
									ImagePullPolicy: v1.PullIfNotPresent,
									Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)},
								},
							},
						},
					}
					_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())
				}
				utils.Logf("Created new egress gateway %s", egress)
			}(egressName)
		}

		// Delete existing egress gateway pods
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer GinkgoRecover()

			for i := 0; i < egressPods; i++ {
				podName := fmt.Sprintf("%s-pod-%d", baselineEgress, i)
				err := cs.CoreV1().Pods(ns.Name).Delete(ctx, podName, metav1.DeleteOptions{})
				Expect(err).NotTo(HaveOccurred())
			}
			utils.Logf("Deleted all pods from egress gateway %s", baselineEgress)
		}()

		wg.Wait()

		By("Scaling CCM to 1 replica")
		err = scaleCCMDeployment(ctx, ccmClient, 1)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for CCM to be fully up")
		err = waitForCCMFullyUp(ctx, ccmClient, utils.CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting 5 minutes for full reconciliation of all operations")
		time.Sleep(5 * time.Minute)

		By("Verifying 4 active services exist (3 new + 1 existing)")
		allActiveServices := append(newServices, baselineServices[1])
		for _, serviceName := range allActiveServices {
			svc, err := cs.CoreV1().Services(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "Service %s should exist", serviceName)

			// Note: K8s service.Status.LoadBalancer.Ingress will NOT be set - this is the known limitation
			// But Azure resources should exist
			if len(svc.Status.LoadBalancer.Ingress) > 0 {
				utils.Logf("Service %s has K8s external IP: %s (unexpected)",
					serviceName, svc.Status.LoadBalancer.Ingress[0].IP)
			} else {
				utils.Logf("Service %s has no K8s external IP (expected)", serviceName)
			}

			// Verify Azure resources exist
			serviceUID := string(svc.UID)
			err = verifyAzureResources(serviceUID)
			Expect(err).NotTo(HaveOccurred(), "Azure resources should exist for service %s", serviceName)
			utils.Logf("Service %s has Azure PIPs and Load Balancer", serviceName)
		}

		By("Verifying deleted service is gone")
		_, err = cs.CoreV1().Services(ns.Name).Get(ctx, baselineServices[0], metav1.GetOptions{})
		Expect(err).To(HaveOccurred(), "Service %s should be deleted", baselineServices[0])

		By("Verifying existing service has 6 endpoints")
		alResponse, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())

		existingSvc, err := cs.CoreV1().Services(ns.Name).Get(ctx, baselineServices[1], metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		existingSvcUID := string(existingSvc.UID)

		registeredPods := 0
		for _, location := range alResponse.Value {
			for _, addr := range location.Addresses {
				for _, svcName := range addr.Services {
					if svcName == existingSvcUID {
						registeredPods++
					}
				}
			}
		}
		expectedPods := baselinePods + additionalPods
		utils.Logf("Service %s has %d registered pods (expected %d)", baselineServices[1], registeredPods, expectedPods)
		Expect(registeredPods).To(Equal(expectedPods),
			"Service %s should have %d endpoints", baselineServices[1], expectedPods)

		By("Verifying 2 active egress gateways with 4 pods each")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		foundNewGateways := 0
		for _, svc := range sgResponse.Value {
			if svc.Properties.ServiceType == "Outbound" {
				for _, egressName := range newEgressGateways {
					if svc.Name == egressName {
						foundNewGateways++
						utils.Logf("Found new egress gateway '%s'", egressName)
					}
				}
			}
		}
		Expect(foundNewGateways).To(Equal(len(newEgressGateways)),
			"Both new egress gateways should exist")

		for _, egressName := range newEgressGateways {
			registeredPods := 0
			for _, location := range alResponse.Value {
				for _, addr := range location.Addresses {
					for _, svcName := range addr.Services {
						if svcName == egressName {
							registeredPods++
						}
					}
				}
			}
			utils.Logf("Egress gateway '%s' has %d registered pods (expected %d)",
				egressName, registeredPods, newEgressPods)
			Expect(registeredPods).To(Equal(newEgressPods),
				"Egress gateway %s should have %d pods", egressName, newEgressPods)
		}

		By("Verifying deleted egress gateway is fully cleaned up")
		verifyNATGatewayCleanup([]string{baselineEgress})
	})

	// This test verifies the fix for the K8s LB finalizer race condition.
	// When a service is created during CCM initialization, our code adds the
	// servicegateway.azure.com/service-cleanup finalizer. However, the K8s
	// service controller has a needsCleanup() check that ONLY looks for the
	// K8s LB finalizer (service.kubernetes.io/load-balancer-cleanup).
	// Without the K8s LB finalizer, when the service is deleted, the K8s controller
	// tries to add its own finalizer (which fails on a deleting object) and never
	// calls EnsureLoadBalancerDeleted, causing the namespace to hang forever.
	// The fix ensures both finalizers are added during initialization.
	It("should complete namespace deletion without hanging when service is created during initialization", func() {
		const (
			numServices = 2
			podsPerSvc  = 2
			servicePort = int32(8080)
			targetPort  = 8080
			// We want to verify deletion completes quickly (well under the 10min timeout)
			// With the bug, it would hang forever; with the fix, it should complete in ~5min
			namespaceDeleteTimeout = 6 * time.Minute
		)

		ctx := context.Background()

		By("Scaling CCM to 0 replicas")
		err := scaleCCMDeployment(ctx, ccmClient, 0)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for CCM to be fully down")
		err = waitForCCMFullyDown(ctx, ccmClient, utils.CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())

		By(fmt.Sprintf("Creating %d services while CCM is down", numServices))
		var wg sync.WaitGroup
		for i := 1; i <= numServices; i++ {
			wg.Add(1)
			go func(svcNum int) {
				defer wg.Done()
				defer GinkgoRecover()

				serviceName := fmt.Sprintf("init-finalizer-svc-%d", svcNum)
				serviceLabels := map[string]string{
					"app": serviceName,
				}

				// Create pods for this service
				for j := 0; j < podsPerSvc; j++ {
					pod := &v1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:      fmt.Sprintf("%s-pod-%d", serviceName, j),
							Namespace: ns.Name,
							Labels:    serviceLabels,
						},
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:            "test-app",
									Image:           utils.AgnhostImage,
									ImagePullPolicy: v1.PullIfNotPresent,
									Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)},
								},
							},
						},
					}
					_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())
				}

				// Create service
				service := &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      serviceName,
						Namespace: ns.Name,
						Annotations: map[string]string{
							"service.beta.kubernetes.io/azure-load-balancer-backend-pool-type": "clb",
						},
					},
					Spec: v1.ServiceSpec{
						Type:     v1.ServiceTypeLoadBalancer,
						Selector: serviceLabels,
						Ports: []v1.ServicePort{
							{
								Name:       "http",
								Protocol:   v1.ProtocolTCP,
								Port:       servicePort,
								TargetPort: intstr.FromInt(targetPort),
							},
						},
					},
				}
				_, err := cs.CoreV1().Services(ns.Name).Create(ctx, service, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				utils.Logf("Created service %s with %d pods", serviceName, podsPerSvc)
			}(i)
		}
		wg.Wait()

		By("Scaling CCM to 1 replica")
		err = scaleCCMDeployment(ctx, ccmClient, 1)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for CCM to be fully up")
		err = waitForCCMFullyUp(ctx, ccmClient, utils.CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting briefly for CCM initialization to process services (add finalizers)")
		// This short delay allows CCM to run initialization and add our finalizers.
		// The bug occurs when our finalizer is added but K8s LB finalizer is not,
		// so we delete quickly after init to trigger the race condition.
		time.Sleep(30 * time.Second)

		By("Verifying services have our ServiceGateway finalizer")
		for i := 1; i <= numServices; i++ {
			serviceName := fmt.Sprintf("init-finalizer-svc-%d", i)
			svc, err := cs.CoreV1().Services(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			// Check for our custom finalizer
			hasServiceGatewayFinalizer := false
			hasK8sLBFinalizer := false
			for _, f := range svc.Finalizers {
				if f == "servicegateway.azure.com/service-cleanup" {
					hasServiceGatewayFinalizer = true
				}
				if f == "service.kubernetes.io/load-balancer-cleanup" {
					hasK8sLBFinalizer = true
				}
			}
			utils.Logf("Service %s finalizers: ServiceGateway=%v, K8sLB=%v",
				serviceName, hasServiceGatewayFinalizer, hasK8sLBFinalizer)

			// With the fix, both finalizers should be present
			Expect(hasServiceGatewayFinalizer).To(BeTrue(),
				"Service %s should have ServiceGateway finalizer", serviceName)
			Expect(hasK8sLBFinalizer).To(BeTrue(),
				"Service %s should have K8s LB finalizer (fix for race condition)", serviceName)
		}

		By("Deleting namespace immediately to trigger potential finalizer race condition")
		// Store namespace name before deleting
		testNsName := ns.Name
		// Set ns to nil to prevent AfterEach from trying to delete again
		ns = nil

		// Start namespace deletion
		err = cs.CoreV1().Namespaces().Delete(ctx, testNsName, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Initiated namespace deletion for %s", testNsName)

		By(fmt.Sprintf("Waiting for namespace deletion to complete (max %v)", namespaceDeleteTimeout))
		// This is the critical assertion: without the fix, this would hang forever
		// because K8s controller never calls EnsureLoadBalancerDeleted
		startTime := time.Now()
		err = wait.PollUntilContextTimeout(ctx, 5*time.Second, namespaceDeleteTimeout, true, func(ctx context.Context) (bool, error) {
			_, err := cs.CoreV1().Namespaces().Get(ctx, testNsName, metav1.GetOptions{})
			if err != nil {
				utils.Logf("Namespace %s deleted after %v", testNsName, time.Since(startTime))
				return true, nil // Namespace is gone
			}

			// Log progress periodically
			elapsed := time.Since(startTime)
			if int(elapsed.Seconds())%30 == 0 {
				utils.Logf("Namespace %s still terminating after %v...", testNsName, elapsed)

				// Check service states for debugging
				svcs, _ := cs.CoreV1().Services(testNsName).List(ctx, metav1.ListOptions{})
				for _, svc := range svcs.Items {
					utils.Logf("  Service %s: deletionTimestamp=%v, finalizers=%v",
						svc.Name, svc.DeletionTimestamp, svc.Finalizers)
				}
			}
			return false, nil
		})

		Expect(err).NotTo(HaveOccurred(),
			"Namespace deletion should complete within %v. If this times out, the K8s LB finalizer race condition bug may have resurfaced.",
			namespaceDeleteTimeout)
		utils.Logf("SUCCESS: Namespace deletion completed without hanging!")

		By("Waiting 60 seconds for Azure cleanup to complete")
		time.Sleep(60 * time.Second)

		By("Verifying Service Gateway cleanup")
		verifyServiceGatewayCleanup()

		By("Verifying Address Locations cleanup")
		verifyAddressLocationsCleanup()
	})
})
