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

		By("Waiting for Azure to provision initial resources and register exactly the initial pods")
		Eventually(func() error {
			if err := verifyAzureResources(serviceUID); err != nil {
				return err
			}
			want, err := livePodIPs(cs, ns.Name, serviceLabels)
			if err != nil {
				return err
			}
			if len(want) != initialPods {
				return fmt.Errorf("expected %d live pod IPs, got %d", initialPods, len(want))
			}
			return registeredAddressesMatchErr(serviceUID, want)
		}, waitTime, 10*time.Second).Should(Succeed(),
			"the Service Gateway must register exactly the initial pods' IPs")

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

		By("Waiting for Service Gateway to register exactly the upscaled pod set")
		Eventually(func() error {
			want, err := livePodIPs(cs, ns.Name, serviceLabels)
			if err != nil {
				return err
			}
			if len(want) != finalPods {
				return fmt.Errorf("expected %d live pod IPs, got %d", finalPods, len(want))
			}
			return registeredAddressesMatchErr(serviceUID, want)
		}, waitTime, 10*time.Second).Should(Succeed(),
			"every upscaled pod IP - and no other - must be registered")

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

		By("Waiting for Azure to provision resources and register exactly the pre-downscale pods")
		Eventually(func() error {
			if err := verifyAzureResources(serviceUID); err != nil {
				return err
			}
			want, err := livePodIPs(cs, ns.Name, serviceLabels)
			if err != nil {
				return err
			}
			if len(want) != initialPods {
				return fmt.Errorf("expected %d live pod IPs, got %d", initialPods, len(want))
			}
			return registeredAddressesMatchErr(serviceUID, want)
		}, waitTime, 10*time.Second).Should(Succeed(),
			"the full pre-downscale pod set must be registered, so the later drain is provably observed")

		By("Capturing the survivors' pod IPs before the downscale")
		// A bare count cannot see the failure that matters here: a stale IP left behind paired
		// with a survivor wrongly dropped also yields exactly finalPods.
		survivorIPs := make(map[string]struct{})
		for i := 0; i < finalPods; i++ {
			pod, getErr := cs.CoreV1().Pods(ns.Name).Get(context.TODO(), fmt.Sprintf("%s-pod-%d", serviceName, i), metav1.GetOptions{})
			Expect(getErr).NotTo(HaveOccurred())
			for _, ip := range pod.Status.PodIPs {
				if ip.IP != "" {
					survivorIPs[ip.IP] = struct{}{}
				}
			}
		}
		Expect(survivorIPs).NotTo(BeEmpty())

		By(fmt.Sprintf("Downscaling: Deleting %d pods (keeping %d)", initialPods-finalPods, finalPods))
		for i := finalPods; i < initialPods; i++ {
			podName := fmt.Sprintf("%s-pod-%d", serviceName, i)
			err := cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), podName, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("Verifying exactly the survivors remain registered")
		Eventually(func() error {
			return registeredAddressesMatchErr(serviceUID, survivorIPs)
		}, waitTime+30*time.Second, 10*time.Second).Should(Succeed(),
			"every deleted pod IP must be deregistered and every survivor must remain")
		utils.Logf("After downscale: exactly the %d survivor pod IPs remain", len(survivorIPs))

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

			By("Waiting for Service Gateway to register exactly the live pods")
			// Assert the exact address SET, not just the count: after a scale-down the two are very
			// different claims. Draining a surviving pod's IP while leaving a deleted pod's IP
			// registered keeps the count correct but blackholes live traffic and routes to a pod
			// that no longer exists, which a count-based assertion cannot see.
			//
			// The live set is polled rather than sampled once: WaitPodsToBeReady returns as soon as
			// no pod is pending, but a scaled-down pod can still be listed for a moment before the
			// API server marks it terminating, which yields one address too many.
			var wantAddrs map[string]struct{}
			Eventually(func() (int, error) {
				livePods, listErr := cs.CoreV1().Pods(ns.Name).List(context.TODO(), metav1.ListOptions{
					LabelSelector: labels.SelectorFromSet(serviceLabels).String(),
				})
				if listErr != nil {
					return 0, listErr
				}
				wantAddrs = podIPSet(livePods.Items)
				return len(wantAddrs), nil
			}, 3*time.Minute, 5*time.Second).Should(Equal(targetPods),
				"scale step %d: expected %d live pod IPs to compare against", stepIdx+1, targetPods)

			Eventually(func() error {
				return registeredAddressesMatchErr(serviceUID, wantAddrs)
			}, waitTime, 10*time.Second).Should(Succeed(),
				"Service Gateway must register exactly the live pod IPs at scale step %d", stepIdx+1)
			registeredPods := targetPods

			utils.Logf("Step %d: Expected %d pods, Service Gateway has %d", stepIdx+1, targetPods, registeredPods)
		}

		utils.Logf("\n✓ Rapid scale test passed: %v", scaleSteps)
	})
})

// livePodIPs returns the address set the Service Gateway should currently hold for the pods
// matching selector. Specs assert against this set rather than a count: on a scale-down the two are
// different claims, and only the set can detect that a surviving pod's IP was drained while a
// deleted pod's IP was left registered.
func livePodIPs(cs clientset.Interface, namespace string, selector map[string]string) (map[string]struct{}, error) {
	pods, err := cs.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(selector).String(),
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
