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

var _ = Describe("Container Load Balancer Lifecycle", Label(clbTestLabel), func() {
	basename := "clb-lifecycle-test"
	serviceName := "lifecycle-service"

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

	It("should handle service deletion and recreation", func() {
		const (
			numPods     = 20
			servicePort = int32(8080)
			targetPort  = 8080
			waitTime    = 60 * time.Second
		)

		serviceLabels := map[string]string{
			"app": serviceName,
		}

		By("Creating pods")
		for i := 0; i < numPods; i++ {
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

		By("Waiting for pods to be ready")
		err := utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		By("Creating initial service")
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
		firstServiceUID := string(createdService.UID)
		utils.Logf("First service created with UID: %s", firstServiceUID)

		By("Waiting for Azure to provision initial resources")
		time.Sleep(waitTime)

		By("Verifying initial Service Gateway registration")
		err = verifyAzureResources(firstServiceUID)
		Expect(err).NotTo(HaveOccurred())

		By("Deleting service")
		err = cs.CoreV1().Services(ns.Name).Delete(context.TODO(), serviceName, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Service deleted")

		By("Waiting for service cleanup")
		time.Sleep(90 * time.Second)

		By("Verifying first service UID cleaned up from Service Gateway")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		firstServiceFound := false
		for _, svc := range sgResponse.Value {
			if svc.Name == firstServiceUID {
				firstServiceFound = true
				break
			}
		}
		Expect(firstServiceFound).To(BeFalse(), "First service UID should not exist after deletion")

		By("Recreating service with same name")
		recreatedService, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		secondServiceUID := string(recreatedService.UID)
		utils.Logf("Second service created with UID: %s", secondServiceUID)

		Expect(secondServiceUID).NotTo(Equal(firstServiceUID), "Recreated service should have different UID")

		By("Waiting for Azure to provision new resources")
		time.Sleep(waitTime)

		By("Verifying new Service Gateway registration")
		err = verifyAzureResources(secondServiceUID)
		Expect(err).NotTo(HaveOccurred())

		utils.Logf("\n✓ Service deletion and recreation test passed")
		utils.Logf("  First service UID: %s (cleaned up)", firstServiceUID)
		utils.Logf("  Second service UID: %s (active)", secondServiceUID)
	})

	It("should handle service port updates", func() {
		const (
			numPods     = 15
			initialPort = int32(8080)
			updatedPort = int32(9090)
			targetPort  = 8080
			waitTime    = 60 * time.Second
		)

		serviceLabels := map[string]string{
			"app": serviceName,
		}

		By("Creating pods")
		for i := 0; i < numPods; i++ {
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

		By("Waiting for pods to be ready")
		err := utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		By(fmt.Sprintf("Creating service with initial port %d", initialPort))
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
						Port:       initialPort,
						TargetPort: intstr.FromInt(targetPort),
						Protocol:   v1.ProtocolTCP,
					},
				},
			},
		}

		createdService, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(createdService.UID)

		By("Waiting for initial Azure provisioning")
		time.Sleep(waitTime)

		By("Verifying initial Service Gateway state")
		err = verifyAzureResources(serviceUID)
		Expect(err).NotTo(HaveOccurred())

		By(fmt.Sprintf("Updating service port from %d to %d", initialPort, updatedPort))
		retrievedService, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())

		retrievedService.Spec.Ports[0].Port = updatedPort
		_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), retrievedService, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Service port updated to %d", updatedPort)

		By("Waiting for Azure to process update")
		time.Sleep(waitTime)

		By("Verifying service still registered in Service Gateway after port update")
		err = verifyAzureResources(serviceUID)
		Expect(err).NotTo(HaveOccurred())

		utils.Logf("\n✓ Service port update test passed: %d → %d", initialPort, updatedPort)
	})

	It("should handle pod failures during service provisioning", func() {
		const (
			totalPods   = 30
			crashPods   = 10
			servicePort = int32(8080)
			targetPort  = 8080
			waitTime    = 60 * time.Second
		)

		serviceLabels := map[string]string{
			"app": serviceName,
		}

		By("Creating service first")
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

		By(fmt.Sprintf("Creating %d healthy pods and %d crashing pods", totalPods-crashPods, crashPods))

		// Create healthy pods
		for i := 0; i < totalPods-crashPods; i++ {
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

		// Create crashing pods (invalid command to ensure crash)
		for i := 0; i < crashPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-crash-%d", serviceName, i),
					Namespace: ns.Name,
					Labels:    serviceLabels,
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyNever,
					Containers: []v1.Container{
						{
							Name:            "test-app",
							Image:           utils.AgnhostImage,
							ImagePullPolicy: v1.PullIfNotPresent,
							Command:         []string{"/bin/sh", "-c", "exit 1"},
						},
					},
				},
			}
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("Waiting for healthy pods to be ready")
		time.Sleep(30 * time.Second)

		By("Waiting for Azure provisioning")
		time.Sleep(waitTime)

		By("Verifying Service Gateway only registers healthy pods")
		err = verifyAzureResources(serviceUID)
		Expect(err).NotTo(HaveOccurred())

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

		utils.Logf("Registered pods: %d (expected: %d healthy pods)", registeredPods, totalPods-crashPods)
		Expect(registeredPods).To(Equal(totalPods-crashPods), "Only healthy pods should be registered")

		utils.Logf("\n✓ Pod failure handling test passed: %d healthy pods registered, %d crashed pods ignored", totalPods-crashPods, crashPods)
	})

	It("should handle service selector updates", func() {
		const (
			numInitialPods = 15
			numNewPods     = 15
			servicePort    = int32(8080)
			targetPort     = 8080
			waitTime       = 60 * time.Second
		)

		initialLabels := map[string]string{
			"app":     serviceName,
			"version": "v1",
		}

		newLabels := map[string]string{
			"app":     serviceName,
			"version": "v2",
		}

		By("Creating initial pods with v1 labels")
		for i := 0; i < numInitialPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-v1-%d", serviceName, i),
					Namespace: ns.Name,
					Labels:    initialLabels,
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

		By("Creating service with v1 selector")
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: ns.Name,
			},
			Spec: v1.ServiceSpec{
				Type:                  v1.ServiceTypeLoadBalancer,
				ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
				Selector:              initialLabels,
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

		By("Waiting for pods and Azure provisioning")
		err = utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())
		time.Sleep(waitTime)

		By("Verifying v1 pods registered")
		err = verifyAzureResources(serviceUID)
		Expect(err).NotTo(HaveOccurred())

		By("Creating v2 pods")
		for i := 0; i < numNewPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-v2-%d", serviceName, i),
					Namespace: ns.Name,
					Labels:    newLabels,
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

		err = utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		By("Updating service selector to v2")
		retrievedService, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())

		retrievedService.Spec.Selector = newLabels
		_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), retrievedService, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Service selector updated from v1 to v2")

		By("Waiting for Service Gateway to update")
		time.Sleep(waitTime)

		By("Verifying Service Gateway switched to v2 pods")
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

		utils.Logf("After selector update: %d pods registered (expected %d v2 pods)", registeredPods, numNewPods)
		Expect(registeredPods).To(Equal(numNewPods), "Should have switched to v2 pods")

		utils.Logf("\n✓ Service selector update test passed: v1 → v2")
	})
})
