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
	"strings"
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

var _ = Describe("Container Load Balancer Lifecycle", Label(slbTestLabel), func() {
	basename := "slb-lifecycle-test"
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

			eventuallyAzureCleanup(2 * time.Minute)

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
		Eventually(func() error {
			return serviceReconciledErr(firstServiceUID, -1)
		}, waitTime, 10*time.Second).Should(Succeed(),
			"initial service should be reconciled in Azure and the Service Gateway")

		By("Deleting service")
		err = cs.CoreV1().Services(ns.Name).Delete(context.TODO(), serviceName, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Service deleted")

		By("Waiting for service cleanup")
		Eventually(func() error {
			// The K8s Service object keeps our ServiceGateway finalizer until Azure cleanup
			// completes, so it outlives the SGW unregister by ~the PIP-delete duration. Wait
			// for the object to be fully gone (not just unregistered from the SGW) before
			// recreating with the same name, otherwise the Create races the in-progress
			// deletion ("object is being deleted: ... already exists").
			if _, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{}); err == nil {
				return fmt.Errorf("service %s still exists in K8s (deletion in progress)", serviceName)
			}
			return serviceDeletedErr(firstServiceUID)
		}, 90*time.Second, 10*time.Second).Should(Succeed(),
			"first service should be fully deleted from K8s and the Service Gateway")

		By("Recreating service with same name")
		recreatedService, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		secondServiceUID := string(recreatedService.UID)
		utils.Logf("Second service created with UID: %s", secondServiceUID)

		Expect(secondServiceUID).NotTo(Equal(firstServiceUID), "Recreated service should have different UID")

		By("Waiting for Azure to provision new resources")
		Eventually(func() error {
			return serviceReconciledErr(secondServiceUID, -1)
		}, waitTime, 10*time.Second).Should(Succeed(),
			"new service should be reconciled in Azure and the Service Gateway")

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
		Eventually(func() error {
			return serviceReconciledErr(serviceUID, -1)
		}, waitTime, 10*time.Second).Should(Succeed(),
			"service should be reconciled in Azure and the Service Gateway with the initial port")

		By(fmt.Sprintf("Updating service port from %d to %d", initialPort, updatedPort))
		retrievedService, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())

		retrievedService.Spec.Ports[0].Port = updatedPort
		_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), retrievedService, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Service port updated to %d", updatedPort)

		By("Verifying the Azure LB rule now serves the updated frontend port")
		// serviceReconciledErr only proves the PIP/LB/SGW entry still EXISTS, which was already
		// true before the edit — a CCM that silently dropped the port change would pass. Read the
		// actual LB rule instead.
		Eventually(func() ([]int32, error) {
			return getLoadBalancerFrontendPorts(serviceUID)
		}, waitTime, 10*time.Second).Should(Equal([]int32{updatedPort}),
			"the Azure LB rule must serve the updated frontend port")

		Eventually(func() error {
			return serviceReconciledErr(serviceUID, -1)
		}, waitTime, 10*time.Second).Should(Succeed(),
			"service should still be registered in the Service Gateway after port update")

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

		By("Waiting for the Service Gateway to converge on exactly the healthy pods")
		// Assert the exact healthy set. The previous bound accepted anything from
		// totalPods-crashPods up to totalPods, so registering EVERY crashing pod satisfied it -
		// the precise failure this spec is named for. A crashing pod may briefly hold an IP, so
		// convergence is what is asserted: once it is no longer Ready its address must be drained.
		Eventually(func() error {
			if err := serviceReconciledErr(serviceUID, -1); err != nil {
				return err
			}
			healthy, err := cs.CoreV1().Pods(ns.Name).List(context.TODO(), metav1.ListOptions{
				LabelSelector: labels.SelectorFromSet(serviceLabels).String(),
			})
			if err != nil {
				return err
			}
			want := make(map[string]struct{})
			for i := range healthy.Items {
				pod := &healthy.Items[i]
				if !strings.Contains(pod.Name, "-healthy-") {
					continue
				}
				for _, ip := range pod.Status.PodIPs {
					if ip.IP != "" {
						want[ip.IP] = struct{}{}
					}
				}
			}
			if len(want) != totalPods-crashPods {
				return fmt.Errorf("expected %d healthy pod IPs, got %d", totalPods-crashPods, len(want))
			}
			return registeredAddressesMatchErr(serviceUID, want)
		}, waitTime, 10*time.Second).Should(Succeed(),
			"the Service Gateway must register exactly the healthy pods' IPs and drain the crashing ones")

		utils.Logf("\n✓ Pod failure handling test passed: exactly the %d healthy pod IPs are registered", totalPods-crashPods)
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
		Eventually(func() error {
			return serviceReconciledErr(serviceUID, -1)
		}, waitTime, 10*time.Second).Should(Succeed(),
			"v1 service should be reconciled in Azure and the Service Gateway")

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

		By("Waiting for Service Gateway to switch from the v1 pod IPs to the v2 pod IPs")
		// A count alone proves nothing here: both pod sets are the same size, so "15 registered"
		// is already true before the selector change. Assert the exact address set instead, which
		// fails both if a v2 pod is missing and if a v1 pod was left behind.
		v1Pods, err := cs.CoreV1().Pods(ns.Name).List(context.TODO(), metav1.ListOptions{
			LabelSelector: labels.Set(initialLabels).String(),
		})
		Expect(err).NotTo(HaveOccurred())
		v2Pods, err := cs.CoreV1().Pods(ns.Name).List(context.TODO(), metav1.ListOptions{
			LabelSelector: labels.Set(newLabels).String(),
		})
		Expect(err).NotTo(HaveOccurred())

		wantAddrs := podIPSet(v2Pods.Items)
		Expect(wantAddrs).NotTo(BeEmpty(), "the v2 pods must have IPs to compare against")

		Eventually(func() error {
			return registeredAddressesMatchErr(serviceUID, wantAddrs)
		}, waitTime, 10*time.Second).Should(Succeed(),
			"the Service Gateway must register exactly the v2 pod IPs and drop every v1 pod IP")

		utils.Logf("After selector update: exactly the %d v2 pod IPs are registered (%d v1 pods deregistered)",
			len(wantAddrs), len(podIPSet(v1Pods.Items)))

		utils.Logf("\n✓ Service selector update test passed: v1 → v2")
	})
})
