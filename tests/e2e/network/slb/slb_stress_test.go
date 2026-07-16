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

var _ = Describe("Container Load Balancer Stress Tests", Label(slbTestLabel), func() {
	basename := "slb-stress-test"

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

			eventuallyAzureCleanup(6 * time.Minute)

			By("Verifying Service Gateway cleanup")
			verifyServiceGatewayCleanup()

			By("Verifying Address Locations cleanup")
			verifyAddressLocationsCleanup()
		}

		cs = nil
		ns = nil
	})

	It("should handle service churn - rapid create and delete cycles", func() {
		const (
			cycles      = 5
			podsPerSvc  = 5
			servicePort = int32(8080)
			targetPort  = 8080
			waitTime    = 45 * time.Second
		)

		utils.Logf("Testing service churn: %d create/delete cycles", cycles)

		for cycle := 1; cycle <= cycles; cycle++ {
			serviceName := fmt.Sprintf("churn-service-%d", cycle)
			serviceLabels := map[string]string{
				"app":   serviceName,
				"cycle": fmt.Sprintf("%d", cycle),
			}

			By(fmt.Sprintf("Cycle %d/%d: Creating service and pods", cycle, cycles))

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

			// Create pods
			for i := 0; i < podsPerSvc; i++ {
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

			// Wait for provisioning
			err = utils.WaitPodsToBeReady(cs, ns.Name)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() error {
				return serviceReconciledErr(serviceUID, -1)
			}, waitTime, 10*time.Second).Should(Succeed(),
				"service %s should exist in Service Gateway", serviceUID)

			By(fmt.Sprintf("Cycle %d/%d: Deleting service", cycle, cycles))

			// Delete service
			err = cs.CoreV1().Services(ns.Name).Delete(context.TODO(), serviceName, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())

			// Delete pods
			for i := 0; i < podsPerSvc; i++ {
				podName := fmt.Sprintf("%s-pod-%d", serviceName, i)
				err := cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), podName, metav1.DeleteOptions{})
				Expect(err).NotTo(HaveOccurred())
			}

			// Wait for cleanup
			Eventually(func() error {
				return serviceDeletedErr(serviceUID)
			}, waitTime, 10*time.Second).Should(Succeed(),
				"service %s should be removed from Service Gateway", serviceUID)

			utils.Logf("  ✓ Cycle %d/%d complete", cycle, cycles)
		}

		utils.Logf("\n✓ Service churn test passed: %d cycles completed", cycles)
	})

	It("should handle pod crashes during scaling operations", func() {
		const (
			healthyPods  = 20
			crashingPods = 10
			serviceName  = "pod-crash-service"
			servicePort  = int32(8080)
			targetPort   = 8080
			waitTime     = 60 * time.Second
		)

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

		By(fmt.Sprintf("Creating %d healthy pods", healthyPods))
		for i := 0; i < healthyPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-healthy-%d", serviceName, i),
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

		By(fmt.Sprintf("Creating %d pods with crash-restart behavior", crashingPods))
		for i := 0; i < crashingPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-crash-%d", serviceName, i),
					Namespace: ns.Name,
					Labels:    serviceLabels,
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyAlways, // Will keep restarting
					Containers: []v1.Container{
						{
							Name:            "test-app",
							Image:           utils.AgnhostImage,
							ImagePullPolicy: v1.PullIfNotPresent,
							Command:         []string{"/bin/sh", "-c", "sleep 10 && exit 1"}, // Crashes after 10s
						},
					},
				},
			}
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("Waiting for healthy pods to stabilize")
		time.Sleep(30 * time.Second) // Let some crashes happen

		By("Waiting for Azure provisioning")
		var registeredPods int
		Eventually(func() error {
			count, err := countRegisteredEndpoints(serviceUID)
			if err != nil {
				return err
			}
			registeredPods = count
			if registeredPods < healthyPods {
				return fmt.Errorf("registered pods %d, want at least %d", registeredPods, healthyPods)
			}
			if registeredPods > healthyPods+5 {
				return fmt.Errorf("registered pods %d, want at most %d", registeredPods, healthyPods+5)
			}
			return nil
		}, waitTime, 10*time.Second).Should(Succeed(),
			"Service Gateway should only register healthy/ready pods")

		utils.Logf("Registered pods: %d (expected approximately %d healthy pods)", registeredPods, healthyPods)
		// Allow some tolerance - crashing pods might briefly become ready
		Expect(registeredPods).To(BeNumerically(">=", healthyPods))
		Expect(registeredPods).To(BeNumerically("<=", healthyPods+5)) // Small buffer for transient states

		utils.Logf("\n✓ Pod crash handling test passed: only healthy pods registered")
	})

	It("should handle egress pod churn - rapid create and delete cycles", func() {
		const (
			cycles       = 3
			podsPerCycle = 10
			waitTime     = 45 * time.Second
			targetPort   = 8080
		)

		utils.Logf("Testing egress pod churn: %d create/delete cycles", cycles)

		for cycle := 1; cycle <= cycles; cycle++ {
			egressName := fmt.Sprintf("churn-egress-%d", cycle)

			By(fmt.Sprintf("Cycle %d/%d: Creating egress pods", cycle, cycles))

			for i := 0; i < podsPerCycle; i++ {
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("%s-pod-%d", egressName, i),
						Namespace: ns.Name,
						Labels: map[string]string{
							"kubernetes.azure.com/service-egress-gateway": egressName,
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
				_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
			}

			err := utils.WaitPodsToBeReady(cs, ns.Name)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() error {
				return egressRegisteredErr(egressName, -1)
			}, waitTime, 10*time.Second).Should(Succeed(),
				"NAT Gateway for '%s' should exist", egressName)

			By(fmt.Sprintf("Cycle %d/%d: Deleting egress pods", cycle, cycles))

			for i := 0; i < podsPerCycle; i++ {
				podName := fmt.Sprintf("%s-pod-%d", egressName, i)
				err := cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), podName, metav1.DeleteOptions{})
				Expect(err).NotTo(HaveOccurred())
			}

			Eventually(func() error {
				egressPodCount, err := countRegisteredEndpoints(egressName)
				if err != nil {
					return err
				}
				if egressPodCount != 0 {
					return fmt.Errorf("egress %s has %d pod(s), want 0", egressName, egressPodCount)
				}
				return nil
			}, waitTime, 10*time.Second).Should(Succeed(),
				"no pods should remain for egress '%s'", egressName)

			utils.Logf("  ✓ Cycle %d/%d complete", cycle, cycles)
		}

		utils.Logf("\n✓ Egress pod churn test passed: %d cycles completed", cycles)
	})
})
