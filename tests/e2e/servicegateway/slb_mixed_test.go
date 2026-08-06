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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

var _ = Describe("Container Load Balancer Mixed Workload", Label(slbTestLabel), func() {
	basename := "slb-mixed-test"

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

			By("Waiting for Azure cleanup")
			eventuallyAzureCleanup(2 * time.Minute)

			By("Verifying Service Gateway cleanup")
			verifyServiceGatewayCleanup()

			By("Verifying Address Locations cleanup")
			verifyAddressLocationsCleanup()
		}

		cs = nil
		ns = nil
	})

	It("should handle 10 services with varying pod counts (small, medium, large)", func() {
		const (
			servicePort = int32(8080)
			targetPort  = 8080
			waitTime    = 90 * time.Second
		)

		type serviceConfig struct {
			name     string
			podCount int
		}

		services := []serviceConfig{
			{"small-svc-1", 5},
			{"small-svc-2", 5},
			{"medium-svc-1", 10},
			{"medium-svc-2", 10},
			{"medium-svc-3", 10},
			{"large-svc-1", 15},
			{"large-svc-2", 15},
			{"xlarge-svc-1", 20},
			{"xlarge-svc-2", 15},
			{"mega-svc", 10},
		}

		totalPods := 0
		for _, svc := range services {
			totalPods += svc.podCount
		}

		utils.Logf("Creating mixed workload: 10 services with %d total pods", totalPods)
		Expect(totalPods).To(BeNumerically("<=", 120), "Total pods must not exceed 120")

		serviceUIDs := make(map[string]string)

		By("Creating all services and pods")
		for _, svcConfig := range services {
			serviceLabels := map[string]string{
				"app": svcConfig.name,
			}

			// Create service
			service := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      svcConfig.name,
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
			serviceUIDs[svcConfig.name] = string(createdService.UID)

			// Create pods for this service
			for i := 0; i < svcConfig.podCount; i++ {
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("%s-pod-%d", svcConfig.name, i),
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

			utils.Logf("Created service %s with %d pods", svcConfig.name, svcConfig.podCount)
		}

		By("Waiting for all pods to be ready")
		err := utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("All %d pods are ready", totalPods)

		By("Waiting for Azure to provision all resources")
		// Assert each service's exact address set. A per-service count cannot tell services apart:
		// registering service A's pod IPs under service B keeps every count correct while routing
		// traffic to the wrong workload.
		Eventually(func() error {
			for _, svcConfig := range services {
				want, err := livePodIPsFor(cs, ns.Name, svcConfig.name)
				if err != nil {
					return err
				}
				if len(want) != svcConfig.podCount {
					return fmt.Errorf("service %s: expected %d live pod IPs, got %d", svcConfig.name, svcConfig.podCount, len(want))
				}
				if err := serviceReconciledMatchErr(serviceUIDs[svcConfig.name], want); err != nil {
					return fmt.Errorf("service %s: %w", svcConfig.name, err)
				}
			}
			return nil
		}, waitTime, 10*time.Second).Should(Succeed(),
			"every service must register exactly its own pods' IPs in Azure and the Service Gateway")

		By("Verifying all services in Service Gateway")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		foundServices := 0
		for _, sgSvc := range sgResponse.Value {
			for svcName, uid := range serviceUIDs {
				if sgSvc.Name == uid {
					foundServices++
					utils.Logf("Found service %s (UID: %s) in Service Gateway", svcName, uid)
					break
				}
			}
		}

		Expect(foundServices).To(Equal(len(services)), fmt.Sprintf("Expected %d services in Service Gateway, found %d", len(services), foundServices))

		By("Verifying each service registered exactly its own pod IPs")
		for svcName, uid := range serviceUIDs {
			want, err := livePodIPsFor(cs, ns.Name, svcName)
			Expect(err).NotTo(HaveOccurred())
			Expect(registeredAddressesMatchErr(uid, want)).To(Succeed(),
				"service %s must register exactly its own pods' IPs", svcName)
			utils.Logf("Service %s: %d pod IPs registered and matched exactly", svcName, len(want))
		}

		utils.Logf("\n✓ Mixed workload test passed: 10 services, %d total pods", totalPods)
	})

	It("should handle services with mixed traffic policies (Local vs Cluster)", func() {
		const (
			numServicesPerPolicy = 5
			podsPerService       = 10
			servicePort          = int32(8080)
			targetPort           = 8080
			waitTime             = 60 * time.Second
		)

		serviceUIDs := make(map[string]string)
		totalPods := numServicesPerPolicy * 2 * podsPerService

		utils.Logf("Creating %d services: %d with Local policy, %d with Cluster policy (%d total pods)",
			numServicesPerPolicy*2, numServicesPerPolicy, numServicesPerPolicy, totalPods)

		By("Creating services with Local traffic policy")
		for i := 0; i < numServicesPerPolicy; i++ {
			serviceName := fmt.Sprintf("local-policy-svc-%d", i)
			serviceLabels := map[string]string{
				"app":    serviceName,
				"policy": "local",
			}

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
			serviceUIDs[serviceName] = string(createdService.UID)

			for j := 0; j < podsPerService; j++ {
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
				_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
			}
		}

		By("Creating services with Cluster traffic policy")
		for i := 0; i < numServicesPerPolicy; i++ {
			serviceName := fmt.Sprintf("cluster-policy-svc-%d", i)
			serviceLabels := map[string]string{
				"app":    serviceName,
				"policy": "cluster",
			}

			service := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName,
					Namespace: ns.Name,
				},
				Spec: v1.ServiceSpec{
					Type:                  v1.ServiceTypeLoadBalancer,
					ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster,
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
			serviceUIDs[serviceName] = string(createdService.UID)

			for j := 0; j < podsPerService; j++ {
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
				_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
			}
		}

		By("Waiting for all pods to be ready")
		err := utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for Azure provisioning")
		Eventually(func() error {
			for svcName, uid := range serviceUIDs {
				want, err := livePodIPsFor(cs, ns.Name, svcName)
				if err != nil {
					return err
				}
				if len(want) != podsPerService {
					return fmt.Errorf("service %s: expected %d live pod IPs, got %d", svcName, podsPerService, len(want))
				}
				if err := serviceReconciledMatchErr(uid, want); err != nil {
					return fmt.Errorf("service %s: %w", svcName, err)
				}
			}
			return nil
		}, waitTime, 10*time.Second).Should(Succeed(),
			"all services should be reconciled in Azure and the Service Gateway")

		By("Verifying all services registered in Service Gateway")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		foundServices := 0
		for _, sgSvc := range sgResponse.Value {
			for svcName, uid := range serviceUIDs {
				if sgSvc.Name == uid {
					foundServices++
					utils.Logf("Found service %s (UID: %s) in Service Gateway", svcName, uid)
					break
				}
			}
		}

		Expect(foundServices).To(Equal(len(serviceUIDs)), fmt.Sprintf("Expected %d services, found %d", len(serviceUIDs), foundServices))

		By("Verifying all pod IPs registered")
		for svcName, uid := range serviceUIDs {
			want, err := livePodIPsFor(cs, ns.Name, svcName)
			Expect(err).NotTo(HaveOccurred())
			Expect(registeredAddressesMatchErr(uid, want)).To(Succeed(),
				"service %s must register exactly its own pods' IPs regardless of traffic policy", svcName)
			utils.Logf("Service %s: %d pod IPs registered and matched exactly", svcName, len(want))
		}

		utils.Logf("\n✓ Mixed traffic policy test passed: %d services (%d Local, %d Cluster), %d total pods",
			len(serviceUIDs), numServicesPerPolicy, numServicesPerPolicy, totalPods)
	})

	It("should handle combined large-scale workload with 8 services and 120 total pods", func() {
		const (
			servicePort = int32(8080)
			targetPort  = 8080
			waitTime    = 90 * time.Second
		)

		type serviceConfig struct {
			name          string
			podCount      int
			trafficPolicy v1.ServiceExternalTrafficPolicyType
		}

		services := []serviceConfig{
			{"alpha-svc", 20, v1.ServiceExternalTrafficPolicyTypeLocal},
			{"beta-svc", 15, v1.ServiceExternalTrafficPolicyTypeCluster},
			{"gamma-svc", 18, v1.ServiceExternalTrafficPolicyTypeLocal},
			{"delta-svc", 12, v1.ServiceExternalTrafficPolicyTypeCluster},
			{"epsilon-svc", 20, v1.ServiceExternalTrafficPolicyTypeLocal},
			{"zeta-svc", 15, v1.ServiceExternalTrafficPolicyTypeCluster},
			{"eta-svc", 10, v1.ServiceExternalTrafficPolicyTypeLocal},
			{"theta-svc", 10, v1.ServiceExternalTrafficPolicyTypeCluster},
		}

		totalPods := 0
		for _, svc := range services {
			totalPods += svc.podCount
		}

		utils.Logf("Creating combined workload: %d services with %d total pods", len(services), totalPods)
		Expect(totalPods).To(Equal(120), "Should use exactly 120 pods")

		serviceUIDs := make(map[string]string)

		By("Creating all services and pods")
		for _, svcConfig := range services {
			serviceLabels := map[string]string{
				"app": svcConfig.name,
			}

			service := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      svcConfig.name,
					Namespace: ns.Name,
				},
				Spec: v1.ServiceSpec{
					Type:                  v1.ServiceTypeLoadBalancer,
					ExternalTrafficPolicy: svcConfig.trafficPolicy,
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
			serviceUIDs[svcConfig.name] = string(createdService.UID)

			for i := 0; i < svcConfig.podCount; i++ {
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("%s-pod-%d", svcConfig.name, i),
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

			utils.Logf("Created service %s (%s policy) with %d pods",
				svcConfig.name, svcConfig.trafficPolicy, svcConfig.podCount)
		}

		By("Waiting for all pods to be ready")
		err := utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("All 120 pods are ready")

		By("Waiting for Azure to provision all resources")
		Eventually(func() error {
			for _, svcConfig := range services {
				want, err := livePodIPsFor(cs, ns.Name, svcConfig.name)
				if err != nil {
					return err
				}
				if len(want) != svcConfig.podCount {
					return fmt.Errorf("service %s: expected %d live pod IPs, got %d", svcConfig.name, svcConfig.podCount, len(want))
				}
				if err := serviceReconciledMatchErr(serviceUIDs[svcConfig.name], want); err != nil {
					return fmt.Errorf("service %s: %w", svcConfig.name, err)
				}
			}
			return nil
		}, waitTime, 10*time.Second).Should(Succeed(),
			"all services should be reconciled in Azure and the Service Gateway")

		By("Verifying all services in Service Gateway")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		foundServices := 0
		for _, sgSvc := range sgResponse.Value {
			for _, uid := range serviceUIDs {
				if sgSvc.Name == uid {
					foundServices++
					break
				}
			}
		}

		Expect(foundServices).To(Equal(len(services)), fmt.Sprintf("Expected %d services, found %d", len(services), foundServices))

		By("Verifying all 120 pod IPs registered")
		totalRegisteredIPs := 0
		for svcName, uid := range serviceUIDs {
			expectedPods := 0
			for _, svcConfig := range services {
				if svcConfig.name == svcName {
					expectedPods = svcConfig.podCount
					break
				}
			}

			want, err := livePodIPsFor(cs, ns.Name, svcName)
			Expect(err).NotTo(HaveOccurred())
			Expect(want).To(HaveLen(expectedPods), "service %s should have %d live pods", svcName, expectedPods)
			Expect(registeredAddressesMatchErr(uid, want)).To(Succeed(),
				"service %s must register exactly its own pods' IPs", svcName)
			totalRegisteredIPs += len(want)
		}

		Expect(totalRegisteredIPs).To(Equal(120), "Should have exactly 120 pod IPs registered")

		utils.Logf("\n✓ Combined large-scale workload test passed: %d services, 120 pods", len(services))
	})
})

// livePodIPsFor returns the address set the given service's pods should have registered. Specs
// compare against this set rather than a per-service count, which cannot detect one service's pods
// being registered under another service's name.
func livePodIPsFor(cs clientset.Interface, namespace, serviceName string) (map[string]struct{}, error) {
	pods, err := cs.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{"app": serviceName}).String(),
	})
	if err != nil {
		return nil, err
	}
	readyPods := make([]v1.Pod, 0, len(pods.Items))
	for i := range pods.Items {
		// Skip pods that are already Terminating: their addresses are being drained, so they
		// are not part of the set the Service Gateway should still hold. Counting them makes
		// a scale-down transiently expect one address too many.
		if pods.Items[i].DeletionTimestamp == nil {
			readyPods = append(readyPods, pods.Items[i])
		}
	}
	return podIPSet(readyPods), nil
}
