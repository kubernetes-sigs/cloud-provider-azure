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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

var _ = Describe("Container Load Balancer Scale Operations", Label(slbTestLabel), func() {
	basename := "slb-scale-ops-test"
	serviceName := "scale-ops-service"

	var (
		cs clientset.Interface
		ns *v1.Namespace
	)

	BeforeEach(func() {
		var err error
		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())

		ns, err = utils.CreateTestingNamespace(basename, cs)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if cs != nil && ns != nil {
			err := utils.DeleteNamespace(cs, ns.Name)
			Expect(err).NotTo(HaveOccurred())

			utils.Logf("Waiting 120 seconds for Azure cleanup...")
			time.Sleep(120 * time.Second)

			By("Verifying Service Gateway cleanup")
			verifyServiceGatewayCleanup()

			By("Verifying Address Locations cleanup")
			verifyAddressLocationsCleanup()
		}

		cs = nil
		ns = nil
	})

	It("should handle upscaling from 10 to 100 pods dynamically", func() {
		const (
			initialPods = 10
			finalPods   = 100
			servicePort = int32(8080)
			targetPort  = 8080
			waitTime    = 60 * time.Second
		)

		serviceLabels := map[string]string{
			"app": serviceName,
		}

		By(fmt.Sprintf("Creating service with %d initial pods", initialPods))

		// Create service first
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: ns.Name,
			},
			Spec: v1.ServiceSpec{
				Type:                  v1.ServiceTypeLoadBalancer,
				ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
				Selector:              serviceLabels,
				Ports: []v1.ServicePort{
					{
						Port:       servicePort,
						TargetPort: intstr.FromInt(targetPort),
						Protocol:   v1.ProtocolTCP,
					},
				},
			},
		}

		createdService, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(createdService.UID)
		utils.Logf("Service created with UID: %s", serviceUID)

		// Create initial pods
		utils.Logf("Creating %d initial pods", initialPods)
		for i := 0; i < initialPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-pod-%d", serviceName, i),
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
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("Waiting for initial pods to be ready")
		err = utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("All %d initial pods are ready", initialPods)

		By("Waiting for Azure to provision initial resources")
		utils.Logf("Waiting %v for Azure provisioning...", waitTime)
		time.Sleep(waitTime)

		By("Verifying initial Service Gateway state")
		err = verifyAzureResources(serviceUID)
		Expect(err).NotTo(HaveOccurred())

		alResponse, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())

		initialPodIPs := 0
		for _, location := range alResponse.Value {
			for _, addr := range location.Addresses {
				for _, svc := range addr.Services {
					if svc == serviceUID {
						initialPodIPs++
					}
				}
			}
		}
		utils.Logf("Initial state: %d pod IPs registered in Service Gateway", initialPodIPs)
		Expect(initialPodIPs).To(Equal(initialPods))

		By(fmt.Sprintf("Upscaling: Creating additional %d pods (total %d)", finalPods-initialPods, finalPods))
		for i := initialPods; i < finalPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-pod-%d", serviceName, i),
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
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("Waiting for all pods to be ready after upscale")
		err = utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("All %d pods are ready after upscale", finalPods)

		By("Waiting for Service Gateway to register new pods")
		utils.Logf("Waiting %v for Service Gateway updates...", waitTime)
		time.Sleep(waitTime)

		By("Verifying all pods registered in Service Gateway after upscale")
		alResponseFinal, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())

		finalPodIPs := 0
		for _, location := range alResponseFinal.Value {
			for _, addr := range location.Addresses {
				for _, svc := range addr.Services {
					if svc == serviceUID {
						finalPodIPs++
					}
				}
			}
		}
		utils.Logf("After upscale: %d pod IPs registered in Service Gateway", finalPodIPs)
		Expect(finalPodIPs).To(Equal(finalPods), fmt.Sprintf("Expected %d pod IPs, got %d", finalPods, finalPodIPs))

		utils.Logf("\n✓ Upscale test passed: %d → %d pods", initialPods, finalPods)
	})

	It("should handle downscaling from 100 to 10 pods with proper cleanup", func() {
		const (
			initialPods = 100
			finalPods   = 10
			servicePort = int32(8080)
			targetPort  = 8080
			waitTime    = 60 * time.Second
		)

		serviceLabels := map[string]string{
			"app": serviceName,
		}

		By(fmt.Sprintf("Creating service with %d initial pods", initialPods))

		// Create service
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: ns.Name,
			},
			Spec: v1.ServiceSpec{
				Type:                  v1.ServiceTypeLoadBalancer,
				ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
				Selector:              serviceLabels,
				Ports: []v1.ServicePort{
					{
						Port:       servicePort,
						TargetPort: intstr.FromInt(targetPort),
						Protocol:   v1.ProtocolTCP,
					},
				},
			},
		}

		createdService, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(createdService.UID)

		// Create all pods
		utils.Logf("Creating %d initial pods", initialPods)
		for i := 0; i < initialPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-pod-%d", serviceName, i),
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
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("Waiting for all pods to be ready")
		err = utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for Azure to provision resources")
		time.Sleep(waitTime)

		By("Verifying initial Service Gateway state")
		err = verifyAzureResources(serviceUID)
		Expect(err).NotTo(HaveOccurred())

		alResponse, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())

		initialPodIPs := 0
		for _, location := range alResponse.Value {
			for _, addr := range location.Addresses {
				for _, svc := range addr.Services {
					if svc == serviceUID {
						initialPodIPs++
					}
				}
			}
		}
		utils.Logf("Initial state: %d pod IPs registered", initialPodIPs)
		Expect(initialPodIPs).To(Equal(initialPods))

		By(fmt.Sprintf("Downscaling: Deleting %d pods (keeping %d)", initialPods-finalPods, finalPods))
		for i := finalPods; i < initialPods; i++ {
			podName := fmt.Sprintf("%s-pod-%d", serviceName, i)
			err := cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), podName, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("Waiting for pod deletions to complete")
		time.Sleep(30 * time.Second)

		By("Waiting for Service Gateway to deregister deleted pods")
		utils.Logf("Waiting %v for Service Gateway updates...", waitTime)
		time.Sleep(waitTime)

		By("Verifying Service Gateway cleaned up deleted pod IPs")
		alResponseFinal, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())

		finalPodIPs := 0
		for _, location := range alResponseFinal.Value {
			for _, addr := range location.Addresses {
				for _, svc := range addr.Services {
					if svc == serviceUID {
						finalPodIPs++
					}
				}
			}
		}
		utils.Logf("After downscale: %d pod IPs registered in Service Gateway", finalPodIPs)
		Expect(finalPodIPs).To(Equal(finalPods), fmt.Sprintf("Expected %d pod IPs, got %d", finalPods, finalPodIPs))

		utils.Logf("\n✓ Downscale test passed: %d → %d pods", initialPods, finalPods)
	})

	It("should handle rapid scale operations (10→50→100→20→80)", func() {
		const (
			servicePort = int32(8080)
			targetPort  = 8080
			waitTime    = 45 * time.Second
		)

		scaleSteps := []int{10, 50, 100, 20, 80}

		serviceLabels := map[string]string{
			"app": serviceName,
		}

		By("Creating service")
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: ns.Name,
			},
			Spec: v1.ServiceSpec{
				Type:                  v1.ServiceTypeLoadBalancer,
				ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
				Selector:              serviceLabels,
				Ports: []v1.ServicePort{
					{
						Port:       servicePort,
						TargetPort: intstr.FromInt(targetPort),
						Protocol:   v1.ProtocolTCP,
					},
				},
			},
		}

		createdService, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(createdService.UID)

		currentPods := 0
		for stepIdx, targetPods := range scaleSteps {
			By(fmt.Sprintf("Scale step %d/%d: %d → %d pods", stepIdx+1, len(scaleSteps), currentPods, targetPods))

			if targetPods > currentPods {
				// Scale up
				utils.Logf("Scaling up: creating %d new pods", targetPods-currentPods)
				for i := currentPods; i < targetPods; i++ {
					pod := &v1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:      fmt.Sprintf("%s-pod-%d", serviceName, i),
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
					_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())
				}
			} else if targetPods < currentPods {
				// Scale down
				utils.Logf("Scaling down: deleting %d pods", currentPods-targetPods)
				for i := targetPods; i < currentPods; i++ {
					podName := fmt.Sprintf("%s-pod-%d", serviceName, i)
					err := cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), podName, metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())
				}
			}

			currentPods = targetPods

			By("Waiting for pods to stabilize")
			err = utils.WaitPodsToBeReady(cs, ns.Name)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for Service Gateway to update")
			time.Sleep(waitTime)

			By("Verifying Service Gateway state")
			alResponse, err := queryServiceGatewayAddressLocations()
			Expect(err).NotTo(HaveOccurred())

			registeredPods := 0
			for _, location := range alResponse.Value {
				for _, addr := range location.Addresses {
					for _, svc := range addr.Services {
						if svc == serviceUID {
							registeredPods++
						}
					}
				}
			}

			utils.Logf("Step %d: Expected %d pods, Service Gateway has %d", stepIdx+1, targetPods, registeredPods)
			Expect(registeredPods).To(Equal(targetPods), fmt.Sprintf("Scale step %d failed: expected %d pods, got %d", stepIdx+1, targetPods, registeredPods))
		}

		utils.Logf("\n✓ Rapid scale test passed: %v", scaleSteps)
	})
})
