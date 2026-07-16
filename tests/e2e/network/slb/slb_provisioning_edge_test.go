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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

// Provisioning-path edge cases: a service that belongs to a different load-balancer controller
// (foreign loadBalancerClass) must be left completely alone, and a service that opts out of node
// ports must still provision under the PodIP model.
var _ = Describe("SLB - Provisioning Edge Cases", Label(slbTestLabel), func() {
	basename := "slb-provisioning-edge-test"

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

	makeNetexecPod := func(name string, labels map[string]string, targetPort int) *v1.Pod {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name, Labels: labels},
			Spec: v1.PodSpec{
				Containers: []v1.Container{{
					Name:            "test-app",
					Image:           utils.AgnhostImage,
					ImagePullPolicy: v1.PullIfNotPresent,
					Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)},
				}},
			},
		}
	}

	It("should ignore a service that belongs to a foreign loadBalancerClass", func() {
		const (
			servicePort = int32(80)
			targetPort  = 8080
		)
		serviceName := "foreign-class-service"
		labels := map[string]string{"app": serviceName}
		foreignClass := "example.com/other-lb"

		By("Creating a pod and a LoadBalancer service with a foreign loadBalancerClass")
		_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), makeNetexecPod(serviceName+"-pod", labels, targetPort), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns.Name},
			Spec: v1.ServiceSpec{
				Type:              v1.ServiceTypeLoadBalancer,
				LoadBalancerClass: &foreignClass,
				Selector:          labels,
				Ports: []v1.ServicePort{{
					Port:       servicePort,
					TargetPort: intstr.FromInt(targetPort),
					Protocol:   v1.ProtocolTCP,
				}},
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(created.UID)
		utils.Logf("Foreign-class service created with UID=%s, class=%s", serviceUID, foreignClass)

		By("Verifying the SGW path never touches the service (no Azure resources, no finalizer, no registration)")
		Consistently(func() error {
			if err := verifyAzureResources(serviceUID); err == nil {
				return fmt.Errorf("unexpected Azure resources exist for foreign-class service %s", serviceUID)
			}
			svc, getErr := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
			if getErr != nil {
				return getErr
			}
			for _, f := range svc.Finalizers {
				if f == "servicegateway.azure.com/service-cleanup" {
					return fmt.Errorf("foreign-class service unexpectedly carries the ServiceGateway finalizer")
				}
			}
			n, cErr := countRegisteredEndpoints(serviceUID)
			if cErr != nil {
				return cErr
			}
			if n != 0 {
				return fmt.Errorf("foreign-class service unexpectedly registered %d endpoints", n)
			}
			return nil
		}, 40*time.Second, 10*time.Second).Should(Succeed(),
			"a service with a foreign loadBalancerClass must be ignored by the Azure SGW path")

		By("Verifying the foreign-class service still deletes cleanly")
		Expect(cs.CoreV1().Services(ns.Name).Delete(context.TODO(), serviceName, metav1.DeleteOptions{})).To(Succeed())
		Eventually(func() bool {
			_, getErr := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
			return getErr != nil
		}, 60*time.Second, 5*time.Second).Should(BeTrue(), "foreign-class service should delete without stranding")

		utils.Logf("\n✓ Foreign loadBalancerClass service was ignored and deleted cleanly")
	})

	It("should provision and register pods when node-port allocation is disabled", func() {
		const (
			numPods     = 3
			servicePort = int32(80)
			targetPort  = 8080
			waitTime    = 90 * time.Second
		)
		serviceName := "no-nodeports-service"
		labels := map[string]string{"app": serviceName}

		By(fmt.Sprintf("Creating %d pods", numPods))
		for i := 0; i < numPods; i++ {
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), makeNetexecPod(fmt.Sprintf("%s-pod-%d", serviceName, i), labels, targetPort), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By("Creating a LoadBalancer service with allocateLoadBalancerNodePorts=false")
		noNodePorts := false
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns.Name},
			Spec: v1.ServiceSpec{
				Type:                          v1.ServiceTypeLoadBalancer,
				AllocateLoadBalancerNodePorts: &noNodePorts,
				Selector:                      labels,
				Ports: []v1.ServicePort{{
					Port:       servicePort,
					TargetPort: intstr.FromInt(targetPort),
					Protocol:   v1.ProtocolTCP,
				}},
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(created.UID)

		By("Verifying the service provisions and registers all pods")
		eventuallyServiceReconciled(serviceUID, numPods, waitTime)

		By("Confirming the service spec carries no node ports")
		svc, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		for _, p := range svc.Spec.Ports {
			Expect(p.NodePort).To(Equal(int32(0)), "no node port should be allocated when disabled")
		}

		utils.Logf("\n✓ Service provisioned with node-port allocation disabled")
	})

	It("should provision an IPv6 single-stack service (skips on an IPv4-only cluster)", func() {
		const (
			numPods     = 2
			servicePort = int32(80)
			targetPort  = 8080
			waitTime    = 90 * time.Second
		)
		serviceName := "ipv6-singlestack-service"
		labels := map[string]string{"app": serviceName}

		By("Creating an IPv6 single-stack LoadBalancer service")
		singleStack := v1.IPFamilyPolicySingleStack
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns.Name},
			Spec: v1.ServiceSpec{
				Type:           v1.ServiceTypeLoadBalancer,
				IPFamilyPolicy: &singleStack,
				IPFamilies:     []v1.IPFamily{v1.IPv6Protocol},
				Selector:       labels,
				Ports: []v1.ServicePort{{
					Port:       servicePort,
					TargetPort: intstr.FromInt(targetPort),
					Protocol:   v1.ProtocolTCP,
				}},
			},
		}
		created, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		if err != nil {
			if strings.Contains(err.Error(), "IPv6") || strings.Contains(err.Error(), "ipFamilies") || strings.Contains(err.Error(), "family") {
				Skip("cluster does not support IPv6 services: " + err.Error())
			}
			Expect(err).NotTo(HaveOccurred())
		}
		if len(created.Spec.IPFamilies) == 0 || created.Spec.IPFamilies[0] != v1.IPv6Protocol {
			Skip("cluster did not honor an IPv6 single-stack request; nothing to validate")
		}
		serviceUID := string(created.UID)

		By("Creating IPv6-reachable pods")
		for i := 0; i < numPods; i++ {
			_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), makeNetexecPod(fmt.Sprintf("%s-pod-%d", serviceName, i), labels, targetPort), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(utils.WaitPodsToBeReady(cs, ns.Name)).To(Succeed())

		By("Verifying the IPv6 service provisions and registers its pods")
		eventuallyServiceReconciled(serviceUID, numPods, waitTime)

		utils.Logf("\n✓ IPv6 single-stack service provisioned and registered %d pods", numPods)
	})
})
