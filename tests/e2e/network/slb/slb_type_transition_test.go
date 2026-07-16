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

// loadBalancerCleanupFinalizer mirrors the upstream service controller's finalizer; the
// difftracker adds it alongside its own so EnsureLoadBalancerDeleted is always invoked.
const (
	serviceGatewayCleanupFinalizer = "servicegateway.azure.com/service-cleanup"
	loadBalancerCleanupFinalizer   = "service.kubernetes.io/load-balancer-cleanup"
)

// A Service can flip between LoadBalancer and ClusterIP at runtime. When it leaves
// LoadBalancer the upstream controller must call EnsureLoadBalancerDeleted (tearing down the
// Azure LB/PIP, deregistering from the Service Gateway, and dropping our finalizers); when it
// returns to LoadBalancer the resources must be re-provisioned against the same Service UID.
// This exercises the deprovision-without-delete and reprovision-on-same-object paths, which the
// create/delete lifecycle specs do not cover.
var _ = Describe("SLB - Service Type Transition", Label(slbTestLabel), func() {
	basename := "slb-type-transition-test"
	serviceName := "type-transition-service"

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
			Expect(utils.DeleteNamespace(cs, ns.Name)).To(Succeed())

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

	It("should deprovision on LoadBalancer->ClusterIP and reprovision on ClusterIP->LoadBalancer", func() {
		const (
			numPods          = 6
			servicePort      = int32(8080)
			targetPort       = 8080
			provisionTimeout = 90 * time.Second
			deleteTimeout    = 90 * time.Second
		)

		serviceLabels := map[string]string{"app": serviceName}

		By("Creating backend pods")
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
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By("Creating a LoadBalancer service")
		// ExternalTrafficPolicy is intentionally left at the default (Cluster) so the
		// LoadBalancer->ClusterIP transition only has to clear the type, not also the policy.
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: ns.Name,
			},
			Spec: v1.ServiceSpec{
				Type:     v1.ServiceTypeLoadBalancer,
				Selector: serviceLabels,
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
		utils.Logf("LoadBalancer service created with UID: %s", serviceUID)

		By("Waiting for Azure to provision the LoadBalancer")
		eventuallyServiceReconciled(serviceUID, numPods, provisionTimeout)

		By("Verifying both ServiceGateway and LoadBalancer finalizers are present")
		Eventually(func() ([]string, error) {
			return getServiceFinalizers(cs, ns.Name, serviceName)
		}, 30*time.Second, defaultPollInterval).Should(
			And(
				ContainElement(serviceGatewayCleanupFinalizer),
				ContainElement(loadBalancerCleanupFinalizer),
			),
			"a provisioned LoadBalancer should carry both cleanup finalizers")

		By("Transitioning the service from LoadBalancer to ClusterIP")
		Eventually(func() error {
			svc, getErr := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
			if getErr != nil {
				return getErr
			}
			svc.Spec.Type = v1.ServiceTypeClusterIP
			// LoadBalancer-only fields must be cleared or the API server rejects the update.
			svc.Spec.ExternalTrafficPolicy = ""
			svc.Spec.HealthCheckNodePort = 0
			for i := range svc.Spec.Ports {
				svc.Spec.Ports[i].NodePort = 0
			}
			_, updErr := cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc, metav1.UpdateOptions{})
			return updErr
		}, 30*time.Second, 2*time.Second).Should(Succeed(),
			"LoadBalancer->ClusterIP update should eventually apply")
		utils.Logf("Service type changed to ClusterIP")

		By("Waiting for Azure resources to be deprovisioned")
		eventuallyServiceDeleted(serviceUID, deleteTimeout)

		By("Verifying the Service Gateway finalizers are dropped while the ClusterIP service remains")
		Eventually(func() ([]string, error) {
			return getServiceFinalizers(cs, ns.Name, serviceName)
		}, 60*time.Second, defaultPollInterval).Should(
			And(
				Not(ContainElement(serviceGatewayCleanupFinalizer)),
				Not(ContainElement(loadBalancerCleanupFinalizer)),
			),
			"a ClusterIP service should not retain LoadBalancer cleanup finalizers")
		clusterIPSvc, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "the service object must survive the ClusterIP transition")
		Expect(clusterIPSvc.Spec.Type).To(Equal(v1.ServiceTypeClusterIP))

		By("Transitioning the service back from ClusterIP to LoadBalancer")
		Eventually(func() error {
			svc, getErr := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
			if getErr != nil {
				return getErr
			}
			svc.Spec.Type = v1.ServiceTypeLoadBalancer
			_, updErr := cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc, metav1.UpdateOptions{})
			return updErr
		}, 30*time.Second, 2*time.Second).Should(Succeed(),
			"ClusterIP->LoadBalancer update should eventually apply")
		utils.Logf("Service type changed back to LoadBalancer")

		By("Waiting for Azure to re-provision the LoadBalancer against the same UID")
		eventuallyServiceReconciled(serviceUID, numPods, provisionTimeout)

		By("Verifying the cleanup finalizers are re-added")
		Eventually(func() ([]string, error) {
			return getServiceFinalizers(cs, ns.Name, serviceName)
		}, 30*time.Second, defaultPollInterval).Should(
			And(
				ContainElement(serviceGatewayCleanupFinalizer),
				ContainElement(loadBalancerCleanupFinalizer),
			),
			"a re-provisioned LoadBalancer should regain both cleanup finalizers")

		utils.Logf("\n✓ Service type transition test passed: LoadBalancer → ClusterIP → LoadBalancer")
	})

	It("should preserve endpoints on a rapid LoadBalancer->ClusterIP->LoadBalancer toggle (recreate-during-deletion)", func() {
		// When the Service flips back to LoadBalancer while the delete is still in flight, the engine
		// replays the create as a recreate. The recreated LoadBalancer must come up with its backend
		// pods, not an empty pool. Unlike the sequential transition above, this test does NOT wait for
		// the deletion to finish before flipping back, so it exercises the RecreateAfterDeletion path.
		// It can never fail spuriously: the endpoints must end up registered regardless of which path
		// is taken.
		const (
			rapidService     = "rapid-toggle-service"
			numPods          = 4
			servicePort      = int32(8080)
			targetPort       = 8080
			provisionTimeout = 120 * time.Second
		)
		labels := map[string]string{"app": rapidService}

		By("Creating backend pods")
		for i := 0; i < numPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-pod-%d", rapidService, i), Namespace: ns.Name, Labels: labels},
				Spec: v1.PodSpec{Containers: []v1.Container{{
					Name:            "test-app",
					Image:           utils.AgnhostImage,
					ImagePullPolicy: v1.PullIfNotPresent,
					Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)},
				}}},
			}
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By("Creating a LoadBalancer service and waiting for it to register its pods")
		svc := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: rapidService, Namespace: ns.Name},
			Spec: v1.ServiceSpec{
				Type:     v1.ServiceTypeLoadBalancer,
				Selector: labels,
				Ports:    []v1.ServicePort{{Port: servicePort, TargetPort: intstr.FromInt(targetPort), Protocol: v1.ProtocolTCP}},
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(created.UID)
		eventuallyServiceReconciled(serviceUID, numPods, provisionTimeout)

		By("Flipping LoadBalancer->ClusterIP and immediately back to LoadBalancer (no settle wait)")
		Eventually(func() error {
			s, getErr := cs.CoreV1().Services(ns.Name).Get(context.TODO(), rapidService, metav1.GetOptions{})
			if getErr != nil {
				return getErr
			}
			s.Spec.Type = v1.ServiceTypeClusterIP
			s.Spec.ExternalTrafficPolicy = ""
			s.Spec.HealthCheckNodePort = 0
			for i := range s.Spec.Ports {
				s.Spec.Ports[i].NodePort = 0
			}
			_, updErr := cs.CoreV1().Services(ns.Name).Update(context.TODO(), s, metav1.UpdateOptions{})
			return updErr
		}, 30*time.Second, 1*time.Second).Should(Succeed(), "LoadBalancer->ClusterIP update should apply")

		// Flip straight back without waiting for the async deletion to finish.
		Eventually(func() error {
			s, getErr := cs.CoreV1().Services(ns.Name).Get(context.TODO(), rapidService, metav1.GetOptions{})
			if getErr != nil {
				return getErr
			}
			s.Spec.Type = v1.ServiceTypeLoadBalancer
			_, updErr := cs.CoreV1().Services(ns.Name).Update(context.TODO(), s, metav1.UpdateOptions{})
			return updErr
		}, 30*time.Second, 1*time.Second).Should(Succeed(), "ClusterIP->LoadBalancer update should apply")
		utils.Logf("Rapid toggle applied; verifying the recreated LoadBalancer keeps its backend pods")

		By("Verifying the recreated LoadBalancer registers all backend pods (not an empty pool)")
		eventuallyServiceReconciled(serviceUID, numPods, provisionTimeout)

		utils.Logf("\n✓ Rapid LoadBalancer→ClusterIP→LoadBalancer toggle preserved all %d endpoints", numPods)
	})
})

// getServiceFinalizers returns the finalizers currently set on the named service.
func getServiceFinalizers(cs clientset.Interface, namespace, name string) ([]string, error) {
	svc, err := cs.CoreV1().Services(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return svc.Finalizers, nil
}
